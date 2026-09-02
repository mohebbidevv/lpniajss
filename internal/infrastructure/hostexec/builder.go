package hostexec

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"

	"golaunch/internal/domain/entities"
	"golaunch/internal/infrastructure/nodedetect"
)

// HostExecImageBuilder implements repository.ImageBuilder for the host-exec
// runtime. There's no real image — "building" means running npm install and
// the framework build step in place in the source directory. The imageRef it
// returns is that directory's path; HostExecRuntime.Start treats
// RuntimeSpec.ImageRef as the directory to run the start command from.
type HostExecImageBuilder struct{}

func NewHostExecImageBuilder() *HostExecImageBuilder {
	return &HostExecImageBuilder{}
}

func (b *HostExecImageBuilder) Build(ctx context.Context, req entities.BuildRequest, logSink func(entities.LogLine)) (string, error) {
	specs := nodedetect.GetProjectSpecs(req.SourceDir, 0) // port doesn't affect install/build commands

	logSink(entities.LogLine{Stream: entities.LogInfo, Text: "running npm install..."})
	if err := runStreamed(ctx, req.SourceDir, buildEnv(), specs.InstallCmd, logSink); err != nil {
		return "", fmt.Errorf("install failed: %w", err)
	}

	if len(specs.BuildCmd) > 0 {
		logSink(entities.LogLine{Stream: entities.LogInfo, Text: "building..."})
		if err := runStreamed(ctx, req.SourceDir, buildEnv(), specs.BuildCmd, logSink); err != nil {
			return "", fmt.Errorf("build failed: %w", err)
		}
	}

	return req.SourceDir, nil
}

// RemoveImage is a no-op — the built artifact is just files in the source
// directory, and directory cleanup is upload/project deletion's job, not
// the builder's.
func (b *HostExecImageBuilder) RemoveImage(ctx context.Context, imageRef string) error {
	return nil
}

func (b *HostExecImageBuilder) ImageExists(ctx context.Context, imageRef string) (bool, error) {
	_, err := os.Stat(imageRef)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// runStreamed runs a command and pushes every stdout/stderr line to logSink
// live, unlike the old runBuffered which only surfaced output on failure —
// the whole point of the builder taking a logSink is that install/build
// output streams to the caller as it happens.
func runStreamed(ctx context.Context, dir string, env []string, cmdArgs []string, logSink func(entities.LogLine)) error {
	cmd := exec.CommandContext(ctx, cmdArgs[0], cmdArgs[1:]...)
	cmd.Dir = dir
	cmd.Env = env

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", cmdArgs[0], err)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	pipe := func(r interface{ Read([]byte) (int, error) }, stream entities.LogStream) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			logSink(entities.LogLine{Stream: stream, Text: sc.Text()})
		}
	}
	go pipe(stdout, entities.LogStdout)
	go pipe(stderr, entities.LogStderr)
	wg.Wait()

	return cmd.Wait()
}
