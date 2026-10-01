package application

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"golaunch/internal/domain/entities"
	"golaunch/internal/domain/repository"
)

// passwordResetTTL is short on purpose: a reset link is a live credential
// sitting in an inbox.
const passwordResetTTL = time.Hour

type RequestPasswordResetUseCase struct {
	UserRepo  repository.UserRepository
	TokenRepo repository.UserTokenRepository
	Mailer    repository.Mailer
}

func NewRequestPasswordResetUseCase(userRepo repository.UserRepository, tokenRepo repository.UserTokenRepository, mailer repository.Mailer) *RequestPasswordResetUseCase {
	return &RequestPasswordResetUseCase{UserRepo: userRepo, TokenRepo: tokenRepo, Mailer: mailer}
}

// Execute always reports success, even for an address with no account.
//
// Reporting "no such user" would turn this endpoint into an
// account-enumeration oracle — precisely what the login error message is
// careful to avoid, and trivially scriptable against a list of addresses.
func (uc *RequestPasswordResetUseCase) Execute(ctx context.Context, email string) error {
	user, err := uc.UserRepo.GetByEmail(ctx, NormalizeEmail(email))
	if err != nil || user == nil {
		return nil
	}

	// Burn outstanding links first, so requesting a new one immediately
	// kills any older one still sitting in an inbox.
	if err := uc.TokenRepo.InvalidateAll(ctx, user.ID, entities.TokenPasswordReset); err != nil {
		return fmt.Errorf("invalidate previous reset tokens: %w", err)
	}

	raw, err := NewUserToken()
	if err != nil {
		return err
	}
	token := &entities.UserToken{
		UserID:    user.ID,
		Purpose:   entities.TokenPasswordReset,
		TokenHash: HashToken(raw),
		ExpiresAt: time.Now().Add(passwordResetTTL),
	}
	if err := uc.TokenRepo.Create(ctx, token); err != nil {
		return fmt.Errorf("create reset token: %w", err)
	}

	return uc.Mailer.SendPasswordReset(ctx, user.Email, raw)
}

type ResetPasswordUseCase struct {
	UserRepo    repository.UserRepository
	TokenRepo   repository.UserTokenRepository
	SessionRepo repository.SessionRepository
}

func NewResetPasswordUseCase(userRepo repository.UserRepository, tokenRepo repository.UserTokenRepository, sessionRepo repository.SessionRepository) *ResetPasswordUseCase {
	return &ResetPasswordUseCase{UserRepo: userRepo, TokenRepo: tokenRepo, SessionRepo: sessionRepo}
}

func (uc *ResetPasswordUseCase) Execute(ctx context.Context, rawToken, newPassword string) error {
	if len(newPassword) < 8 {
		return fmt.Errorf("password must be at least 8 characters")
	}

	token, err := uc.TokenRepo.GetValid(ctx, HashToken(rawToken), entities.TokenPasswordReset)
	if err != nil || token == nil {
		// One message for expired, already-used and never-existed alike.
		return fmt.Errorf("this reset link is invalid or has expired")
	}

	hash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	if err := uc.UserRepo.UpdatePasswordHash(ctx, token.UserID, hash); err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	if err := uc.TokenRepo.MarkUsed(ctx, token.ID); err != nil {
		return fmt.Errorf("mark token used: %w", err)
	}

	// The step that is easiest to forget and matters most: a reset is often
	// requested precisely because someone else has the account. If their
	// session survives it, the reset accomplished nothing.
	//
	// This is the concrete payoff of server-side sessions over stateless
	// JWTs — with JWTs this would need a separate revocation list, which is
	// a session store wearing a disguise.
	if err := uc.SessionRepo.DeleteAllForUser(ctx, token.UserID); err != nil {
		// The password is already changed; failing the request now would
		// tell the user it did not work when it did. Log loudly instead.
		slog.Error("password reset: could not revoke existing sessions",
			"user_id", token.UserID, "error", err)
	}

	return nil
}
