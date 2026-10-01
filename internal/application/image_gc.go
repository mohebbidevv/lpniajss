package application

import (
	"context"
	"log/slog"
	"time"

	"golaunch/internal/domain/entities"
	"golaunch/internal/domain/repository"
	"golaunch/internal/infrastructure/logging"
)

// defaultImageRetention is how many distinct images to keep per project.
// This is a direct trade of disk for rollback depth: every retained image is
// a few hundred MB, and every one you drop is a version the user can no
// longer roll back to.
const defaultImageRetention = 5

// projectLister is the narrow slice of the project repository this needs.
// Declared at the point of use rather than added to
// repository.ProjectRepository, so one caller's requirement doesn't force a
// stub method into every fake in the test suite.
type projectLister interface {
	ListAll(ctx context.Context) ([]*entities.Project, error)
}

// buildCachePruner is implemented by builders that keep a layer cache. It is
// deliberately optional and type-asserted rather than being part of
// repository.ImageBuilder: the host-exec builder has no cache, and putting
// it on the port would force a meaningless no-op onto it.
type buildCachePruner interface {
	PruneBuildCache(ctx context.Context, keepSince time.Duration) (uint64, error)
}

// buildCacheMaxAge is how long an unused build cache layer is kept. Never
// prune everything: the warm cache is what makes an incremental redeploy
// fast, so only entries nothing has touched in a week are worth reclaiming.
const buildCacheMaxAge = 168 * time.Hour

// ImageGC reclaims disk from images no longer worth keeping.
//
// It exists because DeployPipeline deliberately stops deleting the previous
// image on every successful deploy — doing so made rollback structurally
// impossible. Nothing replaced that deletion, so images accumulate forever;
// this is the replacement, with a retention window instead of a delete.
type ImageGC struct {
	ProjectRepo    projectLister
	DeploymentRepo repository.DeploymentRepository
	Builder        repository.ImageBuilder
	Retain         int
}

func NewImageGC(projectRepo projectLister, deploymentRepo repository.DeploymentRepository, builder repository.ImageBuilder, retain int) *ImageGC {
	if retain <= 0 {
		retain = defaultImageRetention
	}
	return &ImageGC{
		ProjectRepo:    projectRepo,
		DeploymentRepo: deploymentRepo,
		Builder:        builder,
		Retain:         retain,
	}
}

// CollectProject removes every image for projectID except the newest Retain
// distinct ones, and never the image the live deployment is using.
//
// It returns nothing on purpose: GC is best-effort housekeeping and must
// never fail a deploy that has already succeeded.
func (g *ImageGC) CollectProject(ctx context.Context, projectID string) {
	log := logging.From(ctx).With("component", "image-gc", "project_id", projectID)

	deployments, err := g.DeploymentRepo.ListByProject(ctx, projectID)
	if err != nil {
		log.Error("cannot list deployments", "error", err)
		return
	}

	// The live deployment's image is untouchable regardless of age.
	// Removing it would break the running container's ability to restart
	// and leave Docker's own refcounting as the only thing between us and
	// an outage.
	protected := ""
	if current, err := g.DeploymentRepo.GetCurrentForProject(ctx, projectID); err == nil && current != nil {
		protected = current.ImageRef
	}

	// ListByProject is newest-first, which is what this walk depends on.
	// Dedupe by image ref before counting: a rollback creates a new
	// deployment row pointing at an existing image, so counting rows rather
	// than images would silently shrink the retention window.
	seen := map[string]bool{}
	kept := 0
	var candidates []string

	for _, d := range deployments {
		ref := d.ImageRef
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true

		if ref == protected || kept < g.Retain {
			kept++
			continue
		}

		// Only reclaim images whose deployment reached a terminal state —
		// a row still building or running may be about to reference it.
		if d.Status == entities.DeploymentRunning || d.Status == entities.DeploymentBuilding {
			continue
		}
		candidates = append(candidates, ref)
	}

	for _, ref := range candidates {
		if err := g.Builder.RemoveImage(ctx, ref); err != nil {
			// A conflict here means a container still holds the image.
			// That is expected and benign; the next sweep will get it.
			log.Debug("could not remove image", "image", ref, "error", err)
			continue
		}
		log.Info("reclaimed image", "image", ref)
	}
}

// Run sweeps every project on a timer, catching images orphaned by paths
// that never reach a successful deploy — a project deleted mid-flight, a
// build that succeeded before its deploy failed, images predating this GC.
func (g *ImageGC) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.sweep(ctx)
		}
	}
}

func (g *ImageGC) sweep(ctx context.Context) {
	projects, err := g.ProjectRepo.ListAll(ctx)
	if err != nil {
		slog.Error("image gc: cannot list projects", "error", err)
		return
	}
	for _, p := range projects {
		g.CollectProject(ctx, p.ID)
	}

	// Retention on tagged images does nothing for the build cache, which
	// grows independently and just as unboundedly.
	if pruner, ok := g.Builder.(buildCachePruner); ok {
		reclaimed, err := pruner.PruneBuildCache(ctx, buildCacheMaxAge)
		if err != nil {
			slog.Error("image gc: build cache prune failed", "error", err)
		} else if reclaimed > 0 {
			slog.Info("image gc: pruned build cache", "reclaimed_bytes", reclaimed)
		}
	}

	slog.Info("image gc: sweep complete", "projects", len(projects))
}
