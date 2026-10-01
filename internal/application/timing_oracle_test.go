package application

import (
	"context"
	"testing"
	"time"
)

func TestUnknownEmailCostsSameAsWrongPassword(t *testing.T) {
	ctx := context.Background()
	users := newFakeUserRepo()
	sessions := newFakeSessionRepo()
	seedUser(t, users, "real@example.com")
	uc := NewLoginUserUseCase(users, sessions)

	measure := func(email string) time.Duration {
		start := time.Now()
		_, _, _ = uc.Execute(ctx, email, "wrong-password-here")
		return time.Since(start)
	}

	unknown := measure("nobody@example.com")
	known := measure("real@example.com")

	ratio := float64(unknown) / float64(known)
	t.Logf("unknown=%v known=%v ratio=%.2f", unknown, known, ratio)
	if ratio < 0.5 {
		t.Fatalf("unknown-account path is %.0f%% faster than wrong-password: timing oracle", (1-ratio)*100)
	}
}
