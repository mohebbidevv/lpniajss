package repository

// import (
// 	"context"
// 	entity "golaunch/internal/domain/entities"
// )

// ProjectRepository defines the interface for interacting with project data storage.
// It abstracts away the database implementation details.
// type ProjectRepository interface {
// 	// Create saves a new project to the repository.
// 	Create(ctx context.Context, p *entity.Project) (string, error)

// GetByID retrieves a project by its database ID.
// GetByID(ctx context.Context, id int64) (*entity.Project, error)

// GetByUniqueKey retrieves a project by its unique access key.
// GetByUniqueKey(ctx context.Context, uniqueKey string) (*entity.Project, error)

// GetSettings retrieves the settings for a specific project.
// GetSettings(ctx context.Context, projectID int64) (entity.Settings, error)

// SaveSettings saves or updates the settings for a project.
// SaveSettings(ctx context.Context, projectID int64, settings entity.Settings) error

// UpdateStatus updates the status of a project.
// UpdateStatus(ctx context.Context, projectID int64, status entity.ProjectStatus) error

// SetPort updates the running port for a project.
// SetPort(ctx context.Context, projectID int64, port int) error

// SetDeployedAt updates the deployed timestamp for a project.
// SetDeployedAt(ctx context.Context, projectID int64, t time.Time) error

// ListByUserID retrieves all projects for a given user.
// ListByUserID(ctx context.Context, userID int64) ([]*entity.Project, error)

// Update updates an existing project. Use with caution, prefer specific update methods.
// Update(ctx context.Context, p *entity.Project) error // Consider if needed, specific methods are often better.

// Delete removes a project and its associated settings.
// Delete(ctx context.Context, projectID int64) error
// }

import (
	"context"
	"golaunch/internal/domain/entities"
)

type ProjectRepository interface {
	Create(ctx context.Context, p *entities.Project) (string, error)
	GetByID(ctx context.Context, id string) (*entities.Project, error)
	UpdateStatus(ctx context.Context, id string, status entities.ProjectStatus) error
	UpdatePortAndStatus(ctx context.Context, id string, port int, status entities.ProjectStatus) error
	ListByStatus(ctx context.Context, status entities.ProjectStatus) ([]*entities.Project, error)
	GetBySlug(ctx context.Context, slug string) (*entities.Project, error)
	SlugExists(ctx context.Context, slug string) (bool, error)
	SetCurrentDeployment(ctx context.Context, projectID string, deploymentID string) error
}

// User represents a user in the domain (minimal for now).
type User struct {
	ID       int64
	Username string
}
