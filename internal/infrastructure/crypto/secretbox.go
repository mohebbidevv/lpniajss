// Package crypto provides authenticated encryption for values that must
// survive a database compromise — currently project env vars, which are the
// highest-value data this platform stores.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

var ErrInvalidKey = errors.New("crypto: key must be 32 bytes, base64-encoded")

// cipherPrefix tags ciphertext this package produced. Two jobs: it lets
// Decrypt pass through rows written before encryption existed, and the
// version number makes key rotation possible later without having to guess
// which scheme produced a given row.
const cipherPrefix = "enc:v1:"

// SecretBox is AES-256-GCM.
//
// GCM is authenticated (AEAD): a tampered ciphertext fails to decrypt rather
// than decrypting to attacker-chosen plaintext. That matters specifically
// here, because these values become environment variables inside a container
// this platform then executes — with unauthenticated encryption, write
// access to the database would be a code-execution primitive.
type SecretBox struct{ aead cipher.AEAD }

// NewSecretBox takes a base64-encoded 32-byte key. Generate one with:
//
//	openssl rand -base64 32
func NewSecretBox(base64Key string) (*SecretBox, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(base64Key))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidKey, err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("%w: got %d bytes", ErrInvalidKey, len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &SecretBox{aead: aead}, nil
}

// Encrypt returns cipherPrefix + base64(nonce || ciphertext || tag).
//
// A fresh random nonce per call is mandatory, not a nicety: reusing a nonce
// under the same key in GCM leaks the XOR of the two plaintexts AND allows
// forging the authentication tag. It is a total break, not a weakening. The
// nonce is not secret, so it is simply prepended.
func (s *SecretBox) Encrypt(plaintext string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	// Seal appends to its first argument, so passing nonce produces the
	// nonce-prefixed output in one allocation.
	sealed := s.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return cipherPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt.
//
// Input without cipherPrefix is returned unchanged: rows written before
// encryption existed keep working and get upgraded on their next write.
// Remove that branch once a backfill confirms no unprefixed rows remain.
func (s *SecretBox) Decrypt(encoded string) (string, error) {
	if !strings.HasPrefix(encoded, cipherPrefix) {
		return encoded, nil
	}

	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(encoded, cipherPrefix))
	if err != nil {
		return "", fmt.Errorf("decode ciphertext: %w", err)
	}
	n := s.aead.NonceSize()
	if len(raw) < n {
		return "", errors.New("crypto: ciphertext too short")
	}

	// Open verifies the tag before returning anything, so a non-nil error
	// means tampering or the wrong key. Callers must never swallow it and
	// fall back to the raw column value.
	out, err := s.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(out), nil
}
