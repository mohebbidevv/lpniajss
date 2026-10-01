package application

import (
	"context"

	"golaunch/internal/domain/repository"
)

// ListProjectsUseCase returns every project a user owns, newest first.
type ListProjectsUseCase struct {
	ProjectRepo repository.ProjectRepository
	Domain      string // for computing LiveURL server-side; never talks to Caddy
}

func NewListProjectsUseCase(projectRepo repository.ProjectRepository, domain string) *ListProjectsUseCase {
	return &ListProjectsUseCase{ProjectRepo: projectRepo, Domain: domain}
}

func (uc *ListProjectsUseCase) Execute(ctx context.Context, userID string) ([]ProjectSummary, error) {
	projects, err := uc.ProjectRepo.ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}

	summaries := make([]ProjectSummary, 0, len(projects))
	for _, p := range projects {
		summaries = append(summaries, newProjectSummary(p, uc.Domain))
	}
	return summaries, nil
}
