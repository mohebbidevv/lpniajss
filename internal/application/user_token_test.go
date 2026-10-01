package application

import (
	"context"
	"strings"
	"testing"
	"time"

	"golaunch/internal/domain/entities"
)

type fakeTokenRepo struct {
	tokens []*entities.UserToken
	nextID int
}

func (r *fakeTokenRepo) Create(ctx context.Context, t *entities.UserToken) error {
	r.nextID++
	t.ID = string(rune('a' + r.nextID))
	r.tokens = append(r.tokens, t)
	return nil
}
func (r *fakeTokenRepo) GetValid(ctx context.Context, hash string, purpose entities.TokenPurpose) (*entities.UserToken, error) {
	for _, t := range r.tokens {
		if t.TokenHash == hash && t.Purpose == purpose && t.UsedAt == nil && t.ExpiresAt.After(time.Now()) {
			return t, nil
		}
	}
	return nil, context.Canceled // any error; callers treat it as "invalid"
}
func (r *fakeTokenRepo) MarkUsed(ctx context.Context, id string) error {
	for _, t := range r.tokens {
		if t.ID == id {
			now := time.Now()
			t.UsedAt = &now
		}
	}
	return nil
}
func (r *fakeTokenRepo) InvalidateAll(ctx context.Context, userID string, purpose entities.TokenPurpose) error {
	for _, t := range r.tokens {
		if t.UserID == userID && t.Purpose == purpose && t.UsedAt == nil {
			now := time.Now()
			t.UsedAt = &now
		}
	}
	return nil
}

type captureMailer struct{ lastToken, lastTo string }

func (m *captureMailer) SendPasswordReset(ctx context.Context, to, raw string) error {
	m.lastTo, m.lastToken = to, raw
	return nil
}
func (m *captureMailer) SendEmailVerification(ctx context.Context, to, raw string) error {
	m.lastTo, m.lastToken = to, raw
	return nil
}

func seedUser(t *testing.T, users *fakeUserRepo, email string) *entities.User {
	t.Helper()
	uc := NewRegisterUserUseCase(users)
	u, err := uc.Execute(context.Background(), email, "correct-horse")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestResetRequestIsEnumerationSafe: an unknown address must be
// indistinguishable from a known one, or this endpoint becomes an oracle for
// which addresses have accounts.
func TestResetRequestIsEnumerationSafe(t *testing.T) {
	users := newFakeUserRepo()
	tokens := &fakeTokenRepo{}
	mail := &captureMailer{}
	uc := NewRequestPasswordResetUseCase(users, tokens, mail)

	if err := uc.Execute(context.Background(), "nobody@example.com"); err != nil {
		t.Fatalf("unknown address returned an error, leaking non-existence: %v", err)
	}
	if mail.lastToken != "" {
		t.Fatal("sent mail for an address with no account")
	}
	if len(tokens.tokens) != 0 {
		t.Fatal("created a token for an address with no account")
	}
}

// TestResetRevokesAllSessions is the step most implementations forget. A
// reset is often requested because someone else has the account; if their
// session survives, the reset accomplished nothing.
func TestResetRevokesAllSessions(t *testing.T) {
	ctx := context.Background()
	users := newFakeUserRepo()
	sessions := newFakeSessionRepo()
	tokens := &fakeTokenRepo{}
	mail := &captureMailer{}

	user := seedUser(t, users, "victim@example.com")

	// An attacker's live session.
	raw, _ := NewSessionToken()
	if err := sessions.Create(ctx, &entities.Session{
		TokenHash: HashToken(raw), UserID: user.ID, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	if err := NewRequestPasswordResetUseCase(users, tokens, mail).Execute(ctx, "victim@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := NewResetPasswordUseCase(users, tokens, sessions).Execute(ctx, mail.lastToken, "brand-new-password"); err != nil {
		t.Fatal(err)
	}

	if _, err := sessions.GetByTokenHash(ctx, HashToken(raw)); err == nil {
		t.Fatal("the attacker's session survived the password reset")
	}
}

// TestResetTokenIsSingleUse: a link in an inbox must not be replayable.
func TestResetTokenIsSingleUse(t *testing.T) {
	ctx := context.Background()
	users := newFakeUserRepo()
	sessions := newFakeSessionRepo()
	tokens := &fakeTokenRepo{}
	mail := &captureMailer{}

	seedUser(t, users, "user@example.com")
	_ = NewRequestPasswordResetUseCase(users, tokens, mail).Execute(ctx, "user@example.com")

	reset := NewResetPasswordUseCase(users, tokens, sessions)
	if err := reset.Execute(ctx, mail.lastToken, "first-new-password"); err != nil {
		t.Fatal(err)
	}
	if err := reset.Execute(ctx, mail.lastToken, "second-new-password"); err == nil {
		t.Fatal("the same reset token was accepted twice")
	}
}

// TestNewResetInvalidatesOlderOne: requesting a fresh link must kill any
// older one still sitting in an inbox.
func TestNewResetInvalidatesOlderOne(t *testing.T) {
	ctx := context.Background()
	users := newFakeUserRepo()
	sessions := newFakeSessionRepo()
	tokens := &fakeTokenRepo{}
	mail := &captureMailer{}

	seedUser(t, users, "user@example.com")
	request := NewRequestPasswordResetUseCase(users, tokens, mail)

	_ = request.Execute(ctx, "user@example.com")
	stale := mail.lastToken
	_ = request.Execute(ctx, "user@example.com")

	if err := NewResetPasswordUseCase(users, tokens, sessions).Execute(ctx, stale, "np"+strings.Repeat("x", 8)); err == nil {
		t.Fatal("the superseded reset link still worked")
	}
}

// TestTokenPurposesDoNotCross: a verification token must never be redeemable
// as a password reset, which is what the purpose column exists for.
func TestTokenPurposesDoNotCross(t *testing.T) {
	ctx := context.Background()
	users := newFakeUserRepo()
	sessions := newFakeSessionRepo()
	tokens := &fakeTokenRepo{}
	mail := &captureMailer{}

	user := seedUser(t, users, "user@example.com")
	if err := NewRequestEmailVerificationUseCase(users, tokens, mail).Execute(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	verifyToken := mail.lastToken

	if err := NewResetPasswordUseCase(users, tokens, sessions).Execute(ctx, verifyToken, "hijacked-password"); err == nil {
		t.Fatal("an email-verification token was redeemed as a password reset")
	}
}

func TestVerifyEmailFlipsFlag(t *testing.T) {
	ctx := context.Background()
	users := newFakeUserRepo()
	tokens := &fakeTokenRepo{}
	mail := &captureMailer{}

	user := seedUser(t, users, "user@example.com")
	if err := NewRequestEmailVerificationUseCase(users, tokens, mail).Execute(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	if err := NewVerifyEmailUseCase(users, tokens).Execute(ctx, mail.lastToken); err != nil {
		t.Fatal(err)
	}

	got, err := users.GetByID(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.EmailVerified {
		t.Error("email_verified was not set")
	}
}
