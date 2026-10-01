package postgres

import (
	"context"

	"golaunch/internal/domain/entities"

	"github.com/jackc/pgx/v5/pgxpool"
)

type UserRepository struct {
	DB *pgxpool.Pool
}

func NewUserRepository(db *pgxpool.Pool) *UserRepository {
	return &UserRepository{DB: db}
}

func (repo *UserRepository) Create(ctx context.Context, u *entities.User) (string, error) {
	var id string
	query := `
		INSERT INTO users (email, password_hash, created_at)
		VALUES ($1, $2, NOW())
		RETURNING id
	`
	err := repo.DB.QueryRow(ctx, query, u.Email, u.PasswordHash).Scan(&id)
	if err != nil {
		return "", entities.MapPostgresError(err)
	}
	return id, nil
}

func (repo *UserRepository) GetByEmail(ctx context.Context, email string) (*entities.User, error) {
	query := `
		SELECT id, email, password_hash, email_verified, created_at
		FROM users
		WHERE email = $1
	`
	u := &entities.User{}
	err := repo.DB.QueryRow(ctx, query, email).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.EmailVerified, &u.CreatedAt)
	if err != nil {
		return nil, entities.MapPostgresError(err)
	}
	return u, nil
}

func (repo *UserRepository) EmailExists(ctx context.Context, email string) (bool, error) {
	var exists bool
	err := repo.DB.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE email = $1)`,
		email,
	).Scan(&exists)
	if err != nil {
		return false, entities.MapPostgresError(err)
	}
	return exists, nil
}

func (repo *UserRepository) GetByID(ctx context.Context, id string) (*entities.User, error) {
	query := `
		SELECT id, email, password_hash, email_verified, created_at
		FROM users
		WHERE id = $1
	`
	u := &entities.User{}
	err := repo.DB.QueryRow(ctx, query, id).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.EmailVerified, &u.CreatedAt)
	if err != nil {
		return nil, entities.MapPostgresError(err)
	}
	return u, nil
}

func (repo *UserRepository) UpdatePasswordHash(ctx context.Context, userID, passwordHash string) error {
	_, err := repo.DB.Exec(ctx,
		`UPDATE users SET password_hash = $1 WHERE id = $2`,
		passwordHash, userID,
	)
	return entities.MapPostgresError(err)
}

func (repo *UserRepository) SetEmailVerified(ctx context.Context, userID string, verified bool) error {
	_, err := repo.DB.Exec(ctx,
		`UPDATE users SET email_verified = $1 WHERE id = $2`,
		verified, userID,
	)
	return entities.MapPostgresError(err)
}
