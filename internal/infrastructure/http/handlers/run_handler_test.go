package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golaunch/internal/application"
	"golaunch/internal/domain/entities"
	middleware "golaunch/internal/infrastructure/http/middlewares"
	"golaunch/internal/queue"
)

// stubProjectRepo satisfies repository.ProjectRepository; only GetByID is
// exercised by the run handler.
type stubProjectRepo struct{ project *entities.Project }

func (s *stubProjectRepo) GetByID(ctx context.Context, id string) (*entities.Project, error) {
	return s.project, nil
}
func (s *stubProjectRepo) Create(ctx context.Context, p *entities.Project) (string, error) {
	return "", nil
}
func (s *stubProjectRepo) UpdateStatus(ctx context.Context, id string, st entities.ProjectStatus) error {
	return nil
}
func (s *stubProjectRepo) UpdatePortAndStatus(ctx context.Context, id string, port int, st entities.ProjectStatus) error {
	return nil
}
func (s *stubProjectRepo) ListByStatus(ctx context.Context, st entities.ProjectStatus) ([]*entities.Project, error) {
	return nil, nil
}
func (s *stubProjectRepo) ListByUser(ctx context.Context, userID string) ([]*entities.Project, error) {
	return nil, nil
}
func (s *stubProjectRepo) ClaimProject(ctx context.Context, id, userID string) error { return nil }
func (s *stubProjectRepo) ListUnclaimed(ctx context.Context, olderThan time.Time) ([]*entities.Project, error) {
	return nil, nil
}
func (s *stubProjectRepo) Delete(ctx context.Context, id string) error { return nil }
func (s *stubProjectRepo) GetBySlug(ctx context.Context, slug string) (*entities.Project, error) {
	return nil, nil
}
func (s *stubProjectRepo) SlugExists(ctx context.Context, slug string) (bool, error) {
	return false, nil
}
func (s *stubProjectRepo) UpdateSlug(ctx context.Context, id, slug string) error { return nil }
func (s *stubProjectRepo) SetCurrentDeployment(ctx context.Context, projectID, deploymentID string) error {
	return nil
}

// TestRunHandlerDisconnectReleasesRequestAndProducer covers both halves of
// the SSE fix at once, which is the point — each half breaks differently.
//
//   - ServeHTTP must RETURN when the client goes away. The old `for line :=
//     range logCh` kept the handler alive until the producer closed the
//     channel, which is what makes http.Server.Shutdown block for its whole
//     timeout: an SSE connection is never idle.
//   - The producer must NOT block afterwards. DeployPipeline.streamLog sends
//     under the *job* context, not the request's, so simply returning would
//     fill the 64-line buffer and then stall a build worker for the rest of
//     the job timeout. Four disconnects would deadlock the pool.
func TestRunHandlerDisconnectReleasesRequestAndProducer(t *testing.T) {
	repo := &stubProjectRepo{project: &entities.Project{ID: "p1", UserID: "u1"}}
	registry := application.NewLogRegistry()

	// A processor that parks: the job stays "running" so nothing closes the
	// log channel behind the test's back.
	release := make(chan struct{})
	wp := queue.NewWorkerPool(1, func(ctx context.Context, job queue.Job) error {
		<-release
		return nil
	}, time.Minute)
	wp.Start()
	defer func() { close(release); wp.Drain(2 * time.Second) }()

	h := NewRunHandler(application.NewRunProjectUseCase(repo, wp, registry, nil), nil)

	reqCtx, disconnect := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/run/p1", nil)
	req.SetPathValue("projectID", "p1")
	req = req.WithContext(middleware.WithUser(reqCtx, &entities.User{ID: "u1"}))

	served := make(chan struct{})
	go func() {
		h.ServeHTTP(context.Background(), httptest.NewRecorder(), req)
		close(served)
	}()

	// Wait for Execute to register the channel before touching it.
	var logCh chan application.LogLine
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ch, ok := registry.Get("p1"); ok {
			logCh = ch
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if logCh == nil {
		t.Fatal("run use case never registered a log channel")
	}

	disconnect()

	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHTTP did not return after client disconnect — Shutdown would hang on this connection")
	}

	// Well past the channel's 64-line buffer: without the drain hand-off
	// these sends block forever and this test times out.
	for i := 0; i < 300; i++ {
		select {
		case logCh <- application.LogLine{Stream: "stdout", Text: "line"}:
		case <-time.After(2 * time.Second):
			t.Fatalf("producer blocked on send %d after client disconnect", i)
		}
	}
	close(logCh)
}

// TestRunHandlerShutdownReleasesRequestAndProducer is the shutdown twin of
// the disconnect test. The client is still connected here — it is the
// process that is going away — and the same two properties must hold:
// ServeHTTP returns (so http.Server.Shutdown can finish rather than waiting
// out its budget on a never-idle SSE connection), and the producer is not
// left blocked behind a consumer that stopped reading.
func TestRunHandlerShutdownReleasesRequestAndProducer(t *testing.T) {
	repo := &stubProjectRepo{project: &entities.Project{ID: "p1", UserID: "u1"}}
	registry := application.NewLogRegistry()

	release := make(chan struct{})
	wp := queue.NewWorkerPool(1, func(ctx context.Context, job queue.Job) error {
		<-release
		return nil
	}, time.Minute)
	wp.Start()
	defer func() { close(release); wp.Drain(2 * time.Second) }()

	shutdown := make(chan struct{})
	h := NewRunHandler(application.NewRunProjectUseCase(repo, wp, registry, nil), shutdown)

	req := httptest.NewRequest(http.MethodGet, "/run/p1", nil)
	req.SetPathValue("projectID", "p1")
	req = req.WithContext(middleware.WithUser(context.Background(), &entities.User{ID: "u1"}))

	served := make(chan struct{})
	go func() {
		h.ServeHTTP(context.Background(), httptest.NewRecorder(), req)
		close(served)
	}()

	var logCh chan application.LogLine
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ch, ok := registry.Get("p1"); ok {
			logCh = ch
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if logCh == nil {
		t.Fatal("run use case never registered a log channel")
	}

	close(shutdown)

	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHTTP did not return on shutdown — Shutdown would wait out its whole budget here")
	}

	for i := 0; i < 300; i++ {
		select {
		case logCh <- application.LogLine{Stream: "stdout", Text: "line"}:
		case <-time.After(2 * time.Second):
			t.Fatalf("producer blocked on send %d after shutdown", i)
		}
	}
	close(logCh)
}

// TestRunHandlerNilShutdownIsInert: a nil channel blocks forever in a
// select, so handlers constructed without one simply never take that branch.
func TestRunHandlerNilShutdownIsInert(t *testing.T) {
	h := NewRunHandler(nil, nil)
	if h.Shutdown != nil {
		t.Fatal("expected a nil shutdown channel to stay nil")
	}
}
