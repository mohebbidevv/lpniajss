package application

import (
	"fmt"

	"golaunch/internal/domain/entities"
)

// mustOwnProject reports an error if project doesn't belong to userID. The
// message is deliberately identical to a plain not-found — confirming a
// project ID exists but belongs to someone else is its own information leak.
func mustOwnProject(project *entities.Project, userID string) error {
	if project.UserID != userID {
		return fmt.Errorf("project not found")
	}
	return nil
}
