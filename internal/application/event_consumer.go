package application

import (
	"context"
	"log"
	"sync"
	"time"

	"golaunch/internal/domain/entities"
	"golaunch/internal/domain/repository"
	"golaunch/internal/infrastructure/caddy"
	"golaunch/internal/infrastructure/utils"
	"golaunch/internal/queue"
)

// defaultMaxRestarts caps how many times in a row EventConsumer will
// auto-redeploy a project after it crashes, before giving up and leaving it
// failed for a human to look at. defaultRestartDelay is how long it waits
// before resubmitting the deploy job, so a fast-crashing bad build doesn't
// hot-loop the build workers.
const (
	defaultMaxRestarts  = 3
	defaultRestartDelay = 5 * time.Second
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
	events, err := c.Runtime.Events(ctx)
	if err != nil {
		log.Printf("[event-consumer] failed to subscribe to runtime events: %v", err)
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
	if evt.Type != entities.RuntimeEventDied && evt.Type != entities.RuntimeEventOOM {
		return
	}

	deploymentID := evt.Labels["deployment_id"]
	if deploymentID == "" {
		log.Printf("[event-consumer] death event with no deployment_id label, ignoring: %+v", evt)
		return
	}

	deployment, err := c.DeploymentRepo.GetByID(ctx, deploymentID)
	if err != nil {
		log.Printf("[event-consumer] deployment %s not found: %v", deploymentID, err)
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
		log.Printf("[event-consumer] failed to mark deployment %s crashed: %v", deploymentID, err)
	}
	// SetFailed sets DeploymentFailed; a death after the fact is a crash,
	// not a build failure — record that distinction explicitly.
	if err := c.DeploymentRepo.UpdateStatus(ctx, deploymentID, entities.DeploymentCrashed); err != nil {
		log.Printf("[event-consumer] failed to set deployment %s status crashed: %v", deploymentID, err)
	}

	project, err := c.ProjectRepo.GetByID(ctx, deployment.ProjectID)
	if err != nil {
		log.Printf("[event-consumer] project %s not found, cannot remove Caddy route: %v", deployment.ProjectID, err)
		return
	}
	if err := c.Caddy.RemoveRoute(project.Slug); err != nil {
		log.Printf("[event-consumer] failed to remove Caddy route for slug %s: %v", project.Slug, err)
	}
	if err := c.ProjectRepo.UpdateStatus(ctx, project.ID, entities.StatusFailed); err != nil {
		log.Printf("[event-consumer] failed to mark project %s failed: %v", project.ID, err)
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
		log.Printf("[event-consumer] project %s crashed %d times in a row, giving up on auto-restart", projectID, count)
		return
	}

	log.Printf("[event-consumer] project %s crashed (attempt %d/%d), scheduling auto-restart in %s", projectID, count, c.MaxRestarts, c.RestartDelay)
	time.AfterFunc(c.RestartDelay, func() {
		c.WorkerPool.Submit(queue.Job{ID: utils.NewID(), ProjectID: projectID})
	})
}
