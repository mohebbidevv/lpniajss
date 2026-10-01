package application

import (
	"context"
	"testing"
	"time"

	"golaunch/internal/domain/entities"
)

// fakeSlugRepo is the minimal repository.ProjectRepository stub needed to
// drive uniqueSlug in isolation, without a database.
type fakeSlugRepo struct {
	taken map[string]bool
}

func newFakeSlugRepo(taken ...string) *fakeSlugRepo {
	r := &fakeSlugRepo{taken: make(map[string]bool)}
	for _, s := range taken {
		r.taken[s] = true
	}
	return r
}

func (r *fakeSlugRepo) SlugExists(ctx context.Context, slug string) (bool, error) {
	return r.taken[slug], nil
}

func (r *fakeSlugRepo) Create(ctx context.Context, p *entities.Project) (string, error) {
	return "", nil
}
func (r *fakeSlugRepo) GetByID(ctx context.Context, id string) (*entities.Project, error) {
	return nil, nil
}
func (r *fakeSlugRepo) UpdateStatus(ctx context.Context, id string, status entities.ProjectStatus) error {
	return nil
}
func (r *fakeSlugRepo) UpdatePortAndStatus(ctx context.Context, id string, port int, status entities.ProjectStatus) error {
	return nil
}
func (r *fakeSlugRepo) ListByStatus(ctx context.Context, status entities.ProjectStatus) ([]*entities.Project, error) {
	return nil, nil
}
func (r *fakeSlugRepo) GetBySlug(ctx context.Context, slug string) (*entities.Project, error) {
	return nil, nil
}
func (r *fakeSlugRepo) UpdateSlug(ctx context.Context, id, slug string) error { return nil }
func (r *fakeSlugRepo) ListByUser(ctx context.Context, userID string) ([]*entities.Project, error) {
	return nil, nil
}
func (r *fakeSlugRepo) ClaimProject(ctx context.Context, id, userID string) error { return nil }
func (r *fakeSlugRepo) ListUnclaimed(ctx context.Context, olderThan time.Time) ([]*entities.Project, error) {
	return nil, nil
}
func (r *fakeSlugRepo) Delete(ctx context.Context, id string) error { return nil }
func (r *fakeSlugRepo) SetCurrentDeployment(ctx context.Context, projectID, deploymentID string) error {
	return nil
}

func TestUniqueSlugReturnsBaseWhenFree(t *testing.T) {
	repo := newFakeSlugRepo()

	got, err := uniqueSlug(context.Background(), repo, "portfolio")
	if err != nil {
		t.Fatalf("uniqueSlug: %v", err)
	}
	if got != "portfolio" {
		t.Errorf("got %q, want the unsuffixed base", got)
	}
}

func TestUniqueSlugSuffixesOnCollision(t *testing.T) {
	repo := newFakeSlugRepo("portfolio")

	got, err := uniqueSlug(context.Background(), repo, "portfolio")
	if err != nil {
		t.Fatalf("uniqueSlug: %v", err)
	}
	if got == "portfolio" {
		t.Fatal("a taken base must not be returned unsuffixed")
	}
	if len(got) <= len("portfolio-") {
		t.Errorf("got %q, expected base plus a real suffix", got)
	}
}

func TestUniqueSlugRetriesPastRepeatedCollisions(t *testing.T) {
	// simulate the suffixed form also being taken by rejecting every
	// candidate up front, then unblocking after a few attempts — this only
	// works if uniqueSlug actually asks again on each attempt rather than
	// generating one suffix and giving up.
	repo := newFakeSlugRepo("portfolio")
	calls := 0
	blocking := &blockingRepo{fakeSlugRepo: repo, allowAfter: 2, calls: &calls}

	got, err := uniqueSlug(context.Background(), blocking, "portfolio")
	if err != nil {
		t.Fatalf("uniqueSlug: %v", err)
	}
	if got == "portfolio" {
		t.Fatal("must not return the known-taken base")
	}
	if calls < 3 {
		t.Errorf("expected uniqueSlug to retry past early collisions, only saw %d SlugExists calls", calls)
	}
}

// blockingRepo rejects every candidate slug until allowAfter calls have
// been made, to prove uniqueSlug actually retries rather than trusting the
// first generated suffix.
type blockingRepo struct {
	*fakeSlugRepo
	allowAfter int
	calls      *int
}

func (r *blockingRepo) SlugExists(ctx context.Context, slug string) (bool, error) {
	*r.calls++
	if *r.calls <= r.allowAfter {
		return true, nil
	}
	return r.fakeSlugRepo.SlugExists(ctx, slug)
}

func TestUniqueSlugFailsAfterExhaustingAttempts(t *testing.T) {
	repo := newFakeSlugRepo("portfolio")
	alwaysTaken := &blockingRepo{fakeSlugRepo: repo, allowAfter: 1000, calls: new(int)}

	_, err := uniqueSlug(context.Background(), alwaysTaken, "portfolio")
	if err == nil {
		t.Fatal("expected an error when no free slug can be found")
	}
}

// TestReservedSlugsRejected: nothing may let a tenant claim api.<domain> or
// www.<domain> — those are phishing and cookie-scoping problems, not just
// naming collisions.
func TestReservedSlugsRejected(t *testing.T) {
	for _, s := range []string{"api", "www", "admin", "mail", "healthz", "dashboard"} {
		if !IsReservedSlug(s) {
			t.Errorf("%q should be reserved", s)
		}
	}
	for _, s := range []string{"my-app", "portfolio", "apiary", "wwwx"} {
		if IsReservedSlug(s) {
			t.Errorf("%q should NOT be reserved", s)
		}
	}
}
