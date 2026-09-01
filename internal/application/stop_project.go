package application

import (
	"context"
	"fmt"

	"golaunch/internal/domain/entities"
	"golaunch/internal/domain/repository"
	"golaunch/internal/infrastructure/caddy"
)

// StopProjectUseCase intentionally stops a project's currently-live
// deployment.
type StopProjectUseCase struct {
	ProjectRepo    repository.ProjectRepository
	DeploymentRepo repository.DeploymentRepository
	Runtime        repository.Runtime
	Caddy          *caddy.CaddyClient
}

func NewStopProjectUseCase(
	projectRepo repository.ProjectRepository,
	deploymentRepo repository.DeploymentRepository,
	runtime repository.Runtime,
	caddyClient *caddy.CaddyClient,
) *StopProjectUseCase {
	return &StopProjectUseCase{
		ProjectRepo:    projectRepo,
		DeploymentRepo: deploymentRepo,
		Runtime:        runtime,
		Caddy:          caddyClient,
	}
}

// Execute stops projectID's live deployment. The DB row is marked stopped
// FIRST, before anything actually happens to the runtime — that's what
// tells EventConsumer the coming death event was intentional rather than a
// crash, so ordering here matters as much as in DeployPipeline.
func (uc *StopProjectUseCase) Execute(ctx context.Context, projectID string) error {
	project, err := uc.ProjectRepo.GetByID(ctx, projectID)
	if err != nil {
		return fmt.Errorf("project not found: %w", err)
	}

	if project.CurrentDeploymentID == nil {
		return fmt.Errorf("project %s has no live deployment", projectID)
	}

	deployment, err := uc.DeploymentRepo.GetByID(ctx, *project.CurrentDeploymentID)
	if err != nil {
		return fmt.Errorf("load current deployment: %w", err)
	}

	if err := uc.DeploymentRepo.UpdateStatus(ctx, deployment.ID, entities.DeploymentStopped); err != nil {
		return fmt.Errorf("mark deployment stopped: %w", err)
	}

	if err := uc.Caddy.RemoveRoute(project.Slug); err != nil {
		return fmt.Errorf("remove caddy route: %w", err)
	}

	handle := entities.RuntimeHandle(deployment.ContainerID)
	if err := uc.Runtime.Stop(ctx, handle, stopTimeoutSeconds); err != nil {
		return fmt.Errorf("stop runtime: %w", err)
	}

	if err := uc.Runtime.Remove(ctx, handle); err != nil {
		return fmt.Errorf("remove runtime: %w", err)
	}

	return uc.ProjectRepo.UpdateStatus(ctx, project.ID, entities.StatusStopped)
}
