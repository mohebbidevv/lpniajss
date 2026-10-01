package handlers

import (
	"context"
	"encoding/json"
	"golaunch/internal/application"
	middleware "golaunch/internal/infrastructure/http/middlewares"
	"net/http"
	"strings"
)

type StopHandler struct {
	StopUseCase *application.StopProjectUseCase
}

func NewStopHandler(uc *application.StopProjectUseCase) *StopHandler {
	return &StopHandler{StopUseCase: uc}
}

func (h *StopHandler) ServeHTTP(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	projectID := r.PathValue("projectID")
	if projectID == "" {
		http.Error(w, "missing projectID", http.StatusBadRequest)
		return
	}

	user, ok := middleware.UserFromContext(r.Context())
	if !ok {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}

	if err := h.StopUseCase.Execute(r.Context(), projectID, user.ID); err != nil {
		status := http.StatusInternalServerError
		if isNotFound(err) {
			status = http.StatusNotFound
		}
		respondError(w, err, status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "stopped"})
}

func isNotFound(err error) bool {
	return strings.Contains(err.Error(), "not found") ||
		strings.Contains(err.Error(), "no live deployment") ||
		strings.Contains(err.Error(), "never been deployed") ||
		strings.Contains(err.Error(), "nothing is currently running")
}
