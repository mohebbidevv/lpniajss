package repository

import (
	"context"

	"golaunch/internal/domain/entities"
)

type UserRepository interface {
	Create(ctx context.Context, u *entities.User) (string, error)
	GetByEmail(ctx context.Context, email string) (*entities.User, error)
	GetByID(ctx context.Context, id string) (*entities.User, error)
	EmailExists(ctx context.Context, email string) (bool, error)
	// UpdatePasswordHash is what a completed password reset writes.
	UpdatePasswordHash(ctx context.Context, userID, passwordHash string) error
	// SetEmailVerified flips the flag a redeemed verification token earns.
	SetEmailVerified(ctx context.Context, userID string, verified bool) error
}
