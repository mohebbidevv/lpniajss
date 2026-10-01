package application

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golaunch/internal/domain/entities"
	"golaunch/internal/infrastructure/caddy"
)

// ── fakes ────────────────────────────────────────────────────────────────

type fakeRollbackProjectRepo struct {
	projects map[string]*entities.Project
}

func (r *fakeRollbackProjectRepo) GetByID(ctx context.Context, id string) (*entities.Project, error) {
	p, ok := r.projects[id]
	if !ok {
		return nil, errNotFound
	}
	cp := *p
	return &cp, nil
}
func (r *fakeRollbackProjectRepo) SetCurrentDeployment(ctx context.Context, projectID, deploymentID string) error {
	r.projects[projectID].CurrentDeploymentID = &deploymentID
	return nil
}
func (r *fakeRollbackProjectRepo) UpdateStatus(ctx context.Context, id string, status entities.ProjectStatus) error {
	r.projects[id].Status = status
	return nil
}

// the rest of repository.ProjectRepository, unused by rollback
func (r *fakeRollbackProjectRepo) Create(ctx context.Context, p *entities.Project) (string, error) {
	return "", nil
}
func (r *fakeRollbackProjectRepo) UpdatePortAndStatus(ctx context.Context, id string, port int, status entities.ProjectStatus) error {
	return nil
}
func (r *fakeRollbackProjectRepo) ListByStatus(ctx context.Context, status entities.ProjectStatus) ([]*entities.Project, error) {
	return nil, nil
}
func (r *fakeRollbackProjectRepo) ListByUser(ctx context.Context, userID string) ([]*entities.Project, error) {
	return nil, nil
}
func (r *fakeRollbackProjectRepo) ClaimProject(ctx context.Context, id, userID string) error {
	return nil
}
func (r *fakeRollbackProjectRepo) ListUnclaimed(ctx context.Context, olderThan time.Time) ([]*entities.Project, error) {
	return nil, nil
}
func (r *fakeRollbackProjectRepo) Delete(ctx context.Context, id string) error { return nil }
func (r *fakeRollbackProjectRepo) GetBySlug(ctx context.Context, slug string) (*entities.Project, error) {
	return nil, nil
}
func (r *fakeRollbackProjectRepo) SlugExists(ctx context.Context, slug string) (bool, error) {
	return false, nil
}
func (r *fakeRollbackProjectRepo) UpdateSlug(ctx context.Context, id, slug string) error { return nil }

type fakeRollbackDeploymentRepo struct {
	deployments map[string]*entities.Deployment
	nextID      int
}

func newFakeRollbackDeploymentRepo() *fakeRollbackDeploymentRepo {
	return &fakeRollbackDeploymentRepo{deployments: map[string]*entities.Deployment{}}
}

func (r *fakeRollbackDeploymentRepo) Create(ctx context.Context, d *entities.Deployment) (string, error) {
	r.nextID++
	id := "dep-" + string(rune('0'+r.nextID))
	stored := *d
	stored.ID = id
	r.deployments[id] = &stored
	return id, nil
}
func (r *fakeRollbackDeploymentRepo) GetByID(ctx context.Context, id string) (*entities.Deployment, error) {
	d, ok := r.deployments[id]
	if !ok {
		return nil, errNotFound
	}
	cp := *d
	return &cp, nil
}
func (r *fakeRollbackDeploymentRepo) SetContainerInfo(ctx context.Context, id, containerID string) error {
	r.deployments[id].ContainerID = containerID
	r.deployments[id].Status = entities.DeploymentRunning
	return nil
}
func (r *fakeRollbackDeploymentRepo) SetFailed(ctx context.Context, id, reason string, exitCode *int) error {
	r.deployments[id].Status = entities.DeploymentFailed
	r.deployments[id].FailureReason = reason
	return nil
}
func (r *fakeRollbackDeploymentRepo) UpdateStatus(ctx context.Context, id string, status entities.DeploymentStatus) error {
	r.deployments[id].Status = status
	return nil
}

// unused by rollback
func (r *fakeRollbackDeploymentRepo) GetCurrentForProject(ctx context.Context, projectID string) (*entities.Deployment, error) {
	return nil, nil
}
func (r *fakeRollbackDeploymentRepo) ListByStatus(ctx context.Context, status entities.DeploymentStatus) ([]*entities.Deployment, error) {
	return nil, nil
}
func (r *fakeRollbackDeploymentRepo) ListByProject(ctx context.Context, projectID string) ([]*entities.Deployment, error) {
	return nil, nil
}
func (r *fakeRollbackDeploymentRepo) SetImageRef(ctx context.Context, id, imageRef string) error {
	return nil
}

type fakeRollbackRuntime struct {
	startErr     error
	waitReadyErr error
	stopped      []entities.RuntimeHandle
	removed      []entities.RuntimeHandle
}

