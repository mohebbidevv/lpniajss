package repository

import (
	"context"
	"golaunch/internal/domain/entities"
)

type DeploymentRepository interface {
	Create(ctx context.Context, d *entities.Deployment) (string, error)
	GetByID(ctx context.Context, id string) (*entities.Deployment, error)
	GetCurrentForProject(ctx context.Context, projectID string) (*entities.Deployment, error)
	ListByStatus(ctx context.Context, status entities.DeploymentStatus) ([]*entities.Deployment, error)
	// ListByProject returns every deployment ever attempted for projectID,
	// newest first — a rollback picker's input.
	ListByProject(ctx context.Context, projectID string) ([]*entities.Deployment, error)
	UpdateStatus(ctx context.Context, id string, status entities.DeploymentStatus) error
	SetImageRef(ctx context.Context, id string, imageRef string) error
	SetContainerInfo(ctx context.Context, id string, containerID string) error
	SetFailed(ctx context.Context, id string, reason string, exitCode *int) error
}
