package handlers

import (
	"context"
	"encoding/json"
	"golaunch/internal/application"
	middleware "golaunch/internal/infrastructure/http/middlewares"
	"net/http"
)

type RollbackHandler struct {
	UseCase *application.RollbackDeploymentUseCase
}

func NewRollbackHandler(uc *application.RollbackDeploymentUseCase) *RollbackHandler {
	return &RollbackHandler{UseCase: uc}
}

type rollbackRequest struct {
	DeploymentID string `json:"deployment_id"`
}

func (h *RollbackHandler) ServeHTTP(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	projectID := r.PathValue("projectID")
	if projectID == "" {
		http.Error(w, "missing projectID", http.StatusBadRequest)
		return
	}

	var req rollbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.DeploymentID == "" {
		http.Error(w, "deployment_id is required", http.StatusBadRequest)
		return
	}

	user, ok := middleware.UserFromContext(r.Context())
	if !ok {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}

	if err := h.UseCase.Execute(r.Context(), projectID, user.ID, req.DeploymentID); err != nil {
		status := http.StatusInternalServerError
		switch {
		case isNotFound(err):
			status = http.StatusNotFound
		case isConflict(err):
			status = http.StatusConflict
		case isInvalidRollback(err):
			status = http.StatusBadRequest
		}
		respondError(w, err, status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "rolled back"})
}
