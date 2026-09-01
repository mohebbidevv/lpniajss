package postgres

import (
	"context"
	"fmt"

	"golaunch/internal/domain/entities"

	"github.com/jackc/pgx/v5/pgxpool"
)

type DeploymentRepository struct {
	DB *pgxpool.Pool
}

func NewDeploymentRepository(db *pgxpool.Pool) *DeploymentRepository {
	return &DeploymentRepository{DB: db}
}

func (repo *DeploymentRepository) Create(ctx context.Context, d *entities.Deployment) (string, error) {
	var id string
	query := `
		INSERT INTO deployments (project_id, image_ref, status, created_at)
		VALUES ($1, $2, $3, NOW())
		RETURNING id
	`
	err := repo.DB.QueryRow(ctx, query, d.ProjectID, d.ImageRef, d.Status).Scan(&id)
	if err != nil {
		return "", entities.MapPostgresError(err)
	}
	return id, nil
}

func (repo *DeploymentRepository) GetByID(ctx context.Context, id string) (*entities.Deployment, error) {
	query := `
		SELECT id, project_id, image_ref, container_id, status, port,
		       failure_reason, exit_code, created_at, started_at, stopped_at
		FROM deployments
		WHERE id = $1
	`
	d := &entities.Deployment{}
	err := repo.DB.QueryRow(ctx, query, id).Scan(
		&d.ID, &d.ProjectID, &d.ImageRef, &d.ContainerID, &d.Status, &d.Port,
		&d.FailureReason, &d.ExitCode, &d.CreatedAt, &d.StartedAt, &d.StoppedAt,
	)
	if err != nil {
		return nil, entities.MapPostgresError(err)
	}
	return d, nil
}

func (repo *DeploymentRepository) GetCurrentForProject(ctx context.Context, projectID string) (*entities.Deployment, error) {
	query := `
		SELECT d.id, d.project_id, d.image_ref, d.container_id, d.status, d.port,
		       d.failure_reason, d.exit_code, d.created_at, d.started_at, d.stopped_at
		FROM deployments d
		JOIN projects p ON p.current_deployment_id = d.id
		WHERE p.id = $1
	`
	d := &entities.Deployment{}
	err := repo.DB.QueryRow(ctx, query, projectID).Scan(
		&d.ID, &d.ProjectID, &d.ImageRef, &d.ContainerID, &d.Status, &d.Port,
		&d.FailureReason, &d.ExitCode, &d.CreatedAt, &d.StartedAt, &d.StoppedAt,
	)
	if err != nil {
		return nil, entities.MapPostgresError(err)
	}
	return d, nil
}

func (repo *DeploymentRepository) ListByStatus(ctx context.Context, status entities.DeploymentStatus) ([]*entities.Deployment, error) {
	query := `
		SELECT id, project_id, image_ref, container_id, status, port,
		       failure_reason, exit_code, created_at, started_at, stopped_at
		FROM deployments
		WHERE status = $1
	`
	rows, err := repo.DB.Query(ctx, query, status)
	if err != nil {
		return nil, entities.MapPostgresError(err)
	}
	defer rows.Close()

	var out []*entities.Deployment
	for rows.Next() {
		d := &entities.Deployment{}
		if err := rows.Scan(
			&d.ID, &d.ProjectID, &d.ImageRef, &d.ContainerID, &d.Status, &d.Port,
			&d.FailureReason, &d.ExitCode, &d.CreatedAt, &d.StartedAt, &d.StoppedAt,
		); err != nil {
			return nil, fmt.Errorf("scan deployment row: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	return out, nil
}

func (repo *DeploymentRepository) UpdateStatus(ctx context.Context, id string, status entities.DeploymentStatus) error {
	_, err := repo.DB.Exec(ctx,
		`UPDATE deployments SET status = $1 WHERE id = $2`,
		status, id,
	)
	return err
}

func (repo *DeploymentRepository) SetContainerInfo(ctx context.Context, id, containerID string) error {
	_, err := repo.DB.Exec(ctx,
		`UPDATE deployments SET container_id = $1, status = $2, started_at = NOW() WHERE id = $3`,
		containerID, entities.DeploymentRunning, id,
	)
	return err
}

func (repo *DeploymentRepository) SetFailed(ctx context.Context, id, reason string, exitCode *int) error {
	_, err := repo.DB.Exec(ctx,
		`UPDATE deployments SET status = $1, failure_reason = $2, exit_code = $3, stopped_at = NOW() WHERE id = $4`,
		entities.DeploymentFailed, reason, exitCode, id,
	)
	return err
}