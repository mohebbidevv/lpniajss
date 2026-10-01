package http

import (
	"context"
	"encoding/json"
	"golaunch/internal/application"
	"golaunch/internal/domain/repository"
	"golaunch/internal/infrastructure/caddy"
	"golaunch/internal/infrastructure/crypto"
	"golaunch/internal/infrastructure/database/postgres"
	handler "golaunch/internal/infrastructure/http/handlers"
	middleware "golaunch/internal/infrastructure/http/middlewares"
	"golaunch/internal/infrastructure/storage"
	"golaunch/internal/infrastructure/vcsgit"
	"golaunch/internal/queue"
	"net/http"
	"time"

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

func InitializeRunProjectHandler(db *pgxpool.Pool, wp *queue.WorkerPool, registry *application.LogRegistry, shutdown <-chan struct{}) *handler.RunHandler {
	dbRepo := postgres.NewProjectRepository(db) // same repo, fresh instance
	useCase := application.NewRunProjectUseCase(dbRepo, wp, registry, postgres.NewUserRepository(db))
	return handler.NewRunHandler(useCase, shutdown)
}

func InitializeStopProjectHandler(db *pgxpool.Pool, runtime repository.Runtime, caddyClient *caddy.CaddyClient) *handler.StopHandler {
	dbRepo := postgres.NewProjectRepository(db)
	deploymentRepo := postgres.NewDeploymentRepository(db)
	useCase := application.NewStopProjectUseCase(dbRepo, deploymentRepo, runtime, caddyClient)
	return handler.NewStopHandler(useCase)
}

func InitializeRollbackProjectHandler(db *pgxpool.Pool, runtime repository.Runtime, builder repository.ImageBuilder, caddyClient *caddy.CaddyClient, box *crypto.SecretBox) *handler.RollbackHandler {
	dbRepo := postgres.NewProjectRepository(db)
	deploymentRepo := postgres.NewDeploymentRepository(db)
	envVarRepo := postgres.NewEnvVarRepository(db, box)
	useCase := application.NewRollbackDeploymentUseCase(dbRepo, deploymentRepo, runtime, builder, envVarRepo, caddyClient)
	return handler.NewRollbackHandler(useCase)
}

func InitializeListDeploymentsHandler(db *pgxpool.Pool) *handler.ListDeploymentsHandler {
	dbRepo := postgres.NewProjectRepository(db)
	deploymentRepo := postgres.NewDeploymentRepository(db)
	useCase := application.NewListDeploymentsUseCase(dbRepo, deploymentRepo)
	return handler.NewListDeploymentsHandler(useCase)
}

func InitializeRenameHandler(db *pgxpool.Pool, caddyClient *caddy.CaddyClient) *handler.RenameHandler {
	dbRepo := postgres.NewProjectRepository(db)
	useCase := application.NewRenameProjectUseCase(dbRepo, caddyClient)
	return handler.NewRenameHandler(useCase)
}

func InitializeProjectEnvHandler(db *pgxpool.Pool, box *crypto.SecretBox) *handler.ProjectEnvHandler {
	dbRepo := postgres.NewProjectRepository(db)
	envVarRepo := postgres.NewEnvVarRepository(db, box)
	listUC := application.NewListProjectEnvUseCase(dbRepo, envVarRepo)
	setUC := application.NewSetProjectEnvUseCase(dbRepo, envVarRepo)
	return handler.NewProjectEnvHandler(listUC, setUC)
}

func InitializeDeleteProjectHandler(db *pgxpool.Pool, runtime repository.Runtime, builder repository.ImageBuilder, caddyClient *caddy.CaddyClient) *handler.DeleteProjectHandler {
	dbRepo := postgres.NewProjectRepository(db)
	deploymentRepo := postgres.NewDeploymentRepository(db)
	useCase := application.NewDeleteProjectUseCase(dbRepo, deploymentRepo, runtime, builder, caddyClient)
	return handler.NewDeleteProjectHandler(useCase)
}

func InitializeListProjectsHandler(db *pgxpool.Pool, domain string) *handler.ListProjectsHandler {
	dbRepo := postgres.NewProjectRepository(db)
	useCase := application.NewListProjectsUseCase(dbRepo, domain)
	return handler.NewListProjectsHandler(useCase)
}

func InitializeGetProjectHandler(db *pgxpool.Pool, domain string) *handler.GetProjectHandler {
	dbRepo := postgres.NewProjectRepository(db)
	deploymentRepo := postgres.NewDeploymentRepository(db)
	useCase := application.NewGetProjectUseCase(dbRepo, deploymentRepo, domain)
	return handler.NewGetProjectHandler(useCase)
}

func InitializeClaimHandler(db *pgxpool.Pool, domain string) *handler.ClaimHandler {
	dbRepo := postgres.NewProjectRepository(db)
	useCase := application.NewClaimProjectUseCase(dbRepo, domain)
	return handler.NewClaimHandler(useCase)
}

func InitializeGetProjectLogsHandler(db *pgxpool.Pool, runtime repository.Runtime, shutdown <-chan struct{}) *handler.GetProjectLogsHandler {
	dbRepo := postgres.NewProjectRepository(db)
	deploymentRepo := postgres.NewDeploymentRepository(db)
	useCase := application.NewGetProjectLogsUseCase(dbRepo, deploymentRepo, runtime)
	return handler.NewGetProjectLogsHandler(useCase, shutdown)
}

func InitializeAuthHandler(db *pgxpool.Pool, secureCookies bool, limiter *middleware.LoginLimiter, mailer repository.Mailer) *handler.AuthHandler {
	userRepo := postgres.NewUserRepository(db)
	sessionRepo := postgres.NewSessionRepository(db)
	tokenRepo := postgres.NewUserTokenRepository(db)

	registerUC := application.NewRegisterUserUseCase(userRepo)
	loginUC := application.NewLoginUserUseCase(userRepo, sessionRepo)
	logoutUC := application.NewLogoutUserUseCase(sessionRepo)

	h := handler.NewAuthHandler(registerUC, loginUC, logoutUC, secureCookies, limiter)
	h.RequestResetUseCase = application.NewRequestPasswordResetUseCase(userRepo, tokenRepo, mailer)
	h.ResetPasswordUseCase = application.NewResetPasswordUseCase(userRepo, tokenRepo, sessionRepo)
	h.RequestVerifyUseCase = application.NewRequestEmailVerificationUseCase(userRepo, tokenRepo, mailer)
	h.VerifyEmailUseCase = application.NewVerifyEmailUseCase(userRepo, tokenRepo)
	return h
}

// ServerConfig is the subset of config InitializeRoutes needs, kept
// separate from config.AppConfig so this package doesn't import the whole
// config tree for two fields.
type ServerConfig struct {
	AllowedOrigins []string
	SecureCookies  bool
	Domain         string // for computing a project's LiveURL

	// AnonymousSubmitLimit/Window rate-limit /upload and /import/github by
	// client IP, since both accept unauthenticated requests now.
	AnonymousSubmitLimit  int
	AnonymousSubmitWindow time.Duration

	// LoginIPLimit / LoginAccountLimit / LoginWindow throttle failed
	// sign-ins. Both keys are needed: per-IP catches spraying and CPU
	// exhaustion, per-account catches a distributed attacker on one target.
	LoginIPLimit      int
	LoginAccountLimit int
	LoginWindow       time.Duration

	// InternalToken guards /internal/*. Empty disables those endpoints
	// entirely, so a missing config value fails closed.
	InternalToken string

	// Version is reported by /healthz.
	Version string

	// EnvSecretBox encrypts project env vars at rest. Nil stores them as
	// plaintext.
	EnvSecretBox *crypto.SecretBox

	// Mailer delivers password-reset and verification links.
	Mailer repository.Mailer

	// Shutdown is closed when the process starts going down. The SSE
	// handlers select on it so they return promptly: an SSE connection is
	// never idle, and http.Server.Shutdown waits on non-idle connections,
	// so without this every shutdown burns its full timeout.
	Shutdown <-chan struct{}
}

func InitializeRoutes(
	ctx context.Context,
	db *pgxpool.Pool,
	wp *queue.WorkerPool,
	caddyClient *caddy.CaddyClient,
	registry *application.LogRegistry,
	runtime repository.Runtime,
	builder repository.ImageBuilder,
	cfg ServerConfig,
	mux *http.ServeMux,
) {
	uploadHandler := InitializeUploadHandler(db)
	importGithubHandler := InitializeImportGithubHandler(db)
	runHandler := InitializeRunProjectHandler(db, wp, registry, cfg.Shutdown)
	stopHandler := InitializeStopProjectHandler(db, runtime, caddyClient)
	renameHandler := InitializeRenameHandler(db, caddyClient)
	deleteProjectHandler := InitializeDeleteProjectHandler(db, runtime, builder, caddyClient)
	projectEnvHandler := InitializeProjectEnvHandler(db, cfg.EnvSecretBox)
	listProjectsHandler := InitializeListProjectsHandler(db, cfg.Domain)
	getProjectHandler := InitializeGetProjectHandler(db, cfg.Domain)
	claimHandler := InitializeClaimHandler(db, cfg.Domain)
	getProjectLogsHandler := InitializeGetProjectLogsHandler(db, runtime, cfg.Shutdown)
	rollbackHandler := InitializeRollbackProjectHandler(db, runtime, builder, caddyClient, cfg.EnvSecretBox)
	listDeploymentsHandler := InitializeListDeploymentsHandler(db)
	loginLimiter := middleware.NewLoginLimiter(cfg.LoginIPLimit, cfg.LoginAccountLimit, cfg.LoginWindow)
	authHandler := InitializeAuthHandler(db, cfg.SecureCookies, loginLimiter, cfg.Mailer)

	userRepo := postgres.NewUserRepository(db)
	sessionRepo := postgres.NewSessionRepository(db)
	requireAuth := middleware.RequireAuth(sessionRepo, userRepo)
	optionalAuth := middleware.OptionalAuth(sessionRepo, userRepo)
	cors := withCORS(cfg.AllowedOrigins)
	anonSubmitLimiter := middleware.NewIPRateLimiter(cfg.AnonymousSubmitLimit, cfg.AnonymousSubmitWindow)

	// adapt turns a handler method — they all take an explicit ctx — into
	// a plain http.Handler.
	adapt := func(fn func(context.Context, http.ResponseWriter, *http.Request)) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fn(ctx, w, r) })
	}

	// jsonAuthed is the shape nearly every endpoint here wants: CORS, a
	// response deadline, then a session check.
	jsonAuthed := func(fn func(context.Context, http.ResponseWriter, *http.Request)) http.Handler {
		return cors(bounded(requireAuth(adapt(fn))))
	}
	jsonPublic := func(fn func(context.Context, http.ResponseWriter, *http.Request)) http.Handler {
		return cors(bounded(adapt(fn)))
	}

	// teardownAuthed carries a much longer bound. Stopping and deleting do
	// real work with real latency — a 10s container stop, an image removal,
	// an os.RemoveAll over a source tree — and TimeoutHandler CANCELS the
	// request context when it fires. Truncating one of these mid-teardown
	// would leave the route removed, the container half-stopped and the DB
	// never updated, which is worse than waiting.
	teardownAuthed := func(fn func(context.Context, http.ResponseWriter, *http.Request)) http.Handler {
		return cors(boundedLong(requireAuth(adapt(fn))))
	}

	// streamAuthed deliberately omits bounded — see the comment on bounded.
	// These routes either hold a connection open for the life of a build or
	// legitimately outrun any short deadline.
	streamAuthed := func(fn func(context.Context, http.ResponseWriter, *http.Request)) http.Handler {
		return cors(requireAuth(adapt(fn)))
	}

	// Operational endpoints: no CORS (no browser origin involved) and no
	// session (a probe has no cookie). Wrapping them in either would make
	// them fail for the wrong reason.
	health := handler.NewHealthHandler(db, runtime, cfg.Version)
	mux.HandleFunc("GET /healthz", health.Live)
	mux.HandleFunc("GET /readyz", health.Ready)

	mux.Handle("GET /internal/stats", middleware.RequireInternalToken(cfg.InternalToken,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(wp.GetStats())
		})))

	// public — no session required to create or start one
	mux.Handle("/auth/register", jsonPublic(authHandler.Register))
	mux.Handle("/auth/login", jsonPublic(authHandler.Login))
	mux.Handle("/auth/password-reset/request", jsonPublic(authHandler.RequestPasswordReset))
	mux.Handle("/auth/password-reset/confirm", jsonPublic(authHandler.ResetPassword))
	// Redeeming a verification link must work while signed out — the user
	// may open it in a different browser than they signed up in.
	mux.Handle("/auth/verify-email/confirm", jsonPublic(authHandler.VerifyEmail))

	// authed
	mux.Handle("/auth/logout", jsonAuthed(authHandler.Logout))
	mux.Handle("/auth/me", jsonAuthed(authHandler.Me))
	mux.Handle("/auth/verify-email/request", jsonAuthed(authHandler.RequestEmailVerification))

	// anonymous-allowed: get the repo/zip in first, gate on signup before
	// the actual build (see /projects/{projectID}/claim below). An
	// already-logged-in caller is still recognized so their project is
	// owned immediately.
	//
	// Not bounded: /upload streams a multipart body up to the size cap and
	// /import/github clones a repository inline.
	mux.Handle("/upload", cors(anonSubmitLimiter.Middleware(optionalAuth(adapt(uploadHandler.ServeHTTP)))))
	mux.Handle("/import/github", cors(anonSubmitLimiter.Middleware(optionalAuth(adapt(importGithubHandler.ServeHTTP)))))

	mux.Handle("/projects/{projectID}/claim", jsonAuthed(claimHandler.ServeHTTP))

	// {projectID} is Go 1.22+ stdlib path param — r.PathValue("projectID") reads it.
	// Both of these are SSE.
	mux.Handle("/run/{projectID}", streamAuthed(runHandler.ServeHTTP))
	mux.Handle("/projects/{projectID}/logs", streamAuthed(getProjectLogsHandler.ServeHTTP))

	// Rollback starts a container and waits for it to become ready —
	// ReadyTimeout alone is 90s, well past the JSON bound.
	mux.Handle("/projects/{projectID}/rollback", streamAuthed(rollbackHandler.ServeHTTP))

	mux.Handle("/stop/{projectID}", teardownAuthed(stopHandler.ServeHTTP))
	mux.Handle("/projects/{projectID}/slug", jsonAuthed(renameHandler.ServeHTTP))
	mux.Handle("/projects", jsonAuthed(listProjectsHandler.ServeHTTP))
	mux.Handle("/projects/{projectID}", jsonAuthed(getProjectHandler.ServeHTTP))

	// method-prefixed pattern so this coexists with the GET-only
	// "/projects/{projectID}" route above on the same path — Go's mux
	// dispatches DELETE here and every other method to the general one.
	mux.Handle("DELETE /projects/{projectID}", teardownAuthed(deleteProjectHandler.ServeHTTP))

	mux.Handle("/projects/{projectID}/env", jsonAuthed(projectEnvHandler.ServeHTTP))
	mux.Handle("/projects/{projectID}/deployments", jsonAuthed(listDeploymentsHandler.ServeHTTP))
}

