😂😂😂 حق با توئه داداش! من اشتباه کردم. تو گفتی کامنت بذار، من موندم بین "کامنت فارسی" و "بدون کامنت" و رفتم توی افراط.

گفتی **کامنت انگلیسی** می‌خوای همش رو توضیح بدم. بیا اینبار درستش می‌کنم:

---

## `queue/job.go`

```go
package queue

import "time"

// JobStatus represents the current state of a job in the queue.
type JobStatus int

const (
	StatusPending    JobStatus = iota // Job is waiting to be picked by a worker
	StatusProcessing                  // Job is currently being processed by a worker
	StatusCompleted                   // Job finished successfully
	StatusFailed                      // Job failed (will be retried if retries left)
)

// Job represents a unit of work that needs to be processed in the background.
// It contains all the information a worker needs to process the upload.
type Job struct {
	ID           string    // Unique identifier for the job (usually same as project's uniqueID)
	ProjectID    string    // Database ID of the project (for updating status later)
	TempFilePath string    // Path to the temporary file saved by the usecase
	FinalDir     string    // Directory where the zip should be extracted
	Priority     int       // Higher number = higher priority (default 0)
	CreatedAt    time.Time // When the job was created (for timeout calculations)
	RetryCount   int       // How many times we've retried this job (max 3)
}

// JobResult contains the outcome of a processed job.
// Used for logging and monitoring, not for client response.
type JobResult struct {
	JobID     string
	ProjectID string
	Status    JobStatus
	Error     error
}
```

---

## `queue/workerpool.go`

