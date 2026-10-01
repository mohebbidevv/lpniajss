package application

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"golaunch/internal/domain/entities"
	"golaunch/internal/domain/repository"
)

// nonTerminalDeploymentStatuses are the states a deployment can only sit in
// while some goroutine is actively working on it. If nobody is, the row is
// orphaned by definition — nothing in the system will ever advance it.
//
// Deliberately only "building": entities.DeploymentStatus has no separate
// "deploying" state, so this is the complete set of non-terminal values.
var nonTerminalDeploymentStatuses = []entities.DeploymentStatus{
	entities.DeploymentBuilding,
}

const abandonedReason = "deploy was interrupted (control plane restarted)"

// FailAbandonedDeployments gives a terminal status to every non-terminal
// deployment older than olderThan, and drops its project out of a building
// state with it.
//
// This exists because Reconciler only ever queries DeploymentRunning. A row
// parked at "building" is invisible to every other recovery path, so without
// this it stays there forever and the user watches a spinner that will never
// resolve. A crash, an OOM kill or a power loss all produce one, and none of
// those get to run any shutdown code — which is why this has to run at
// startup and not only on the way out.
//
// olderThan == 0 means "all of them", which is correct at both call sites
// with a single control plane: a fresh process owns no in-flight builds, and
// at shutdown this process was the only one that could have owned them. Pass
// a grace period only if more than one instance can ever run at once.
//
// It is a free function rather than a struct because it holds no state and
// its two callers have entirely different lifecycles.
func FailAbandonedDeployments(
	ctx context.Context,
	deploymentRepo repository.DeploymentRepository,
	projectRepo repository.ProjectRepository,
	olderThan time.Duration,
) (int, error) {
	cutoff := time.Now().Add(-olderThan)
	swept := 0

	for _, status := range nonTerminalDeploymentStatuses {
		rows, err := deploymentRepo.ListByStatus(ctx, status)
		if err != nil {
			return swept, fmt.Errorf("list %s deployments: %w", status, err)
		}

		for _, d := range rows {
			if olderThan > 0 && d.CreatedAt.After(cutoff) {
				continue
			}

			// Per-row failures are logged and stepped over: one bad row
			// must not stop the rest of the sweep.
			if err := deploymentRepo.SetFailed(ctx, d.ID, abandonedReason, nil); err != nil {
				slog.Error("sweeper: cannot fail deployment", "deployment_id", d.ID, "error", err)
				continue
			}
			if err := projectRepo.UpdateStatus(ctx, d.ProjectID, entities.StatusFailed); err != nil {
				slog.Error("sweeper: cannot fail project", "project_id", d.ProjectID, "error", err)
			}
			swept++
		}
	}

	return swept, nil
}
