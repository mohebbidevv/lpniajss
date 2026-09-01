آره داداش، کاملاً درست می‌گی! **هندلر فقط validation رو انجام می‌ده و بقیه کار رو می‌ده به usecase**. UseCase هم یه کار سریع (ذخیره در دیتابیس) می‌کنه و کار سنگین رو می‌فرسته به صف. بیا باهم این رو پیاده‌سازی کنیم! 🚀

---

## 📦 معماری نهایی

```text
HTTP Request
    ↓
Handler → validation سریع → call UseCase.Execute()
    ↓
UseCase.Execute():
    1. یک رکورد توی دیتابیس با status='pending' ذخیره کن (سریع)
    2. فایل رو توی یه فایل موقت (temp) ذخیره کن (چون بعد از بسته شدن request دیگه access نداریم)
    3. اطلاعات Job رو به Worker Pool بفرست (همینجا برمی‌گرده)
    4. project_id و status='pending' رو برگردون به کاربر
Worker Pool (background):
    Job رو از صف برمیداره:
        · فایل موقت رو به مسیر نهایی منتقل کن
        · Unzip کن
        · status='completed' رو توی دیتابیس آپدیت کن
```

---

## 📁 فایل `queue/job.go` (تعریف Job و Result)

```go
package queue

import (
	"time"
)

// 🎯 JobStatus: وضعیت‌های مختلف یه Job
type JobStatus int

const (
	StatusPending    JobStatus = iota // 0: منتظر پردازش
	StatusProcessing                  // 1: در حال پردازش
	StatusCompleted                   // 2: انجام شده
	StatusFailed                      // 3: خطا خورده
)

// 📦 Job: اطلاعاتی که Worker برای پردازش نیاز داره
type Job struct {
	ID             string    // شناسه یکتا (مثلاً uuid)
	ProjectID      string    // شناسه پروژه توی دیتابیس
	TempFilePath   string    // مسیر فایل موقتی که handler ذخیره کرده
	FinalDir       string    // مسیر نهایی برای extract
	Priority       int       // اولویت (0=عادی، 1=بالا، 2=فوری)
	CreatedAt      time.Time // زمان ایجاد
	RetryCount     int       // تعداد تلاش مجدد
}

// 📨 JobResult: نتیجه پردازش (برای گزارش داخلی)
type JobResult struct {
	JobID     string
	ProjectID string
	Status    JobStatus
	Error     error
}
```

---

## 📁 فایل `queue/workerpool.go` (استخر کارگران)

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

// 👷 WorkerPool: مدیریت کارگرانی که Jobها رو از صف برمیدارن و اجرا می‌کنن
type WorkerPool struct {
	// jobs: صف بافرشده (FIFO) - ظرفیت ۲۰۰ تا
	jobs chan Job

	// maxWorkers: تعداد کارگران همزمان
	maxWorkers int

	// processor: تابعی که کار واقعی رو انجام میده (مثلاً ذخیره و unzip)
	// این تابع رو از بیرون به WorkerPool میدیم
	processor func(ctx context.Context, job Job) error

	// برای خاموش شدن تمیز
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// آمار و ارقام
	stats struct {
		submitted int
		completed int
		failed    int
	}
}

// 🏗️ NewWorkerPool: ساختن یک استخر کارگر جدید
// maxWorkers: حداکثر تعداد کارگران همزمان (مثلاً ۵)
// processor: تابعی که هر Job رو پردازش می‌کنه
func NewWorkerPool(maxWorkers int, processor func(ctx context.Context, job Job) error) *WorkerPool {
	// اگه حداکثر کارگر مشخص نشده، از تعداد هسته‌های CPU استفاده کن
	if maxWorkers <= 0 {
		maxWorkers = runtime.NumCPU()
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &WorkerPool{
		jobs:       make(chan Job, 200), // صف با ظرفیت ۲۰۰ تا Job
		maxWorkers: maxWorkers,
		processor:  processor,
		ctx:        ctx,
		cancel:     cancel,
	}
}

// ▶️ Start: راه‌اندازی کارگرها
func (wp *WorkerPool) Start() {
	log.Printf("🚀 Starting %d workers...", wp.maxWorkers)
	for i := 0; i < wp.maxWorkers; i++ {
		wp.wg.Add(1)
		go wp.worker(i)
	}
}

// 👷 worker: هر کارگر توی این حلقه بی‌نهایت منتظر Job می‌مونه
func (wp *WorkerPool) worker(id int) {
	defer wp.wg.Done()
	log.Printf("👷 Worker %d started", id)

	for {
		select {
		case <-wp.ctx.Done():
			// کانتکست کنسل شده (خاموشی)
			log.Printf("🛑 Worker %d shutting down", id)
			return

		case job, ok := <-wp.jobs:
			if !ok {
				// صف بسته شده
				return
			}
			wp.process(id, job)
		}
	}
}

// 🔄 process: اجرای Job و مدیریت خطا
func (wp *WorkerPool) process(workerID int, job Job) {
	start := time.Now()
	log.Printf("📦 Worker %d processing job %s (project: %s)", workerID, shortID(job.ID), job.ProjectID)

	// timeout برای کل پردازش (۵ دقیقه)
	ctx, cancel := context.WithTimeout(wp.ctx, 5*time.Minute)
	defer cancel()

	err := wp.processor(ctx, job)

	if err != nil {
		log.Printf("❌ Worker %d: job %s failed: %v", workerID, shortID(job.ID), err)
		wp.stats.failed++
		// اگه کمتر از ۳ بار تلاش کرده، دوباره تلاش کن (با delay)
		if job.RetryCount < 3 {
			job.RetryCount++
			time.Sleep(time.Duration(job.RetryCount) * 2 * time.Second)
			wp.Submit(job) // دوباره بفرست به صف
		}
	} else {
		log.Printf("✅ Worker %d: job %s completed in %v", workerID, shortID(job.ID), time.Since(start))
		wp.stats.completed++
	}
	wp.stats.submitted++
}

// ➕ Submit: اضافه کردن Job به صف
func (wp *WorkerPool) Submit(job Job) error {
	select {
	case wp.jobs <- job:
		return nil
	default:
		return fmt.Errorf("queue is full (capacity 200)")
	}
}

// 🛑 Shutdown: خاموش کردن ملایم
func (wp *WorkerPool) Shutdown() {
	log.Println("🛑 Shutting down worker pool...")
	wp.cancel()        // به کارگرها بگو دیگه کار جدید نگیرن
	close(wp.jobs)     // صف رو ببند
	wp.wg.Wait()       // صبر کن تا همه کارگرها تموم شن
}

// helper: کوتاه کردن ID برای لاگ
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
```

---

## 📁 فایل `application/upload.go` (UseCase جدید با صف)

```go
package application

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"your-project/domain/entities"
	"your-project/queue"
	"your-project/utils"
)

