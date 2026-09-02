package dockerrun

import "golaunch/internal/domain/entities"

const (
	// DefaultNetwork is the shared bridge every app container joins. Caddy
	// sits on it too and reaches apps by container name, which is why no app
	// container ever needs to publish a host port.
	DefaultNetwork = "golaunch-edge"

	// DefaultContainerPort must match the port dockerbuild bakes into the
	// generated image. RuntimeSpec.Port overrides it when set.
	DefaultContainerPort = 3000
)

// EndpointMode selects how a running container is addressed, which depends
// entirely on where the reverse proxy runs.
type EndpointMode string

const (
	// EndpointIP returns the container's address on the edge network. The
	// host routes to the bridge subnet directly, so this is what a proxy
	// running on the host must use — container names only resolve through
	// Docker's embedded DNS, which is reachable from inside a container.
	EndpointIP EndpointMode = "ip"

	// EndpointDNS returns the container name, for a proxy that is itself a
	// container on the edge network.
	EndpointDNS EndpointMode = "dns"
)

// Config is the deployment-wide policy for containers this runtime creates.
// Nothing here is per-project; per-project knobs travel on entities.RuntimeSpec.
type Config struct {
	Network       string
	ContainerPort int
	EndpointMode  EndpointMode

	// DefaultLimits fills in any zero field of RuntimeSpec.Limits, so a spec
	// that specifies nothing still lands inside a cgroup.
	DefaultLimits entities.ResourceLimits

	// Tmpfs are the writable mounts granted on top of a read-only rootfs,
	// keyed by container path. Values are raw mount options.
	Tmpfs map[string]string

	// NoFileSoft/NoFileHard bound open file descriptors per container.
	NoFileSoft int64
	NoFileHard int64
}

func (c Config) withDefaults() Config {
	if c.Network == "" {
		c.Network = DefaultNetwork
	}
	if c.ContainerPort == 0 {
		c.ContainerPort = DefaultContainerPort
	}
	if c.EndpointMode == "" {
		c.EndpointMode = EndpointIP
	}
	if c.DefaultLimits.MemoryMB == 0 {
		c.DefaultLimits.MemoryMB = 512
	}
	if c.DefaultLimits.CPUCores == 0 {
		c.DefaultLimits.CPUCores = 1
	}
	if c.DefaultLimits.PidsLimit == 0 {
		c.DefaultLimits.PidsLimit = 256
	}
	if c.Tmpfs == nil {
		c.Tmpfs = map[string]string{
			"/tmp":             "rw,noexec,nosuid,size=64m",
			"/app/.next/cache": "rw,noexec,nosuid,size=256m",
		}
	}
	if c.NoFileSoft == 0 {
		c.NoFileSoft = 1024
	}
	if c.NoFileHard == 0 {
		c.NoFileHard = 4096
	}
	return c
}

// resolveLimits overlays a spec's limits on the deployment defaults.
func (c Config) resolveLimits(l entities.ResourceLimits) entities.ResourceLimits {
	if l.MemoryMB == 0 {
		l.MemoryMB = c.DefaultLimits.MemoryMB
	}
	if l.CPUCores == 0 {
		l.CPUCores = c.DefaultLimits.CPUCores
	}
	if l.PidsLimit == 0 {
		l.PidsLimit = c.DefaultLimits.PidsLimit
	}
	return l
}
