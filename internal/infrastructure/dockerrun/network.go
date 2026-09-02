package dockerrun

import (
	"context"
	"fmt"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

// EnsureNetwork creates the edge network if it isn't there yet. Call it once
// at boot, before any deploy: a container create against a missing network
// fails, and every app container joins this one.
//
// ICC is disabled, so containers on the network cannot reach each other —
// only Caddy, which initiates the connection, can reach them. That turns a
// single compromised tenant app into a dead end instead of a foothold on
// every other app on the box.
func EnsureNetwork(ctx context.Context, cli *client.Client, name string) error {
	if name == "" {
		name = DefaultNetwork
	}

	if _, err := cli.NetworkInspect(ctx, name, network.InspectOptions{}); err == nil {
		return nil
	} else if !client.IsErrNotFound(err) {
		return fmt.Errorf("inspect network %s: %w", name, err)
	}

	_, err := cli.NetworkCreate(ctx, name, network.CreateOptions{
		Driver: "bridge",
		Options: map[string]string{
			"com.docker.network.bridge.enable_icc": "false",
		},
		Labels: map[string]string{labelManaged: managedValue},
	})
	// a concurrent creator winning the race is a success, not a failure
	if err != nil && !errdefs.IsConflict(err) {
		return fmt.Errorf("create network %s: %w", name, err)
	}
	return nil
}
