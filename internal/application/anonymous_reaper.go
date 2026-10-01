package application

import (
	"context"
	"log/slog"
	"os"
	"time"

	"golaunch/internal/domain/repository"
)

// AnonymousReaper deletes projects staged via /upload or /import/github that
// never got claimed (someone dropped a file/repo, then abandoned the signup
// screen). Unclaimed projects can never reach the deploy pipeline —
// mustOwnProject rejects every authed caller against an empty UserID — so
// there's never anything running to tear down, just the DB row and the
// extracted source directory on disk.
type AnonymousReaper struct {
	ProjectRepo repository.ProjectRepository
	TTL         time.Duration
}

func NewAnonymousReaper(projectRepo repository.ProjectRepository, ttl time.Duration) *AnonymousReaper {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &AnonymousReaper{ProjectRepo: projectRepo, TTL: ttl}
}

// Run sweeps on interval until ctx is cancelled.
func (r *AnonymousReaper) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	r.sweep(ctx)
	for {
		select {
		case <-ticker.C:
			r.sweep(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (r *AnonymousReaper) sweep(ctx context.Context) {
	stale, err := r.ProjectRepo.ListUnclaimed(ctx, time.Now().Add(-r.TTL))
	if err != nil {
		slog.Error("reaper: failed to list unclaimed projects", "error", err)
		return
	}

	for _, p := range stale {
		if err := os.RemoveAll(p.SourceLocation); err != nil {
			slog.Error("reaper: failed to remove source dir", "project_id", p.ID, "error", err)
		}
		if err := r.ProjectRepo.Delete(ctx, p.ID); err != nil {
			slog.Error("reaper: failed to delete unclaimed project", "project_id", p.ID, "error", err)
			continue
		}
		slog.Info("reaper: deleted unclaimed project", "project_id", p.ID, "staged_for", time.Since(p.CreatedAt))
	}
}
