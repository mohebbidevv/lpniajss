package crypto

import (
	"encoding/base64"
	"strings"
	"testing"
)

func testBox(t *testing.T) *SecretBox {
	t.Helper()
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	box, err := NewSecretBox(key)
	if err != nil {
		t.Fatal(err)
	}
	return box
}

func TestRoundTrip(t *testing.T) {
	box := testBox(t)
	const secret = "sk_live_totally_real_stripe_key"

	ct, err := box.Encrypt(secret)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ct, secret) {
		t.Fatal("plaintext is visible in the ciphertext")
	}
	got, err := box.Decrypt(ct)
	if err != nil {
		t.Fatal(err)
	}
	if got != secret {
		t.Errorf("got %q, want %q", got, secret)
	}
}

// TestNonceIsFresh: encrypting the same plaintext twice must not produce the
// same ciphertext. A repeated nonce under one key in GCM leaks the XOR of
// the plaintexts and allows tag forgery — a total break, not a weakening.
func TestNonceIsFresh(t *testing.T) {
	box := testBox(t)
	a, _ := box.Encrypt("same value")
	b, _ := box.Encrypt("same value")
	if a == b {
		t.Fatal("identical ciphertexts: the nonce is being reused")
	}
}

// TestTamperedCiphertextFails is the reason for AEAD. Without
// authentication, flipping bits would produce attacker-influenced plaintext
// — and this plaintext becomes an env var inside a container we execute.
func TestTamperedCiphertextFails(t *testing.T) {
	box := testBox(t)
	ct, _ := box.Encrypt("value")

	raw := []byte(ct)
	raw[len(raw)-2] ^= 0xff // flip a bit in the base64 payload

	if _, err := box.Decrypt(string(raw)); err == nil {
		t.Fatal("tampered ciphertext decrypted without error")
	}
}

// TestWrongKeyFails: a leaked database is useless without the key.
func TestWrongKeyFails(t *testing.T) {
	ct, _ := testBox(t).Encrypt("value")

	other, err := NewSecretBox(base64.StdEncoding.EncodeToString([]byte("ffffffffffffffffffffffffffffffff")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Decrypt(ct); err == nil {
		t.Fatal("decrypted with the wrong key")
	}
}

// TestPlaintextPassthrough: rows written before encryption existed must keep
// working and get upgraded on their next write.
func TestPlaintextPassthrough(t *testing.T) {
	got, err := testBox(t).Decrypt("legacy-plaintext-value")
	if err != nil {
		t.Fatal(err)
	}
	if got != "legacy-plaintext-value" {
		t.Errorf("got %q, want the value unchanged", got)
	}
}

func TestRejectsBadKeys(t *testing.T) {
	for _, key := range []string{
		"",
		"not-base64!!",
		base64.StdEncoding.EncodeToString([]byte("too-short")),
	} {
		if _, err := NewSecretBox(key); err == nil {
			t.Errorf("accepted invalid key %q", key)
		}
	}
}
