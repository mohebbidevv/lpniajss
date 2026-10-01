package handlers

import (
	"context"
	"encoding/json"
	"golaunch/internal/application"
	middleware "golaunch/internal/infrastructure/http/middlewares"
	"net/http"
)

type ImportGithubHandler struct {
	ImportUseCase application.ImportGithubProjectUseCase
}

func NewImportGithubHandler(importUC *application.ImportGithubProjectUseCase) *ImportGithubHandler {
	return &ImportGithubHandler{ImportUseCase: *importUC}
}

type importGithubRequest struct {
	RepoURL string `json:"repo_url"`
	Ref     string `json:"ref"`
}

func (handler *ImportGithubHandler) ServeHTTP(
	ctx context.Context, w http.ResponseWriter, r *http.Request) {

	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var req importGithubRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if req.RepoURL == "" {
		http.Error(w, "repo_url is required", http.StatusBadRequest)
		return
	}

	// anonymous is allowed here — see upload_handler.go for why.
	var userID string
	if user, ok := middleware.UserFromContext(r.Context()); ok {
		userID = user.ID
	}

	result, err := handler.ImportUseCase.Execute(ctx, application.ImportGithubInput{
		UserID:  userID,
		RepoURL: req.RepoURL,
		Ref:     req.Ref,
	})
	if err != nil {
		status := http.StatusBadRequest
		switch {
		case isConflict(err):
			status = http.StatusConflict
		case isLimitReached(err):
			status = http.StatusForbidden
		}
		respondError(w, err, status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(result)
}
