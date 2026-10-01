package repository

import (
	"context"
	"time"

	"golaunch/internal/domain/entities"
)

type Runtime interface {
	Start(ctx context.Context, spec entities.RuntimeSpec) (entities.RuntimeHandle, error)
	Stop(ctx context.Context, handle entities.RuntimeHandle, timeoutSeconds int) error
	Remove(ctx context.Context, handle entities.RuntimeHandle) error
	Status(ctx context.Context, handle entities.RuntimeHandle) (entities.RuntimeStatus, error)

	// Endpoint is the address a reverse proxy should dial to reach the
	// instance. Only the runtime knows how its instances are addressed —
	// a host port for host-exec, a container name on a shared network for
	// Docker — so callers must never construct this themselves.
	Endpoint(ctx context.Context, handle entities.RuntimeHandle) (string, error)

	// WaitReady blocks until the instance is accepting connections, or
	// fails if it dies or the timeout elapses. Start only guarantees the
	// process was launched; flipping traffic before this returns serves
	// errors while the app is still booting.
	WaitReady(ctx context.Context, handle entities.RuntimeHandle, timeout time.Duration) error

	// Ping reports whether the runtime backend is reachable. Readiness
	// probes call it; nothing on the deploy path does, so an implementation
	// with no daemon behind it should simply return nil.
	Ping(ctx context.Context) error

	Logs(ctx context.Context, handle entities.RuntimeHandle, opts entities.LogOptions) (<-chan entities.LogLine, error)
	List(ctx context.Context, labelFilter map[string]string) ([]entities.RuntimeInstance, error)
	Events(ctx context.Context) (<-chan entities.RuntimeEvent, error)
}
