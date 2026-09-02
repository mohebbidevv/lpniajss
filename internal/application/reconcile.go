package application

import (
	"context"
	"log"

	"golaunch/internal/domain/entities"
	"golaunch/internal/domain/repository"
	"golaunch/internal/infrastructure/caddy"
)

// Reconciler runs once at startup to settle DB state against whatever the
// runtime actually has running — the process may have been killed and
// restarted while deployments were live.
type Reconciler struct {
	DeploymentRepo repository.DeploymentRepository
	Runtime        repository.Runtime
	Caddy          *caddy.CaddyClient
	Pipeline       *DeployPipeline
}

func NewReconciler(deploymentRepo repository.DeploymentRepository, runtime repository.Runtime, caddyClient *caddy.CaddyClient, pipeline *DeployPipeline) *Reconciler {
	return &Reconciler{
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
	dbRunning, err := r.DeploymentRepo.ListByStatus(ctx, entities.DeploymentRunning)
	if err != nil {
		log.Printf("[reconcile] failed to list running deployments: %v", err)
		return
	}

	instances, err := r.Runtime.List(ctx, nil)
	if err != nil {
		log.Printf("[reconcile] failed to list runtime instances: %v", err)
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
			log.Printf("[reconcile] deployment %s marked running in DB but not found in runtime — redeploying", d.ID)
			if err := r.Pipeline.Deploy(ctx, d.ProjectID); err != nil {
				log.Printf("[reconcile] %s: redeploy failed: %v", d.ProjectID, err)
			}
			continue
		}

		slug := inst.Labels["slug"]
		if slug == "" {
			log.Printf("[reconcile] deployment %s missing slug label, cannot verify route", d.ID)
			continue
		}
		upstream, err := r.Runtime.Endpoint(ctx, inst.Handle)
		if err != nil {
			log.Printf("[reconcile] cannot resolve endpoint for deployment %s: %v", d.ID, err)
			continue
		}
		if err := r.Caddy.RegisterRoute(slug, upstream); err != nil {
			log.Printf("[reconcile] failed to ensure route for %s: %v", slug, err)
		}
	}

	for deploymentID, inst := range byDeploymentID {
		if knownDeploymentIDs[deploymentID] {
			continue
		}
		log.Printf("[reconcile] orphaned runtime instance for deployment %s — stopping", deploymentID)
		if err := r.Runtime.Stop(ctx, inst.Handle, stopTimeoutSeconds); err != nil {
			log.Printf("[reconcile] failed to stop orphan %s: %v", deploymentID, err)
		}
		if err := r.Runtime.Remove(ctx, inst.Handle); err != nil {
			log.Printf("[reconcile] failed to remove orphan %s: %v", deploymentID, err)
		}
	}
}
