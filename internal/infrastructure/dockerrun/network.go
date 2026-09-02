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
// ICC stays enabled, and that is a constraint rather than a preference.
// Docker's enable_icc=false installs a blanket DROP for traffic forwarded
// between containers on the bridge, which blocks the proxy from reaching an
// app just as surely as it blocks one app reaching another. Isolating
// tenants from each other needs a network per deployment with the proxy
// attached to each, not a flag on a shared one.
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
		Labels: map[string]string{labelManaged: managedValue},
	})
	// a concurrent creator winning the race is a success, not a failure
	if err != nil && !errdefs.IsConflict(err) {
		return fmt.Errorf("create network %s: %w", name, err)
	}
	return nil
}
