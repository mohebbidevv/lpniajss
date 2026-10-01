package handlers

import (
	"context"
	"encoding/json"
	"golaunch/internal/application"
	middleware "golaunch/internal/infrastructure/http/middlewares"
	"net/http"
)

type ProjectEnvHandler struct {
	ListUseCase *application.ListProjectEnvUseCase
	SetUseCase  *application.SetProjectEnvUseCase
}

func NewProjectEnvHandler(listUC *application.ListProjectEnvUseCase, setUC *application.SetProjectEnvUseCase) *ProjectEnvHandler {
	return &ProjectEnvHandler{ListUseCase: listUC, SetUseCase: setUC}
}

type setEnvRequest struct {
	Env map[string]string `json:"env"`
}

func (h *ProjectEnvHandler) ServeHTTP(ctx context.Context, w http.ResponseWriter, r *http.Request) {
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

	switch r.Method {
	case http.MethodGet:
		env, err := h.ListUseCase.Execute(r.Context(), projectID, user.ID)
		if err != nil {
			status := http.StatusInternalServerError
			if isNotFound(err) {
				status = http.StatusNotFound
			}
			respondError(w, err, status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]map[string]string{"env": env})

	case http.MethodPut, http.MethodPost:
		var req setEnvRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}

		env, err := h.SetUseCase.Execute(r.Context(), projectID, user.ID, req.Env)
		if err != nil {
			status := http.StatusInternalServerError
			switch {
			case isNotFound(err):
				status = http.StatusNotFound
			case isInvalidEnv(err):
				status = http.StatusBadRequest
			}
			respondError(w, err, status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]map[string]string{"env": env})

	default:
		http.Error(w, "GET, PUT, or POST only", http.StatusMethodNotAllowed)
	}
}
