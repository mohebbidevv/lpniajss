package middleware

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// IPRateLimiter enforces at most `limit` requests per `window` per client
// IP. It's an in-memory fixed-window counter — correct for a single
// control-plane process; a deployment with more than one instance behind a
// load balancer would need a shared store (Redis etc.) instead.
type IPRateLimiter struct {
	mu       sync.Mutex
	limit    int
	window   time.Duration
	counters map[string]*windowCounter
}

type windowCounter struct {
	count      int
	windowEnds time.Time
}

func NewIPRateLimiter(limit int, window time.Duration) *IPRateLimiter {
	rl := &IPRateLimiter{
		limit:    limit,
		window:   window,
		counters: make(map[string]*windowCounter),
	}
	go rl.cleanupLoop()
	return rl
}

func (rl *IPRateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	c, ok := rl.counters[ip]
	if !ok || now.After(c.windowEnds) {
		rl.counters[ip] = &windowCounter{count: 1, windowEnds: now.Add(rl.window)}
		return true
	}
	if c.count >= rl.limit {
		return false
	}
	c.count++
	return true
}

// cleanupLoop periodically drops expired entries so a long-running process
// doesn't accumulate one entry per IP that has ever connected.
func (rl *IPRateLimiter) cleanupLoop() {
	ticker := time.NewTicker(rl.window)
	defer ticker.Stop()
	for range ticker.C {
		rl.mu.Lock()
		now := time.Now()
		for ip, c := range rl.counters {
			if now.After(c.windowEnds) {
				delete(rl.counters, ip)
			}
		}
		rl.mu.Unlock()
	}
}

func (rl *IPRateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow(clientIP(r)) {
			http.Error(w, "too many requests, try again shortly", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ClientIP is exported so handlers key their own limiters on exactly the
// same address this package's middleware does.
func ClientIP(r *http.Request) string { return clientIP(r) }

// clientIP prefers X-Forwarded-For's first hop — Caddy sits in front of
// this process as the actual internet-facing edge — falling back to the
// raw connection address for direct/local traffic.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