```go
package queue

import (
	"context"
	"fmt"
	"log"
	"runtime"
	"sync"
	"time"
)

// WorkerPool manages a pool of goroutines that process background jobs.
// It acts like a thread pool: jobs are submitted to a buffered channel,
// and workers pick them up as they become available.
type WorkerPool struct {
	jobs       chan Job                              // Buffered channel acting as the queue (capacity: 200)
	maxWorkers int                                   // Maximum number of concurrent workers
	processor  func(ctx context.Context, job Job) error // The actual work function injected from outside
	ctx        context.Context                       // Context for graceful shutdown
	cancel     context.CancelFunc                    // Cancel function to signal workers to stop
	wg         sync.WaitGroup                        // WaitGroup to track all workers during shutdown

	// Internal stats for monitoring
	stats struct {
		submitted int // Total jobs submitted (including retries)
		completed int // Successfully completed jobs
		failed    int // Failed jobs (all retries exhausted)
	}
}

// NewWorkerPool creates a new worker pool with the specified number of workers.
// If maxWorkers is 0 or negative, it defaults to the number of CPU cores.
// The processor function contains the actual logic for processing each job.
func NewWorkerPool(maxWorkers int, processor func(ctx context.Context, job Job) error) *WorkerPool {
	if maxWorkers <= 0 {
		maxWorkers = runtime.NumCPU() // Default to number of CPU cores
	}

	// Create a cancellable context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())

	return &WorkerPool{
		jobs:       make(chan Job, 200), // Buffered channel holds up to 200 pending jobs
		maxWorkers: maxWorkers,
		processor:  processor,
		ctx:        ctx,
		cancel:     cancel,
	}
}

// Start launches all workers as goroutines.
// Each worker waits in an infinite loop for jobs to arrive on the channel.
func (wp *WorkerPool) Start() {
	log.Printf("[WorkerPool] Starting %d workers (queue capacity: 200)", wp.maxWorkers)
	for i := 0; i < wp.maxWorkers; i++ {
		wp.wg.Add(1)           // Increment WaitGroup counter
		go wp.worker(i)        // Launch worker in its own goroutine
	}
}

// worker runs in its own goroutine and processes jobs one at a time.
// It blocks on reading from the jobs channel until a job arrives
// or the context is cancelled (shutdown).
func (wp *WorkerPool) worker(id int) {
	defer wp.wg.Done() // Decrement WaitGroup when worker exits
	log.Printf("[WorkerPool] Worker %d started", id)

	// Infinite loop - worker keeps running until pool shuts down
	for {
		select {
		case <-wp.ctx.Done():
			// Context cancelled - time to shut down
			log.Printf("[WorkerPool] Worker %d shutting down (context cancelled)", id)
			return

		case job, ok := <-wp.jobs:
			// A job arrived on the channel
			if !ok {
				// Channel closed - no more jobs will come
				log.Printf("[WorkerPool] Worker %d shutting down (channel closed)", id)
				return
			}
			// Process the job
			wp.process(id, job)
		}
	}
}

// process executes a single job and handles retries on failure.
func (wp *WorkerPool) process(workerID int, job Job) {
	start := time.Now()
	log.Printf("[WorkerPool] Worker %d processing job %s (project: %s)", workerID, job.ID[:8], job.ProjectID)

	// Create a context with 5-minute timeout for this specific job
	// This prevents a single job from blocking a worker forever
	ctx, cancel := context.WithTimeout(wp.ctx, 5*time.Minute)
	defer cancel()

	// Execute the actual work (defined by the caller)
	err := wp.processor(ctx, job)

	if err != nil {
		// Job failed
		wp.stats.failed++
		log.Printf("[WorkerPool] Worker %d: job %s FAILED: %v", workerID, job.ID[:8], err)

		// Retry up to 3 times with exponential backoff
		if job.RetryCount < 3 {
			job.RetryCount++
			backoff := time.Duration(job.RetryCount) * 2 * time.Second
			log.Printf("[WorkerPool] Retrying job %s (attempt %d/3) after %v", job.ID[:8], job.RetryCount, backoff)
			time.Sleep(backoff)
			wp.Submit(job) // Re-submit to queue
		} else {
			log.Printf("[WorkerPool] Job %s exhausted all retries", job.ID[:8])
		}
	} else {
		// Job completed successfully
		wp.stats.completed++
		log.Printf("[WorkerPool] Worker %d: job %s COMPLETED in %v", workerID, job.ID[:8], time.Since(start))
	}

	wp.stats.submitted++
}

// Submit adds a new job to the queue.
// Returns an error if the queue is full (non-blocking).
func (wp *WorkerPool) Submit(job Job) error {
	select {
	case wp.jobs <- job:
		// Job successfully added to channel
		return nil
	default:
		// Channel buffer is full - queue is at capacity
		return fmt.Errorf("queue is full (capacity: 200), try again later")
	}
}

// Shutdown gracefully stops all workers.
// 1. Cancels the context (workers will stop picking new jobs)
// 2. Closes the jobs channel
// 3. Waits for all workers to finish their current job
func (wp *WorkerPool) Shutdown() {
	log.Println("[WorkerPool] Initiating graceful shutdown...")
	wp.cancel()     // Signal workers to stop after current job
	close(wp.jobs)  // Close channel (no more submissions)
	wp.wg.Wait()    // Wait for all workers to finish
	log.Printf("[WorkerPool] Shutdown complete. Stats: submitted=%d, completed=%d, failed=%d",
		wp.stats.submitted, wp.stats.completed, wp.stats.failed)
}

// GetStats returns current pool statistics for monitoring.
func (wp *WorkerPool) GetStats() map[string]interface{} {
	return map[string]interface{}{
		"queue_size":    len(wp.jobs),
		"active_workers": wp.maxWorkers,
		"submitted":     wp.stats.submitted,
		"completed":     wp.stats.completed,
		"failed":        wp.stats.failed,
	}
}
```

---

## `application/upload.go`

