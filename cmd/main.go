package main

import (
	"context"
	"golaunch/internal/application"
	"golaunch/internal/infrastructure/caddy"
	"golaunch/internal/infrastructure/config"
	"golaunch/internal/infrastructure/database/postgres"
	"golaunch/internal/infrastructure/hostexec"
	packageHttp "golaunch/internal/infrastructure/http"
	middleware "golaunch/internal/infrastructure/http/middlewares"
	"golaunch/internal/infrastructure/vcsgit"
	"golaunch/internal/queue"
	"log"
	nethttp "net/http"
	"os"
)

var (
	UploadDir = "./uploads"
	WorkDir   = "./work"
)

func main() {

	if err := os.MkdirAll(UploadDir, 0755); err != nil {
		log.Fatalf("Failed to create upload directory: %v", err)
	}
	if err := os.MkdirAll(WorkDir, 0755); err != nil {
		log.Fatalf("Failed to create work directory: %v", err)
	}

	ctx := context.Background()

	configuration, err := config.LoadConfig()
	if err != nil {
		panic(err)
	}

	dbDSN := postgres.BuildDSN(configuration.DB)
	dbPool, err := postgres.ConnectDB(ctx, dbDSN)

	if err != nil {
		log.Fatalf("cant connect db %v", err.Error())
	}

	postgres.RunMigrations(ctx, dbPool, "file://migrations")
	defer func() {
		if dbPool != nil {
			log.Println("Closing database connection pool...")
			dbPool.Close()
		}
	}()
	mux := nethttp.NewServeMux()
	handler := middleware.RequestLogger(mux)

	registry := application.NewLogRegistry()
	dbRepo := postgres.NewProjectRepository(dbPool)
	deploymentRepo := postgres.NewDeploymentRepository(dbPool)

	caddyClient := caddy.NewCaddyClient("http://localhost:2019", "localhost")

	// host-exec runtime today; a Docker implementation of repository.Runtime
	// and repository.ImageBuilder is a drop-in swap here later — nothing
	// above the wiring in main.go needs to change.
	hostRuntime := hostexec.NewHostExecRuntime()
	hostBuilder := hostexec.NewHostExecImageBuilder()
	gitSource := vcsgit.NewGitSource()

	pipeline := application.NewDeployPipeline(dbRepo, deploymentRepo, hostRuntime, hostBuilder, gitSource, caddyClient, registry)

	var eventConsumer *application.EventConsumer

	processor := func(ctx context.Context, job queue.Job) error {
		logCh, ok := registry.Get(job.ProjectID)
		if ok {
			defer registry.Delete(job.ProjectID)
			defer close(logCh)
		}
		err := pipeline.Deploy(ctx, job.ProjectID)
		if err == nil && eventConsumer != nil {
			eventConsumer.ResetCrashCount(job.ProjectID)
		}
		return err
	}

	// jobs are builds now, not long-lived process babysitting — the
	// worker returns as soon as the app is started, so a small pool is
	// plenty.
	workerPool := queue.NewWorkerPool(4, processor)
	workerPool.Start()
	defer workerPool.ShutDown()

	eventConsumer = application.NewEventConsumer(dbRepo, deploymentRepo, hostRuntime, caddyClient, workerPool)
	go eventConsumer.Run(ctx)

	reconciler := application.NewReconciler(deploymentRepo, hostRuntime, caddyClient, pipeline)
	go reconciler.Run(ctx)

	packageHttp.InitializeRoutes(ctx, dbPool, workerPool, caddyClient, registry, hostRuntime, mux)
	server := &nethttp.Server{
		Addr:    ":" + configuration.Server.Port,
		Handler: handler,

		// Open for later Timeouts
	}

	if err := server.ListenAndServe(); err != nil && err != nethttp.ErrServerClosed {
		log.Fatalf("HTTP server failed: %v", err)
	}
}
