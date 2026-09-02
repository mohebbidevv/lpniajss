package dockerrun

import (
	"fmt"
	"strconv"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"

	"golaunch/internal/domain/entities"
)

// namePrefix keeps our container names distinguishable on a shared host and
// makes the name a stable DNS record on the edge network.
const namePrefix = "golaunch-"

// containerName is the address Caddy dials. It's derived from the deployment
// ID rather than the slug on purpose: two deployments of the same project are
// briefly alive together during a route flip, and a shared name — or a shared
// network alias — would make DNS round-robin between the old and new version.
func containerName(spec entities.RuntimeSpec) string {
	return namePrefix + spec.DeploymentID
}

// translate converts a runtime-agnostic spec into the daemon's create
// payload. It is pure: no daemon calls, so the whole security policy below is
// unit-testable without Docker.
//
// A read-only rootfs is a platform guarantee rather than a per-project knob,
// so entities.ResourceLimits.ReadOnlyFS is deliberately not consulted.
func (r *DockerRuntime) translate(spec entities.RuntimeSpec) (*container.Config, *container.HostConfig, *network.NetworkingConfig) {
	port := spec.Port
	if port == 0 {
		port = r.cfg.ContainerPort
	}
	exposed := nat.Port(fmt.Sprintf("%d/tcp", port))

	limits := r.cfg.resolveLimits(spec.Limits)
	memoryBytes := limits.MemoryMB * 1024 * 1024
	pids := limits.PidsLimit
	useInit := true

	cfg := &container.Config{
		Image:        spec.ImageRef,
		Env:          spec.Env,
		Labels:       encodeLabels(spec.Labels),
		ExposedPorts: nat.PortSet{exposed: struct{}{}},
	}

	hostCfg := &container.HostConfig{
		NetworkMode: container.NetworkMode(r.cfg.Network),

		// Restarts belong to EventConsumer, which caps crash loops. Letting
		// the daemon restart too would fight that and hide the loop.
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},

		// tini as PID 1: Node does not reap orphaned children, and a
		// read-only rootfs makes a zombie pile especially hard to notice.
		Init: &useInit,

		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges:true"},
		Privileged:     false,
		ReadonlyRootfs: true,
		Tmpfs:          r.cfg.Tmpfs,

		// No Binds, no Mounts, no PortBindings — an app container is
		// reachable only from the edge network, and can see nothing of the
		// host filesystem.
		PublishAllPorts: false,

		Resources: container.Resources{
			Memory: memoryBytes,
			// swap pinned to the memory limit disables it, so a leaking app
			// is OOM-killed promptly instead of thrashing the host.
			MemorySwap: memoryBytes,
			NanoCPUs:   int64(limits.CPUCores * 1e9),
			PidsLimit:  &pids,
			Ulimits: []*container.Ulimit{
				{Name: "nofile", Soft: r.cfg.NoFileSoft, Hard: r.cfg.NoFileHard},
				{Name: "nproc", Soft: pids, Hard: pids},
			},
		},
	}

	netCfg := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			r.cfg.Network: {},
		},
	}

	return cfg, hostCfg, netCfg
}

// ContainerAddress is the upstream Caddy should route to for a deployment.
// Exported because the deploy pipeline builds the route before it has
// anything back from the runtime but the deployment ID.
func ContainerAddress(deploymentID string, port int) string {
	if port == 0 {
		port = DefaultContainerPort
	}
	return namePrefix + deploymentID + ":" + strconv.Itoa(port)
}
