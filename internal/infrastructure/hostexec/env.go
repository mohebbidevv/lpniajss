package hostexec

import "os"

// hostPassthrough are the only host variables a deployed app inherits.
// A child process needs PATH to find node at all, and npm needs a HOME to
// resolve its cache — everything else in the control plane's environment,
// database credentials included, stays out of the app.
var hostPassthrough = []string{
	"PATH", "HOME", "LANG", "LC_ALL", "TMPDIR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
}

func baseEnv() []string {
	out := make([]string, 0, len(hostPassthrough))
	for _, key := range hostPassthrough {
		if v, ok := os.LookupEnv(key); ok {
			out = append(out, key+"="+v)
		}
	}
	return out
}

// buildEnv is what install and build steps run with. npm resolves the
// registry over the network, so proxy settings pass through, but nothing
// else from the control plane does.
func buildEnv() []string {
	return append(baseEnv(), "NODE_ENV=development")
}

// mergeEnv layers the caller's variables over the passthrough set. exec
// resolves duplicates in favour of the last occurrence, so spec values win.
func mergeEnv(specEnv []string) []string {
	return append(baseEnv(), specEnv...)
}
