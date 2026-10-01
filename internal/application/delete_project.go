package application

import (
	"context"
	"fmt"
	"os"

	"golaunch/internal/domain/entities"
	"golaunch/internal/domain/repository"
	"golaunch/internal/infrastructure/caddy"
)

// DeleteProjectUseCase permanently removes a project: tears down its live
// deployment (container, image, Caddy route) if any, then deletes the
// source directory on disk and the DB row. Deployment rows cascade
// automatically (ON DELETE CASCADE on deployments.project_id), so nothing
// else needs cleaning up there.
type DeleteProjectUseCase struct {
	ProjectRepo    repository.ProjectRepository
	DeploymentRepo repository.DeploymentRepository
	Runtime        repository.Runtime
	Builder        repository.ImageBuilder
	Caddy          *caddy.CaddyClient
}

func NewDeleteProjectUseCase(
	projectRepo repository.ProjectRepository,
	deploymentRepo repository.DeploymentRepository,
	runtime repository.Runtime,
	builder repository.ImageBuilder,
	caddyClient *caddy.CaddyClient,
) *DeleteProjectUseCase {
	return &DeleteProjectUseCase{
		ProjectRepo:    projectRepo,
		DeploymentRepo: deploymentRepo,
		Runtime:        runtime,
		Builder:        builder,
		Caddy:          caddyClient,
	}
}

func (uc *DeleteProjectUseCase) Execute(ctx context.Context, projectID, userID string) error {
	project, err := uc.ProjectRepo.GetByID(ctx, projectID)
	if err != nil {
		return fmt.Errorf("project not found: %w", err)
	}
	if err := mustOwnProject(project, userID); err != nil {
		return err
	}

	if project.CurrentDeploymentID != nil {
		deployment, err := uc.DeploymentRepo.GetByID(ctx, *project.CurrentDeploymentID)
		if err != nil {
			return fmt.Errorf("load current deployment: %w", err)
		}

		// RemoveRoute is a no-op if the route doesn't exist, so it's safe
		// to call unconditionally rather than gating on project status.
		if err := uc.Caddy.RemoveRoute(project.ID); err != nil {
			return fmt.Errorf("remove caddy route: %w", err)
		}

		if deployment.ContainerID != "" {
			handle := entities.RuntimeHandle(deployment.ContainerID)
			if err := uc.Runtime.Stop(ctx, handle, stopTimeoutSeconds); err != nil {
				return fmt.Errorf("stop runtime: %w", err)
			}
			if err := uc.Runtime.Remove(ctx, handle); err != nil {
				return fmt.Errorf("remove runtime: %w", err)
			}
		}

		if deployment.ImageRef != "" {
			if err := uc.Builder.RemoveImage(ctx, deployment.ImageRef); err != nil {
				return fmt.Errorf("remove image: %w", err)
			}
		}
	}

	if err := os.RemoveAll(project.SourceLocation); err != nil {
		return fmt.Errorf("remove source directory: %w", err)
	}

	return uc.ProjectRepo.Delete(ctx, project.ID)
}
