package application

import (
	"bufio"
	"bytes"
	"context"
	"fmt"

	"golaunch/internal/domain/entities"
	"golaunch/internal/domain/repository"
	"golaunch/internal/infrastructure/caddy"
	"golaunch/internal/infrastructure/utils"
	"golaunch/internal/queue"
	"os"
	"path/filepath"
	"sync/atomic"
)

// port counter — starts at 3000, increments per project
var portCounter atomic.Int32

func init() {
	portCounter.Store(3000)
}

func nextPort() int {
	return int(portCounter.Add(1))
}

// LogLine is what gets pushed over SSE
type LogLine struct {
	Stream string // "stdout" | "stderr"
	Text   string
}

type RunProjectUseCase struct {
	ProjectRepo repository.ProjectRepository
	Runner      *ProjectRunner
	WP          *queue.WorkerPool
	Registry    *LogRegistry
	Caddy *caddy.CaddyClient
}

func NewRunProjectUseCase(repo repository.ProjectRepository, wp *queue.WorkerPool, registry *LogRegistry, caddyClient *caddy.CaddyClient) *RunProjectUseCase {
	return &RunProjectUseCase{
		ProjectRepo: repo,
		Runner:      NewProjectRunner(),
		Registry:    registry,
		WP: wp,
		Caddy: caddyClient,
	}
}

func (uc *RunProjectUseCase) Execute(
	ctx context.Context,
	projectID string,
) (<-chan LogLine, error) {

	project, err := uc.ProjectRepo.GetByID(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("project not found: %w", err)
	}
	if project.Status == entities.StatusRunning {
		return nil, fmt.Errorf("project already running on port %d", project.Port)
	}

	port := nextPort()

	if err := uc.ProjectRepo.UpdatePortAndStatus(ctx, projectID, port, entities.StatusBuilding); err != nil {
		return nil, fmt.Errorf("failed to update status: %w", err)
	}

	// logCh := make(chan LogLine, 64) 

	logCh := make(chan LogLine, 64)
	uc.Registry.Register(projectID, logCh)

	err = uc.WP.Submit(queue.Job{
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

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func splitLines(s string) []string {
	var lines []string
	sc := bufio.NewScanner(bytes.NewBufferString(s))
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines
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
