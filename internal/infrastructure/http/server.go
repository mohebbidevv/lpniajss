package http

import (
	"context"
	"golaunch/internal/application"
	"golaunch/internal/domain/repository"
	"golaunch/internal/infrastructure/caddy"
	"golaunch/internal/infrastructure/database/postgres"
	handler "golaunch/internal/infrastructure/http/handlers"
	"golaunch/internal/infrastructure/storage"
	"golaunch/internal/infrastructure/vcsgit"
	"golaunch/internal/queue"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
)

func InitializeUploadHandler(db *pgxpool.Pool) *handler.UploadHandler {
	dbRepo := postgres.NewProjectRepository(db)
	storageRepo := storage.Storage{}
	uploadDir := "./uploads"
	workDir := "./work"
	useCase := application.NewUploadProjectUseCase(dbRepo, &storageRepo, uploadDir, workDir)
	return handler.NewUplaodHandler(useCase)
}

func InitializeImportGithubHandler(db *pgxpool.Pool) *handler.ImportGithubHandler {
	dbRepo := postgres.NewProjectRepository(db)
	gitSource := vcsgit.NewGitSource()
	workDir := "./work"
	useCase := application.NewImportGithubProjectUseCase(dbRepo, gitSource, workDir)
	return handler.NewImportGithubHandler(useCase)
}

func InitializeRunProjectHandler(db *pgxpool.Pool, wp *queue.WorkerPool, registry *application.LogRegistry) *handler.RunHandler {
	dbRepo := postgres.NewProjectRepository(db) // same repo, fresh instance
	useCase := application.NewRunProjectUseCase(dbRepo, wp, registry)
	return handler.NewRunHandler(useCase)
}

func InitializeStopProjectHandler(db *pgxpool.Pool, runtime repository.Runtime, caddyClient *caddy.CaddyClient) *handler.StopHandler {
	dbRepo := postgres.NewProjectRepository(db)
	deploymentRepo := postgres.NewDeploymentRepository(db)
	useCase := application.NewStopProjectUseCase(dbRepo, deploymentRepo, runtime, caddyClient)
	return handler.NewStopHandler(useCase)
}

func InitializeRoutes(ctx context.Context, db *pgxpool.Pool, wp *queue.WorkerPool, caddyClient *caddy.CaddyClient, registry *application.LogRegistry, runtime repository.Runtime, mux *http.ServeMux) {
	uploadHandler := InitializeUploadHandler(db)
	importGithubHandler := InitializeImportGithubHandler(db)
	runHandler := InitializeRunProjectHandler(db, wp, registry)
	stopHandler := InitializeStopProjectHandler(db, runtime, caddyClient)

	mux.Handle("/upload", withCORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploadHandler.ServeHTTP(ctx, w, r)
	})))

	mux.Handle("/import/github", withCORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		importGithubHandler.ServeHTTP(ctx, w, r)
	})))

	// {projectID} is Go 1.22+ stdlib path param — r.PathValue("projectID") reads it
	mux.Handle("/run/{projectID}", withCORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runHandler.ServeHTTP(ctx, w, r)
	})))

	mux.Handle("/stop/{projectID}", withCORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stopHandler.ServeHTTP(ctx, w, r)
	})))
}

func withCORS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}
