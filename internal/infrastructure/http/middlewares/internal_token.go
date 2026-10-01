package middleware

import (
	"crypto/subtle"
	"net/http"
)

// RequireInternalToken guards operational endpoints that expose internals —
// queue depth, failure counts, breaker state. Those are reconnaissance for
// anyone probing capacity, so they are not public.
//
// Two deliberate choices:
//
//   - subtle.ConstantTimeCompare, not ==. A byte-wise short-circuiting
//     comparison leaks the token one byte at a time through response timing.
//   - 404, not 401. A 401 confirms the endpoint exists; a 404 says nothing.
//
// An empty configured token disables the endpoint entirely rather than
// leaving it open, so a missing config value fails closed.
func RequireInternalToken(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		supplied := r.Header.Get("X-Internal-Token")
		if token == "" || subtle.ConstantTimeCompare([]byte(supplied), []byte(token)) != 1 {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
