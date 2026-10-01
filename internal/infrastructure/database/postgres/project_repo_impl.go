package postgres

import (
	"context"
	"fmt"
	"time"

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
		INSERT INTO projects (user_id, name, slug, unique_key, source_type, source_location, status, repo_url, repo_ref, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id
	`

	// UserID is "" for a project staged anonymously (upload/import before
	// signup) — the column is nullable precisely so Create can leave
	// ownership unset until ClaimProject fills it in.
	var userID *string
	if p.UserID != "" {
		userID = &p.UserID
	}

	err := infrastructure.Retry(ctx, 3, func() error {
		return repo.DB.QueryRow(
			ctx,
			query,
			userID,
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
		SELECT id, user_id, name, slug, unique_key, source_type, source_location, status, port,
		       current_deployment_id, repo_url, repo_ref, created_at, updated_at
		FROM projects
		WHERE id = $1
	`
	p := &entities.Project{}
	var userID *string
	err := repo.DB.QueryRow(ctx, query, id).Scan(
		&p.ID, &userID, &p.Name, &p.Slug, &p.UniqueKey, &p.SourceType,
		&p.SourceLocation, &p.Status, &p.Port, &p.CurrentDeploymentID,
		&p.RepoURL, &p.RepoRef, &p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		return nil, entities.MapPostgresError(err)
	}
	p.UserID = derefOrEmpty(userID)
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
		SELECT id, user_id, name, slug, unique_key, source_type, source_location, status, port,
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
		var userID *string
		if err := rows.Scan(
			&p.ID, &userID, &p.Name, &p.Slug, &p.UniqueKey, &p.SourceType,
			&p.SourceLocation, &p.Status, &p.Port, &p.CurrentDeploymentID,
			&p.RepoURL, &p.RepoRef, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan project row: %w", err)
		}
		p.UserID = derefOrEmpty(userID)
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}

	return projects, nil
}

// ListAll returns every project, oldest first. Only the image GC sweep uses
// it, and it is deliberately NOT on repository.ProjectRepository: adding it
// there would force a stub into every fake for one caller's benefit.
func (repo *ProjectRepository) ListAll(ctx context.Context) ([]*entities.Project, error) {
	query := `
		SELECT id, user_id, name, slug, unique_key, source_type, source_location, status, port,
		       current_deployment_id, repo_url, repo_ref, created_at, updated_at
		FROM projects
		ORDER BY created_at ASC
	`
	rows, err := repo.DB.Query(ctx, query)
	if err != nil {
		return nil, entities.MapPostgresError(err)
	}
	defer rows.Close()

	var projects []*entities.Project
	for rows.Next() {
		p := &entities.Project{}
		var uid *string
		if err := rows.Scan(
			&p.ID, &uid, &p.Name, &p.Slug, &p.UniqueKey, &p.SourceType,
			&p.SourceLocation, &p.Status, &p.Port, &p.CurrentDeploymentID,
			&p.RepoURL, &p.RepoRef, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan project row: %w", err)
		}
		p.UserID = derefOrEmpty(uid)
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate project rows: %w", err)
	}
	return projects, nil
}

func (repo *ProjectRepository) ListByUser(ctx context.Context, userID string) ([]*entities.Project, error) {
	query := `
		SELECT id, user_id, name, slug, unique_key, source_type, source_location, status, port,
		       current_deployment_id, repo_url, repo_ref, created_at, updated_at
		FROM projects
		WHERE user_id = $1
		ORDER BY created_at DESC
	`
	rows, err := repo.DB.Query(ctx, query, userID)
	if err != nil {
		return nil, entities.MapPostgresError(err)
	}
	defer rows.Close()

	var projects []*entities.Project
	for rows.Next() {
		p := &entities.Project{}
		var uid *string
		if err := rows.Scan(
			&p.ID, &uid, &p.Name, &p.Slug, &p.UniqueKey, &p.SourceType,
			&p.SourceLocation, &p.Status, &p.Port, &p.CurrentDeploymentID,
			&p.RepoURL, &p.RepoRef, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan project row: %w", err)
		}
		p.UserID = derefOrEmpty(uid)
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}

	return projects, nil
}

func (repo *ProjectRepository) GetBySlug(ctx context.Context, slug string) (*entities.Project, error) {
	query := `
		SELECT id, user_id, name, slug, unique_key, source_type, source_location,
		       status, port, current_deployment_id, repo_url, repo_ref, created_at, updated_at
		FROM projects
		WHERE slug = $1
	`
	p := &entities.Project{}
	var userID *string
	err := repo.DB.QueryRow(ctx, query, slug).Scan(
		&p.ID, &userID, &p.Name, &p.Slug, &p.UniqueKey, &p.SourceType,
		&p.SourceLocation, &p.Status, &p.Port, &p.CurrentDeploymentID,
		&p.RepoURL, &p.RepoRef, &p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		return nil, entities.MapPostgresError(err)
	}
	p.UserID = derefOrEmpty(userID)
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

func (repo *ProjectRepository) UpdateSlug(ctx context.Context, id, slug string) error {
	_, err := repo.DB.Exec(ctx,
		`UPDATE projects SET slug=$1, updated_at=NOW() WHERE id=$2`,
		slug, id,
	)
	return entities.MapPostgresError(err)
}

// ClaimProject attaches an anonymously-created project to userID — the
// WHERE clause makes this succeed exactly once, so a claim raced against
// another claim (or a project that was never anonymous) is rejected
// instead of silently overwriting a real owner.
func (repo *ProjectRepository) ClaimProject(ctx context.Context, id, userID string) error {
	tag, err := repo.DB.Exec(ctx,
		`UPDATE projects SET user_id = $1, updated_at = NOW() WHERE id = $2 AND user_id IS NULL`,
		userID, id,
	)
	if err != nil {
		return entities.MapPostgresError(err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("project already claimed")
	}
	return nil
}

func (repo *ProjectRepository) ListUnclaimed(ctx context.Context, olderThan time.Time) ([]*entities.Project, error) {
	query := `
		SELECT id, user_id, name, slug, unique_key, source_type, source_location, status, port,
		       current_deployment_id, repo_url, repo_ref, created_at, updated_at
		FROM projects
		WHERE user_id IS NULL AND created_at < $1
	`
	rows, err := repo.DB.Query(ctx, query, olderThan)
	if err != nil {
		return nil, entities.MapPostgresError(err)
	}
	defer rows.Close()

	var projects []*entities.Project
	for rows.Next() {
		p := &entities.Project{}
		var uid *string
		if err := rows.Scan(
			&p.ID, &uid, &p.Name, &p.Slug, &p.UniqueKey, &p.SourceType,
			&p.SourceLocation, &p.Status, &p.Port, &p.CurrentDeploymentID,
			&p.RepoURL, &p.RepoRef, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan project row: %w", err)
		}
		p.UserID = derefOrEmpty(uid)
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}

	return projects, nil
}

func (repo *ProjectRepository) Delete(ctx context.Context, id string) error {
	_, err := repo.DB.Exec(ctx, `DELETE FROM projects WHERE id = $1`, id)
	return entities.MapPostgresError(err)
}

// derefOrEmpty reads user_id as "" for rows created before auth existed —
// the column is nullable specifically to not break those, since every row
// created going forward always sets it.
func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (repo *ProjectRepository) SetCurrentDeployment(ctx context.Context, projectID, deploymentID string) error {
	_, err := repo.DB.Exec(ctx,
		`UPDATE projects SET current_deployment_id = $1, updated_at = NOW() WHERE id = $2`,
		deploymentID, projectID,
	)
	return err
}
