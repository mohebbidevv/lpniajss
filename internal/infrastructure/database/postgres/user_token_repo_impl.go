package postgres

import (
	"context"

	"golaunch/internal/domain/entities"

	"github.com/jackc/pgx/v5/pgxpool"
)

type UserTokenRepository struct {
	DB *pgxpool.Pool
}

func NewUserTokenRepository(db *pgxpool.Pool) *UserTokenRepository {
	return &UserTokenRepository{DB: db}
}

func (repo *UserTokenRepository) Create(ctx context.Context, t *entities.UserToken) error {
	_, err := repo.DB.Exec(ctx,
		`INSERT INTO user_tokens (user_id, purpose, token_hash, expires_at)
		 VALUES ($1, $2, $3, $4)`,
		t.UserID, string(t.Purpose), t.TokenHash, t.ExpiresAt,
	)
	return entities.MapPostgresError(err)
}

// GetValid enforces all three conditions — right purpose, unexpired, unused
// — in the query rather than leaving any of them to the caller. A token that
// fails any of them is indistinguishable from one that does not exist.
func (repo *UserTokenRepository) GetValid(ctx context.Context, tokenHash string, purpose entities.TokenPurpose) (*entities.UserToken, error) {
	t := &entities.UserToken{}
	err := repo.DB.QueryRow(ctx,
		`SELECT id, user_id, purpose, token_hash, expires_at, used_at, created_at
		 FROM user_tokens
		 WHERE token_hash = $1
		   AND purpose = $2
		   AND used_at IS NULL
		   AND expires_at > NOW()`,
		tokenHash, string(purpose),
	).Scan(&t.ID, &t.UserID, &t.Purpose, &t.TokenHash, &t.ExpiresAt, &t.UsedAt, &t.CreatedAt)
	if err != nil {
		return nil, entities.MapPostgresError(err)
	}
	return t, nil
}

func (repo *UserTokenRepository) MarkUsed(ctx context.Context, id string) error {
	_, err := repo.DB.Exec(ctx, `UPDATE user_tokens SET used_at = NOW() WHERE id = $1`, id)
	return entities.MapPostgresError(err)
}

// InvalidateAll marks outstanding tokens used rather than deleting them, so
// the audit trail of what was issued survives.
func (repo *UserTokenRepository) InvalidateAll(ctx context.Context, userID string, purpose entities.TokenPurpose) error {
	_, err := repo.DB.Exec(ctx,
		`UPDATE user_tokens SET used_at = NOW()
		 WHERE user_id = $1 AND purpose = $2 AND used_at IS NULL`,
		userID, string(purpose),
	)
	return entities.MapPostgresError(err)
}
