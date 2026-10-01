package handlers

import (
	"context"
	"encoding/json"
	"golaunch/internal/application"
	middleware "golaunch/internal/infrastructure/http/middlewares"
	"net/http"
)

type ClaimHandler struct {
	ClaimUseCase *application.ClaimProjectUseCase
}

func NewClaimHandler(uc *application.ClaimProjectUseCase) *ClaimHandler {
	return &ClaimHandler{ClaimUseCase: uc}
}

func (h *ClaimHandler) ServeHTTP(ctx context.Context, w http.ResponseWriter, r *http.Request) {
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

	summary, err := h.ClaimUseCase.Execute(r.Context(), projectID, user.ID)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case isNotFound(err):
			status = http.StatusNotFound
		case isLimitReached(err):
			status = http.StatusForbidden
		case isConflict(err):
			status = http.StatusConflict
		}
		respondError(w, err, status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(summary)
}
