package application

import (
	"context"
	"crypto/rand"
	"fmt"

	"golaunch/internal/domain/repository"
)

const suffixAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// reservedSlugs are subdomains a tenant must never be able to claim.
//
// A user holding api.<domain> or www.<domain> is a credible phishing vector
// and can break cookie scoping for the real site, and the mail/ns names
// would let them interfere with domain infrastructure. This is a denylist
// on the public hostname, not on the project's display name.
var reservedSlugs = map[string]bool{
	"www": true, "api": true, "admin": true, "app": true, "dashboard": true,
	"auth": true, "login": true, "logout": true, "register": true,
	"static": true, "assets": true, "cdn": true, "media": true,
	"mail": true, "smtp": true, "imap": true, "pop": true, "ftp": true,
	"ns": true, "ns1": true, "ns2": true, "dns": true, "mx": true,
	"docs": true, "blog": true, "status": true, "support": true, "help": true,
	"internal": true, "test": true, "staging": true, "dev": true, "demo": true,
	"billing": true, "account": true, "accounts": true, "security": true,
	"healthz": true, "readyz": true,
}

// IsReservedSlug reports whether slug is claimed by the platform itself.
func IsReservedSlug(slug string) bool { return reservedSlugs[slug] }

// uniqueSlug returns base if it's free, otherwise base with a short random
// suffix, retrying on the astronomically unlikely chance the suffixed form
// collides too. This is for flows where the name is incidental — "app.zip"
// is one of the most common zip filenames there is, and two strangers
// colliding on it has nothing to do with either of them — so silently
// disambiguating is the right default. An explicit rename, where the user
// chose the name on purpose, should reject a collision instead of using this.
func uniqueSlug(ctx context.Context, repo repository.ProjectRepository, base string) (string, error) {
	taken, err := repo.SlugExists(ctx, base)
	if err != nil {
		return "", err
	}
	// A reserved name is treated exactly like a taken one here: this is the
	// incidental-name path (an uploaded "api.zip"), where silently
	// suffixing is friendlier than rejecting. An explicit rename rejects
	// instead — see RenameProjectUseCase.
	if !taken && !IsReservedSlug(base) {
		return base, nil
	}

	const maxAttempts = 5
	for i := 0; i < maxAttempts; i++ {
		suffix, err := randomSuffix(5)
		if err != nil {
			return "", err
		}
		candidate := base + "-" + suffix

		taken, err := repo.SlugExists(ctx, candidate)
		if err != nil {
			return "", err
		}
		if !taken && !IsReservedSlug(candidate) {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("could not find a free slug for %q after %d attempts", base, maxAttempts)
}

func randomSuffix(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate slug suffix: %w", err)
	}
	out := make([]byte, n)
	for i, b := range raw {
		out[i] = suffixAlphabet[int(b)%len(suffixAlphabet)]
	}
	return string(out), nil
}
