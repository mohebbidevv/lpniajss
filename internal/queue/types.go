package queue

import "time"

type JobStatus int

const (
	StatusPending JobStatus = iota
	StatusProcessing
	StatusCompleted
	StatusFailed
)

type Job struct {
	ID          string
	ProjectID   string
	ZipFilePath string
	FinalDir    string
	Priority    int
	CreatedAt   time.Time
	RetryCount  int

	// RequestID is the HTTP request that enqueued this job, carried by
	// value because that request's context is long gone by the time a
	// worker picks the job up. It is the only thread connecting a user's
	// click to the build log five minutes later. Empty for jobs the system
	// submits itself (reconciler, crash restart).
	RequestID string
}

type JobResult struct {
	JobID     string
	ProjectID string
	Status    JobStatus
	Error     error
}
