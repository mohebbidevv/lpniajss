package repository

import (
	"context"

	"golaunch/internal/domain/entities"
)

// UserTokenRepository stores the single-use tokens behind email
// verification and password reset.
type UserTokenRepository interface {
	Create(ctx context.Context, t *entities.UserToken) error
	// GetValid returns the token only if it matches the purpose, has not
	// expired and has not been used. Every one of those conditions is
	// enforced here rather than by callers, so no call site can forget one.
	GetValid(ctx context.Context, tokenHash string, purpose entities.TokenPurpose) (*entities.UserToken, error)
	MarkUsed(ctx context.Context, id string) error
	// InvalidateAll burns every outstanding token of a purpose for a user,
	// so requesting a new reset link immediately kills any older one still
	// sitting in an inbox.
	InvalidateAll(ctx context.Context, userID string, purpose entities.TokenPurpose) error
}
