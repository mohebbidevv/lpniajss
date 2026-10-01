package queue

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// defaultJobTimeout only applies when the caller doesn't supply one. It
// exists purely as a fallback so a zero-value construction doesn't mean
// "no timeout at all" — pick a real ceiling for your actual job shape
// instead of relying on this.
const defaultJobTimeout = 20 * time.Minute

const (
	defaultMaxRetries = 3
	defaultQueueSize  = 200

	// breakerThreshold consecutive terminal failures trip the breaker;
	// breakerCooldown is how long new submissions are refused afterwards.
	// Any single success resets the count, so this only fires on a
	// genuinely systemic fault — a wedged Docker daemon, a full disk, an
	// unreachable registry — and not on a run of users pushing broken code.
	//
	// Without it, a shared cause makes every queued project retry
	// simultaneously and generates peak load exactly when the system is
	// least able to serve it.
	breakerThreshold = 10
	breakerCooldown  = 30 * time.Second
)

var (
	ErrQueueFull   = errors.New("queue: job buffer full")
	ErrPoolClosed  = errors.New("queue: pool is shutting down")
	ErrBreakerOpen = errors.New("queue: too many consecutive failures, backing off")
)

// poolStats counters are atomics because four workers mutate them
// concurrently. A plain int++ is load/add/store — three separate machine
// operations — so concurrent increments silently lose updates, and an
// unsynchronized read can observe a partially updated struct.
type poolStats struct {
	submitted atomic.Int64 // accepted by Submit, once per job
	attempts  atomic.Int64 // processor invocations, retries included
	completed atomic.Int64 // terminal success
	failed    atomic.Int64 // terminal failure only, after retries are exhausted
	retried   atomic.Int64 // requeues
	rejected  atomic.Int64 // Submit refused: full, closed, or breaker open
}

type WorkerPool struct {
	jobs       chan Job
	maxWorkers int
	maxRetries int
	jobTimeout time.Duration
	processor  func(ctx context.Context, job Job) error

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// inflight tracks retry timers sleeping outside the pool. Draining has
	// to wait on these too, or shutdown races a pending requeue.
	inflight sync.WaitGroup

	// mu guards closed. Submit takes it for reading — concurrent and cheap,
	// so workers never contend — and ShutDown takes it for writing exactly
	// once.
	//
	// The jobs channel is deliberately NEVER closed. Senders live in HTTP
	// handlers, the reconciler, and EventConsumer's AfterFunc timers, which
	// fire seconds after the event that scheduled them; there is no
	// sequencing that would make close() provably safe, and a send on a
	// closed channel is a process-killing panic rather than an error.
	mu     sync.RWMutex
	closed bool

	breakerMu    sync.Mutex
	consecFails  int
	breakerUntil time.Time

	stats poolStats
}

