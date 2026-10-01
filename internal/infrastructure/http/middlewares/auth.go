package middleware

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"golaunch/internal/application"
	"golaunch/internal/domain/entities"
	"golaunch/internal/domain/repository"
)

// SessionCookieName is shared with the auth handlers (setting/clearing the
// cookie) and this middleware (reading it) — one name, one place.
const SessionCookieName = "golaunch_session"

type contextKey int

const userContextKey contextKey = iota

// resolveSessionUser reads the session cookie off r and resolves it to a
// user, or returns an error if the cookie is missing, unknown, or expired.
// Shared by RequireAuth (which rejects the request on error) and
// OptionalAuth (which just proceeds anonymously).
func resolveSessionUser(r *http.Request, sessionRepo repository.SessionRepository, userRepo repository.UserRepository) (*entities.User, error) {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil {
		return nil, err
	}

	session, err := sessionRepo.GetByTokenHash(r.Context(), application.HashToken(cookie.Value))
	if err != nil {
		return nil, err
	}
	if time.Now().After(session.ExpiresAt) {
		return nil, fmt.Errorf("session expired")
	}

	return userRepo.GetByID(r.Context(), session.UserID)
}

// RequireAuth resolves the session cookie to a user and rejects the
// request if it's missing, unknown, or expired. On success the user is
// attached to the request context for downstream handlers.
func RequireAuth(sessionRepo repository.SessionRepository, userRepo repository.UserRepository) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, err := resolveSessionUser(r, sessionRepo, userRepo)
			if err != nil {
				http.Error(w, "not authenticated", http.StatusUnauthorized)
				return
			}

			ctx := context.WithValue(r.Context(), userContextKey, user)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// OptionalAuth attaches a user to the request context when a valid session
// cookie is present, but never rejects the request when one isn't — for
// routes an anonymous caller may use (upload/import before signup), where
// an already-logged-in caller should still be recognized so their
// submission is owned immediately instead of needing a claim step.
func OptionalAuth(sessionRepo repository.SessionRepository, userRepo repository.UserRepository) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if user, err := resolveSessionUser(r, sessionRepo, userRepo); err == nil {
				r = r.WithContext(context.WithValue(r.Context(), userContextKey, user))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// WithUser attaches a user to ctx the same way RequireAuth and OptionalAuth
// do. It is the exported counterpart to UserFromContext, so callers outside
// this package — handler tests especially — can build an authenticated
// context without reaching for the unexported key or standing up a whole
// session store.
func WithUser(ctx context.Context, user *entities.User) context.Context {
	return context.WithValue(ctx, userContextKey, user)
}

func UserFromContext(ctx context.Context) (*entities.User, bool) {
	u, ok := ctx.Value(userContextKey).(*entities.User)
	return u, ok
}
