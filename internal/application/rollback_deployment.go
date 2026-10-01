package application

import (
	"context"
	"fmt"
	"time"

	"golaunch/internal/domain/entities"
	"golaunch/internal/domain/repository"
	"golaunch/internal/infrastructure/caddy"
)

const defaultRollbackReadyTimeout = 90 * time.Second

// RollbackDeploymentUseCase starts a new deployment from an already-built
// image instead of running a fresh build — the second half of DeployPipeline
// with the build step skipped. It only works because DeployPipeline.Deploy
// keeps every superseded deployment's image around instead of deleting it;
// nothing here would have a target to start otherwise.
type RollbackDeploymentUseCase struct {
	ProjectRepo    repository.ProjectRepository
	DeploymentRepo repository.DeploymentRepository
	Runtime        repository.Runtime
	Builder        repository.ImageBuilder
	EnvVarRepo     repository.EnvVarRepository
	Caddy          *caddy.CaddyClient
	ReadyTimeout   time.Duration
}

func NewRollbackDeploymentUseCase(
	projectRepo repository.ProjectRepository,
	deploymentRepo repository.DeploymentRepository,
	runtime repository.Runtime,
	builder repository.ImageBuilder,
	envVarRepo repository.EnvVarRepository,
	caddyClient *caddy.CaddyClient,
) *RollbackDeploymentUseCase {
	return &RollbackDeploymentUseCase{
		ProjectRepo:    projectRepo,
		DeploymentRepo: deploymentRepo,
		Runtime:        runtime,
		Builder:        builder,
		EnvVarRepo:     envVarRepo,
		Caddy:          caddyClient,
		ReadyTimeout:   defaultRollbackReadyTimeout,
	}
}

// Execute starts targetDeploymentID's image as a brand-new deployment and
// flips traffic to it — never mutates or restarts the old row in place, so
// the deployment history stays an honest record of what actually ran when.
// The current deployment is only torn down after the rollback target is
// confirmed live, matching DeployPipeline's ordering for the same reason:
// a bad rollback must not take the project down.
func (uc *RollbackDeploymentUseCase) Execute(ctx context.Context, projectID, userID, targetDeploymentID string) error {
	project, err := uc.ProjectRepo.GetByID(ctx, projectID)
	if err != nil {
		return fmt.Errorf("project not found: %w", err)
	}
	if err := mustOwnProject(project, userID); err != nil {
		return err
	}

	target, err := uc.DeploymentRepo.GetByID(ctx, targetDeploymentID)
	if err != nil {
		return fmt.Errorf("deployment not found: %w", err)
	}
	if target.ProjectID != project.ID {
		// exists, but not this caller's — same not-found shape as anywhere
		// else ownership fails, so a deployment ID from another project
		// doesn't confirm that project's existence
		return fmt.Errorf("deployment not found")
	}
	if target.ImageRef == "" {
		return fmt.Errorf("this deployment never finished building and has no image to roll back to")
	}

	exists, err := uc.Builder.ImageExists(ctx, target.ImageRef)
	if err != nil {
		return fmt.Errorf("check image: %w", err)
	}
	if !exists {
		return fmt.Errorf("this deployment's image is no longer available and can't be rolled back to")
	}

	var previous *entities.Deployment
	if project.CurrentDeploymentID != nil {
		if *project.CurrentDeploymentID == target.ID {
			return fmt.Errorf("this deployment is already live")
		}
		previous, err = uc.DeploymentRepo.GetByID(ctx, *project.CurrentDeploymentID)
		if err != nil {
			return fmt.Errorf("load current deployment: %w", err)
		}
	}

	envVars, err := uc.EnvVarRepo.ListForProject(ctx, project.ID)
	if err != nil {
		return fmt.Errorf("load env vars: %w", err)
	}

	// the image is already known, unlike a fresh deploy where the row must
	// exist before the build can tag anything — so it goes straight into
	// NewDeployment instead of starting empty and getting SetImageRef'd later
	rollback := entities.NewDeployment(project.ID, target.ImageRef)
	rollbackID, err := uc.DeploymentRepo.Create(ctx, rollback)
	if err != nil {
		return fmt.Errorf("create deployment row: %w", err)
	}
	rollback.ID = rollbackID

	spec := entities.RuntimeSpec{
		DeploymentID: rollback.ID,
		ProjectID:    project.ID,
		Slug:         project.Slug,
		ImageRef:     target.ImageRef,
		Env:          appEnv(envVars),
		Labels: map[string]string{
			"project_id":    project.ID,
			"deployment_id": rollback.ID,
			"slug":          project.Slug,
		},
	}

	handle, err := uc.Runtime.Start(ctx, spec)
	if err != nil {
		return uc.fail(ctx, rollback.ID, fmt.Sprintf("start failed: %v", err))
	}

	if err := uc.DeploymentRepo.SetContainerInfo(ctx, rollback.ID, string(handle)); err != nil {
		return fmt.Errorf("record container info: %w", err)
	}

	if err := uc.Runtime.WaitReady(ctx, handle, uc.ReadyTimeout); err != nil {
		uc.discard(ctx, handle)
		return uc.fail(ctx, rollback.ID, fmt.Sprintf("app never became ready: %v", err))
	}

	upstream, err := uc.Runtime.Endpoint(ctx, handle)
	if err != nil {
		uc.discard(ctx, handle)
		return uc.fail(ctx, rollback.ID, fmt.Sprintf("resolve endpoint failed: %v", err))
	}

	if err := uc.Caddy.RegisterRoute(project.ID, project.Slug, upstream); err != nil {
		uc.discard(ctx, handle)
		return uc.fail(ctx, rollback.ID, fmt.Sprintf("register route failed: %v", err))
	}

	if err := uc.ProjectRepo.SetCurrentDeployment(ctx, project.ID, rollback.ID); err != nil {
		return fmt.Errorf("update current deployment: %w", err)
	}
	_ = uc.ProjectRepo.UpdateStatus(ctx, project.ID, entities.StatusRunning)

	// only now that traffic is flowing to the rollback target do we tear
	// down what was live before it — same ordering as DeployPipeline, same
	// reason: a failure above must leave the previously-live version alone
	if previous != nil {
		prevHandle := entities.RuntimeHandle(previous.ContainerID)
		_ = uc.Runtime.Stop(ctx, prevHandle, stopTimeoutSeconds)
		_ = uc.Runtime.Remove(ctx, prevHandle)
		// previous.ImageRef is kept, not removed — it's now itself a
		// rollback target, same reasoning as DeployPipeline's teardown
		if previous.Status == entities.DeploymentRunning {
			_ = uc.DeploymentRepo.UpdateStatus(ctx, previous.ID, entities.DeploymentStopped)
		}
	}

	return nil
}

func (uc *RollbackDeploymentUseCase) discard(ctx context.Context, handle entities.RuntimeHandle) {
	_ = uc.Runtime.Stop(ctx, handle, stopTimeoutSeconds)
	_ = uc.Runtime.Remove(ctx, handle)
}

// fail marks the rollback attempt failed. The deployment being rolled back
// FROM is never touched here — it's still live, since the flip only
// happens after the new one is confirmed ready.
func (uc *RollbackDeploymentUseCase) fail(ctx context.Context, deploymentID, reason string) error {
	_ = uc.DeploymentRepo.SetFailed(ctx, deploymentID, reason, nil)
	return fmt.Errorf("%s", reason)
}
