package handlers

import (
	"context"
	"encoding/json"
	"golaunch/internal/application"
	middleware "golaunch/internal/infrastructure/http/middlewares"
	"net/http"
)

type ListProjectsHandler struct {
	UseCase *application.ListProjectsUseCase
}

func NewListProjectsHandler(uc *application.ListProjectsUseCase) *ListProjectsHandler {
	return &ListProjectsHandler{UseCase: uc}
}

func (h *ListProjectsHandler) ServeHTTP(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	user, ok := middleware.UserFromContext(r.Context())
	if !ok {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}

	projects, err := h.UseCase.Execute(r.Context(), user.ID)
	if err != nil {
		respondError(w, err, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(projects)
}
