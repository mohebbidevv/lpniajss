package repository

import (
	"context"
	"time"

	"golaunch/internal/domain/entities"
)

type ProjectRepository interface {
	Create(ctx context.Context, p *entities.Project) (string, error)
	GetByID(ctx context.Context, id string) (*entities.Project, error)
	UpdateStatus(ctx context.Context, id string, status entities.ProjectStatus) error
	UpdatePortAndStatus(ctx context.Context, id string, port int, status entities.ProjectStatus) error
	ListByStatus(ctx context.Context, status entities.ProjectStatus) ([]*entities.Project, error)
	ListByUser(ctx context.Context, userID string) ([]*entities.Project, error)
	// ClaimProject attaches a project created anonymously (Create called
	// with UserID == "") to a now-known user. It only succeeds the first
	// time — an already-claimed project is left untouched.
	ClaimProject(ctx context.Context, id string, userID string) error
	// ListUnclaimed returns anonymously-created projects (Create called with
	// UserID == "") older than olderThan — a reaper's input for cleaning up
	// submissions abandoned before signup.
	ListUnclaimed(ctx context.Context, olderThan time.Time) ([]*entities.Project, error)
	Delete(ctx context.Context, id string) error
	GetBySlug(ctx context.Context, slug string) (*entities.Project, error)
	SlugExists(ctx context.Context, slug string) (bool, error)
	UpdateSlug(ctx context.Context, id string, slug string) error
	SetCurrentDeployment(ctx context.Context, projectID string, deploymentID string) error
}
