package postgres

import (
	"context"
	"fmt"

	"golaunch/internal/domain/entities"
	"golaunch/internal/infrastructure/crypto"

	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvVarRepository encrypts values at rest.
//
// Encryption lives here rather than in a use case so nothing above the
// persistence layer knows ciphertext exists: use cases, the deploy pipeline
// and the handlers all keep dealing in plaintext, and there is exactly one
// place a bug could write a raw secret into a column.
type EnvVarRepository struct {
	DB *pgxpool.Pool

	// Box may be nil, in which case values are stored as plaintext. That is
	// the pre-encryption behaviour, kept so an existing deployment without a
	// key configured still boots — main warns loudly when it happens.
	Box *crypto.SecretBox
}

func NewEnvVarRepository(db *pgxpool.Pool, box *crypto.SecretBox) *EnvVarRepository {
	return &EnvVarRepository{DB: db, Box: box}
}

func (repo *EnvVarRepository) encrypt(value string) (string, error) {
	if repo.Box == nil {
		return value, nil
	}
	return repo.Box.Encrypt(value)
}

func (repo *EnvVarRepository) decrypt(stored string) (string, error) {
	if repo.Box == nil {
		return stored, nil
	}
	return repo.Box.Decrypt(stored)
}

func (repo *EnvVarRepository) ListForProject(ctx context.Context, projectID string) (map[string]string, error) {
	rows, err := repo.DB.Query(ctx,
		`SELECT key, value FROM project_env_vars WHERE project_id = $1`,
		projectID,
	)
	if err != nil {
		return nil, entities.MapPostgresError(err)
	}
	defer rows.Close()

	vars := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scan env var row: %w", err)
		}
		plain, err := repo.decrypt(v)
		if err != nil {
			// Deliberately fatal to the read rather than falling back to
			// the raw column. A decrypt failure means a wrong key or
			// tampering, and handing ciphertext to a container as an env
			// var would be worse than failing the deploy.
			return nil, fmt.Errorf("decrypt env var %s/%s: %w", projectID, k, err)
		}
		vars[k] = plain
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}

	return vars, nil
}

// ReplaceForProject deletes and re-inserts inside one transaction — the
// full-set-replace semantics PUT implies are simpler to get right this way
// than diffing against what's already stored.
func (repo *EnvVarRepository) ReplaceForProject(ctx context.Context, projectID string, vars map[string]string) error {
	tx, err := repo.DB.Begin(ctx)
	if err != nil {
		return entities.MapPostgresError(err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `DELETE FROM project_env_vars WHERE project_id = $1`, projectID); err != nil {
		return entities.MapPostgresError(err)
	}

	for k, v := range vars {
		ciphertext, err := repo.encrypt(v)
		if err != nil {
			return fmt.Errorf("encrypt env var %s/%s: %w", projectID, k, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO project_env_vars (project_id, key, value) VALUES ($1, $2, $3)`,
			projectID, k, ciphertext,
		); err != nil {
			return entities.MapPostgresError(err)
		}
	}

	return entities.MapPostgresError(tx.Commit(ctx))
}