```go
package application

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"your-project/domain/entities"
	"your-project/queue"
	"your-project/utils"
)

// UploadInput contains the data coming from the HTTP request.
type UploadInput struct {
	Filename string    // Original filename from the multipart form
	File     io.Reader // The actual file content stream
}

// UploadOutput is returned immediately to the client (non-blocking).
type UploadOutput struct {
	ProjectID string // Database ID of the created project
	UniqueKey string // Unique identifier for file operations
	Status    string // Always "pending" since processing happens async
}

// UploadProjectUseCase handles the upload logic with async queue processing.
// It only does fast operations synchronously (DB write, temp file save),
// then delegates the heavy work (unzip, file move) to the worker pool.
type UploadProjectUseCase struct {
	ProjectRepo ProjectRepository  // Database operations
	Storage     StorageService     // File system operations
	WorkerPool  *queue.WorkerPool  // Background job queue
	UploadDir   string             // Directory for temporary uploads (./uploads)
	WorkDir     string             // Directory for final extraction (./work)
}

func NewUploadProjectUseCase(
	repo ProjectRepository,
	storage StorageService,
	workerPool *queue.WorkerPool,
	uploadDir, workDir string,
) *UploadProjectUseCase {
	return &UploadProjectUseCase{
		ProjectRepo: repo,
		Storage:     storage,
		WorkerPool:  workerPool,
		UploadDir:   uploadDir,
		WorkDir:     workDir,
	}
}

// Execute processes an upload request synchronously for the fast parts,
// then submits the heavy work to the background queue.
//
// Flow:
// 1. Generate unique ID for the project
// 2. Save project in DB with status="pending" (fast)
// 3. Save uploaded file as a temporary file on disk (fast enough)
// 4. Create a job and submit it to the worker pool (non-blocking)
// 5. Return project ID and status immediately to the client
func (uc *UploadProjectUseCase) Execute(ctx context.Context, input UploadInput) (*UploadOutput, error) {
	// Step 1: Generate a unique identifier for this project
	uniqueID := utils.NewID()

	// Step 2: Save project in database with "pending" status
	// This is fast because it's just an INSERT query
	project := entities.NewProject(input.Filename, uniqueID, "zip", "")
	project.Status = "pending" // Mark as pending - worker will update to "completed"

	projID, err := uc.ProjectRepo.Create(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("failed to create project in database: %w", err)
	}

	// Step 3: Save the uploaded file as a temporary file
	// We MUST do this because after the HTTP handler returns,
	// the request body (input.File) will be closed and unreadable
	tempPath := filepath.Join(uc.UploadDir, uniqueID+".tmp")
	tempFile, err := os.Create(tempPath)
	if err != nil {
		// Cleanup: delete the project from DB since we couldn't save the file
		uc.ProjectRepo.Delete(ctx, projID)
		return nil, fmt.Errorf("failed to create temporary file: %w", err)
	}
	defer tempFile.Close()

	// Copy the entire file content to disk
	_, err = io.Copy(tempFile, input.File)
	if err != nil {
		// Cleanup: remove temp file and DB record
		tempFile.Close()
		os.Remove(tempPath)
		uc.ProjectRepo.Delete(ctx, projID)
		return nil, fmt.Errorf("failed to save uploaded file: %w", err)
	}
	tempFile.Close() // Close explicitly so worker can use the file

	// Step 4: Create a job and submit to the worker pool
	// The worker will:
	//   - Rename .tmp to .zip
	//   - Unzip the file
	//   - Update project status to "completed"
	job := queue.Job{
		ID:           uniqueID,
		ProjectID:    projID,
		TempFilePath: tempPath,
		FinalDir:     filepath.Join(uc.WorkDir, uniqueID),
		Priority:     0,       // Normal priority
		CreatedAt:    time.Now(), // For timeout tracking
	}

	// Submit to queue (non-blocking - returns error if queue is full)
	err = uc.WorkerPool.Submit(job)
	if err != nil {
		// Queue is full - clean up and tell client to try again
		os.Remove(tempPath)
		uc.ProjectRepo.Delete(ctx, projID)
		return nil, fmt.Errorf("server is busy processing other files, please try again later")
	}

	log.Printf("[UploadUseCase] Job submitted: project=%s, temp_file=%s", projID, tempPath)

	// Step 5: Return immediately without waiting for the heavy work
	return &UploadOutput{
		ProjectID: projID,
		UniqueKey: project.UniqueKey,
		Status:    "pending", // Client knows the file is queued, not processed yet
	}, nil
}
```

---

## `handler/upload.go`

