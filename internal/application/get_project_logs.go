package application

import (
	"context"
	"fmt"

	"golaunch/internal/domain/entities"
	"golaunch/internal/domain/repository"
)

// logTailLines is how much history a newly-opened log stream gets before
// switching to following new output — enough to see what a crash looked
// like without replaying the app's entire lifetime.
const logTailLines = 200

// GetProjectLogsUseCase streams the currently-live container's logs —
// "what is this app doing right now," as opposed to RunProjectUseCase's
// build-time log stream, which only exists for the duration of a deploy.
type GetProjectLogsUseCase struct {
	ProjectRepo    repository.ProjectRepository
	DeploymentRepo repository.DeploymentRepository
	Runtime        repository.Runtime
}

func NewGetProjectLogsUseCase(projectRepo repository.ProjectRepository, deploymentRepo repository.DeploymentRepository, runtime repository.Runtime) *GetProjectLogsUseCase {
	return &GetProjectLogsUseCase{ProjectRepo: projectRepo, DeploymentRepo: deploymentRepo, Runtime: runtime}
}

func (uc *GetProjectLogsUseCase) Execute(ctx context.Context, projectID, userID string) (<-chan entities.LogLine, error) {
	project, err := uc.ProjectRepo.GetByID(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("project not found: %w", err)
	}
	if err := mustOwnProject(project, userID); err != nil {
		return nil, err
	}

	if project.CurrentDeploymentID == nil {
		return nil, fmt.Errorf("project has never been deployed")
	}

	deployment, err := uc.DeploymentRepo.GetCurrentForProject(ctx, project.ID)
	if err != nil {
		return nil, fmt.Errorf("load current deployment: %w", err)
	}
	if deployment.ContainerID == "" {
		return nil, fmt.Errorf("nothing is currently running for this project")
	}

	handle := entities.RuntimeHandle(deployment.ContainerID)
	return uc.Runtime.Logs(ctx, handle, entities.LogOptions{Follow: true, Tail: logTailLines})
}
