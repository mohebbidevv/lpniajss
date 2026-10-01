package application

import (
	"context"
	"fmt"

	"golaunch/internal/domain/repository"
)

// GetProjectUseCase returns one project's detail, including its current
// deployment's status if it has ever been deployed.
type GetProjectUseCase struct {
	ProjectRepo    repository.ProjectRepository
	DeploymentRepo repository.DeploymentRepository
	Domain         string
}

func NewGetProjectUseCase(projectRepo repository.ProjectRepository, deploymentRepo repository.DeploymentRepository, domain string) *GetProjectUseCase {
	return &GetProjectUseCase{ProjectRepo: projectRepo, DeploymentRepo: deploymentRepo, Domain: domain}
}

func (uc *GetProjectUseCase) Execute(ctx context.Context, projectID, userID string) (*ProjectDetail, error) {
	project, err := uc.ProjectRepo.GetByID(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("project not found: %w", err)
	}
	if err := mustOwnProject(project, userID); err != nil {
		return nil, err
	}

	detail := &ProjectDetail{ProjectSummary: newProjectSummary(project, uc.Domain)}

	// only present once at least one deploy has happened — mustOwnProject
	// already ran, so CurrentDeploymentID nil is "never deployed", not
	// "hidden from this caller"
	if project.CurrentDeploymentID != nil {
		deployment, err := uc.DeploymentRepo.GetCurrentForProject(ctx, project.ID)
		if err != nil {
			return nil, fmt.Errorf("load current deployment: %w", err)
		}
		detail.Deployment = newDeploymentStatusInfo(deployment)
	}

	return detail, nil
}
