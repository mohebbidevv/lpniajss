package application

import (
	"context"
	"fmt"

	"golaunch/internal/domain/repository"
	"golaunch/internal/infrastructure/utils"
	"golaunch/internal/queue"
	"os"
	"path/filepath"
)

// LogLine is what gets pushed over SSE
type LogLine struct {
	Stream string // "stdout" | "stderr"
	Text   string
}

// RunProjectUseCase is a fast receptionist: validate, register a log
// channel, submit the deploy job, return the channel. All the actual
// port allocation, deployment bookkeeping, and status transitions belong
// to DeployPipeline, which the submitted job runs.
type RunProjectUseCase struct {
	ProjectRepo repository.ProjectRepository
	WP          *queue.WorkerPool
	Registry    *LogRegistry
}

func NewRunProjectUseCase(repo repository.ProjectRepository, wp *queue.WorkerPool, registry *LogRegistry) *RunProjectUseCase {
	return &RunProjectUseCase{
		ProjectRepo: repo,
		Registry:    registry,
		WP:          wp,
	}
}

func (uc *RunProjectUseCase) Execute(
	ctx context.Context,
	projectID string,
) (<-chan LogLine, error) {

	if _, err := uc.ProjectRepo.GetByID(ctx, projectID); err != nil {
		return nil, fmt.Errorf("project not found: %w", err)
	}

	logCh := make(chan LogLine, 64)
	uc.Registry.Register(projectID, logCh)

	err := uc.WP.Submit(queue.Job{
		ID:        utils.NewID(),
		ProjectID: projectID,
	})

	// queue full
	if err != nil {
		uc.Registry.Delete(projectID)
		close(logCh)
		return nil, fmt.Errorf("Runner is busy, try again shortly. %v ", err.Error())
	}

	return logCh, nil
}

func ResolveProjectRoot(extractPath string) (string, error) {
	entries, err := os.ReadDir(extractPath)
	if err != nil {
		return "", err
	}

	// if there's exactly one entry and it's a directory,
	// the zip was packed with a wrapper folder — step into it
	if len(entries) == 1 && entries[0].IsDir() {
		return filepath.Join(extractPath, entries[0].Name()), nil
	}

	// files are at the root of the extract — use as-is
	return extractPath, nil
}
