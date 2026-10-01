package application

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

const userTokenBytes = 32

// NewUserToken generates a raw email-delivered token. 32 bytes from
// crypto/rand — never math/rand, never a UUID, never anything derived from
// the user's data or the clock, all of which are guessable.
//
// The raw value is returned once and never persisted; only HashToken of it
// is stored, so a leaked database yields no working links.
func NewUserToken() (string, error) {
	raw := make([]byte, userTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate user token: %w", err)
	}
	return hex.EncodeToString(raw), nil
}
