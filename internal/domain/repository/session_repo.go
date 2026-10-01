package repository

import (
	"context"

	"golaunch/internal/domain/entities"
)

type SessionRepository interface {
	// Create persists a session. There's no generated ID to hand back —
	// TokenHash is the natural key, chosen by the caller before this is
	// called.
	Create(ctx context.Context, s *entities.Session) error
	GetByTokenHash(ctx context.Context, tokenHash string) (*entities.Session, error)
	// Delete removes one session — a normal logout.
	Delete(ctx context.Context, tokenHash string) error
	// DeleteAllForUser removes every session for a user — "log out
	// everywhere", and what a password change or account deletion should
	// call to invalidate every device at once.
	DeleteAllForUser(ctx context.Context, userID string) error
}
