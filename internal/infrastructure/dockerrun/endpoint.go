package dockerrun

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"

	"golaunch/internal/domain/entities"
)

const (
	readyPollInterval = 250 * time.Millisecond

	// settlePeriod is how long a container without a HEALTHCHECK must stay
	// up before it counts as ready. It catches the dominant failure — an
	// app that exits on boot — without pretending to be a real probe.
	settlePeriod = 2 * time.Second
)

// Endpoint returns the address the reverse proxy should dial. Either form
// reaches the container without publishing a host port; which one is
// correct depends on where the proxy runs (see EndpointMode).
func (r *DockerRuntime) Endpoint(ctx context.Context, handle entities.RuntimeHandle) (string, error) {
	inspected, err := r.cli.ContainerInspect(ctx, string(handle))
	if err != nil {
		return "", fmt.Errorf("inspect container %s: %w", handle, err)
	}
	if inspected.ContainerJSONBase == nil {
		return "", fmt.Errorf("container %s returned no detail", handle)
	}

	port := strconv.Itoa(r.cfg.ContainerPort)

	if r.cfg.EndpointMode == EndpointDNS {
		if inspected.Name == "" {
			return "", fmt.Errorf("container %s has no name", handle)
		}
		// the daemon reports names with a leading slash
		return net.JoinHostPort(strings.TrimPrefix(inspected.Name, "/"), port), nil
	}

	ip, err := r.networkIP(inspected)
	if err != nil {
		return "", fmt.Errorf("container %s: %w", handle, err)
	}
	return net.JoinHostPort(ip, port), nil
}

func (r *DockerRuntime) networkIP(inspected container.InspectResponse) (string, error) {
	if inspected.NetworkSettings == nil {
		return "", fmt.Errorf("no network settings")
	}
	endpoint, ok := inspected.NetworkSettings.Networks[r.cfg.Network]
	if !ok || endpoint == nil {
		return "", fmt.Errorf("not attached to network %s", r.cfg.Network)
	}
	if endpoint.IPAddress == "" {
		return "", fmt.Errorf("no address on network %s yet", r.cfg.Network)
	}
	return endpoint.IPAddress, nil
}

// WaitReady polls the container until it is serving. An image with a
// HEALTHCHECK gets a real readiness signal; one without gets the weaker
// guarantee that it stayed alive through settlePeriod.
func (r *DockerRuntime) WaitReady(ctx context.Context, handle entities.RuntimeHandle, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(readyPollInterval)
	defer ticker.Stop()

	var runningSince time.Time

	for {
		state, err := r.inspectState(ctx, handle)
		if err != nil {
			return err
		}

		if !state.Running {
			return fmt.Errorf("container %s exited during startup with code %d", handle, state.ExitCode)
		}

		switch {
		case state.Health != nil:
			if state.Health.Status == container.Healthy {
				return nil
			}
		default:
			if runningSince.IsZero() {
				runningSince = time.Now()
			}
			if time.Since(runningSince) >= settlePeriod {
				return nil
			}
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			return fmt.Errorf("container %s did not become ready within %s", handle, timeout)
		}
	}
}

func (r *DockerRuntime) inspectState(ctx context.Context, handle entities.RuntimeHandle) (*container.State, error) {
	inspected, err := r.cli.ContainerInspect(ctx, string(handle))
	if err != nil {
		if client.IsErrNotFound(err) {
			return nil, fmt.Errorf("container %s disappeared during startup", handle)
		}
		return nil, fmt.Errorf("inspect container %s: %w", handle, err)
	}
	if inspected.ContainerJSONBase == nil || inspected.State == nil {
		return nil, fmt.Errorf("container %s returned no state", handle)
	}
	return inspected.State, nil
}
