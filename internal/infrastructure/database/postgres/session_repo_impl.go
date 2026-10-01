package postgres

import (
	"context"

	"golaunch/internal/domain/entities"

	"github.com/jackc/pgx/v5/pgxpool"
)

type SessionRepository struct {
	DB *pgxpool.Pool
}

func NewSessionRepository(db *pgxpool.Pool) *SessionRepository {
	return &SessionRepository{DB: db}
}

func (repo *SessionRepository) Create(ctx context.Context, s *entities.Session) error {
	query := `
		INSERT INTO sessions (token_hash, user_id, created_at, expires_at)
		VALUES ($1, $2, NOW(), $3)
	`
	_, err := repo.DB.Exec(ctx, query, s.TokenHash, s.UserID, s.ExpiresAt)
	return entities.MapPostgresError(err)
}

func (repo *SessionRepository) GetByTokenHash(ctx context.Context, tokenHash string) (*entities.Session, error) {
	query := `
		SELECT token_hash, user_id, created_at, expires_at
		FROM sessions
		WHERE token_hash = $1
	`
	s := &entities.Session{}
	err := repo.DB.QueryRow(ctx, query, tokenHash).Scan(&s.TokenHash, &s.UserID, &s.CreatedAt, &s.ExpiresAt)
	if err != nil {
		return nil, entities.MapPostgresError(err)
	}
	return s, nil
}

func (repo *SessionRepository) Delete(ctx context.Context, tokenHash string) error {
	_, err := repo.DB.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, tokenHash)
	return entities.MapPostgresError(err)
}

func (repo *SessionRepository) DeleteAllForUser(ctx context.Context, userID string) error {
	_, err := repo.DB.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID)
	return entities.MapPostgresError(err)
}
