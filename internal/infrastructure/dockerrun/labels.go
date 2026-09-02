package dockerrun

import (
	"strings"

	"github.com/docker/docker/api/types/filters"
)

const (
	// labelPrefix namespaces our labels. Docker labels are a flat namespace
	// shared with every other container on the host, so bare keys like
	// "slug" would be a collision waiting to happen.
	labelPrefix = "golaunch."

	// labelManaged marks a container as ours. Every List and Events call
	// filters on it, which is what stops a nil label filter from returning —
	// and the reconciler from stopping — unrelated containers such as Caddy
	// or Postgres.
	labelManaged = labelPrefix + "managed"
	managedValue = "true"
)

// encodeLabels namespaces application labels for the daemon.
func encodeLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+1)
	out[labelManaged] = managedValue
	for k, v := range in {
		out[labelPrefix+k] = v
	}
	return out
}

// decodeLabels is the inverse. It also serves event actor attributes, where
// container labels arrive mixed in with daemon-supplied keys like "name" and
// "exitCode"; the prefix check drops those.
func decodeLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if k == labelManaged || !strings.HasPrefix(k, labelPrefix) {
			continue
		}
		out[strings.TrimPrefix(k, labelPrefix)] = v
	}
	return out
}

// labelFilters builds a daemon-side filter from an application label filter,
// always scoped to containers this platform manages.
func labelFilters(in map[string]string) filters.Args {
	args := filters.NewArgs(filters.Arg("label", labelManaged+"="+managedValue))
	for k, v := range in {
		args.Add("label", labelPrefix+k+"="+v)
	}
	return args
}
