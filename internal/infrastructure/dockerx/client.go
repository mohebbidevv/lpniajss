// Package dockerx holds the Docker daemon connection shared by the image
// builder and the container runtime. Both take the same *client.Client so a
// single connection (and a single startup health check) serves both.
package dockerx

import (
	"context"
	"fmt"
	"time"

	"github.com/docker/docker/client"
)

const pingTimeout = 5 * time.Second

// NewClient connects to the daemon described by the environment
// (DOCKER_HOST and friends, falling back to the default unix socket) and
// verifies it's actually reachable before returning. Failing here at
// startup is deliberate: a control plane that can't talk to Docker cannot
// deploy anything, and finding that out on the first user deploy is far
// worse than refusing to boot.
func NewClient(ctx context.Context) (*client.Client, error) {
	cli, err := client.NewClientWithOpts(
		client.FromEnv,
		// negotiate down to whatever the daemon speaks, so a newer SDK
		// doesn't hard-fail against an older daemon
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	if _, err := cli.Ping(pingCtx); err != nil {
		cli.Close()
		return nil, fmt.Errorf(
			"docker daemon unreachable: %w (is the daemon running, and does this user have access to the socket?)",
			err,
		)
	}

	return cli, nil
}
