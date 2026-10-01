package application

import (
	"context"
	"fmt"

	"golaunch/internal/domain/repository"
	"golaunch/internal/infrastructure/logging"
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

	// UserRepo is only consulted to enforce the email-verification gate.
	// Nil disables that gate, which keeps existing tests constructing this
	// use case unchanged.
	UserRepo repository.UserRepository
}

func NewRunProjectUseCase(repo repository.ProjectRepository, wp *queue.WorkerPool, registry *LogRegistry, userRepo repository.UserRepository) *RunProjectUseCase {
	return &RunProjectUseCase{
		ProjectRepo: repo,
		Registry:    registry,
		WP:          wp,
		UserRepo:    userRepo,
	}
}

func (uc *RunProjectUseCase) Execute(
	ctx context.Context,
	projectID, userID string,
) (<-chan LogLine, error) {

	project, err := uc.ProjectRepo.GetByID(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("project not found: %w", err)
	}
	if err := mustOwnProject(project, userID); err != nil {
		return nil, err
	}

	// Deploying is gated on a verified address, but signing in is not.
	// Deploy is the expensive, abusable action, so putting the gate here
	// stops throwaway-address abuse without adding friction to signup.
	if uc.UserRepo != nil {
		user, err := uc.UserRepo.GetByID(ctx, userID)
		if err != nil || user == nil {
			return nil, fmt.Errorf("user not found")
		}
		if !user.EmailVerified {
			return nil, ErrEmailNotVerified
		}
	}

	logCh := make(chan LogLine, 64)
	uc.Registry.Register(projectID, logCh)

	err = uc.WP.Submit(queue.Job{
		ID:        utils.NewID(),
		ProjectID: projectID,
		RequestID: logging.RequestIDFrom(ctx),
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
