package handlers

import (
	"context"
	"fmt"
	"golaunch/internal/application"
	middleware "golaunch/internal/infrastructure/http/middlewares"
	"net/http"
)

type RunHandler struct {
	RunUseCase *application.RunProjectUseCase

	// Shutdown is closed when the process is going down. A nil channel
	// blocks forever in a select, so leaving it unset simply means "never
	// shut down early" — which is what tests and any non-server caller
	// want.
	Shutdown <-chan struct{}
}

func NewRunHandler(uc *application.RunProjectUseCase, shutdown <-chan struct{}) *RunHandler {
	return &RunHandler{RunUseCase: uc, Shutdown: shutdown}
}

func (h *RunHandler) ServeHTTP(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	// if r.Method != http.MethodPost {
	// 	http.Error(w, "POST only", http.StatusMethodNotAllowed)
	// 	return
	// }

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

	// SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// use request context so SSE disconnect cancels the stream
	logCh, err := h.RunUseCase.Execute(r.Context(), projectID, user.ID)
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
		flusher.Flush()
		return
	}

	for {
		select {
		case line, ok := <-logCh:
			// the ok form matters: a bare receive on a closed channel
			// yields zero values forever and spins this select at 100% CPU
			if !ok {
				fmt.Fprintf(w, "event: done\ndata: process finished\n\n")
				flusher.Flush()
				return
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", line.Stream, line.Text)
			flusher.Flush()

		case <-h.Shutdown:
			// The process is going down. Same hand-off as a disconnect: an
			// SSE connection is never idle, so http.Server.Shutdown waits
			// on it until this handler returns.
			fmt.Fprintf(w, "event: error\ndata: server is shutting down, reconnect to resume\n\n")
			flusher.Flush()
			go drainLogChannel(logCh)
			return

		case <-r.Context().Done():
			// The client is gone. Hand the channel off before returning:
			// the producer here is DeployPipeline.streamLog running on a
			// worker goroutine, and its send only aborts on the *job*
			// context — not this request's. Simply returning would let the
			// 64-line buffer fill and then stall that worker for the rest
			// of the job timeout, so four disconnects would deadlock the
			// whole build pool.
			go drainLogChannel(logCh)
			return
		}
	}
}

// drainLogChannel discards whatever the producer still has to say so it is
// never blocked on a consumer that has gone away. It ends when the deploy
// job closes the channel, which the worker's processor does unconditionally
// once Deploy returns.
func drainLogChannel(ch <-chan application.LogLine) {
	for range ch {
	}
}
