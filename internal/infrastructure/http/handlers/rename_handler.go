package handlers

import (
	"context"
	"encoding/json"
	"golaunch/internal/application"
	middleware "golaunch/internal/infrastructure/http/middlewares"
	"net/http"
)

type RenameHandler struct {
	RenameUseCase *application.RenameProjectUseCase
}

func NewRenameHandler(uc *application.RenameProjectUseCase) *RenameHandler {
	return &RenameHandler{RenameUseCase: uc}
}

type renameRequest struct {
	Slug string `json:"slug"`
}

func (h *RenameHandler) ServeHTTP(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPatch {
		http.Error(w, "POST or PATCH only", http.StatusMethodNotAllowed)
		return
	}

	projectID := r.PathValue("projectID")
	if projectID == "" {
		http.Error(w, "missing projectID", http.StatusBadRequest)
		return
	}

	var req renameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}

	user, ok := middleware.UserFromContext(r.Context())
	if !ok {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}

	newSlug, err := h.RenameUseCase.Execute(r.Context(), projectID, user.ID, req.Slug)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case isConflict(err):
			status = http.StatusConflict
		case isNotFound(err):
			status = http.StatusNotFound
		}
		respondError(w, err, status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"slug": newSlug})
}
