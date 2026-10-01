package application

import (
	"context"
	"testing"
	"time"

	"golaunch/internal/domain/entities"
)

type sweeperDeploymentRepo struct {
	byStatus map[entities.DeploymentStatus][]*entities.Deployment
	failed   map[string]string
}

func (r *sweeperDeploymentRepo) ListByStatus(ctx context.Context, st entities.DeploymentStatus) ([]*entities.Deployment, error) {
	return r.byStatus[st], nil
}
func (r *sweeperDeploymentRepo) SetFailed(ctx context.Context, id, reason string, exitCode *int) error {
	if r.failed == nil {
		r.failed = map[string]string{}
	}
	r.failed[id] = reason
	return nil
}
func (r *sweeperDeploymentRepo) Create(ctx context.Context, d *entities.Deployment) (string, error) {
	return "", nil
}
func (r *sweeperDeploymentRepo) GetByID(ctx context.Context, id string) (*entities.Deployment, error) {
	return nil, nil
}
func (r *sweeperDeploymentRepo) GetCurrentForProject(ctx context.Context, projectID string) (*entities.Deployment, error) {
	return nil, nil
}
func (r *sweeperDeploymentRepo) ListByProject(ctx context.Context, projectID string) ([]*entities.Deployment, error) {
	return nil, nil
}
func (r *sweeperDeploymentRepo) UpdateStatus(ctx context.Context, id string, st entities.DeploymentStatus) error {
	return nil
}
func (r *sweeperDeploymentRepo) SetImageRef(ctx context.Context, id, imageRef string) error {
	return nil
}
func (r *sweeperDeploymentRepo) SetContainerInfo(ctx context.Context, id, containerID string) error {
	return nil
}

type sweeperProjectRepo struct {
	statuses map[string]entities.ProjectStatus
}

func (r *sweeperProjectRepo) UpdateStatus(ctx context.Context, id string, st entities.ProjectStatus) error {
	if r.statuses == nil {
		r.statuses = map[string]entities.ProjectStatus{}
	}
	r.statuses[id] = st
	return nil
}
func (r *sweeperProjectRepo) Create(ctx context.Context, p *entities.Project) (string, error) {
	return "", nil
}
func (r *sweeperProjectRepo) GetByID(ctx context.Context, id string) (*entities.Project, error) {
	return nil, nil
}
func (r *sweeperProjectRepo) UpdatePortAndStatus(ctx context.Context, id string, port int, st entities.ProjectStatus) error {
	return nil
}
func (r *sweeperProjectRepo) ListByStatus(ctx context.Context, st entities.ProjectStatus) ([]*entities.Project, error) {
	return nil, nil
}
func (r *sweeperProjectRepo) ListByUser(ctx context.Context, userID string) ([]*entities.Project, error) {
	return nil, nil
}
func (r *sweeperProjectRepo) ClaimProject(ctx context.Context, id, userID string) error { return nil }
func (r *sweeperProjectRepo) ListUnclaimed(ctx context.Context, olderThan time.Time) ([]*entities.Project, error) {
	return nil, nil
}
func (r *sweeperProjectRepo) Delete(ctx context.Context, id string) error { return nil }
func (r *sweeperProjectRepo) GetBySlug(ctx context.Context, slug string) (*entities.Project, error) {
	return nil, nil
}
func (r *sweeperProjectRepo) SlugExists(ctx context.Context, slug string) (bool, error) {
	return false, nil
}
func (r *sweeperProjectRepo) UpdateSlug(ctx context.Context, id, slug string) error { return nil }
func (r *sweeperProjectRepo) SetCurrentDeployment(ctx context.Context, projectID, deploymentID string) error {
	return nil
}

// TestSweeperFailsBuildingRows is the core case: a deployment left mid-build
// by a crash or restart is invisible to Reconciler, which only ever queries
// DeploymentRunning. Without this sweep the row — and the user's spinner —
// stay there forever.
func TestSweeperFailsBuildingRows(t *testing.T) {
	dRepo := &sweeperDeploymentRepo{
		byStatus: map[entities.DeploymentStatus][]*entities.Deployment{
			entities.DeploymentBuilding: {
				{ID: "d1", ProjectID: "p1", CreatedAt: time.Now().Add(-time.Hour)},
				{ID: "d2", ProjectID: "p2", CreatedAt: time.Now().Add(-time.Minute)},
			},
		},
	}
	pRepo := &sweeperProjectRepo{}

	n, err := FailAbandonedDeployments(context.Background(), dRepo, pRepo, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("swept %d, want 2", n)
	}
	for _, id := range []string{"d1", "d2"} {
		if _, ok := dRepo.failed[id]; !ok {
			t.Errorf("deployment %s was not failed", id)
		}
	}
	// The project has to follow the deployment, or the dashboard keeps
	// showing "building" even though the deployment row says failed.
	for _, id := range []string{"p1", "p2"} {
		if got := pRepo.statuses[id]; got != entities.StatusFailed {
			t.Errorf("project %s status = %q, want %q", id, got, entities.StatusFailed)
		}
	}
}

// TestSweeperRespectsGracePeriod: with olderThan set, a build that started
// moments ago is presumed to still be owned by a live worker.
func TestSweeperRespectsGracePeriod(t *testing.T) {
	dRepo := &sweeperDeploymentRepo{
		byStatus: map[entities.DeploymentStatus][]*entities.Deployment{
			entities.DeploymentBuilding: {
				{ID: "old", ProjectID: "p1", CreatedAt: time.Now().Add(-time.Hour)},
				{ID: "fresh", ProjectID: "p2", CreatedAt: time.Now()},
			},
		},
	}
	n, err := FailAbandonedDeployments(context.Background(), dRepo, &sweeperProjectRepo{}, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
	if _, ok := dRepo.failed["fresh"]; ok {
		t.Error("swept a deployment inside the grace period")
	}
}

// TestSweeperLeavesTerminalRowsAlone guards against the sweeper reaching
// into finished history — a running deployment must never be touched.
func TestSweeperLeavesTerminalRowsAlone(t *testing.T) {
	dRepo := &sweeperDeploymentRepo{
		byStatus: map[entities.DeploymentStatus][]*entities.Deployment{
			entities.DeploymentRunning: {{ID: "live", ProjectID: "p1"}},
			entities.DeploymentFailed:  {{ID: "old", ProjectID: "p2"}},
		},
	}
	n, err := FailAbandonedDeployments(context.Background(), dRepo, &sweeperProjectRepo{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("swept %d terminal rows, want 0", n)
	}
}
