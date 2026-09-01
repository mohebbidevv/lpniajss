package repository

import "context"

// GitSource fetches a git repo's contents onto local disk. Sync is
// idempotent — clone if destDir doesn't exist yet, or fetch+reset to latest
// if it does — so the same call works both for a project's initial import
// and for every later redeploy.
type GitSource interface {
	Sync(ctx context.Context, repoURL, ref, destDir string) error
}
