package handlers

import (
	"context"
	"encoding/json"
	"golaunch/internal/application"
	middleware "golaunch/internal/infrastructure/http/middlewares"
	"net/http"
)

type GetProjectHandler struct {
	UseCase *application.GetProjectUseCase
}

func NewGetProjectHandler(uc *application.GetProjectUseCase) *GetProjectHandler {
	return &GetProjectHandler{UseCase: uc}
}

func (h *GetProjectHandler) ServeHTTP(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
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

	detail, err := h.UseCase.Execute(r.Context(), projectID, user.ID)
	if err != nil {
		status := http.StatusInternalServerError
		if isNotFound(err) {
			status = http.StatusNotFound
		}
		respondError(w, err, status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(detail)
}
