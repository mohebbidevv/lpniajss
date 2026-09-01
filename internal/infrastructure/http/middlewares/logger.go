// internal/infrastructure/http/middleware/logger.go
package middleware

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"golaunch/internal/infrastructure/utils"
)

// ANSI colors — cheap, no dependency, works in any real terminal
const (
	colorReset  = "\033[0m"
	colorGray   = "\033[90m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorRed    = "\033[31m"
	colorCyan   = "\033[36m"
)

func statusColor(status int) string {
	switch {
	case status >= 500:
		return colorRed
	case status >= 400:
		return colorYellow
	case status >= 300:
		return colorCyan
	default:
		return colorGreen
	}
}

// statusRecorder wraps http.ResponseWriter to capture the status code for
// logging. It re-implements Flusher (needed for SSE) so wrapping doesn't
// silently break streaming handlers downstream.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying ResponseWriter's Flush, if it has one.
// Without this, any handler doing `w.(http.Flusher)` (SSE, streaming logs)
// fails its type assertion against this wrapper and breaks.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// RequestLogger — one clean line per request, straight to stdout.
// format: 14:32:07 | 200 |   842ms | GET    /api/projects | req_a1b2c3
func RequestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		reqID := r.Header.Get("X-Request-ID")
		if reqID == "" {
			reqID = utils.NewID()
		}
		w.Header().Set("X-Request-ID", reqID)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		dur := time.Since(start)
		c := statusColor(rec.status)

		fmt.Fprintf(os.Stdout,
			"%s%s%s | %s%3d%s | %s%7s%s | %-6s %-30s | %s%s%s\n",
			colorGray, start.Format("15:04:05"), colorReset,
			c, rec.status, colorReset,
			colorGray, dur.Round(time.Millisecond), colorReset,
			r.Method, r.URL.Path,
			colorGray, shortID(reqID), colorReset,
		)
	})
}

// shortID trims a long id down to something readable in a terminal line
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}