```go
package handler

import (
	"encoding/json"
	"net/http"
	"strings"

	"your-project/application"
)

// UploadHandler handles HTTP upload requests.
// It does minimal work: validates the request, then delegates to the usecase.
// The heavy processing happens asynchronously in the background.
type UploadHandler struct {
	useCase *application.UploadProjectUseCase
}

func NewUploadHandler(uc *application.UploadProjectUseCase) *UploadHandler {
	return &UploadHandler{useCase: uc}
}

// ServeHTTP handles POST /upload requests.
// Flow:
// 1. Validate HTTP method (POST only)
// 2. Limit body size to 20MB
// 3. Parse multipart form to get the file
// 4. Validate file extension (.zip only)
// 5. Call usecase (which returns immediately with a pending status)
// 6. Return 202 Accepted with project info
func (h *UploadHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Method validation
	if r.Method != http.MethodPost {
		http.Error(w, "this endpoint only accepts POST requests", http.StatusMethodNotAllowed)
		return
	}

	// Limit request body to 20MB
	r.Body = http.MaxBytesReader(w, r.Body, 20<<20)

	// Parse the multipart form and get the uploaded file
	file, header, err := r.FormFile("file")
	if err != nil {
		if err.Error() == "http: request body too large" {
			http.Error(w, "file size exceeds the 20MB limit", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "missing 'file' field in multipart form data", http.StatusBadRequest)
		}
		return
	}
	defer file.Close() // Ensure file handle is closed

	// Validate file extension
	if !strings.HasSuffix(strings.ToLower(header.Filename), ".zip") {
		http.Error(w, "only .zip files are supported", http.StatusBadRequest)
		return
	}

	// Delegate to usecase (this is fast - returns immediately)
	output, err := h.useCase.Execute(r.Context(), application.UploadInput{
		Filename: header.Filename,
		File:     file,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Return 202 Accepted to indicate the request was received but not yet processed
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted) // 202 Accepted (not 200 OK)
	json.NewEncoder(w).Encode(output)
}
```

---

## `main.go`

```go
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"

	"your-project/application"
	"your-project/handler"
	"your-project/infrastructure/postgres"
	"your-project/infrastructure/storage"
	"your-project/queue"
)

func main() {
	// Initialize database connection
	db, err := postgres.Connect()
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer db.Close()

	// Initialize repositories and services
	repo := postgres.NewProjectRepository(db)
	store := storage.New()

	// Define directories for file operations
	uploadDir := "./uploads" // Temporary files before queuing
	workDir := "./work"      // Final extracted files

	// Ensure directories exist
	os.MkdirAll(uploadDir, 0755)
	os.MkdirAll(workDir, 0755)

	// Define the processor function that workers will execute in the background.
	// This function contains the HEAVY work that we don't want to do synchronously.
	// It runs inside a worker goroutine, not in the HTTP handler.
	processor := func(ctx context.Context, job queue.Job) error {
		// Step 1: Rename the temporary file to its final .zip name
		zipPath := fmt.Sprintf("%s/%s.zip", uploadDir, job.ID)
		if err := os.Rename(job.TempFilePath, zipPath); err != nil {
			return fmt.Errorf("failed to move temp file: %w", err)
		}

		// Step 2: Extract the zip archive to the work directory
		if err := store.Unzip(zipPath, job.FinalDir); err != nil {
			return fmt.Errorf("failed to unzip file: %w", err)
		}

		// Step 3: Update the project status in the database to "completed"
		// The client can now poll for this status using the project ID
		if err := repo.UpdateStatus(ctx, job.ProjectID, "completed", job.FinalDir); err != nil {
			return fmt.Errorf("failed to update project status: %w", err)
		}

		log.Printf("[Processor] Project %s completed successfully (path: %s)", job.ProjectID, job.FinalDir)
		return nil
	}

	// Create and start the worker pool with 5 concurrent workers
	wp := queue.NewWorkerPool(5, processor)
	wp.Start()
	defer wp.Shutdown()

	// Create the usecase with the worker pool
	uc := application.NewUploadProjectUseCase(repo, store, wp, uploadDir, workDir)

	// Create the HTTP handler
	h := handler.NewUploadHandler(uc)

	// Register routes and start the server
	http.HandleFunc("/upload", h.ServeHTTP)

	log.Println("Server starting on :8080")
	log.Println("POST /upload - Upload a .zip file (returns 202 Accepted)")
	log.Println("GET /status/{id} - Check project processing status (implement separately)")

	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}
```

---

## خلاصه چی شد:

1. **`queue/job.go`**: تعریف Job و Result با توضیح هر فیلد
2. **`queue/workerpool.go`**: صف بافرشده با ۲۰۰ تا ظرفیت، ۵ تا worker، retry ۳ بار
3. **`application/upload.go`**: فقط کار سریع (ذخیره دیتابیس، فایل موقت) بعد submit به صف
4. **`handler/upload.go`**: فقط validation و call usecase
5. **`main.go`**: processor که توی پس‌زمینه فایل رو unzip می‌کنه و status رو آپدیت می‌کنه

حالا راجع به **RabbitMQ** نظرت عوض شد یا همون DIY رو می‌خوای بزنی؟ 🚀