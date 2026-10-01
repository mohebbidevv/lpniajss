package application

import (
	"context"
	"testing"

	"golaunch/internal/domain/entities"
)

// ── fakes ────────────────────────────────────────────────────────────────

type fakeUserRepo struct {
	byEmail map[string]*entities.User
	byID    map[string]*entities.User
	nextID  int
}

func newFakeUserRepo() *fakeUserRepo {
	return &fakeUserRepo{byEmail: map[string]*entities.User{}, byID: map[string]*entities.User{}}
}

func (r *fakeUserRepo) Create(ctx context.Context, u *entities.User) (string, error) {
	r.nextID++
	id := string(rune('a' + r.nextID))
	stored := *u
	stored.ID = id
	r.byEmail[u.Email] = &stored
	r.byID[id] = &stored
	return id, nil
}

func (r *fakeUserRepo) GetByEmail(ctx context.Context, email string) (*entities.User, error) {
	u, ok := r.byEmail[email]
	if !ok {
		return nil, errNotFound
	}
	return u, nil
}

func (r *fakeUserRepo) GetByID(ctx context.Context, id string) (*entities.User, error) {
	u, ok := r.byID[id]
	if !ok {
		return nil, errNotFound
	}
	return u, nil
}

func (r *fakeUserRepo) UpdatePasswordHash(ctx context.Context, userID, passwordHash string) error {
	for _, u := range r.byEmail {
		if u.ID == userID {
			u.PasswordHash = passwordHash
			return nil
		}
	}
	return nil
}

func (r *fakeUserRepo) SetEmailVerified(ctx context.Context, userID string, verified bool) error {
	for _, u := range r.byEmail {
		if u.ID == userID {
			u.EmailVerified = verified
			return nil
		}
	}
	return nil
}

func (r *fakeUserRepo) EmailExists(ctx context.Context, email string) (bool, error) {
	_, ok := r.byEmail[email]
	return ok, nil
}

type fakeSessionRepo struct {
	byHash map[string]*entities.Session
}

func newFakeSessionRepo() *fakeSessionRepo {
	return &fakeSessionRepo{byHash: map[string]*entities.Session{}}
}

func (r *fakeSessionRepo) Create(ctx context.Context, s *entities.Session) error {
	stored := *s
	r.byHash[s.TokenHash] = &stored
	return nil
}

func (r *fakeSessionRepo) GetByTokenHash(ctx context.Context, tokenHash string) (*entities.Session, error) {
	s, ok := r.byHash[tokenHash]
	if !ok {
		return nil, errNotFound
	}
	return s, nil
}

func (r *fakeSessionRepo) Delete(ctx context.Context, tokenHash string) error {
	delete(r.byHash, tokenHash)
	return nil
}

func (r *fakeSessionRepo) DeleteAllForUser(ctx context.Context, userID string) error {
	for hash, s := range r.byHash {
		if s.UserID == userID {
			delete(r.byHash, hash)
		}
	}
	return nil
}

var errNotFound = &notFoundErr{}

type notFoundErr struct{}

func (e *notFoundErr) Error() string { return "not found" }

// ── password ─────────────────────────────────────────────────────────────

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !VerifyPassword(hash, "correct horse battery staple") {
		t.Error("correct password must verify")
	}
	if VerifyPassword(hash, "wrong password") {
		t.Error("wrong password must not verify")
	}
}

func TestPasswordHashIsSalted(t *testing.T) {
	h1, _ := HashPassword("same password")
	h2, _ := HashPassword("same password")
	if h1 == h2 {
		t.Error("hashing the same password twice must not produce identical hashes")
	}
}

// ── session tokens ───────────────────────────────────────────────────────

func TestSessionTokensAreUnique(t *testing.T) {
	a, err := NewSessionToken()
	if err != nil {
		t.Fatalf("NewSessionToken: %v", err)
	}
	b, _ := NewSessionToken()
	if a == b {
		t.Error("two generated tokens must not collide")
	}
}

func TestHashTokenIsDeterministicAndDistinct(t *testing.T) {
	if HashToken("abc") != HashToken("abc") {
		t.Error("hashing the same token twice must produce the same hash")
	}
	if HashToken("abc") == HashToken("xyz") {
		t.Error("different tokens must hash differently")
	}
	if HashToken("abc") == "abc" {
		t.Error("HashToken must not be the identity function")
	}
}

// ── ownership ────────────────────────────────────────────────────────────

func TestMustOwnProject(t *testing.T) {
	p := &entities.Project{UserID: "user-1"}

	if err := mustOwnProject(p, "user-1"); err != nil {
		t.Errorf("owner must pass: %v", err)
	}
	if err := mustOwnProject(p, "user-2"); err == nil {
		t.Error("a different user must be rejected")
	}
}

// ── register / login / logout ───────────────────────────────────────────

func TestRegisterThenLogin(t *testing.T) {
	users := newFakeUserRepo()
	sessions := newFakeSessionRepo()
	ctx := context.Background()

	registered, err := NewRegisterUserUseCase(users).Execute(ctx, "Alice@Example.com", "hunter22222")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if registered.Email != "alice@example.com" {
		t.Errorf("email must be normalized to lowercase, got %q", registered.Email)
	}
	if registered.PasswordHash == "hunter22222" {
		t.Fatal("the plaintext password must never be stored as-is")
	}

	login := NewLoginUserUseCase(users, sessions)

	if _, _, err := login.Execute(ctx, "alice@example.com", "wrong-password"); err == nil {
		t.Error("wrong password must be rejected")
	}

	rawToken, user, err := login.Execute(ctx, "ALICE@example.com", "hunter22222")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if user.ID != registered.ID {
		t.Error("login must resolve back to the same account regardless of email casing")
	}

	session, err := sessions.GetByTokenHash(ctx, HashToken(rawToken))
	if err != nil {
		t.Fatalf("session must have been created: %v", err)
	}
	if session.UserID != user.ID {
		t.Error("session must belong to the logged-in user")
	}

	if err := NewLogoutUserUseCase(sessions).Execute(ctx, rawToken); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, err := sessions.GetByTokenHash(ctx, HashToken(rawToken)); err == nil {
		t.Error("session must be gone after logout")
	}
}

func TestRegisterRejectsDuplicateEmail(t *testing.T) {
	users := newFakeUserRepo()
	ctx := context.Background()
	uc := NewRegisterUserUseCase(users)

	if _, err := uc.Execute(ctx, "bob@example.com", "password123"); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if _, err := uc.Execute(ctx, "Bob@Example.com", "different-password"); err == nil {
		t.Error("a second registration with the same email (any casing) must be rejected")
	}
}

func TestLoginUnknownEmailAndWrongPasswordGiveTheSameError(t *testing.T) {
	users := newFakeUserRepo()
	sessions := newFakeSessionRepo()
	ctx := context.Background()

	NewRegisterUserUseCase(users).Execute(ctx, "carol@example.com", "realpassword")
	login := NewLoginUserUseCase(users, sessions)

	_, _, unknownErr := login.Execute(ctx, "nobody@example.com", "whatever")
	_, _, wrongErr := login.Execute(ctx, "carol@example.com", "wrongpassword")

	if unknownErr == nil || wrongErr == nil {
		t.Fatal("both cases must fail")
	}
	if unknownErr.Error() != wrongErr.Error() {
		t.Errorf("unknown-email and wrong-password must be indistinguishable to the caller, got %q vs %q", unknownErr, wrongErr)
	}
}
