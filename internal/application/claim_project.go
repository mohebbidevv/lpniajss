package application

import (
	"context"
	"fmt"

	"golaunch/internal/domain/repository"
)

// ClaimProjectUseCase attaches a project staged anonymously (upload/import
// called before the user had an account) to the now-authenticated caller.
// It's idempotent for the caller that already owns the project — the
// frontend can call it unconditionally right after auth resolves, whether
// or not the submission happened to already be authenticated.
type ClaimProjectUseCase struct {
	ProjectRepo repository.ProjectRepository
	Domain      string
}

func NewClaimProjectUseCase(projectRepo repository.ProjectRepository, domain string) *ClaimProjectUseCase {
	return &ClaimProjectUseCase{ProjectRepo: projectRepo, Domain: domain}
}

func (uc *ClaimProjectUseCase) Execute(ctx context.Context, projectID, userID string) (*ProjectSummary, error) {
	project, err := uc.ProjectRepo.GetByID(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("project not found: %w", err)
	}

	switch project.UserID {
	case userID:
		// already claimed by this caller — nothing to do
	case "":
		if err := enforceProjectLimit(ctx, uc.ProjectRepo, userID); err != nil {
			return nil, err
		}
		if err := uc.ProjectRepo.ClaimProject(ctx, projectID, userID); err != nil {
			return nil, err
		}
		project.UserID = userID
	default:
		// owned by someone else — same message as mustOwnProject, so this
		// doesn't confirm the project's existence to a caller who doesn't
		// own it
		return nil, fmt.Errorf("project not found")
	}

	summary := newProjectSummary(project, uc.Domain)
	return &summary, nil
}
