package middleware

import (
	"sync"
	"time"
)

// LoginLimiter throttles authentication attempts on two independent keys.
//
// Both are necessary and neither is sufficient:
//
//   - by IP catches one attacker spraying many accounts, and is what stops
//     login being a CPU-exhaustion vector — bcrypt at cost 12 is ~250ms of
//     pure CPU per attempt, so a handful of concurrent attackers can
//     saturate a core.
//   - by account catches a distributed attacker working one account from a
//     botnet or proxy pool, where every request has a different IP and the
//     IP limiter never fires.
//
// Only failures count, so a user with the right password is never locked
// out by their own successful logins.
type LoginLimiter struct {
	mu sync.Mutex

	byIP      map[string]*attemptWindow
	byAccount map[string]*attemptWindow

	ipLimit      int
	accountLimit int
	window       time.Duration
}

type attemptWindow struct {
	failures int
	resetAt  time.Time
}

func NewLoginLimiter(ipLimit, accountLimit int, window time.Duration) *LoginLimiter {
	if ipLimit <= 0 {
		ipLimit = 20
	}
	if accountLimit <= 0 {
		accountLimit = 5
	}
	if window <= 0 {
		window = 15 * time.Minute
	}
	l := &LoginLimiter{
		byIP:         map[string]*attemptWindow{},
		byAccount:    map[string]*attemptWindow{},
		ipLimit:      ipLimit,
		accountLimit: accountLimit,
		window:       window,
	}
	go l.cleanupLoop()
	return l
}

// Allow must be consulted BEFORE the password is verified. That ordering is
// the point: a throttled request never reaches bcrypt, which is what closes
// the CPU-exhaustion angle rather than merely slowing a guesser down.
func (l *LoginLimiter) Allow(ip, account string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	return !windowBlocked(l.byIP[ip], l.ipLimit, now) &&
		!windowBlocked(l.byAccount[account], l.accountLimit, now)
}

func windowBlocked(w *attemptWindow, limit int, now time.Time) bool {
	return w != nil && now.Before(w.resetAt) && w.failures >= limit
}

// RecordFailure is called only on a genuine authentication failure.
//
// The reset is sliding — each further failure pushes the window out — so a
// persistent attacker stays locked out instead of collecting a fresh budget
// every window.
func (l *LoginLimiter) RecordFailure(ip, account string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	bumpWindow(l.byIP, ip, now, l.window)
	bumpWindow(l.byAccount, account, now, l.window)
}

func bumpWindow(m map[string]*attemptWindow, key string, now time.Time, window time.Duration) {
	if key == "" {
		return
	}
	w, ok := m[key]
	if !ok || now.After(w.resetAt) {
		m[key] = &attemptWindow{failures: 1, resetAt: now.Add(window)}
		return
	}
	w.failures++
	w.resetAt = now.Add(window)
}

// RecordSuccess clears the account's counter but deliberately NOT the IP's.
// One correct password from behind a shared NAT gateway must not hand back
// the budget an attacker on the same gateway is burning.
func (l *LoginLimiter) RecordSuccess(account string) {
	l.mu.Lock()
	delete(l.byAccount, account)
	l.mu.Unlock()
}

func (l *LoginLimiter) cleanupLoop() {
	t := time.NewTicker(l.window)
	defer t.Stop()
	for range t.C {
		l.mu.Lock()
		now := time.Now()
		for _, m := range []map[string]*attemptWindow{l.byIP, l.byAccount} {
			for k, w := range m {
				if now.After(w.resetAt) {
					delete(m, k)
				}
			}
		}
		l.mu.Unlock()
	}
}
