package hostexec

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"golaunch/internal/domain/entities"
	"golaunch/internal/infrastructure/nodedetect"
	"golaunch/internal/infrastructure/utils"
)

type HostExecRuntime struct {
	mu        sync.RWMutex
	processes map[string]*TrackedProcess
	events    chan entities.RuntimeEvent
}

func NewHostExecRuntime() *HostExecRuntime {
	return &HostExecRuntime{
		processes: make(map[string]*TrackedProcess),
		events:    make(chan entities.RuntimeEvent, 64),
	}
}

// ── registry ─────────────────────────────────────────────────────────

func (r *HostExecRuntime) add(handle string, tp *TrackedProcess) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.processes[handle] = tp
}

func (r *HostExecRuntime) get(handle string) (*TrackedProcess, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tp, ok := r.processes[handle]
	return tp, ok
}

func (r *HostExecRuntime) remove(handle string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.processes, handle)
}

func (r *HostExecRuntime) all() []*TrackedProcess {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*TrackedProcess, 0, len(r.processes))
	for _, tp := range r.processes {
		out = append(out, tp)
	}
	return out
}

// ── Start ────────────────────────────────────────────────────────────

// Start spawns the already-built app. spec.ImageRef is the source directory
// that HostExecImageBuilder.Build produced (install+build already ran there);
// this method only starts the process — it does not install or build.
//
// Deliberately exec.Command, not exec.CommandContext(ctx, ...): ctx here is
// the caller's deploy-job context, which the worker pool cancels the moment
// Deploy() returns — but Start() is meant to return immediately while the
// app keeps running for the deployment's whole lifetime. Tying the process
// to ctx would SIGKILL the app within milliseconds of every "successful"
// deploy. The process's lifetime is controlled explicitly via Stop/Remove
// instead.
func (r *HostExecRuntime) Start(ctx context.Context, spec entities.RuntimeSpec) (entities.RuntimeHandle, error) {
	path := spec.ImageRef

	// the port is the runtime's to choose: nothing above this layer knows
	// or needs to know which host port a process ended up on
	port := spec.Port
	if port == 0 {
		allocated, err := allocatePort()
		if err != nil {
			return "", err
		}
		port = allocated
	}

	projSpec := nodedetect.GetProjectSpecs(path, port)

	name := projSpec.StartCmd[0]
	args := projSpec.StartCmd[1:]
	cmd := exec.Command(name, args...)
	cmd.Dir = path
	cmd.Env = append(mergeEnv(spec.Env), fmt.Sprintf("PORT=%d", port))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("stderr pipe: %w", err)
	}

	handle := entities.RuntimeHandle(utils.NewID())
	tp := NewTrackedProcess(string(handle), cmd, spec.Labels, port)

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start process: %w", err)
	}

	r.add(string(handle), tp)
	go r.watch(tp, stdout, stderr)

	return handle, nil
}

// ── watch ────────────────────────────────────────────────────────────

func (r *HostExecRuntime) watch(tp *TrackedProcess, stdout, stderr io.ReadCloser) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		streamLines(tp, entities.LogStdout, stdout)
	}()
	go func() {
		defer wg.Done()
		streamLines(tp, entities.LogStderr, stderr)
	}()

	wg.Wait()
	err := tp.Cmd.Wait()

	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			code = -1
		}
	}

	tp.MarkExited(code)

	r.emit(entities.RuntimeEvent{
		Type:   entities.RuntimeEventDied,
		Handle: entities.RuntimeHandle(tp.Handle),
		Labels: tp.Labels,
	})
}

// streamLines reads a pipe line by line and pushes each one into the
// process's own log buffer/fan-out — this is what makes Logs() work.
func streamLines(tp *TrackedProcess, stream entities.LogStream, r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		tp.appendLog(entities.LogLine{Stream: stream, Text: sc.Text()})
	}
}

func (r *HostExecRuntime) emit(evt entities.RuntimeEvent) {
	select {
	case r.events <- evt:
	default:
		log.Printf("[hostexec] events channel full, dropped %v", evt)
	}
}

// ── Stop ─────────────────────────────────────────────────────────────

