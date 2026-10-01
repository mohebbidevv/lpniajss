package application

import "sort"

// appEnv is the complete set of variables a deployed app receives from the
// platform. It is an allowlist by construction: the control plane's own
// environment — database credentials included — is never passed through.
// custom is the project's own stored env vars (see project_env.go),
// layered on top of the platform defaults; reservedEnvKeys keeps a user
// from shadowing anything platform-controlled even if it somehow ended up
// stored (validateEnvVars already rejects it at write time — this is
// belt-and-suspenders, not the only guard).
//
// PORT is not here. Which port an app listens on is a runtime concern: the
// Docker runtime fixes it, host-exec allocates a free one per process, and
// each injects it into the app's environment itself.
func appEnv(custom map[string]string) []string {
	env := map[string]string{"NODE_ENV": "production"}
	for k, v := range custom {
		if reservedEnvKeys[k] {
			continue
		}
		env[k] = v
	}

	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic order — stable logs/diffs across deploys

	out := make([]string, 0, len(env))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}