func (r *fakeRollbackRuntime) Start(ctx context.Context, spec entities.RuntimeSpec) (entities.RuntimeHandle, error) {
	if r.startErr != nil {
		return "", r.startErr
	}
	return entities.RuntimeHandle("handle-" + spec.DeploymentID), nil
}
func (r *fakeRollbackRuntime) Stop(ctx context.Context, handle entities.RuntimeHandle, timeoutSeconds int) error {
	r.stopped = append(r.stopped, handle)
	return nil
}
func (r *fakeRollbackRuntime) Remove(ctx context.Context, handle entities.RuntimeHandle) error {
	r.removed = append(r.removed, handle)
	return nil
}
func (r *fakeRollbackRuntime) Status(ctx context.Context, handle entities.RuntimeHandle) (entities.RuntimeStatus, error) {
	return entities.RuntimeStatus{State: entities.RuntimeStateRunning}, nil
}
func (r *fakeRollbackRuntime) Endpoint(ctx context.Context, handle entities.RuntimeHandle) (string, error) {
	return string(handle) + ":3000", nil
}
func (r *fakeRollbackRuntime) WaitReady(ctx context.Context, handle entities.RuntimeHandle, timeout time.Duration) error {
	return r.waitReadyErr
}
func (r *fakeRollbackRuntime) Logs(ctx context.Context, handle entities.RuntimeHandle, opts entities.LogOptions) (<-chan entities.LogLine, error) {
	return nil, nil
}
func (r *fakeRollbackRuntime) List(ctx context.Context, labelFilter map[string]string) ([]entities.RuntimeInstance, error) {
	return nil, nil
}
func (r *fakeRollbackRuntime) Ping(ctx context.Context) error { return nil }

func (r *fakeRollbackRuntime) Events(ctx context.Context) (<-chan entities.RuntimeEvent, error) {
	return nil, nil
}

type fakeRollbackBuilder struct {
	existingImages map[string]bool
}

func (b *fakeRollbackBuilder) Build(ctx context.Context, req entities.BuildRequest, logSink func(entities.LogLine)) (string, error) {
	return "", nil
}
func (b *fakeRollbackBuilder) RemoveImage(ctx context.Context, imageRef string) error { return nil }
func (b *fakeRollbackBuilder) ImageExists(ctx context.Context, imageRef string) (bool, error) {
	return b.existingImages[imageRef], nil
}

type fakeRollbackEnvRepo struct{}

func (e *fakeRollbackEnvRepo) ListForProject(ctx context.Context, projectID string) (map[string]string, error) {
	return map[string]string{}, nil
}
func (e *fakeRollbackEnvRepo) ReplaceForProject(ctx context.Context, projectID string, vars map[string]string) error {
	return nil
}

