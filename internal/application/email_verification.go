package application

import (
	"context"
	"fmt"
	"time"

	"golaunch/internal/domain/entities"
	"golaunch/internal/domain/repository"
)

// emailVerificationTTL is longer than a password reset's: this link is a
// convenience rather than a credential, and a user may not check mail for a
// day.
const emailVerificationTTL = 24 * time.Hour

// ErrEmailNotVerified gates the actions an unverified account may not take.
var ErrEmailNotVerified = fmt.Errorf("email address is not verified")

type RequestEmailVerificationUseCase struct {
	UserRepo  repository.UserRepository
	TokenRepo repository.UserTokenRepository
	Mailer    repository.Mailer
}

func NewRequestEmailVerificationUseCase(userRepo repository.UserRepository, tokenRepo repository.UserTokenRepository, mailer repository.Mailer) *RequestEmailVerificationUseCase {
	return &RequestEmailVerificationUseCase{UserRepo: userRepo, TokenRepo: tokenRepo, Mailer: mailer}
}

// Execute issues (or reissues) a verification link for an already
// authenticated user, so unlike password reset there is no enumeration
// concern — the caller already proved who they are.
func (uc *RequestEmailVerificationUseCase) Execute(ctx context.Context, userID string) error {
	user, err := uc.UserRepo.GetByID(ctx, userID)
	if err != nil || user == nil {
		return fmt.Errorf("user not found")
	}
	if user.EmailVerified {
		return nil
	}

	if err := uc.TokenRepo.InvalidateAll(ctx, user.ID, entities.TokenEmailVerify); err != nil {
		return fmt.Errorf("invalidate previous verification tokens: %w", err)
	}

	raw, err := NewUserToken()
	if err != nil {
		return err
	}
	token := &entities.UserToken{
		UserID:    user.ID,
		Purpose:   entities.TokenEmailVerify,
		TokenHash: HashToken(raw),
		ExpiresAt: time.Now().Add(emailVerificationTTL),
	}
	if err := uc.TokenRepo.Create(ctx, token); err != nil {
		return fmt.Errorf("create verification token: %w", err)
	}

	return uc.Mailer.SendEmailVerification(ctx, user.Email, raw)
}

type VerifyEmailUseCase struct {
	UserRepo  repository.UserRepository
	TokenRepo repository.UserTokenRepository
}

func NewVerifyEmailUseCase(userRepo repository.UserRepository, tokenRepo repository.UserTokenRepository) *VerifyEmailUseCase {
	return &VerifyEmailUseCase{UserRepo: userRepo, TokenRepo: tokenRepo}
}

func (uc *VerifyEmailUseCase) Execute(ctx context.Context, rawToken string) error {
	token, err := uc.TokenRepo.GetValid(ctx, HashToken(rawToken), entities.TokenEmailVerify)
	if err != nil || token == nil {
		return fmt.Errorf("this verification link is invalid or has expired")
	}

	if err := uc.UserRepo.SetEmailVerified(ctx, token.UserID, true); err != nil {
		return fmt.Errorf("mark email verified: %w", err)
	}
	return uc.TokenRepo.MarkUsed(ctx, token.ID)
}
