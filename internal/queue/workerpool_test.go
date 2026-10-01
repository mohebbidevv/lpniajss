package queue

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestConcurrentStatsNoRace is the regression test for the unsynchronized
// stats struct. Run under -race: plain int++ from four workers is a genuine
// data race, and GetStats reading them unlocked is a second one.
func TestConcurrentStatsNoRace(t *testing.T) {
	const jobs = 200

	var done sync.WaitGroup
	done.Add(jobs)

	wp := NewWorkerPool(4, func(ctx context.Context, job Job) error {
		done.Done()
		return nil
	}, time.Minute)
	wp.Start()

	// A reader racing the writers is the half the old GetStats got wrong.
	stop := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = wp.GetStats()
				}
			}
		}()
	}

	for i := 0; i < jobs; i++ {
		if err := wp.Submit(Job{ID: "j", ProjectID: "p"}); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}

	done.Wait()
	close(stop)
	readers.Wait()
	wp.Drain(5 * time.Second)

	if got := wp.stats.submitted.Load(); got != jobs {
		t.Errorf("submitted = %d, want %d", got, jobs)
	}
	if got := wp.stats.completed.Load(); got != jobs {
		t.Errorf("completed = %d, want %d", got, jobs)
	}
}

// TestSubmitAfterDrainDoesNotPanic covers the old close(wp.jobs). A late
// sender — EventConsumer's AfterFunc timer is the real one — used to panic
// the process. It must get an error instead.
func TestSubmitAfterDrainDoesNotPanic(t *testing.T) {
	wp := NewWorkerPool(2, func(ctx context.Context, job Job) error { return nil }, time.Minute)
	wp.Start()
	wp.Drain(2 * time.Second)

	err := wp.Submit(Job{ID: "late", ProjectID: "p"})
	if !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("Submit after drain = %v, want ErrPoolClosed", err)
	}

	// Draining twice must also be safe: the shutdown path can be reached
	// from both a signal handler and a defer.
	if !wp.Drain(time.Second) {
		t.Error("second Drain reported failure")
	}
}

// TestFailedCountsTerminalOnly pins the metric semantics: a job that fails
// twice then succeeds is one completion, not two failures. The old code
// incremented `failed` per attempt, inflating the failure rate by exactly
// the retry rate.
func TestFailedCountsTerminalOnly(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	succeeded := make(chan struct{})

	wp := NewWorkerPool(1, func(ctx context.Context, job Job) error {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()

		if n < 3 {
			return errors.New("transient")
		}
		close(succeeded)
		return nil
	}, time.Minute)
	wp.Start()
	defer wp.Drain(5 * time.Second)

	if err := wp.Submit(Job{ID: "retry-me", ProjectID: "p"}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-succeeded:
	case <-time.After(30 * time.Second):
		t.Fatal("job never succeeded")
	}

	if got := wp.stats.failed.Load(); got != 0 {
		t.Errorf("failed = %d, want 0 (job eventually succeeded)", got)
	}
	if got := wp.stats.completed.Load(); got != 1 {
		t.Errorf("completed = %d, want 1", got)
	}
	if got := wp.stats.attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
	if got := wp.stats.submitted.Load(); got != 1 {
		t.Errorf("submitted = %d, want 1 (retries are not new submissions)", got)
	}
}

// TestRetryDoesNotBlockWorker is the retry-amplification regression. The old
// code slept the backoff on the worker goroutine, so a retrying job starved
// the queue. With one worker, an unrelated job must still run promptly while
// a retry is pending.
func TestRetryDoesNotBlockWorker(t *testing.T) {
	other := make(chan struct{})

	wp := NewWorkerPool(1, func(ctx context.Context, job Job) error {
		if job.ID == "fails" {
			return errors.New("boom")
		}
		close(other)
		return nil
	}, time.Minute)
	wp.Start()
	defer wp.Drain(5 * time.Second)

	if err := wp.Submit(Job{ID: "fails", ProjectID: "p"}); err != nil {
		t.Fatal(err)
	}
	if err := wp.Submit(Job{ID: "other", ProjectID: "q"}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-other:
	case <-time.After(2 * time.Second):
		t.Fatal("second job was blocked behind a retry backoff")
	}
}

// TestBreakerOpensAndRefuses proves the storm control: past the threshold of
// consecutive terminal failures, new work is refused rather than piling on.
func TestBreakerOpensAndRefuses(t *testing.T) {
	var ran atomic.Int64

	wp := NewWorkerPool(2, func(ctx context.Context, job Job) error {
		ran.Add(1)
		return errors.New("always fails")
	}, time.Minute)
	wp.maxRetries = 0 // fail terminally on the first attempt
	wp.Start()
	defer wp.Drain(5 * time.Second)

	for i := 0; i < breakerThreshold; i++ {
		if err := wp.Submit(Job{ID: "f", ProjectID: "p"}); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}

	// Wait for all of them to reach a terminal failure rather than counting
	// processor calls, since the submits below deliberately add more.
	deadline := time.Now().Add(10 * time.Second)
	for wp.stats.failed.Load() < breakerThreshold && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := ran.Load(); got < breakerThreshold {
		t.Fatalf("only %d of %d jobs ran", got, breakerThreshold)
	}

	if !wp.breakerOpen() {
		t.Fatal("breaker should be open after threshold consecutive failures")
	}
	if err := wp.Submit(Job{ID: "next", ProjectID: "p"}); !errors.Is(err, ErrBreakerOpen) {
		t.Errorf("Submit with breaker open = %v, want ErrBreakerOpen", err)
	}
	// Retries must still get through — refusing them discards work already done.
	if err := wp.submit(Job{ID: "retry", ProjectID: "p"}, true); err != nil {
		t.Errorf("retry was refused by an open breaker: %v", err)
	}
}
