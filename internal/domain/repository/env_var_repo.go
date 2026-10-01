package repository

import "context"

// EnvVarRepository persists per-project env vars — plain key/value, no
// entity type needed for something this shallow.
type EnvVarRepository interface {
	ListForProject(ctx context.Context, projectID string) (map[string]string, error)
	// ReplaceForProject replaces the entire set for projectID atomically —
	// the API is PUT-shaped (set the whole thing), not per-key patches.
	ReplaceForProject(ctx context.Context, projectID string, vars map[string]string) error
}