// 📦 UploadInput و UploadOutput (همون قبلی)
type UploadInput struct {
	Filename string
	File     io.Reader
}

type UploadOutput struct {
	ProjectID string
	UniqueKey string
	Status    string // "pending"
}

// 🚀 UploadProjectUseCase
type UploadProjectUseCase struct {
	ProjectRepo ProjectRepository   // دیتابیس
	Storage     StorageService      // ذخیره/بازکردن فایل
	WorkerPool  *queue.WorkerPool   // صف پس‌زمینه
	UploadDir   string              // ./uploads
	WorkDir     string              // ./work
}

// NewUploadProjectUseCase: سازنده
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

// Execute: این متد خیلی سریع برمی‌گرده!
func (uc *UploadProjectUseCase) Execute(ctx context.Context, input UploadInput) (*UploadOutput, error) {
	// 1️⃣ یک شناسه یکتا بساز
	uniqueID := utils.NewID()

	// 2️⃣ توی دیتابیس ذخیره کن با status='pending' (سریع)
	project := entities.NewProject(
		input.Filename,
		uniqueID,
		"zip",
		"", // path رو بعداً Worker پر می‌کنه
	)
	project.Status = "pending"

	projID, err := uc.ProjectRepo.Create(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("failed to save project: %w", err)
	}

	// 3️⃣ فایل رو توی یه فایل موقت ذخیره کن
	// چون بعد از بسته شدن request، دیگه نمی‌تونیم از input.File بخونیم!
	tempPath := filepath.Join(uc.UploadDir, uniqueID+".tmp")
	tempFile, err := os.Create(tempPath)
	if err != nil {
		// اگه نتونستیم فایل موقت بسازیم، پروژه رو پاک کن
		uc.ProjectRepo.Delete(ctx, projID)
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}
	defer tempFile.Close()

	// کپی کردن کل فایل به فایل موقت
	_, err = io.Copy(tempFile, input.File)
	if err != nil {
		tempFile.Close()
		os.Remove(tempPath) // تمیزکاری
		uc.ProjectRepo.Delete(ctx, projID)
		return nil, fmt.Errorf("failed to copy file: %w", err)
	}
	tempFile.Close()

	// 4️⃣ یک Job برای Worker درست کن
	job := queue.Job{
		ID:           uniqueID,
		ProjectID:    projID,
		TempFilePath: tempPath,
		FinalDir:     filepath.Join(uc.WorkDir, uniqueID),
		Priority:     0,
		CreatedAt:    time.Now(),
	}

	// 5️⃣ Job رو به صف بفرست (بدون اینکه منتظر باشیم!)
	err = uc.WorkerPool.Submit(job)
	if err != nil {
		// اگه صف پر بود، فایل موقت رو پاک کن و خطا برگردون
		os.Remove(tempPath)
		uc.ProjectRepo.Delete(ctx, projID)
		return nil, fmt.Errorf("server is busy, try again later")
	}

	log.Printf("📤 Job %s submitted for project %s", shortID(uniqueID), projID)

	// 6️⃣ برگردون نتیجه به کاربر (فوری!)
	return &UploadOutput{
		ProjectID: projID,
		UniqueKey: project.UniqueKey,
		Status:    "pending", // می‌گیم پروژه‌ات تو صف هست
	}, nil
}
```

---

## 📁 فایل `main.go` (راه‌اندازی همه چیز)

```go
func main() {
	// ... اتصال به دیتابیس و بقیه کارها ...

	// 1️⃣ ساختن Worker Pool
	processor := func(ctx context.Context, job queue.Job) error {
		// این تابع توی پس‌زمینه اجرا می‌شه
		// کارهای سنگین: ذخیره فایل نهایی، unzip، آپدیت دیتابیس

		// الف) فایل موقت رو به مسیر نهایی منتقل کن (با پسوند zip)
		zipPath := filepath.Join(uploadDir, job.ID+".zip")
		err := os.Rename(job.TempFilePath, zipPath)
		if err != nil {
			return fmt.Errorf("rename failed: %w", err)
		}

		// ب) unzip کن
		err = storage.Unzip(zipPath, job.FinalDir)
		if err != nil {
			return fmt.Errorf("unzip failed: %w", err)
		}

		// ج) status پروژه رو توی دیتابیس به 'completed' تغییر بده
		// فرض کنیم ProjectRepo یه متد UpdateStatus داره
		err = projectRepo.UpdateStatus(ctx, job.ProjectID, "completed", job.FinalDir)
		if err != nil {
			return fmt.Errorf("update db failed: %w", err)
		}

		log.Printf("✅ Project %s completed successfully", job.ProjectID)
		return nil
	}

	wp := queue.NewWorkerPool(5, processor) // ۵ تا کارگر
	wp.Start()
	defer wp.Shutdown()

	// 2️⃣ ساختن UseCase
	uc := application.NewUploadProjectUseCase(
		projectRepo,
		storageService,
		wp,
		uploadDir,
		workDir,
	)

	// 3️⃣ هندلر (فقط validation)
	handler := handler.NewUploadHandler(uc)

	// ... شروع HTTP server ...
}
```

---

## 📁 فایل `handler/upload.go` (فقط validation)

```go
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"your-project/application"
)

