package dockerrun

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"

	"golaunch/internal/domain/entities"
)

const (
	logChanBuffer = 64
	// maxLogLine caps a single line so one pathological log entry cannot
	// grow the buffer without bound.
	maxLogLine = 1 << 20
)

// Logs streams a container's output. Unlike the host-exec runtime's in-memory
// ring buffer, history comes from the daemon's log driver, so it survives a
// control-plane restart and a Tail request can reach back past this process's
// own lifetime.
func (r *DockerRuntime) Logs(ctx context.Context, handle entities.RuntimeHandle, opts entities.LogOptions) (<-chan entities.LogLine, error) {
	tail := "all"
	if opts.Tail > 0 {
		tail = strconv.Itoa(opts.Tail)
	}

	body, err := r.cli.ContainerLogs(ctx, string(handle), container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     opts.Follow,
		Tail:       tail,
	})
	if err != nil {
		return nil, fmt.Errorf("open logs for %s: %w", handle, err)
	}

	out := make(chan entities.LogLine, logChanBuffer)
	go demux(ctx, body, out)
	return out, nil
}

// demux splits the daemon's multiplexed log stream back into stdout and
// stderr. Containers are always created with Tty false, so the stream always
// carries stdcopy framing.
func demux(ctx context.Context, body io.ReadCloser, out chan<- entities.LogLine) {
	defer close(out)
	defer body.Close()

	// a follow stream blocks in Read until the container writes, so
	// cancellation has to arrive by closing the connection underneath it
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			body.Close()
		case <-done:
		}
	}()

	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()

	go func() {
		_, err := stdcopy.StdCopy(stdoutW, stderrW, body)
		stdoutW.CloseWithError(err)
		stderrW.CloseWithError(err)
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); scanLines(ctx, stdoutR, entities.LogStdout, out) }()
	go func() { defer wg.Done(); scanLines(ctx, stderrR, entities.LogStderr, out) }()
	wg.Wait()
}

func scanLines(ctx context.Context, r io.ReadCloser, stream entities.LogStream, out chan<- entities.LogLine) {
	defer r.Close()

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLogLine)

	for sc.Scan() {
		select {
		case out <- entities.LogLine{Stream: stream, Text: sc.Text()}:
		case <-ctx.Done():
			return
		}
	}
}
