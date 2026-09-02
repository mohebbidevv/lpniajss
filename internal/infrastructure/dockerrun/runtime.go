// Package dockerrun implements repository.Runtime on top of the Docker
// daemon. Where the host-exec runtime tracked child processes in memory —
// losing everything when the control plane restarted — containers outlive
// this process, so List() reports what is genuinely running and the
// reconciler has real state to settle against.
package dockerrun

import (
	"context"
	"fmt"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"

	"golaunch/internal/domain/entities"
	"golaunch/internal/domain/repository"
)

type DockerRuntime struct {
	cli *client.Client
	cfg Config
}

var _ repository.Runtime = (*DockerRuntime)(nil)

func New(cli *client.Client, cfg Config) *DockerRuntime {
	return &DockerRuntime{cli: cli, cfg: cfg.withDefaults()}
}

// Start creates and starts the container for spec and returns its ID as the
// handle.
//
// ctx bounds the API calls only. Unlike the host-exec runtime — where the
// caller's deploy context had to be kept away from the child process — a
// container's lifetime is owned by the daemon, so the deploy job's context
// being cancelled the moment Deploy() returns is harmless here.
func (r *DockerRuntime) Start(ctx context.Context, spec entities.RuntimeSpec) (entities.RuntimeHandle, error) {
	name := containerName(spec)
	cfg, hostCfg, netCfg := r.translate(spec)

	created, err := r.cli.ContainerCreate(ctx, cfg, hostCfg, netCfg, nil, name)
	if errdefs.IsConflict(err) {
		// a container from a previous run still holds the name; the
		// deployment ID it encodes is being redeployed, so it is stale
		if rmErr := r.remove(ctx, name, true); rmErr != nil {
			return "", fmt.Errorf("reclaim container name %s: %w", name, rmErr)
		}
		created, err = r.cli.ContainerCreate(ctx, cfg, hostCfg, netCfg, nil, name)
	}
	if err != nil {
		return "", fmt.Errorf("create container %s: %w", name, err)
	}

	if err := r.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		// don't leak a created-but-never-started container holding the name
		_ = r.remove(ctx, created.ID, true)
		return "", fmt.Errorf("start container %s: %w", name, err)
	}

	return entities.RuntimeHandle(created.ID), nil
}

// Stop sends SIGTERM and escalates to SIGKILL after timeoutSeconds. An
// already-gone container is success: the caller wanted it stopped, and it is.
func (r *DockerRuntime) Stop(ctx context.Context, handle entities.RuntimeHandle, timeoutSeconds int) error {
	timeout := timeoutSeconds
	err := r.cli.ContainerStop(ctx, string(handle), container.StopOptions{Timeout: &timeout})
	if err != nil && !client.IsErrNotFound(err) {
		return fmt.Errorf("stop container %s: %w", handle, err)
	}
	return nil
}

// Remove deletes a stopped container. It refuses a running one — same
// semantics as `docker rm` without --force — so a caller that skipped Stop
// finds out rather than silently killing live traffic.
func (r *DockerRuntime) Remove(ctx context.Context, handle entities.RuntimeHandle) error {
	if err := r.remove(ctx, string(handle), false); err != nil {
		if errdefs.IsConflict(err) {
			return fmt.Errorf("cannot remove running container %s, stop it first", handle)
		}
		return fmt.Errorf("remove container %s: %w", handle, err)
	}
	return nil
}

func (r *DockerRuntime) remove(ctx context.Context, ref string, force bool) error {
	err := r.cli.ContainerRemove(ctx, ref, container.RemoveOptions{
		RemoveVolumes: true,
		Force:         force,
	})
	if err != nil && !client.IsErrNotFound(err) {
		return err
	}
	return nil
}

func (r *DockerRuntime) Status(ctx context.Context, handle entities.RuntimeHandle) (entities.RuntimeStatus, error) {
	inspected, err := r.cli.ContainerInspect(ctx, string(handle))
	if err != nil {
		if client.IsErrNotFound(err) {
			return entities.RuntimeStatus{State: entities.RuntimeStateNotFound}, nil
		}
		return entities.RuntimeStatus{}, fmt.Errorf("inspect container %s: %w", handle, err)
	}
	if inspected.ContainerJSONBase == nil {
		return entities.RuntimeStatus{State: entities.RuntimeStateNotFound}, nil
	}
	return statusFromState(inspected.State), nil
}

// List returns every container this platform manages that matches
// labelFilter, running or not. The managed-label scope is applied
// unconditionally, so a nil filter means "all of ours" — never "all on the
// host".
func (r *DockerRuntime) List(ctx context.Context, labelFilter map[string]string) ([]entities.RuntimeInstance, error) {
	summaries, err := r.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: labelFilters(labelFilter),
	})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	out := make([]entities.RuntimeInstance, 0, len(summaries))
	for _, s := range summaries {
		out = append(out, entities.RuntimeInstance{
			Handle: entities.RuntimeHandle(s.ID),
			Labels: decodeLabels(s.Labels),
			Status: statusFromSummary(s.State),
		})
	}
	return out, nil
}
