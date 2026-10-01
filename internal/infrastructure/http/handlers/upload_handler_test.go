package handlers

import (
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

// oversizedMultipart streams a valid multipart body larger than limit, so the
// request fails on MaxBytesReader rather than on a malformed boundary — the
// distinction matters, because only the former produces *http.MaxBytesError.
func oversizedMultipart(t *testing.T, limit int64) (io.Reader, string) {
	t.Helper()

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	go func() {
		defer pw.Close()
		part, err := mw.CreateFormFile("file", "big.zip")
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		// Written lazily so the test never materializes 20MB in memory.
		if _, err := io.CopyN(part, zeroReader{}, limit+(1<<20)); err != nil {
			pw.CloseWithError(err)
			return
		}
		mw.Close()
	}()

	return pr, mw.FormDataContentType()
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

// TestUploadOversizedReturns413 is the regression for the dead error branch.
// The old code compared err.Error() against "http: too large body"; Go's
// actual message is "http: request body too large" and FormFile wraps it, so
// every oversized upload fell through to 400 "missing file field" and told
// the user their form was malformed.
func TestUploadOversizedReturns413(t *testing.T) {
	// The use case is never reached: the body cap trips inside FormFile,
	// well before any of it is needed.
	h := &UploadHandler{}

	body, contentType := oversizedMultipart(t, maxUploadBytes)
	req := httptest.NewRequest(http.MethodPost, "/upload", body)
	req.Header.Set("Content-Type", contentType)

	rec := httptest.NewRecorder()
	h.ServeHTTP(context.Background(), rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d\nbody: %s",
			rec.Code, http.StatusRequestEntityTooLarge, rec.Body.String())
	}
}

// TestUploadMissingFileFieldStillReturns400 guards the other side of the
// branch: a genuinely malformed request must not now be reported as 413.
func TestUploadMissingFileFieldStillReturns400(t *testing.T) {
	h := &UploadHandler{}

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		defer pw.Close()
		_ = mw.WriteField("notafile", "x")
		mw.Close()
	}()

	req := httptest.NewRequest(http.MethodPost, "/upload", pr)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	rec := httptest.NewRecorder()
	h.ServeHTTP(context.Background(), rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}
