package middleware

import (
	"net/http"

	"golaunch/internal/infrastructure/logging"
	"golaunch/internal/infrastructure/utils"
)

// RequestIDHeader is echoed on every response so a user can quote it in a
// bug report and it can be traced end to end.
const RequestIDHeader = "X-Request-ID"

// RequestID assigns each request an ID and puts it in the context.
//
// It honours an inbound header so a request that already crossed another
// service keeps one identity. It must be the outermost middleware: anything
// wrapped outside it logs without an ID.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if id == "" {
			id = utils.NewID()
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(logging.WithRequestID(r.Context(), id)))
	})
}

// RequestIDFrom re-exports the accessor so HTTP-layer callers don't have to
// reach into the logging package for it.
func RequestIDFrom(r *http.Request) string {
	return logging.RequestIDFrom(r.Context())
}
