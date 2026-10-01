package application

import (
	"context"
	"fmt"

	"golaunch/internal/domain/entities"
	"golaunch/internal/domain/repository"
	"golaunch/internal/infrastructure/caddy"
)

// RenameProjectUseCase changes a project's public slug. Unlike upload/import
// — where the name is incidental and auto-suffixing on collision is the
// right call — a rename is the user deliberately choosing a hostname, so a
// collision here is rejected outright instead of silently substituted.
type RenameProjectUseCase struct {
	ProjectRepo repository.ProjectRepository
	Caddy       *caddy.CaddyClient
}

func NewRenameProjectUseCase(projectRepo repository.ProjectRepository, caddyClient *caddy.CaddyClient) *RenameProjectUseCase {
	return &RenameProjectUseCase{ProjectRepo: projectRepo, Caddy: caddyClient}
}

// Execute normalizes requestedSlug the same way upload/import do, and
// returns the slug actually applied so the caller can show the user what
// they really got (slugify may have reshaped what they typed).
func (uc *RenameProjectUseCase) Execute(ctx context.Context, projectID, userID, requestedSlug string) (string, error) {
	newSlug := slugify(requestedSlug)
	if newSlug == "" {
		return "", fmt.Errorf("slug cannot be empty")
	}

	project, err := uc.ProjectRepo.GetByID(ctx, projectID)
	if err != nil {
		return "", fmt.Errorf("project not found: %w", err)
	}
	if err := mustOwnProject(project, userID); err != nil {
		return "", err
	}

	if newSlug == project.Slug {
		return newSlug, nil
	}

	// Rejected rather than suffixed: a rename is the user deliberately
	// choosing a hostname, so silently handing them a different one would
	// be worse than saying no.
	if IsReservedSlug(newSlug) {
		return "", fmt.Errorf("%q is reserved and cannot be used as a project name", newSlug)
	}

	exists, err := uc.ProjectRepo.SlugExists(ctx, newSlug)
	if err != nil {
		return "", err
	}
	if exists {
		return "", fmt.Errorf("a project named %q already exists", newSlug)
	}

	// the DB row is the source of truth Reconciler trusts, so it's updated
	// first — if the Caddy swap below fails partway, the next reconcile
	// pass re-derives the correct route from this row and self-heals.
	if err := uc.ProjectRepo.UpdateSlug(ctx, projectID, newSlug); err != nil {
		return "", fmt.Errorf("update slug: %w", err)
	}

	// only a live project has a route worth moving
	if project.Status == entities.StatusRunning {
		if err := uc.Caddy.RetargetRoute(projectID, newSlug); err != nil {
			return "", fmt.Errorf("slug updated, but the live route swap failed — it will self-heal on the next reconcile pass: %w", err)
		}
	}

	return newSlug, nil
}
