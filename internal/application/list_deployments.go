package application

import (
	"context"
	"fmt"

	"golaunch/internal/domain/repository"
)

// ListDeploymentsUseCase returns a project's full deployment history,
// newest first — the input to a rollback picker.
type ListDeploymentsUseCase struct {
	ProjectRepo    repository.ProjectRepository
	DeploymentRepo repository.DeploymentRepository
}

func NewListDeploymentsUseCase(projectRepo repository.ProjectRepository, deploymentRepo repository.DeploymentRepository) *ListDeploymentsUseCase {
	return &ListDeploymentsUseCase{ProjectRepo: projectRepo, DeploymentRepo: deploymentRepo}
}

func (uc *ListDeploymentsUseCase) Execute(ctx context.Context, projectID, userID string) ([]DeploymentSummary, error) {
	project, err := uc.ProjectRepo.GetByID(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("project not found: %w", err)
	}
	if err := mustOwnProject(project, userID); err != nil {
		return nil, err
	}

	deployments, err := uc.DeploymentRepo.ListByProject(ctx, project.ID)
	if err != nil {
		return nil, err
	}

	summaries := make([]DeploymentSummary, 0, len(deployments))
	for _, d := range deployments {
		summaries = append(summaries, newDeploymentSummary(d, project.CurrentDeploymentID))
	}
	return summaries, nil
}
