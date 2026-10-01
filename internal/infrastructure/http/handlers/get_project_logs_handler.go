package handlers

import (
	"context"
	"fmt"
	"golaunch/internal/application"
	middleware "golaunch/internal/infrastructure/http/middlewares"
	"net/http"
)

type GetProjectLogsHandler struct {
	UseCase *application.GetProjectLogsUseCase

	// Shutdown is closed when the process is going down; nil means never.
	Shutdown <-chan struct{}
}

func NewGetProjectLogsHandler(uc *application.GetProjectLogsUseCase, shutdown <-chan struct{}) *GetProjectLogsHandler {
	return &GetProjectLogsHandler{UseCase: uc, Shutdown: shutdown}
}

func (h *GetProjectLogsHandler) ServeHTTP(ctx context.Context, w http.ResponseWriter, r *http.Request) {
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

	// use request context so a client disconnect stops the log stream
	logCh, err := h.UseCase.Execute(r.Context(), projectID, user.ID)
	if err != nil {
		status := http.StatusInternalServerError
		if isNotFound(err) {
			status = http.StatusNotFound
		}
		respondError(w, err, status)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	for {
		select {
		case line, ok := <-logCh:
			// see run_handler: the ok form keeps a closed channel from
			// spinning this select
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", line.Stream, line.Text)
			flusher.Flush()

		case <-h.Shutdown:
			// No drain needed for the same reason as below: cancelling the
			// request context tears the producer down, and returning from
			// here is what lets http.Server.Shutdown finish.
			return

		case <-r.Context().Done():
			// No drain goroutine needed here, unlike the deploy stream:
			// this channel is produced by Runtime.Logs under this same
			// request context, and its sends already abort on
			// cancellation, so the producer tears itself down.
			return
		}
	}
}
