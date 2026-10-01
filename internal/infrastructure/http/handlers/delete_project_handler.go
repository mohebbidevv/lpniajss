package handlers

import (
	"context"
	"golaunch/internal/application"
	middleware "golaunch/internal/infrastructure/http/middlewares"
	"net/http"
)

type DeleteProjectHandler struct {
	UseCase *application.DeleteProjectUseCase
}

func NewDeleteProjectHandler(uc *application.DeleteProjectUseCase) *DeleteProjectHandler {
	return &DeleteProjectHandler{UseCase: uc}
}

func (h *DeleteProjectHandler) ServeHTTP(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
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

	if err := h.UseCase.Execute(r.Context(), projectID, user.ID); err != nil {
		status := http.StatusInternalServerError
		if isNotFound(err) {
			status = http.StatusNotFound
		}
		respondError(w, err, status)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