// fakeCaddyAdmin serves just enough of Caddy's routes API for
// RegisterRoute to succeed against it.
func fakeCaddyAdmin(t *testing.T) *caddy.CaddyClient {
	t.Helper()
	routes := []json.RawMessage{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(routes)
		case http.MethodPatch:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	return caddy.NewCaddyClient(srv.URL, "example.com")
}

// ── setup ────────────────────────────────────────────────────────────────

func newRollbackFixture(t *testing.T) (*RollbackDeploymentUseCase, *fakeRollbackProjectRepo, *fakeRollbackDeploymentRepo, *fakeRollbackRuntime) {
	projects := &fakeRollbackProjectRepo{projects: map[string]*entities.Project{
		"proj-1": {ID: "proj-1", UserID: "user-1", Slug: "myapp", Status: entities.StatusRunning},
	}}
	deployments := newFakeRollbackDeploymentRepo()
	runtime := &fakeRollbackRuntime{}
	builder := &fakeRollbackBuilder{existingImages: map[string]bool{"golaunch/myapp:v1": true}}

	uc := NewRollbackDeploymentUseCase(projects, deployments, runtime, builder, &fakeRollbackEnvRepo{}, fakeCaddyAdmin(t))
	return uc, projects, deployments, runtime
}

// ── tests ────────────────────────────────────────────────────────────────

func TestRollbackStartsOldImageAsNewDeployment(t *testing.T) {
	uc, projects, deployments, runtime := newRollbackFixture(t)
	ctx := context.Background()

	target := &entities.Deployment{ProjectID: "proj-1", ImageRef: "golaunch/myapp:v1", Status: entities.DeploymentStopped}
	targetID, _ := deployments.Create(ctx, target)

	current := &entities.Deployment{ProjectID: "proj-1", ImageRef: "golaunch/myapp:v2", ContainerID: "old-container", Status: entities.DeploymentRunning}
	currentID, _ := deployments.Create(ctx, current)
	projects.projects["proj-1"].CurrentDeploymentID = &currentID

	if err := uc.Execute(ctx, "proj-1", "user-1", targetID); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	newCurrentID := *projects.projects["proj-1"].CurrentDeploymentID
	if newCurrentID == currentID {
		t.Fatal("rollback must create and switch to a NEW deployment row, not reactivate the old one in place")
	}
	newDeployment := deployments.deployments[newCurrentID]
	if newDeployment.ImageRef != "golaunch/myapp:v1" {
		t.Errorf("new deployment image = %q, want the target's image", newDeployment.ImageRef)
	}
	if newDeployment.Status != entities.DeploymentRunning {
		t.Errorf("new deployment status = %q, want running", newDeployment.Status)
	}

	if len(runtime.stopped) != 1 || runtime.stopped[0] != "old-container" {
		t.Errorf("expected the previously-live container to be stopped, got %v", runtime.stopped)
	}
	if deployments.deployments[currentID].Status != entities.DeploymentStopped {
		t.Error("the deployment rolled back FROM must be marked stopped")
	}
}

func TestRollbackRejectsDeploymentWithNoImage(t *testing.T) {
	uc, _, deployments, _ := newRollbackFixture(t)
	ctx := context.Background()

	target := &entities.Deployment{ProjectID: "proj-1", ImageRef: ""}
	targetID, _ := deployments.Create(ctx, target)

	if err := uc.Execute(ctx, "proj-1", "user-1", targetID); err == nil {
		t.Fatal("a deployment with no image must be rejected")
	}
}

func TestRollbackRejectsMissingImage(t *testing.T) {
	uc, _, deployments, _ := newRollbackFixture(t)
	ctx := context.Background()

	target := &entities.Deployment{ProjectID: "proj-1", ImageRef: "golaunch/myapp:deleted"}
	targetID, _ := deployments.Create(ctx, target)

	if err := uc.Execute(ctx, "proj-1", "user-1", targetID); err == nil {
		t.Fatal("a deployment whose image no longer exists must be rejected")
	}
}

func TestRollbackRejectsCrossProjectDeployment(t *testing.T) {
	uc, _, deployments, _ := newRollbackFixture(t)
	ctx := context.Background()

	target := &entities.Deployment{ProjectID: "some-other-project", ImageRef: "golaunch/myapp:v1"}
	targetID, _ := deployments.Create(ctx, target)

	if err := uc.Execute(ctx, "proj-1", "user-1", targetID); err == nil {
		t.Fatal("a deployment ID belonging to a different project must be rejected")
	}
}

func TestRollbackRejectsWrongOwner(t *testing.T) {
	uc, _, deployments, _ := newRollbackFixture(t)
	ctx := context.Background()

	target := &entities.Deployment{ProjectID: "proj-1", ImageRef: "golaunch/myapp:v1"}
	targetID, _ := deployments.Create(ctx, target)

	if err := uc.Execute(ctx, "proj-1", "someone-else", targetID); err == nil {
		t.Fatal("a non-owner must be rejected")
	}
}

func TestRollbackRejectsAlreadyLiveDeployment(t *testing.T) {
	uc, projects, deployments, _ := newRollbackFixture(t)
	ctx := context.Background()

	current := &entities.Deployment{ProjectID: "proj-1", ImageRef: "golaunch/myapp:v1", ContainerID: "c1"}
	currentID, _ := deployments.Create(ctx, current)
	projects.projects["proj-1"].CurrentDeploymentID = &currentID

	if err := uc.Execute(ctx, "proj-1", "user-1", currentID); err == nil {
		t.Fatal("rolling back to the deployment that's already live must be rejected")
	}
}

func TestRollbackDiscardsOnStartFailure(t *testing.T) {
	projects := &fakeRollbackProjectRepo{projects: map[string]*entities.Project{
		"proj-1": {ID: "proj-1", UserID: "user-1", Slug: "myapp", Status: entities.StatusRunning},
	}}
	deployments := newFakeRollbackDeploymentRepo()
	runtime := &fakeRollbackRuntime{waitReadyErr: context.DeadlineExceeded}
	builder := &fakeRollbackBuilder{existingImages: map[string]bool{"golaunch/myapp:v1": true}}
	uc := NewRollbackDeploymentUseCase(projects, deployments, runtime, builder, &fakeRollbackEnvRepo{}, fakeCaddyAdmin(t))
	ctx := context.Background()

	target := &entities.Deployment{ProjectID: "proj-1", ImageRef: "golaunch/myapp:v1"}
	targetID, _ := deployments.Create(ctx, target)

	err := uc.Execute(ctx, "proj-1", "user-1", targetID)
	if err == nil {
		t.Fatal("expected the rollback to fail when the new container never becomes ready")
	}
	if len(runtime.stopped) != 1 || len(runtime.removed) != 1 {
		t.Errorf("a container that never went live must be discarded, got stopped=%v removed=%v", runtime.stopped, runtime.removed)
	}
	if projects.projects["proj-1"].CurrentDeploymentID != nil {
		t.Error("a failed rollback must not switch CurrentDeploymentID")
	}
}
