package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireInternalToken(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })

	cases := []struct {
		name       string
		configured string
		supplied   string
		want       int
	}{
		{"correct token passes", "s3cret", "s3cret", http.StatusTeapot},
		// 404 rather than 401: a 401 would confirm the endpoint exists.
		{"wrong token 404s", "s3cret", "nope", http.StatusNotFound},
		{"missing token 404s", "s3cret", "", http.StatusNotFound},
		// Fails closed — an unset token disables the endpoint rather than
		// leaving internals open to everyone.
		{"unconfigured token disables route", "", "", http.StatusNotFound},
		{"unconfigured token rejects any value", "", "anything", http.StatusNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/internal/stats", nil)
			if tc.supplied != "" {
				req.Header.Set("X-Internal-Token", tc.supplied)
			}
			rec := httptest.NewRecorder()
			RequireInternalToken(tc.configured, ok).ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}
