package entities

import "time"

// User is an account that owns projects. Password hashing and session
// issuance live entirely outside this package — this is just the persisted
// shape a UserRepository reads and writes.
type User struct {
	ID            string    `json:"id"`
	Email         string    `json:"email"`
	PasswordHash  string    `json:"-"`
	EmailVerified bool      `json:"email_verified"`
	CreatedAt     time.Time `json:"created_at"`
}

// TokenPurpose keeps a password-reset token from being redeemable as an
// email verification and vice versa. A token that does not say what it is
// for can be replayed against a different flow.
type TokenPurpose string

const (
	TokenEmailVerify   TokenPurpose = "email_verify"
	TokenPasswordReset TokenPurpose = "password_reset"
)

// UserToken is a single-use, expiring credential delivered by email. Like
// Session, only a hash of it is ever persisted — the raw value exists
// briefly in memory and then only in the user's inbox.
type UserToken struct {
	ID        string
	UserID    string
	Purpose   TokenPurpose
	TokenHash string
	ExpiresAt time.Time
	UsedAt    *time.Time
	CreatedAt time.Time
}

// Session is a server-side record backing one login. TokenHash is its
// natural key: the raw session token only ever exists in the client's
// cookie and briefly in memory right after login, so what's persisted here
// is a hash of it — a leaked sessions table can't be replayed as valid
// cookies, the same reasoning as never storing a plaintext password.
type Session struct {
	TokenHash string    `json:"-"`
	UserID    string    `json:"user_id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}
