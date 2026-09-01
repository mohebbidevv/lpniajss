package repository

import (
	"context"
	"golaunch/internal/domain/entities"
)

type Runtime interface {
	Start(ctx context.Context, spec entities.RuntimeSpec) (entities.RuntimeHandle, error)
	Stop(ctx context.Context, handle entities.RuntimeHandle, timeoutSeconds int) error
	Remove(ctx context.Context, handle entities.RuntimeHandle) error
	Status(ctx context.Context, handle entities.RuntimeHandle) (entities.RuntimeStatus, error)
	Logs(ctx context.Context, handle entities.RuntimeHandle, opts entities.LogOptions) (<-chan entities.LogLine, error)
	List(ctx context.Context, labelFilter map[string]string) ([]entities.RuntimeInstance, error)
	Events(ctx context.Context) (<-chan entities.RuntimeEvent, error)
}