// NewWorkerPool starts a pool of maxWorkers goroutines pulling from a single
// job queue, each job bounded by jobTimeout. jobTimeout <= 0 falls back to
// defaultJobTimeout — the caller should size this to whatever the processor
// actually does; a hardcoded value here would silently outlive (or cut
// short) whatever the real per-job budget turns out to be.
func NewWorkerPool(
	maxWorkers int,
	processor func(ctx context.Context, job Job) error,
	jobTimeout time.Duration,
) *WorkerPool {
	if maxWorkers <= 0 {
		maxWorkers = runtime.NumCPU()
	}
	if jobTimeout <= 0 {
		jobTimeout = defaultJobTimeout
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &WorkerPool{
		jobs:       make(chan Job, defaultQueueSize),
		maxWorkers: maxWorkers,
		maxRetries: defaultMaxRetries,
		jobTimeout: jobTimeout,
		processor:  processor,
		ctx:        ctx,
		cancel:     cancel,
	}
}

func (wp *WorkerPool) Start() {
	for i := 0; i < wp.maxWorkers; i++ {
		wp.wg.Add(1)
		go wp.worker(i)
	}
	slog.Info("worker pool started", "workers", wp.maxWorkers, "job_timeout", wp.jobTimeout)
}

func (wp *WorkerPool) worker(id int) {
	defer wp.wg.Done()

	for {
		select {
		case <-wp.ctx.Done():
			slog.Info("worker stopping: pool cancelled", "worker_id", id)
			return
		case job := <-wp.jobs:
			wp.process(id, job)
		}
	}
}

func (wp *WorkerPool) process(workerID int, job Job) {
	wp.stats.attempts.Add(1)

	// Bound once, so every line this job produces carries the same
	// identifiers — including request_id, which is what ties a build back
	// to the click that started it.
	log := slog.With(
		"worker_id", workerID,
		"job_id", job.ID,
		"project_id", job.ProjectID,
		"request_id", job.RequestID,
		"attempt", job.RetryCount+1,
	)

	// Derived from wp.ctx, not context.Background(): a cancelled pool must
	// be able to abort in-flight work. With Background() a job could
	// outlive shutdown by the full job timeout.
	ctx, cancel := context.WithTimeout(wp.ctx, wp.jobTimeout)
	defer cancel()

	start := time.Now()
	err := wp.processor(ctx, job)
	elapsed := time.Since(start)

	if err == nil {
		wp.stats.completed.Add(1)
		wp.recordSuccess()
		log.Info("job completed", "duration_ms", elapsed.Milliseconds())
		return
	}

	// Our own shutdown is not the job's fault: don't burn a retry on it and
	// don't let it trip the breaker.
	if wp.ctx.Err() != nil {
		log.Warn("job aborted by shutdown", "duration_ms", elapsed.Milliseconds())
		return
	}

	if job.RetryCount >= wp.maxRetries {
		wp.stats.failed.Add(1)
		wp.recordFailure()
		log.Error("job failed permanently", "error", err,
			"attempts", job.RetryCount+1, "duration_ms", elapsed.Milliseconds())
		return
	}

	job.RetryCount++
	backoff := jitteredBackoff(job.RetryCount)
	wp.stats.retried.Add(1)
	log.Warn("job failed, scheduling retry", "error", err, "backoff", backoff)

	// The wait runs on a timer goroutine, never on this worker. The old
	// time.Sleep here idled a worker for the whole backoff, so four
	// simultaneously-retrying jobs meant zero build capacity for seconds
	// while the queue kept filling.
	wp.inflight.Add(1)
	time.AfterFunc(backoff, func() {
		defer wp.inflight.Done()
		if err := wp.submit(job, true); err != nil {
			wp.stats.failed.Add(1)
			log.Error("retry could not be requeued", "error", err)
		}
	})
}

// jitteredBackoff is full jitter: a uniform draw from [0, 2^attempt seconds).
// Plain exponential backoff synchronizes retries — every job that failed on
// the same shared cause wakes at the same instant and hits the recovering
// dependency together. The random draw spreads them out.
func jitteredBackoff(attempt int) time.Duration {
	base := time.Second << uint(attempt) // 2s, 4s, 8s
	if base > time.Minute {
		base = time.Minute
	}
	return time.Duration(rand.Int63n(int64(base)))
}

// Submit accepts new work. It refuses while the breaker is open; retries go
// through submit directly and bypass that check, because discarding an
// in-flight job's retry throws away work already done.
func (wp *WorkerPool) Submit(job Job) error {
	return wp.submit(job, false)
}

func (wp *WorkerPool) submit(job Job, isRetry bool) error {
	wp.mu.RLock()
	defer wp.mu.RUnlock()

	if wp.closed {
		wp.stats.rejected.Add(1)
		return ErrPoolClosed
	}
	if !isRetry && wp.breakerOpen() {
		wp.stats.rejected.Add(1)
		return ErrBreakerOpen
	}

	select {
	case wp.jobs <- job:
		if !isRetry {
			wp.stats.submitted.Add(1)
		}
		return nil
	default:
		wp.stats.rejected.Add(1)
		return ErrQueueFull
	}
}

func (wp *WorkerPool) breakerOpen() bool {
	wp.breakerMu.Lock()
	defer wp.breakerMu.Unlock()
	return time.Now().Before(wp.breakerUntil)
}

func (wp *WorkerPool) recordSuccess() {
	wp.breakerMu.Lock()
	wp.consecFails = 0
	wp.breakerUntil = time.Time{}
	wp.breakerMu.Unlock()
}

func (wp *WorkerPool) recordFailure() {
	wp.breakerMu.Lock()
	defer wp.breakerMu.Unlock()

	wp.consecFails++
	if wp.consecFails >= breakerThreshold && time.Now().After(wp.breakerUntil) {
		wp.breakerUntil = time.Now().Add(breakerCooldown)
		slog.Error("circuit breaker tripped: refusing new jobs",
			"consecutive_failures", wp.consecFails, "cooldown", breakerCooldown)
	}
}

// Drain stops accepting work and waits for in-flight jobs and pending retry
// timers, up to timeout. It reports false if the deadline passed with work
// still running.
//
// Ordering matters: mark closed FIRST so nothing new enters, then wait, and
// only cancel the pool context once there is nothing left to protect.
// Cancelling first would abort builds that were about to succeed.
func (wp *WorkerPool) Drain(timeout time.Duration) bool {
	wp.mu.Lock()
	if wp.closed {
		wp.mu.Unlock()
		return true
	}
	wp.closed = true
	wp.mu.Unlock()

	slog.Info("draining worker pool", "queued_jobs_abandoned", len(wp.jobs), "timeout", timeout)

	done := make(chan struct{})
	go func() {
		wp.inflight.Wait() // retry timers sleeping outside the pool
		wp.wg.Wait()       // worker goroutines
		close(done)
	}()

	// Workers park on <-wp.jobs and only return on ctx.Done, so they will
	// not exit on their own. Poll for quiescence, then cancel to release
	// them; cancel unconditionally once the budget is spent.
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for time.Now().Before(deadline) {
		<-ticker.C
		if len(wp.jobs) == 0 && wp.stats.inFlight() == 0 {
			break
		}
	}

	wp.cancel()

	select {
	case <-done:
		slog.Info("worker pool drained cleanly")
		return true
	case <-time.After(5 * time.Second):
		slog.Error("workers did not exit after cancellation")
		return false
	}
}

// inFlight is the number of jobs that have been accepted but have not yet
// reached a terminal state — still queued, running, or waiting on a retry
// timer. Derived from counters rather than tracked separately so it cannot
// drift out of sync with them.
func (s *poolStats) inFlight() int64 {
	return s.submitted.Load() - s.completed.Load() - s.failed.Load()
}

// ShutDown is Drain with a default budget, kept for call-site
// compatibility. It never closes wp.jobs — see the comment on wp.mu.
func (wp *WorkerPool) ShutDown() { wp.Drain(30 * time.Second) }

func (wp *WorkerPool) GetStats() map[string]interface{} {
	return map[string]interface{}{
		"queue_depth":    len(wp.jobs),
		"queue_capacity": cap(wp.jobs),
		"workers":        wp.maxWorkers,
		"submitted":      wp.stats.submitted.Load(),
		"attempts":       wp.stats.attempts.Load(),
		"completed":      wp.stats.completed.Load(),
		"failed":         wp.stats.failed.Load(),
		"retried":        wp.stats.retried.Load(),
		"rejected":       wp.stats.rejected.Load(),
		"in_flight":      wp.stats.inFlight(),
		"breaker_open":   wp.breakerOpen(),
	}
}
