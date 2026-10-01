package application

import (
	"context"
	"fmt"
	"time"

	"golaunch/internal/domain/entities"
	"golaunch/internal/domain/repository"
)

// SessionTTL is also the cookie's Max-Age (set by the handler) — keeping
// them equal means the cookie never outlives (or expires before) the
// session row it names.
const SessionTTL = 30 * 24 * time.Hour

type LoginUserUseCase struct {
	UserRepo    repository.UserRepository
	SessionRepo repository.SessionRepository
}

func NewLoginUserUseCase(userRepo repository.UserRepository, sessionRepo repository.SessionRepository) *LoginUserUseCase {
	return &LoginUserUseCase{UserRepo: userRepo, SessionRepo: sessionRepo}
}

// Execute returns the raw session token — the only time it's ever in
// plaintext outside the client's cookie — for the handler to set as one.
func (uc *LoginUserUseCase) Execute(ctx context.Context, email, password string) (string, *entities.User, error) {
	email = NormalizeEmail(email)

	user, err := uc.UserRepo.GetByEmail(ctx, email)
	// Same message either way — confirming "no such account" vs "wrong
	// password" through the response is a user-enumeration leak.
	//
	// The two branches are kept separate rather than short-circuited with
	// || so the not-found path still pays bcrypt's cost. Collapsing them
	// would return in ~1ms for an unknown address and ~250ms for a known
	// one, leaking through timing exactly what the shared message hides.
	if err != nil || user == nil {
		VerifyDummyPassword(password)
		return "", nil, fmt.Errorf("invalid email or password")
	}
	if !VerifyPassword(user.PasswordHash, password) {
		return "", nil, fmt.Errorf("invalid email or password")
	}

	rawToken, err := NewSessionToken()
	if err != nil {
		return "", nil, err
	}

	session := &entities.Session{
		TokenHash: HashToken(rawToken),
		UserID:    user.ID,
		ExpiresAt: time.Now().Add(SessionTTL),
	}
	if err := uc.SessionRepo.Create(ctx, session); err != nil {
		return "", nil, err
	}

	return rawToken, user, nil
}
