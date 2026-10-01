package main

import (
	"context"
	"errors"
	"golaunch/internal/application"
	"golaunch/internal/infrastructure/caddy"
	"golaunch/internal/infrastructure/config"
	"golaunch/internal/infrastructure/crypto"
	"golaunch/internal/infrastructure/database/postgres"
	packageHttp "golaunch/internal/infrastructure/http"
	middleware "golaunch/internal/infrastructure/http/middlewares"
	"golaunch/internal/infrastructure/logging"
	"golaunch/internal/infrastructure/mailer"
	"golaunch/internal/infrastructure/vcsgit"
	"golaunch/internal/queue"
	"log"
	"log/slog"
	nethttp "net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var (
	UploadDir = "./uploads"
	WorkDir   = "./work"
)

// version is reported by /healthz. Override at build time with
//
//	go build -ldflags "-X main.version=$(git rev-parse --short HEAD)"
var version = "dev"

func main() {

	if err := os.MkdirAll(UploadDir, 0755); err != nil {
		log.Fatalf("Failed to create upload directory: %v", err)
	}
	if err := os.MkdirAll(WorkDir, 0755); err != nil {
		log.Fatalf("Failed to create work directory: %v", err)
	}

	// Cancellable so the background goroutines below (event consumer,
	// reconciler, reaper) can actually be told to stop. With
	// context.Background() they never could.
	ctx, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()

	// Closed at the start of shutdown to release the SSE handlers. They
	// hold non-idle connections, which http.Server.Shutdown waits on.
	shutdownCh := make(chan struct{})

	configuration, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	logger := logging.New(configuration.Server.LogLevel, configuration.Server.LogFormat)

	dbDSN := postgres.BuildDSN(configuration.DB)
	dbPool, err := postgres.ConnectDB(ctx, dbDSN)

	if err != nil {
		log.Fatalf("cant connect db %v", err.Error())
	}

	postgres.RunMigrations(ctx, dbPool, "file://migrations")

	// Note: no defers for dbPool, the runtime client or the worker pool.
	// They are closed explicitly in the ordered shutdown sequence at the
	// bottom of this function — a defer would run in the wrong order, and
	// on a signal it would not run at all.
	// RequestID must be outermost: RequestLogger and every layer below read
	// the ID it puts in the context.
	mux := nethttp.NewServeMux()
	handler := middleware.RequestID(middleware.RequestLogger(mux))

	registry := application.NewLogRegistry()
	dbRepo := postgres.NewProjectRepository(dbPool)
	deploymentRepo := postgres.NewDeploymentRepository(dbPool)
	var envSecretBox *crypto.SecretBox
	if key := configuration.Server.EnvEncryptionKey; key != "" {
		envSecretBox, err = crypto.NewSecretBox(key)
		if err != nil {
			log.Fatalf("env encryption key: %v", err)
		}
	} else {
		slog.Warn("no env_encryption_key configured: project env vars will be stored as PLAINTEXT — set GOLAUNCH_ENV_ENCRYPTION_KEY (openssl rand -base64 32)")
	}

	envVarRepo := postgres.NewEnvVarRepository(dbPool, envSecretBox)

	// LogMailer prints links instead of sending them. Swap for a provider
	// implementation before launch — anyone who can read the process logs
	// can currently complete a password reset.
	appMailer := mailer.NewLogMailer(configuration.Server.FrontendBaseURL)

	caddyClient := caddy.NewCaddyClient(configuration.Caddy.AdminURL, configuration.Caddy.Domain)

	stack, err := buildDeployStack(ctx, configuration.Runtime)
	if err != nil {
		log.Fatalf("runtime %q unavailable: %v", configuration.Runtime.Driver, err)
	}
	slog.Info("runtime driver selected", "driver", configuration.Runtime.Driver)

	gitSource := vcsgit.NewGitSource()

	dc := deployConfig(configuration.Runtime)
	pipeline := application.NewDeployPipeline(
		dbRepo, deploymentRepo, stack.Runtime, stack.Builder,
		gitSource, envVarRepo, caddyClient, registry,
		dc,
	)

	// Images are never deleted on the deploy path any more — that is what
	// made rollback possible — so something has to reclaim them. Retention,
	// not deletion: keep the last N per project.
	imageGC := application.NewImageGC(dbRepo, deploymentRepo, stack.Builder, configuration.Runtime.ImageRetentionCount)
	pipeline.ImageGC = imageGC

	var eventConsumer *application.EventConsumer

	processor := func(ctx context.Context, job queue.Job) error {
		logCh, ok := registry.Get(job.ProjectID)
		if ok {
			defer registry.Delete(job.ProjectID)
			defer close(logCh)
		}

		// Rebuild the correlation the request context could not carry
		// across the queue, so pipeline/builder/runtime lines all join up
		// with the HTTP request that triggered this deploy.
		ctx = logging.Into(ctx, logger.With(
			"request_id", job.RequestID,
			"job_id", job.ID,
			"project_id", job.ProjectID,
		))

		err := pipeline.Deploy(ctx, job.ProjectID)
		if err == nil && eventConsumer != nil {
			eventConsumer.ResetCrashCount(job.ProjectID)
		}
		return err
	}

	// jobs are builds now, not long-lived process babysitting — the
	// worker returns as soon as the app is started, so a small pool is
	// plenty. The per-job ceiling is sized off the same DeployConfig the
	// pipeline itself uses, so BuildTimeout is never silently unreachable.
	workerPool := queue.NewWorkerPool(4, processor, jobTimeoutFor(dc))
	workerPool.Start()

	eventConsumer = application.NewEventConsumer(dbRepo, deploymentRepo, stack.Runtime, caddyClient, workerPool)
	go eventConsumer.Run(ctx)

	reconciler := application.NewReconciler(dbRepo, deploymentRepo, stack.Runtime, caddyClient, pipeline)
	go reconciler.Run(ctx)

	go imageGC.Run(ctx, time.Duration(configuration.Runtime.ImageSweepIntervalMinutes)*time.Minute)

	reaper := application.NewAnonymousReaper(dbRepo, time.Duration(configuration.Housekeeping.AnonymousProjectTTLMinutes)*time.Minute)
	go reaper.Run(ctx, time.Duration(configuration.Housekeeping.AnonymousReapIntervalMinutes)*time.Minute)

	packageHttp.InitializeRoutes(ctx, dbPool, workerPool, caddyClient, registry, stack.Runtime, stack.Builder, packageHttp.ServerConfig{
		AllowedOrigins:        configuration.Server.AllowedOrigins,
		SecureCookies:         configuration.Server.SecureCookies,
		Domain:                configuration.Caddy.Domain,
		AnonymousSubmitLimit:  configuration.Server.AnonymousSubmitLimit,
		AnonymousSubmitWindow: time.Duration(configuration.Server.AnonymousSubmitWindowSeconds) * time.Second,
		LoginIPLimit:          configuration.Server.LoginIPLimit,
		LoginAccountLimit:     configuration.Server.LoginAccountLimit,
		LoginWindow:           time.Duration(configuration.Server.LoginWindowSeconds) * time.Second,
		InternalToken:         configuration.Server.InternalToken,
		Version:               version,
		EnvSecretBox:          envSecretBox,
		Mailer:                appMailer,
		Shutdown:              shutdownCh,
	}, mux)
	server := &nethttp.Server{
		Addr:    configuration.Server.BindAddress + ":" + configuration.Server.Port,
		Handler: handler,

		// WriteTimeout is deliberately unset. It is a deadline on the
		// entire response measured from the end of the request headers —
		// not an idle timeout — so any value would cut the two SSE streams
		// off mid-deploy. Non-streaming routes are bounded individually in
		// InitializeRoutes instead.
		ReadHeaderTimeout: 10 * time.Second, // slowloris
		ReadTimeout:       60 * time.Second, // stalled upload bodies
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	// Buffered: signal delivery is non-blocking, so an unbuffered channel
	// silently drops the signal if the receiver is not already parked.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("http server listening", "addr", server.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, nethttp.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		slog.Error("http server failed", "error", err)
	case sig := <-signals:
		slog.Info("shutdown signal received", "signal", sig.String())
	}

	// ── ordered shutdown ────────────────────────────────────────────────
	// Each step depends on the one before it; the order is not arbitrary.

	// 0. Release the SSE streams. They are never idle, so step 1 would
	//    otherwise block for its entire budget waiting on them.
	close(shutdownCh)

	// 1. Stop accepting new requests, let in-flight ones finish. Before the
	//    pool drain, or handlers keep enqueuing work we are about to drop.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 15*time.Second)
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Warn("graceful http shutdown timed out, forcing close", "error", err)
		_ = server.Close()
	}
	cancelShutdown()

	// 2. Let in-flight builds finish. Submissions already return
	//    ErrPoolClosed from the moment Drain marks the pool closed.
	if !workerPool.Drain(45 * time.Second) {
		slog.Warn("worker pool did not drain cleanly")
	}

	// 3. Stop the background goroutines. After step 2, because the deploy
	//    pipeline builds contexts derived from this one.
	cancelRoot()

	// 4. Give anything we abandoned mid-build a terminal status, so the UI
	//    shows a real error rather than a spinner that never resolves. On a
	//    fresh context: ctx is cancelled now, so reusing it would fail
	//    every query instantly.
	sweepCtx, cancelSweep := context.WithTimeout(context.Background(), 10*time.Second)
	if n, err := application.FailAbandonedDeployments(sweepCtx, deploymentRepo, dbRepo, 0); err != nil {
		slog.Error("abandoned-deployment sweep failed", "error", err)
	} else if n > 0 {
		slog.Info("marked abandoned deployments as failed", "count", n)
	}
	cancelSweep()

	// 5. Runtime client, then the pool step 4 just used.
	if stack.Closer != nil {
		_ = stack.Closer.Close()
	}
	dbPool.Close()
	slog.Info("shutdown complete")
}