func (r *HostExecRuntime) Stop(ctx context.Context, handle entities.RuntimeHandle, timeoutSeconds int) error {
	tp, ok := r.get(string(handle))
	if !ok {
		return nil
	}

	if exited, _ := tp.IsExited(); exited {
		return nil
	}

	tp.MarkStopRequested()

	pgid, err := syscall.Getpgid(tp.Cmd.Process.Pid)
	if err != nil {
		return nil
	}

	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		if err == syscall.ESRCH {
			return nil
		}
		return fmt.Errorf("sigterm: %w", err)
	}

	select {
	case <-tp.done:
		return nil
	case <-time.After(time.Duration(timeoutSeconds) * time.Second):
	}

	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return fmt.Errorf("sigkill: %w", err)
	}

	select {
	case <-tp.done:
	case <-time.After(2 * time.Second):
	}

	return nil
}

// ── Remove ───────────────────────────────────────────────────────────

// Remove drops a process from the registry. Refuses to remove one that's
// still running — same semantics as `docker rm` without --force — call
// Stop first.
func (r *HostExecRuntime) Remove(ctx context.Context, handle entities.RuntimeHandle) error {
	tp, ok := r.get(string(handle))
	if !ok {
		return nil
	}

	exited, _ := tp.IsExited()
	if !exited {
		return fmt.Errorf("cannot remove running process %s, stop it first", handle)
	}

	r.remove(string(handle))
	return nil
}

// ── Status ───────────────────────────────────────────────────────────

func (r *HostExecRuntime) Status(ctx context.Context, handle entities.RuntimeHandle) (entities.RuntimeStatus, error) {
	tp, ok := r.get(string(handle))
	if !ok {
		return entities.RuntimeStatus{State: entities.RuntimeStateNotFound}, nil
	}

	exited, code := tp.IsExited()
	if !exited {
		return entities.RuntimeStatus{State: entities.RuntimeStateRunning}, nil
	}

	return entities.RuntimeStatus{State: entities.RuntimeStateExited, ExitCode: code}, nil
}

// ── List ─────────────────────────────────────────────────────────────

func (r *HostExecRuntime) List(ctx context.Context, labelFilter map[string]string) ([]entities.RuntimeInstance, error) {
	var out []entities.RuntimeInstance

	for _, tp := range r.all() {
		if !matchesLabels(tp.Labels, labelFilter) {
			continue
		}

		exited, code := tp.IsExited()
		state := entities.RuntimeStateRunning
		if exited {
			state = entities.RuntimeStateExited
		}

		out = append(out, entities.RuntimeInstance{
			Handle: entities.RuntimeHandle(tp.Handle),
			Labels: tp.Labels,
			Status: entities.RuntimeStatus{State: state, ExitCode: code},
		})
	}

	return out, nil
}

func matchesLabels(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// ── Events ───────────────────────────────────────────────────────────

func (r *HostExecRuntime) Events(ctx context.Context) (<-chan entities.RuntimeEvent, error) {
	return r.events, nil
}

// ── Logs ─────────────────────────────────────────────────────────────

func (r *HostExecRuntime) Logs(ctx context.Context, handle entities.RuntimeHandle, opts entities.LogOptions) (<-chan entities.LogLine, error) {
	tp, ok := r.get(string(handle))
	if !ok {
		return nil, fmt.Errorf("no such process: %s", handle)
	}

	subID := utils.NewID()
	backlog, live := tp.subscribe(subID)

	out := make(chan entities.LogLine, 64)

	go func() {
		defer close(out)

		tail := backlog
		if opts.Tail > 0 && len(tail) > opts.Tail {
			tail = tail[len(tail)-opts.Tail:]
		}
		for _, line := range tail {
			select {
			case out <- line:
			case <-ctx.Done():
				tp.unsubscribe(subID)
				return
			}
		}

		if !opts.Follow {
			tp.unsubscribe(subID)
			return
		}

		for {
			select {
			case line, ok := <-live:
				if !ok {
					return
				}
				select {
				case out <- line:
				case <-ctx.Done():
					tp.unsubscribe(subID)
					return
				}
			case <-ctx.Done():
				tp.unsubscribe(subID)
				return
			}
		}
	}()

	return out, nil
}
