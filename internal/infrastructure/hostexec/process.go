package hostexec

import (
	"os/exec"
	"sync"

	"golaunch/internal/domain/entities"
)

type ProcessStatus string

const (
	StatusRunning ProcessStatus = "running"
	StatusExited  ProcessStatus = "exited"
)

const logBufferCap = 500 // last N lines kept per process, for late subscribers

type TrackedProcess struct {
	Handle string
	Cmd    *exec.Cmd
	Labels map[string]string

	mu            sync.Mutex
	status        ProcessStatus
	exitCode      *int
	stopRequested bool
	done          chan struct{}

	logMu   sync.Mutex
	logBuf  []entities.LogLine
	logSubs map[string]chan entities.LogLine
}

func NewTrackedProcess(handle string, cmd *exec.Cmd, labels map[string]string) *TrackedProcess {
	return &TrackedProcess{
		Handle:  handle,
		Cmd:     cmd,
		Labels:  labels,
		status:  StatusRunning,
		done:    make(chan struct{}),
		logSubs: make(map[string]chan entities.LogLine),
	}
}

// ── lifecycle state ─────────────────────────────────────────────────

func (tp *TrackedProcess) MarkExited(code int) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	tp.status = StatusExited
	tp.exitCode = &code
	close(tp.done)
}

func (tp *TrackedProcess) IsExited() (bool, *int) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return tp.status == StatusExited, tp.exitCode
}

func (tp *TrackedProcess) MarkStopRequested() {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	tp.stopRequested = true
}

func (tp *TrackedProcess) WasStopRequested() bool {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return tp.stopRequested
}

// ── logs ─────────────────────────────────────────────────────────────

// appendLog records one line into the ring buffer and fans it out live
// to every currently-subscribed reader. Called from the watch goroutine
// only — one writer, many readers.
func (tp *TrackedProcess) appendLog(line entities.LogLine) {
	tp.logMu.Lock()
	defer tp.logMu.Unlock()

	tp.logBuf = append(tp.logBuf, line)
	if len(tp.logBuf) > logBufferCap {
		tp.logBuf = tp.logBuf[len(tp.logBuf)-logBufferCap:]
	}

	for _, ch := range tp.logSubs {
		select {
		case ch <- line:
		default:
			// subscriber too slow to keep up — drop the line rather than
			// block the process's own log-reading goroutine on a slow client
		}
	}
}

// subscribe returns the current backlog plus a channel that receives every
// new line from this point on. subscriptionID must be passed back to
// unsubscribe when the caller stops listening, or the channel leaks forever.
func (tp *TrackedProcess) subscribe(subscriptionID string) (backlog []entities.LogLine, ch chan entities.LogLine) {
	tp.logMu.Lock()
	defer tp.logMu.Unlock()

	backlog = make([]entities.LogLine, len(tp.logBuf))
	copy(backlog, tp.logBuf)

	ch = make(chan entities.LogLine, 64)
	tp.logSubs[subscriptionID] = ch
	return backlog, ch
}

func (tp *TrackedProcess) unsubscribe(subscriptionID string) {
	tp.logMu.Lock()
	defer tp.logMu.Unlock()

	if ch, ok := tp.logSubs[subscriptionID]; ok {
		close(ch)
		delete(tp.logSubs, subscriptionID)
	}
}