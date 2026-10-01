package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"golaunch/internal/domain/repository"

	"github.com/jackc/pgx/v5/pgxpool"
)

// readinessTimeout bounds the dependency checks. A probe must never inherit
// a slow client's deadline or hang on a wedged dependency — a readiness
// endpoint that itself hangs is indistinguishable from a dead process.
const readinessTimeout = 2 * time.Second

type HealthHandler struct {
	DB        *pgxpool.Pool
	Runtime   repository.Runtime
	Version   string
	StartedAt time.Time
}

func NewHealthHandler(db *pgxpool.Pool, runtime repository.Runtime, version string) *HealthHandler {
	return &HealthHandler{DB: db, Runtime: runtime, Version: version, StartedAt: time.Now()}
}

// Live is the liveness probe: "is this process wedged?"
//
// It deliberately touches nothing external. If liveness checked Postgres, a
// five-second database blip would make the supervisor kill an otherwise
// healthy process — turning a hiccup into a restart cascade. Dependency
// health belongs in Ready, where the consequence is losing traffic rather
// than losing the process.
func (h *HealthHandler) Live(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"version":        h.Version,
		"uptime_seconds": int(time.Since(h.StartedAt).Seconds()),
	})
}

// Ready is the readiness probe: "can this process serve traffic right now?"
//
// A 503 here pulls the instance out of rotation without killing it, so it
// recovers on its own when the dependency does. Each dependency is reported
// by name so a failure is diagnosable from the response alone.
func (h *HealthHandler) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()

	checks := map[string]string{}
	ready := true

	if h.DB == nil {
		checks["database"] = "not configured"
		ready = false
	} else if err := h.DB.Ping(ctx); err != nil {
		checks["database"] = err.Error()
		ready = false
	} else {
		checks["database"] = "ok"
	}

	if h.Runtime == nil {
		checks["runtime"] = "not configured"
		ready = false
	} else if err := h.Runtime.Ping(ctx); err != nil {
		checks["runtime"] = err.Error()
		ready = false
	} else {
		checks["runtime"] = "ok"
	}

	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"ready": ready, "checks": checks})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
