package repository

import (
	"context"
	"golaunch/internal/domain/entities"
)

type ImageBuilder interface {
	Build(ctx context.Context, req entities.BuildRequest, logSink func(entities.LogLine)) (imageRef string, err error)
	RemoveImage(ctx context.Context, imageRef string) error
	ImageExists(ctx context.Context, imageRef string) (bool, error)
}