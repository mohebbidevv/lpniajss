package application

import (
	"context"

	"golaunch/internal/domain/entities"
	"golaunch/internal/domain/repository"
	"golaunch/internal/infrastructure/caddy"
	"golaunch/internal/infrastructure/logging"
)

// Reconciler runs once at startup to settle DB state against whatever the
// runtime actually has running — the process may have been killed and
// restarted while deployments were live.
type Reconciler struct {
	ProjectRepo    repository.ProjectRepository
	DeploymentRepo repository.DeploymentRepository
	Runtime        repository.Runtime
	Caddy          *caddy.CaddyClient
	Pipeline       *DeployPipeline
}

func NewReconciler(projectRepo repository.ProjectRepository, deploymentRepo repository.DeploymentRepository, runtime repository.Runtime, caddyClient *caddy.CaddyClient, pipeline *DeployPipeline) *Reconciler {
	return &Reconciler{
		ProjectRepo:    projectRepo,
		DeploymentRepo: deploymentRepo,
		Runtime:        runtime,
		Caddy:          caddyClient,
		Pipeline:       pipeline,
	}
}

// Run compares the DB's view of what should be running against the
// runtime's view of what actually is, and settles the three possible
// mismatches:
//   - in both: ensure the Caddy route exists (it may not have survived a
//     Caddy restart even though the process did).
//   - DB says running, runtime doesn't have it: the process is gone —
//     trigger a fresh deploy to bring the project back.
//   - runtime has it, DB doesn't know about it as running: an orphan left
//     over from a previous run — stop it.
func (r *Reconciler) Run(ctx context.Context) {
	log := logging.From(ctx).With("component", "reconciler")

	// First, before anything else looks at deployments: a row left
	// mid-build by a crash or restart will never be advanced by anyone,
	// because the goroutine that owned it is gone. Nothing else in this
	// function considers non-terminal statuses, so without this the row
	// stays "building" forever and the user stares at a dead spinner.
	if n, err := FailAbandonedDeployments(ctx, r.DeploymentRepo, r.ProjectRepo, 0); err != nil {
		log.Error("abandoned-deployment sweep failed", "error", err)
	} else if n > 0 {
		log.Info("marked abandoned deployments as failed", "count", n)
	}

	dbRunning, err := r.DeploymentRepo.ListByStatus(ctx, entities.DeploymentRunning)
	if err != nil {
		log.Error("failed to list running deployments", "error", err)
		return
	}

	instances, err := r.Runtime.List(ctx, nil)
	if err != nil {
		log.Error("failed to list runtime instances", "error", err)
		return
	}

	byDeploymentID := make(map[string]entities.RuntimeInstance, len(instances))
	for _, inst := range instances {
		if id := inst.Labels["deployment_id"]; id != "" {
			byDeploymentID[id] = inst
		}
	}

	knownDeploymentIDs := make(map[string]bool, len(dbRunning))
	for _, d := range dbRunning {
		knownDeploymentIDs[d.ID] = true

		inst, ok := byDeploymentID[d.ID]
		if !ok {
			log.Warn("deployment marked running in DB but absent from runtime, redeploying", "deployment_id", d.ID)
			if err := r.Pipeline.Deploy(ctx, d.ProjectID); err != nil {
				log.Error("redeploy failed", "project_id", d.ProjectID, "error", err)
			}
			continue
		}

		// slug comes from the project row, never from the container's own
		// labels — those are frozen at Start() time, so trusting them here
		// would silently resurrect a slug the user has since renamed away
		// from on every restart.
		project, err := r.ProjectRepo.GetByID(ctx, d.ProjectID)
		if err != nil {
			log.Error("cannot load project", "project_id", d.ProjectID, "error", err)
			continue
		}
		upstream, err := r.Runtime.Endpoint(ctx, inst.Handle)
		if err != nil {
			log.Error("cannot resolve endpoint", "deployment_id", d.ID, "error", err)
			continue
		}
		if err := r.Caddy.RegisterRoute(project.ID, project.Slug, upstream); err != nil {
			log.Error("failed to ensure route", "slug", project.Slug, "error", err)
		}
	}

	for deploymentID, inst := range byDeploymentID {
		if knownDeploymentIDs[deploymentID] {
			continue
		}
		log.Warn("stopping orphaned runtime instance", "deployment_id", deploymentID)
		if err := r.Runtime.Stop(ctx, inst.Handle, stopTimeoutSeconds); err != nil {
			log.Error("failed to stop orphan", "deployment_id", deploymentID, "error", err)
		}
		if err := r.Runtime.Remove(ctx, inst.Handle); err != nil {
			log.Error("failed to remove orphan", "deployment_id", deploymentID, "error", err)
		}
	}
}
