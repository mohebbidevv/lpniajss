package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golaunch/internal/domain/entities"
)

type pingRuntime struct{ err error }

func (r *pingRuntime) Ping(ctx context.Context) error { return r.err }
func (r *pingRuntime) Start(ctx context.Context, spec entities.RuntimeSpec) (entities.RuntimeHandle, error) {
	return "", nil
}
func (r *pingRuntime) Stop(ctx context.Context, h entities.RuntimeHandle, t int) error { return nil }
func (r *pingRuntime) Remove(ctx context.Context, h entities.RuntimeHandle) error      { return nil }
func (r *pingRuntime) Status(ctx context.Context, h entities.RuntimeHandle) (entities.RuntimeStatus, error) {
	return entities.RuntimeStatus{}, nil
}
func (r *pingRuntime) Endpoint(ctx context.Context, h entities.RuntimeHandle) (string, error) {
	return "", nil
}
func (r *pingRuntime) WaitReady(ctx context.Context, h entities.RuntimeHandle, d time.Duration) error {
	return nil
}
func (r *pingRuntime) Logs(ctx context.Context, h entities.RuntimeHandle, o entities.LogOptions) (<-chan entities.LogLine, error) {
	return nil, nil
}
func (r *pingRuntime) List(ctx context.Context, f map[string]string) ([]entities.RuntimeInstance, error) {
	return nil, nil
}
func (r *pingRuntime) Events(ctx context.Context) (<-chan entities.RuntimeEvent, error) {
	return nil, nil
}

// TestLiveIgnoresDependencies is the whole point of splitting liveness from
// readiness: a dead dependency must not make the supervisor kill an
// otherwise healthy process. DB is nil and the runtime is failing here, and
// /healthz must still be 200.
func TestLiveIgnoresDependencies(t *testing.T) {
	h := &HealthHandler{Runtime: &pingRuntime{err: errors.New("daemon gone")}, Version: "test", StartedAt: time.Now()}

	rec := httptest.NewRecorder()
	h.Live(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("liveness = %d, want 200 — it must not consult dependencies", rec.Code)
	}
}

// TestReadyReportsFailingDependency: readiness is where a dependency outage
// shows up, as a 503 that pulls the instance out of rotation without killing
// it, plus a per-dependency reason.
func TestReadyReportsFailingDependency(t *testing.T) {
	h := &HealthHandler{Runtime: &pingRuntime{err: errors.New("daemon gone")}}

	rec := httptest.NewRecorder()
	h.Ready(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness = %d, want 503", rec.Code)
	}

	var body struct {
		Ready  bool              `json:"ready"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Ready {
		t.Error("ready = true despite a failing runtime")
	}
	if body.Checks["runtime"] != "daemon gone" {
		t.Errorf("checks[runtime] = %q, want the underlying error", body.Checks["runtime"])
	}
}
