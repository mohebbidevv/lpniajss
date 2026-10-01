package application

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

const sessionTokenBytes = 32

// NewSessionToken generates a raw session token — this exact string is
// what goes in the client's cookie. It is never persisted anywhere; only
// its hash is.
func NewSessionToken() (string, error) {
	raw := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// HashToken maps a raw token to what's actually stored in the sessions
// table — a leaked table can't be replayed as valid cookies, the same
// reasoning as never storing a plaintext password. Exported because both
// the login/logout use cases and the RequireAuth middleware (a different
// package) need the exact same mapping.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
