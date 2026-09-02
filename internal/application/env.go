package application

// appEnv is the complete set of variables a deployed app receives from the
// platform. It is an allowlist by construction: the control plane's own
// environment — database credentials included — is never passed through.
//
// PORT is not here. Which port an app listens on is a runtime concern: the
// Docker runtime fixes it, host-exec allocates a free one per process, and
// each injects it into the app's environment itself.
func appEnv() []string {
	return []string{"NODE_ENV=production"}
}
