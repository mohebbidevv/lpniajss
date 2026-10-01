package application

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"golaunch/internal/domain/entities"
	"golaunch/internal/domain/repository"
	"golaunch/internal/infrastructure/caddy"
	"golaunch/internal/infrastructure/logging"
	"golaunch/internal/infrastructure/utils"
	"golaunch/internal/queue"
)

// defaultMaxRestarts caps how many times in a row EventConsumer will
// auto-redeploy a project after it crashes, before giving up and leaving it
// failed for a human to look at. defaultRestartDelay is how long it waits
// before resubmitting the deploy job, so a fast-crashing bad build doesn't
// hot-loop the build workers.
const (
	// defaultMaxRestarts is deliberately lower than the worker pool's own
	// retry count. The two systems compound: every restart here is a fresh
	// Submit that gets its own full set of pool retries, so 3 and 3 meant a
	// worst case near a dozen build attempts per project — all of them
	// firing at once across every project when the cause is shared.
	defaultMaxRestarts  = 2
	defaultRestartDelay = 5 * time.Second

	// maxRestartDelay caps the exponential growth below, so a project that
	// keeps crashing backs off instead of retrying on a fixed short timer.
	maxRestartDelay = 2 * time.Minute
)

// EventConsumer is a long-lived goroutine that watches Runtime.Events() and
// reconciles unexpected deaths back into the DB. It never initiates a stop
// itself — that's stop_project.go's job — it only reacts to what the
// runtime reports. On an unexpected death it also auto-redeploys the
// project (up to MaxRestarts consecutive crashes) by resubmitting a job to
// the same worker pool normal /run requests use.
type EventConsumer struct {
	ProjectRepo    repository.ProjectRepository
	DeploymentRepo repository.DeploymentRepository
	Runtime        repository.Runtime
	Caddy          *caddy.CaddyClient
	WorkerPool     *queue.WorkerPool
	MaxRestarts    int
	RestartDelay   time.Duration

	mu          sync.Mutex
	crashCounts map[string]int
}

func NewEventConsumer(projectRepo repository.ProjectRepository, deploymentRepo repository.DeploymentRepository, runtime repository.Runtime, caddyClient *caddy.CaddyClient, workerPool *queue.WorkerPool) *EventConsumer {
	return &EventConsumer{
		ProjectRepo:    projectRepo,
		DeploymentRepo: deploymentRepo,
		Runtime:        runtime,
		Caddy:          caddyClient,
		WorkerPool:     workerPool,
		MaxRestarts:    defaultMaxRestarts,
		RestartDelay:   defaultRestartDelay,
		crashCounts:    make(map[string]int),
	}
}

// ResetCrashCount clears projectID's consecutive-crash count. Call it after
// a deploy for projectID succeeds, so a crash that happens long after a
// healthy run doesn't inherit an old streak.
func (c *EventConsumer) ResetCrashCount(projectID string) {
	c.mu.Lock()
	delete(c.crashCounts, projectID)
	c.mu.Unlock()
}

// Run blocks consuming events until ctx is cancelled or the runtime's event
// channel closes. Call it in its own goroutine.
func (c *EventConsumer) Run(ctx context.Context) {
	log := logging.From(ctx).With("component", "event-consumer")

	events, err := c.Runtime.Events(ctx)
	if err != nil {
		log.Error("failed to subscribe to runtime events", "error", err)
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			c.handle(ctx, evt)
		}
	}
}

func (c *EventConsumer) handle(ctx context.Context, evt entities.RuntimeEvent) {
	log := logging.From(ctx).With("component", "event-consumer")

	if evt.Type != entities.RuntimeEventDied && evt.Type != entities.RuntimeEventOOM {
		return
	}

	deploymentID := evt.Labels["deployment_id"]
	if deploymentID == "" {
		log.Warn("death event carries no deployment_id label, ignoring", "event", evt)
		return
	}

	deployment, err := c.DeploymentRepo.GetByID(ctx, deploymentID)
	if err != nil {
		log.Error("deployment not found", "deployment_id", deploymentID, "error", err)
		return
	}

	// already marked stopped means this death was intentional (stop_project
	// marks the row stopped BEFORE it actually stops the runtime) — nothing
	// to do here.
	if deployment.Status == entities.DeploymentStopped {
		return
	}

	status, err := c.Runtime.Status(ctx, evt.Handle)
	var exitCode *int
	if err == nil {
		exitCode = status.ExitCode
	}

	reason := "process exited unexpectedly"
	if evt.Type == entities.RuntimeEventOOM {
		reason = "out of memory"
	}

	if err := c.DeploymentRepo.SetFailed(ctx, deploymentID, reason, exitCode); err != nil {
		log.Error("failed to mark deployment crashed", "deployment_id", deploymentID, "error", err)
	}
	// SetFailed sets DeploymentFailed; a death after the fact is a crash,
	// not a build failure — record that distinction explicitly.
	if err := c.DeploymentRepo.UpdateStatus(ctx, deploymentID, entities.DeploymentCrashed); err != nil {
		log.Error("failed to set deployment status crashed", "deployment_id", deploymentID, "error", err)
	}

	project, err := c.ProjectRepo.GetByID(ctx, deployment.ProjectID)
	if err != nil {
		log.Error("project not found, cannot remove route", "project_id", deployment.ProjectID, "error", err)
		return
	}
	if err := c.Caddy.RemoveRoute(project.ID); err != nil {
		log.Error("failed to remove route", "slug", project.Slug, "error", err)
	}
	if err := c.ProjectRepo.UpdateStatus(ctx, project.ID, entities.StatusFailed); err != nil {
		log.Error("failed to mark project failed", "project_id", project.ID, "error", err)
	}

	c.maybeRestart(project.ID)
}

// maybeRestart resubmits a deploy job for projectID after RestartDelay, as
// long as it hasn't already crashed MaxRestarts times in a row. The delay
// runs on its own timer goroutine so it never blocks Run's event loop.
func (c *EventConsumer) maybeRestart(projectID string) {
	c.mu.Lock()
	c.crashCounts[projectID]++
	count := c.crashCounts[projectID]
	c.mu.Unlock()

	if count > c.MaxRestarts {
		slog.Warn("giving up on auto-restart after consecutive crashes", "project_id", projectID, "crashes", count)
		return
	}

	// The delay grows with the crash count so a crash-looping app backs off
	// rather than hammering the build workers on a fixed 5s timer.
	delay := c.RestartDelay << uint(count-1)
	if delay > maxRestartDelay {
		delay = maxRestartDelay
	}

	slog.Info("scheduling auto-restart after crash", "project_id", projectID, "attempt", count, "max", c.MaxRestarts, "delay", delay)
	time.AfterFunc(delay, func() {
		// Submit can now legitimately refuse — pool draining, queue full,
		// or the circuit breaker open. Dropping that silently would make a
		// restart look scheduled when it never happened.
		if err := c.WorkerPool.Submit(queue.Job{ID: utils.NewID(), ProjectID: projectID}); err != nil {
			slog.Error("could not resubmit project for restart", "project_id", projectID, "error", err)
		}
	})
}
