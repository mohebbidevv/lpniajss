package middleware

import (
	"testing"
	"time"
)

func TestPerAccountLimitCatchesDistributedAttack(t *testing.T) {
	l := NewLoginLimiter(1000, 3, time.Minute) // effectively no IP limit

	// Every attempt from a different IP, as a botnet would. The per-IP
	// limiter never fires here; only the per-account one can stop this.
	for i := 0; i < 3; i++ {
		if !l.Allow(ipFor(i), "victim@example.com") {
			t.Fatalf("blocked too early at attempt %d", i)
		}
		l.RecordFailure(ipFor(i), "victim@example.com")
	}
	if l.Allow("10.0.0.99", "victim@example.com") {
		t.Fatal("a distributed attack on one account was not blocked")
	}
}

func TestPerIPLimitCatchesSpraying(t *testing.T) {
	l := NewLoginLimiter(3, 1000, time.Minute) // effectively no account limit

	// One attacker, a different account each time — the account limiter
	// never fires, so only the IP limiter can stop this.
	for i := 0; i < 3; i++ {
		if !l.Allow("10.0.0.1", accountFor(i)) {
			t.Fatalf("blocked too early at attempt %d", i)
		}
		l.RecordFailure("10.0.0.1", accountFor(i))
	}
	if l.Allow("10.0.0.1", "someone-else@example.com") {
		t.Fatal("spraying from one IP was not blocked")
	}
}

// TestSuccessDoesNotClearIPBudget: one correct password from behind a shared
// NAT gateway must not hand back the budget an attacker on that gateway is
// burning.
func TestSuccessDoesNotClearIPBudget(t *testing.T) {
	l := NewLoginLimiter(2, 100, time.Minute)

	l.Allow("10.0.0.1", "a@example.com")
	l.RecordFailure("10.0.0.1", "a@example.com")
	l.Allow("10.0.0.1", "b@example.com")
	l.RecordFailure("10.0.0.1", "b@example.com")

	l.RecordSuccess("legit@example.com")

	if l.Allow("10.0.0.1", "c@example.com") {
		t.Fatal("a success reset the IP budget an attacker was burning")
	}
}

// TestSuccessClearsAccountBudget: a legitimate user who mistypes then gets
// it right must not stay penalised.
func TestSuccessClearsAccountBudget(t *testing.T) {
	l := NewLoginLimiter(100, 2, time.Minute)

	l.RecordFailure("10.0.0.1", "user@example.com")
	l.RecordFailure("10.0.0.2", "user@example.com")
	if l.Allow("10.0.0.3", "user@example.com") {
		t.Fatal("account should be blocked before the success")
	}

	l.RecordSuccess("user@example.com")
	if !l.Allow("10.0.0.3", "user@example.com") {
		t.Fatal("account still blocked after a successful sign-in")
	}
}

// TestSlidingWindow: further failures push the window out, so an attacker
// does not collect a fresh budget every window.
func TestSlidingWindow(t *testing.T) {
	l := NewLoginLimiter(100, 2, 50*time.Millisecond)

	l.RecordFailure("10.0.0.1", "user@example.com")
	l.RecordFailure("10.0.0.1", "user@example.com")

	time.Sleep(30 * time.Millisecond)
	l.RecordFailure("10.0.0.1", "user@example.com") // pushes resetAt out

	time.Sleep(30 * time.Millisecond) // past the ORIGINAL window, not the new one
	if l.Allow("10.0.0.1", "user@example.com") {
		t.Fatal("window did not slide: attacker got a fresh budget")
	}
}

func ipFor(i int) string      { return "10.0.0." + string(rune('1'+i)) }
func accountFor(i int) string { return string(rune('a'+i)) + "@example.com" }
