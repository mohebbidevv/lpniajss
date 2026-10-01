package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"golaunch/internal/application"
	middleware "golaunch/internal/infrastructure/http/middlewares"
	"net/http"
	"strings"
)

// maxUploadBytes caps the request body. It is referenced by both the reader
// and the error message so the limit and what we tell the user can't drift
// apart.
const maxUploadBytes = 20 << 20

type UploadHandler struct {
	UploadUseCase application.UploadProjectUseCase
}

func NewUplaodHandler(uploadUC *application.UploadProjectUseCase) *UploadHandler {
	return &UploadHandler{UploadUseCase: *uploadUC}
}

func (handler *UploadHandler) ServeHTTP(
	ctx context.Context, w http.ResponseWriter, r *http.Request) {

	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	// MaxBytesReader, not a size check after the fact: this makes the
	// server stop reading and close the connection once the cap is hit,
	// instead of accepting the whole body and only then rejecting it.
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)

	// file validation
	file, hdr, err := r.FormFile("file")
	if err != nil {
		// errors.As, not a string compare: MaxBytesReader reports
		// *http.MaxBytesError and FormFile wraps it, so neither the type
		// nor the message survives a == against a literal.
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, fmt.Sprintf("file is too large (max %dMB)", maxUploadBytes>>20), http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "missing file field (multipart form: file)", http.StatusBadRequest)
		}
		return
	}
	defer file.Close()

	if !strings.HasSuffix(strings.ToLower(hdr.Filename), ".zip") {
		http.Error(w, "only .zip supported", http.StatusBadRequest)
		return
	}

	// anonymous is allowed here — an unauthenticated upload is staged with
	// no owner and picked up by /projects/{id}/claim right after signup.
	// An already-logged-in caller (OptionalAuth resolved a user) owns it
	// immediately instead.
	var userID string
	if user, ok := middleware.UserFromContext(r.Context()); ok {
		userID = user.ID
	}

	uploadResult, err := handler.UploadUseCase.Execute(ctx, application.UploadInput{
		UserID:   userID,
		Filename: hdr.Filename,
		File:     file,
	})

	if err != nil {
		status := http.StatusInternalServerError
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

	_ = json.NewEncoder(w).Encode(uploadResult)

}
