package main

import (
	"context"
	"fmt"
	"golaunch/internal/application"
	"golaunch/internal/domain/entities"
	"golaunch/internal/infrastructure/caddy"
	"golaunch/internal/infrastructure/config"
	"golaunch/internal/infrastructure/database/postgres"
	packageHttp "golaunch/internal/infrastructure/http"
	"golaunch/internal/queue"
	"log"
	nethttp "net/http"
	"os"
	"os/exec"
	"sync"
)

var (
	RunningMu sync.Mutex
	Running   = map[string]*exec.Cmd{}
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
	
	registry := application.NewLogRegistry()
	projRunner := application.NewProjectRunner()
	dbRepo := postgres.NewProjectRepository(dbPool)
	
	caddyClient := caddy.NewCaddyClient("http://localhost:2019", "launchpad.ir")

	processor := func(ctx context.Context, job queue.Job) error {
		logCh, ok := registry.Get(job.ProjectID)
		if !ok {
			return fmt.Errorf("no log channel found for project %s", job.ProjectID)
		}

		defer registry.Delete(job.ProjectID)
		defer close(logCh)

		send := func(stream, text string) {
			select {
			case logCh <- application.LogLine{Stream: stream, Text: text}:
			case <-ctx.Done():
			}
		}

		project, err := dbRepo.GetByID(ctx, job.ProjectID)
		if err != nil {
			return fmt.Errorf("project lookup failed: %w", err)
		}

		path, err := application.ResolveProjectRoot(project.SourceLocation)
		if err != nil {
			send("stderr", fmt.Sprintf("[runner] failed to resolve project root: %v", err))
			_ = dbRepo.UpdateStatus(context.Background(), job.ProjectID, entities.StatusFailed)
			return err
		}

		if err := projRunner.Run(ctx, path, project.Port, send); err != nil {
			send("stderr", fmt.Sprintf("[runner] %v", err))
			_ = dbRepo.UpdateStatus(context.Background(), job.ProjectID, entities.StatusFailed)
			return err
		}

		if err := caddyClient.RegisterRoute(project.UniqueKey, project.Port); err != nil {
			// don't fail the whole deploy over this — log it and continue
			send("stderr", fmt.Sprintf("[runner] warning: failed to register route: %v", err))
		} else {
			send("stdout", fmt.Sprintf("[runner] live at https://%s.%s", project.UniqueKey, "golaunch.dev"))
		}

		_ = dbRepo.UpdateStatus(context.Background(), job.ProjectID, entities.StatusStopped)
		return nil
	}



	workerPool := queue.NewWorkerPool(15, processor)
	workerPool.Start()
	defer workerPool.ShutDown()

	packageHttp.InitializeRoutes(ctx, dbPool, workerPool, registry, mux)
	server := &nethttp.Server{
		Addr:    ":" + configuration.Server.Port,
		Handler: mux,

		// Open for later Timeouts
	}

	if err := server.ListenAndServe(); err != nil && err != nethttp.ErrServerClosed {
		log.Fatalf("HTTP server failed: %v", err)
	}
}