type UploadHandler struct {
	useCase *application.UploadProjectUseCase
}

func NewUploadHandler(uc *application.UploadProjectUseCase) *UploadHandler {
	return &UploadHandler{useCase: uc}
}

func (h *UploadHandler) ServeHTTP(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	// فقط POST
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	// محدودیت حجم
	r.Body = http.MaxBytesReader(w, r.Body, 20<<20)

	// گرفتن فایل
	file, hdr, err := r.FormFile("file")
	if err != nil {
		if err.Error() == "http: too large body" {
			http.Error(w, "file too large (max 20MB)", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "missing file field", http.StatusBadRequest)
		}
		return
	}
	defer file.Close()

	// فقط zip
	if !strings.HasSuffix(strings.ToLower(hdr.Filename), ".zip") {
		http.Error(w, "only .zip supported", http.StatusBadRequest)
		return
	}

	// حالا call UseCase
	output, err := h.useCase.Execute(ctx, application.UploadInput{
		Filename: hdr.Filename,
		File:     file,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// برگردوندن پاسخ (حتی اگه پردازش تموم نشده!)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted) // 202 Accepted
	json.NewEncoder(w).Encode(output)
}
```

---

## 🎯 نظر من در مورد RabbitMQ vs DIY

| **DIY (خودساخته)** | **RabbitMQ** |
|-------------------|--------------|
| 👍 ساده و سریع برای پروژه‌های کوچک | 👍 مقیاس‌پذیر، مطمئن، مدیریت پیچیدگی بالا |
| 👍 نیازی به نصب سرویس جدا نیست | 👍 پیام‌ها رو روی دیسک ذخیره می‌کنه (برنامه ری‌استارت بشه از بین نمی‌رن) |
| ❌ اگه برنامه کرش کنه، Jobهای توی صف از دست می‌رن | 👍 می‌تونه بین چند سرور پخش بشه |
| ❌ محدود به یک ماشین | ❌ نیاز به نصب و راه‌اندازی RabbitMQ داره |
| ❌ امکانات محدود (مثل dead letter, priority queue به صورت حرفه‌ای) | ❌ پیچیده‌تر برای شروع |

**پیشنهاد من:** برای الان همون DIY رو بزن. وقتی پروژه‌ات بزرگتر شد و نیاز به قابلیت اطمینان بیشتر (مثل persisting jobs on disk) و مقیاس‌پذیری چند سرور پیدا کردی، برو سراغ RabbitMQ. یاد گرفتنش هم خیلی سخت نیست، ولی ارزشش رو داره.

---

## 🔥 خلاصه نهایی

- **Handler**: فقط validation و call UseCase
- **UseCase.Execute**: سریع: ذخیره در دیتابیس + ذخیره فایل موقت + submit به صف
- **Worker Pool (background)**: کارهای سنگین (ذخیره نهایی، unzip، آپدیت دیتابیس)
- **کاربر**: بلافاصله `202 Accepted` + `project_id` می‌گیره و بعداً با یه GET می‌تونه وضعیت رو چک کنه.

حالا برو این رو اجرا کن و ببین چقدر فرق می‌کنه! 🚀

اگه سوالی داری بپرس، کمکت می‌کنم.