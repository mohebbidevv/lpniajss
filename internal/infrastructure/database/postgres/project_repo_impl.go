package postgres

import (
	"context"
	"fmt"
	"golaunch/internal/domain/entities"
	"golaunch/internal/infrastructure"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ProjectRepository struct {
	DB *pgxpool.Pool
}

func NewProjectRepository(db *pgxpool.Pool) *ProjectRepository {
	return &ProjectRepository{
		DB: db,
	}
}

func (repo *ProjectRepository) Create(ctx context.Context, p *entities.Project) (string, error) {

	var projectID string
	query := `
		INSERT INTO projects (name, slug, unique_key, source_type, source_location, status, repo_url, repo_ref, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id
	`

	err := infrastructure.Retry(ctx, 3, func() error {
		return repo.DB.QueryRow(
			ctx,
			query,
			// p.UserID,
			p.Name,
			p.Slug,
			p.UniqueKey,
			p.SourceType,
			p.SourceLocation,
			p.Status,
			p.RepoURL,
			p.RepoRef,
			p.CreatedAt,
		).Scan(&projectID)
	})

	err = entities.MapPostgresError(err)

	if err != nil {
		return "", err
	}
	return projectID, nil
}

func (repo *ProjectRepository) GetByID(ctx context.Context, id string) (*entities.Project, error) {
	query := `
		SELECT id, name, slug, unique_key, source_type, source_location, status, port,
		       current_deployment_id, repo_url, repo_ref, created_at, updated_at
		FROM projects
		WHERE id = $1
	`
	p := &entities.Project{}
	err := repo.DB.QueryRow(ctx, query, id).Scan(
		&p.ID, &p.Name, &p.Slug, &p.UniqueKey, &p.SourceType,
		&p.SourceLocation, &p.Status, &p.Port, &p.CurrentDeploymentID,
		&p.RepoURL, &p.RepoRef, &p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		return nil, entities.MapPostgresError(err)
	}
	return p, nil
}

func (repo *ProjectRepository) UpdateStatus(ctx context.Context, id string, status entities.ProjectStatus) error {
	_, err := repo.DB.Exec(ctx,
		`UPDATE projects SET status=$1, updated_at=NOW() WHERE id=$2`,
		status, id,
	)
	return err
}

func (repo *ProjectRepository) UpdatePortAndStatus(ctx context.Context, id string, port int, status entities.ProjectStatus) error {
	_, err := repo.DB.Exec(ctx,
		`UPDATE projects SET port=$1, status=$2, updated_at=NOW() WHERE id=$3`,
		port, status, id,
	)
	return err
}

func (repo *ProjectRepository) ListByStatus(ctx context.Context, status entities.ProjectStatus) ([]*entities.Project, error) {
	query := `
		SELECT id, name, slug, unique_key, source_type, source_location, status, port,
		       current_deployment_id, repo_url, repo_ref, created_at, updated_at
		FROM projects
		WHERE status = $1
	`
	rows, err := repo.DB.Query(ctx, query, status)
	if err != nil {
		return nil, entities.MapPostgresError(err)
	}
	defer rows.Close()

	var projects []*entities.Project
	for rows.Next() {
		p := &entities.Project{}
		if err := rows.Scan(
			&p.ID, &p.Name, &p.Slug, &p.UniqueKey, &p.SourceType,
			&p.SourceLocation, &p.Status, &p.Port, &p.CurrentDeploymentID,
			&p.RepoURL, &p.RepoRef, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan project row: %w", err)
		}
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}

	return projects, nil
}

func (repo *ProjectRepository) GetBySlug(ctx context.Context, slug string) (*entities.Project, error) {
	query := `
		SELECT id, name, slug, unique_key, source_type, source_location,
		       status, port, current_deployment_id, repo_url, repo_ref, created_at, updated_at
		FROM projects
		WHERE slug = $1
	`
	p := &entities.Project{}
	err := repo.DB.QueryRow(ctx, query, slug).Scan(
		&p.ID, &p.Name, &p.Slug, &p.UniqueKey, &p.SourceType,
		&p.SourceLocation, &p.Status, &p.Port, &p.CurrentDeploymentID,
		&p.RepoURL, &p.RepoRef, &p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		return nil, entities.MapPostgresError(err)
	}
	return p, nil
}

func (repo *ProjectRepository) SlugExists(ctx context.Context, slug string) (bool, error) {
	var exists bool
	err := repo.DB.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM projects WHERE slug = $1)`,
		slug,
	).Scan(&exists)
	if err != nil {
		return false, entities.MapPostgresError(err)
	}
	return exists, nil
}

func (repo *ProjectRepository) SetCurrentDeployment(ctx context.Context, projectID, deploymentID string) error {
	_, err := repo.DB.Exec(ctx,
		`UPDATE projects SET current_deployment_id = $1, updated_at = NOW() WHERE id = $2`,
		deploymentID, projectID,
	)
	return err
}