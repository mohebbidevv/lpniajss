package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
)

// isConflict reports whether err is a use-case-level "already exists" check
// (e.g. UploadProjectUseCase's slug pre-check), as opposed to an unexpected
// failure. Matches stop_handler.go's isNotFound in spirit: a plain string
// check rather than a typed error, until there's a real need for more.
func isConflict(err error) bool {
	return strings.Contains(err.Error(), "already exists") ||
		strings.Contains(err.Error(), "already claimed") ||
		strings.Contains(err.Error(), "already live")
}

// isInvalidRollback reports whether err is RollbackDeploymentUseCase's
// validation rejection (no image ever built, or the image is gone) — a
// client mistake given the current state, not a server-side failure.
func isInvalidRollback(err error) bool {
	return strings.Contains(err.Error(), "no image to roll back to") ||
		strings.Contains(err.Error(), "is no longer available")
}

// isLimitReached reports whether err is enforceProjectLimit's rejection —
// a policy restriction the caller can't retry past, not a transient or
// server-side failure.
func isLimitReached(err error) bool {
	return strings.Contains(err.Error(), "project limit")
}

// isInvalidEnv reports whether err is SetProjectEnvUseCase's validation
// rejection (bad key format, reserved key, too many vars, value too long)
// — a client mistake, not a server-side failure.
func isInvalidEnv(err error) bool {
	return strings.Contains(err.Error(), "invalid env")
}

// respondError writes a JSON {"error": "..."} body for status, consistently
// across every handler. A 500 never echoes err.Error() to the client — that
// text can be a wrapped internal failure (a raw Postgres error, a Docker
// daemon message) that has no business reaching a user's browser — instead
// it's logged server-side and replaced with a generic message. Every other
// status is assumed to have been raised deliberately by a use case with a
// message that's already safe to show as-is.
func respondError(w http.ResponseWriter, err error, status int) {
	message := err.Error()
	if status == http.StatusInternalServerError {
		log.Printf("[http] internal error: %v", err)
		message = "something went wrong, please try again"
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
