// Package logging owns the process logger and the two pieces of per-request
// context every layer needs to correlate its output: the request ID and the
// logger already carrying it.
//
// The context helpers live here rather than in the HTTP middleware package
// so the application layer can read a request ID without importing anything
// HTTP-shaped.
package logging

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

// New builds the process logger and installs it as slog's default, so any
// package that logs through the package-level slog functions lands in the
// same stream with the same formatting.
//
// JSON is queryable and belongs in production; text is readable and belongs
// in a terminal. Source locations are attached only at debug level — they
// are not free.
func New(level, format string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: lvl, AddSource: lvl == slog.LevelDebug}

	var h slog.Handler
	if strings.EqualFold(strings.TrimSpace(format), "json") {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}

	l := slog.New(h)
	slog.SetDefault(l)
	return l
}

type loggerKey struct{}
type requestIDKey struct{}

// Into stashes a logger — normally one already carrying request_id, job_id
// and project_id — for the layers below to pick up.
func Into(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, l)
}

// From never returns nil. A missing logger is a wiring bug, not a reason to
// nil-panic in the middle of an incident, so it degrades to the default.
func From(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFrom returns "" when there is none — a worker-initiated deploy
// (reconciler, crash restart) legitimately has no originating request.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}
