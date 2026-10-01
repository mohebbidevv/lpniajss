package dockerbuild

import (
	"regexp"

	"golaunch/internal/infrastructure/nodedetect"
)

// publicNPMRegistry is what every rewritten "resolved" URL points at.
const publicNPMRegistry = "https://registry.npmjs.org"

// legitimateNpmHosts are left untouched — registry.yarnpkg.com is Yarn's
// own free, public CDN mirror of the npm registry, not a private one, so
// rewriting it would just be pointless churn.
var legitimateNpmHosts = map[string]bool{
	"registry.npmjs.org":   true,
	"registry.yarnpkg.com": true,
}

// npmResolvedPattern matches package-lock.json's "resolved": "https://..."
// entries. The shape is identical across lockfileVersion 1, 2, and 3, so
// no version-aware JSON parsing is needed — a plain string rewrite is both
// simpler and safer than reserializing the whole file.
var npmResolvedPattern = regexp.MustCompile(`"resolved":\s*"https://([^/"]+)(/[^"]*)"`)

// yarnResolvedPattern matches yarn.lock's `resolved "https://..."` entries
// — no colon, and the path commonly carries a trailing "#<hash>" fragment
// that the capture group preserves untouched.
var yarnResolvedPattern = regexp.MustCompile(`resolved "https://([^/"]+)(/[^"]*)"`)

// forcePublicRegistry rewrites any package "resolved" from a third-party
// registry mirror back to the public one, wherever the lockfile happens to
// pin it. Multi-tenant builds have no business trusting whatever registry a
// project's lockfile was generated against — one built inside another
// platform's own paid mirror otherwise fails for every deploy here with a
// payment error that has nothing to do with this platform.
//
// This is safe: every lockfile entry also carries an integrity hash, so a
// swapped host that doesn't serve identical bytes fails the checksum
// instead of silently installing something different. pnpm-lock.yaml isn't
// covered — different, non-regex-friendly format — so this is a no-op for
// pnpm projects rather than a false promise.
func forcePublicRegistry(pm nodedetect.PackageManager, content []byte) (rewritten []byte, changed bool) {
	pattern := npmResolvedPattern
	prefix := `"resolved": "`
	suffix := `"`
	if pm == nodedetect.PackageManagerYarn {
		pattern = yarnResolvedPattern
		prefix = `resolved "`
	}

	out := pattern.ReplaceAllFunc(content, func(match []byte) []byte {
		groups := pattern.FindSubmatch(match)
		if legitimateNpmHosts[string(groups[1])] {
			return match
		}
		return []byte(prefix + publicNPMRegistry + string(groups[2]) + suffix)
	})

	return out, string(out) != string(content)
}
