package application

import (
	"context"
	"testing"
	"time"

	"golaunch/internal/domain/entities"
)

// fakeLogRuntime is the minimal repository.Runtime stub needed to drive
// captureFailureLogs — only Logs is exercised.
type fakeLogRuntime struct {
	lines []entities.LogLine
	err   error
}

func (r *fakeLogRuntime) Logs(ctx context.Context, handle entities.RuntimeHandle, opts entities.LogOptions) (<-chan entities.LogLine, error) {
	if r.err != nil {
		return nil, r.err
	}
	ch := make(chan entities.LogLine, len(r.lines))
	for _, l := range r.lines {
		ch <- l
	}
	close(ch)
	return ch, nil
}

func (r *fakeLogRuntime) Start(ctx context.Context, spec entities.RuntimeSpec) (entities.RuntimeHandle, error) {
	return "", nil
}
func (r *fakeLogRuntime) Stop(ctx context.Context, handle entities.RuntimeHandle, timeoutSeconds int) error {
	return nil
}
func (r *fakeLogRuntime) Remove(ctx context.Context, handle entities.RuntimeHandle) error { return nil }
func (r *fakeLogRuntime) Status(ctx context.Context, handle entities.RuntimeHandle) (entities.RuntimeStatus, error) {
	return entities.RuntimeStatus{}, nil
}
func (r *fakeLogRuntime) Endpoint(ctx context.Context, handle entities.RuntimeHandle) (string, error) {
	return "", nil
}
func (r *fakeLogRuntime) WaitReady(ctx context.Context, handle entities.RuntimeHandle, timeout time.Duration) error {
	return nil
}
func (r *fakeLogRuntime) List(ctx context.Context, labelFilter map[string]string) ([]entities.RuntimeInstance, error) {
	return nil, nil
}
func (r *fakeLogRuntime) Ping(ctx context.Context) error { return nil }

func (r *fakeLogRuntime) Events(ctx context.Context) (<-chan entities.RuntimeEvent, error) {
	return nil, nil
}

func TestCaptureFailureLogsStreamsBeforeDiscardWouldDeleteThem(t *testing.T) {
	runtime := &fakeLogRuntime{lines: []entities.LogLine{
		{Stream: entities.LogStderr, Text: "Error: EROFS: read-only file system, open '.next/trace'"},
		{Stream: entities.LogStderr, Text: "    at Object.openSync (node:fs:590:3)"},
	}}
	registry := NewLogRegistry()
	p := &DeployPipeline{Runtime: runtime, Registry: registry}

	logCh := make(chan LogLine, 10)
	registry.Register("proj-1", logCh)

	p.captureFailureLogs(context.Background(), "proj-1", "some-handle")

	close(logCh)
	var got []LogLine
	for l := range logCh {
		got = append(got, l)
	}

	if len(got) != 2 {
		t.Fatalf("got %d streamed lines, want 2: %+v", len(got), got)
	}
	if got[0].Text != "Error: EROFS: read-only file system, open '.next/trace'" {
		t.Errorf("first line = %q, want the real crash message, not just an exit code", got[0].Text)
	}
	if got[0].Stream != "stderr" {
		t.Errorf("stream = %q, want stderr preserved from the runtime", got[0].Stream)
	}
}

func TestCaptureFailureLogsIsSilentWhenLogsUnavailable(t *testing.T) {
	// the instance is already gone, or the runtime can't fetch logs for
	// some other reason — this must never panic or block the caller,
	// since it always runs on the way to reporting a real error anyway.
	runtime := &fakeLogRuntime{err: context.DeadlineExceeded}
	registry := NewLogRegistry()
	p := &DeployPipeline{Runtime: runtime, Registry: registry}

	logCh := make(chan LogLine, 10)
	registry.Register("proj-1", logCh)

	p.captureFailureLogs(context.Background(), "proj-1", "some-handle")

	close(logCh)
	var got []LogLine
	for l := range logCh {
		got = append(got, l)
	}
	if len(got) != 0 {
		t.Errorf("expected no lines streamed when Logs fails, got %+v", got)
	}
}
