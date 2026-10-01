package application

import (
	"context"
	"testing"

	"golaunch/internal/domain/entities"
)

type gcDeploymentRepo struct {
	byProject map[string][]*entities.Deployment
	current   *entities.Deployment
}

func (r *gcDeploymentRepo) ListByProject(ctx context.Context, projectID string) ([]*entities.Deployment, error) {
	return r.byProject[projectID], nil
}
func (r *gcDeploymentRepo) GetCurrentForProject(ctx context.Context, projectID string) (*entities.Deployment, error) {
	return r.current, nil
}
func (r *gcDeploymentRepo) Create(ctx context.Context, d *entities.Deployment) (string, error) {
	return "", nil
}
func (r *gcDeploymentRepo) GetByID(ctx context.Context, id string) (*entities.Deployment, error) {
	return nil, nil
}
func (r *gcDeploymentRepo) ListByStatus(ctx context.Context, st entities.DeploymentStatus) ([]*entities.Deployment, error) {
	return nil, nil
}
func (r *gcDeploymentRepo) UpdateStatus(ctx context.Context, id string, st entities.DeploymentStatus) error {
	return nil
}
func (r *gcDeploymentRepo) SetImageRef(ctx context.Context, id, imageRef string) error { return nil }
func (r *gcDeploymentRepo) SetContainerInfo(ctx context.Context, id, containerID string) error {
	return nil
}
func (r *gcDeploymentRepo) SetFailed(ctx context.Context, id, reason string, exitCode *int) error {
	return nil
}

type gcBuilder struct{ removed []string }

func (b *gcBuilder) Build(ctx context.Context, req entities.BuildRequest, sink func(entities.LogLine)) (string, error) {
	return "", nil
}
func (b *gcBuilder) RemoveImage(ctx context.Context, imageRef string) error {
	b.removed = append(b.removed, imageRef)
	return nil
}
func (b *gcBuilder) ImageExists(ctx context.Context, imageRef string) (bool, error) {
	return true, nil
}

func stopped(id, ref string) *entities.Deployment {
	return &entities.Deployment{ID: id, ImageRef: ref, Status: entities.DeploymentStopped}
}

// TestCollectKeepsNewestRetain: ListByProject is newest-first, so the first
// Retain distinct images survive and everything older is reclaimed.
func TestCollectKeepsNewestRetain(t *testing.T) {
	deployments := []*entities.Deployment{
		stopped("d7", "img7"), stopped("d6", "img6"), stopped("d5", "img5"),
		stopped("d4", "img4"), stopped("d3", "img3"), stopped("d2", "img2"),
		stopped("d1", "img1"),
	}
	repo := &gcDeploymentRepo{byProject: map[string][]*entities.Deployment{"p1": deployments}}
	builder := &gcBuilder{}

	NewImageGC(nil, repo, builder, 5).CollectProject(context.Background(), "p1")

	want := map[string]bool{"img2": true, "img1": true}
	if len(builder.removed) != len(want) {
		t.Fatalf("removed %v, want exactly the two oldest", builder.removed)
	}
	for _, ref := range builder.removed {
		if !want[ref] {
			t.Errorf("removed %s, which should have been retained", ref)
		}
	}
}

// TestCollectNeverRemovesLiveImage is the one that would cause an outage.
// The live deployment's image is untouchable regardless of how far down the
// history it has fallen — rollback to an old version puts it there.
func TestCollectNeverRemovesLiveImage(t *testing.T) {
	// The rollback shape: the live deployment is a NEW row pointing at an
	// OLD image, and the original row for that image is long since stopped.
	// Only the explicit protection check saves img1 here — the terminal
	// status check does not, because d1 is stopped.
	deployments := []*entities.Deployment{
		stopped("d7", "img7"), stopped("d6", "img6"), stopped("d5", "img5"),
		stopped("d4", "img4"), stopped("d3", "img3"), stopped("d2", "img2"),
		stopped("d1", "img1"),
	}
	repo := &gcDeploymentRepo{
		byProject: map[string][]*entities.Deployment{"p1": deployments},
		current:   &entities.Deployment{ID: "d8", ImageRef: "img1"},
	}
	builder := &gcBuilder{}

	NewImageGC(nil, repo, builder, 3).CollectProject(context.Background(), "p1")

	for _, ref := range builder.removed {
		if ref == "img1" {
			t.Fatal("removed the image the live deployment is running")
		}
	}
}

// TestCollectDedupesByImageRef: a rollback creates a NEW deployment row
// pointing at an EXISTING image. Counting rows rather than distinct images
// would silently shrink the retention window — here, five rows covering only
// three images must still keep all three.
func TestCollectDedupesByImageRef(t *testing.T) {
	deployments := []*entities.Deployment{
		stopped("d5", "img3"), // rollback to img3
		stopped("d4", "img2"), // rollback to img2
		stopped("d3", "img3"),
		stopped("d2", "img2"),
		stopped("d1", "img1"),
	}
	repo := &gcDeploymentRepo{byProject: map[string][]*entities.Deployment{"p1": deployments}}
	builder := &gcBuilder{}

	NewImageGC(nil, repo, builder, 3).CollectProject(context.Background(), "p1")

	if len(builder.removed) != 0 {
		t.Fatalf("removed %v, but there are only 3 distinct images and Retain is 3", builder.removed)
	}
}

// TestCollectSkipsNonTerminalDeployments: a row still building may be about
// to reference its image.
func TestCollectSkipsNonTerminalDeployments(t *testing.T) {
	deployments := []*entities.Deployment{
		stopped("d4", "img4"), stopped("d3", "img3"),
		{ID: "d2", ImageRef: "img2", Status: entities.DeploymentBuilding},
		stopped("d1", "img1"),
	}
	repo := &gcDeploymentRepo{byProject: map[string][]*entities.Deployment{"p1": deployments}}
	builder := &gcBuilder{}

	NewImageGC(nil, repo, builder, 1).CollectProject(context.Background(), "p1")

	for _, ref := range builder.removed {
		if ref == "img2" {
			t.Fatal("reclaimed an image belonging to a still-building deployment")
		}
	}
}

// TestCollectIgnoresEmptyImageRefs: a deploy that failed before the build
// produced anything leaves a row with no image.
func TestCollectIgnoresEmptyImageRefs(t *testing.T) {
	deployments := []*entities.Deployment{
		stopped("d3", ""), stopped("d2", "img2"), stopped("d1", "img1"),
	}
	repo := &gcDeploymentRepo{byProject: map[string][]*entities.Deployment{"p1": deployments}}
	builder := &gcBuilder{}

	NewImageGC(nil, repo, builder, 1).CollectProject(context.Background(), "p1")

	for _, ref := range builder.removed {
		if ref == "" {
			t.Fatal("tried to remove an empty image ref")
		}
	}
	if len(builder.removed) != 1 || builder.removed[0] != "img1" {
		t.Errorf("removed %v, want only img1", builder.removed)
	}
}
