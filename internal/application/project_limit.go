package application

import (
	"context"
	"fmt"

	"golaunch/internal/domain/repository"
)

const maxProjectsPerUser = 2

// enforceProjectLimit rejects with a clear, client-facing message once
// userID already owns maxProjectsPerUser projects. Called wherever a
// project actually becomes owned — direct creation by an already-logged-in
// caller, and claiming a project staged anonymously before signup. It's
// deliberately not enforced on the anonymous upload/import call itself:
// there's no user identity yet to count against at that point, so the
// limit can only ever be checked once one exists.
func enforceProjectLimit(ctx context.Context, repo repository.ProjectRepository, userID string) error {
	owned, err := repo.ListByUser(ctx, userID)
	if err != nil {
		return err
	}
	if len(owned) >= maxProjectsPerUser {
		return fmt.Errorf("you've reached the %d-project limit — delete a project to deploy a new one", maxProjectsPerUser)
	}
	return nil
}