// jsonRequestTimeout bounds ordinary JSON endpoints. It is applied per route
// rather than as http.Server.WriteTimeout, because a server-wide write
// deadline is measured from the end of the request headers and would cut the
// SSE streams off mid-deploy.
const jsonRequestTimeout = 15 * time.Second

// bounded caps how long a handler may take to produce a response.
//
// Never wrap a streaming route with it: http.TimeoutHandler buffers the
// entire response in memory before writing anything, so it would suppress
// every SSE event until the handler returned — quite apart from severing the
// stream at the deadline.
func bounded(h http.Handler) http.Handler {
	return http.TimeoutHandler(h, jsonRequestTimeout, `{"error":"request timed out"}`)
}

// longRequestTimeout covers endpoints that legitimately do slow work:
// stopping a container is a 10s SIGTERM grace period on its own, and
// deleting also removes an image and a source tree.
const longRequestTimeout = 60 * time.Second

func boundedLong(h http.Handler) http.Handler {
	return http.TimeoutHandler(h, longRequestTimeout, `{"error":"request timed out"}`)
}

// withCORS returns middleware scoped to an explicit origin allowlist.
// Cookie-based auth cannot use "*" — browsers refuse to send credentials to
// a wildcard origin — so the request's Origin is only ever echoed back when
// it's actually on the list.
func withCORS(allowedOrigins []string) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(allowedOrigins))
	for _, o := range allowedOrigins {
		allowed[o] = true
	}

	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if origin := r.Header.Get("Origin"); allowed[origin] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Set("Vary", "Origin")
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			h.ServeHTTP(w, r)
		})
	}
}
