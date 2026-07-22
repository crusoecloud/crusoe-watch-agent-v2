package bugreport

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeReport writes a report file with the given contents and returns its path.
func writeReport(t *testing.T, name, contents string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))

	return path
}

func TestUpload_Success(t *testing.T) {
	var (
		gotMethod   string
		gotAuth     string
		gotFileName string
		gotFile     string
		gotFields   = map[string]string{}
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")

		require.NoError(t, r.ParseMultipartForm(1<<20))

		part, header, err := r.FormFile("file")
		require.NoError(t, err)
		defer func() { _ = part.Close() }()

		gotFileName = header.Filename
		body, _ := io.ReadAll(part)
		gotFile = string(body)

		for _, name := range []string{"vm_id", "event_id", "node_name", "status"} {
			gotFields[name] = r.FormValue(name)
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	path := writeReport(t, "bug-report-evt-1.log.gz", "report-bytes")
	up := NewHTTPUploader(srv.URL, "sekret", "vm-123", "node-a")

	require.NoError(t, up.Upload(context.Background(), path, "evt-1"))

	// The archive is POSTed with the monitoring token, its basename, contents, and
	// the metadata fields the coordinator associates it by.
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "Bearer sekret", gotAuth)
	assert.Equal(t, "bug-report-evt-1.log.gz", gotFileName)
	assert.Equal(t, "report-bytes", gotFile)
	assert.Equal(t, map[string]string{
		"vm_id":     "vm-123",
		"event_id":  "evt-1",
		"node_name": "node-a",
		"status":    "success",
	}, gotFields)
}

func TestUpload_PermanentStatusIsNotRetryable(t *testing.T) {
	// A definitive 4xx (validation, auth, bad request) must not be retried.
	for _, code := range []int{
		http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
		http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))

		up := NewHTTPUploader(srv.URL, "t", "vm", "node")
		err := up.Upload(context.Background(), writeReport(t, "r.gz", "x"), "evt")
		srv.Close()

		require.ErrorIs(t, err, ErrUploadPermanent, "status %d should be permanent", code)
	}
}

func TestUpload_TransientStatusIsRetryable(t *testing.T) {
	// Transient statuses are errors but not permanent, so the caller may retry.
	for _, code := range []int{
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusRequestTimeout, http.StatusTooManyRequests,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))

		up := NewHTTPUploader(srv.URL, "t", "vm", "node")
		err := up.Upload(context.Background(), writeReport(t, "r.gz", "x"), "evt")
		srv.Close()

		require.Error(t, err, "status %d should error", code)
		require.NotErrorIs(t, err, ErrUploadPermanent, "status %d should be retryable", code)
	}
}

func TestUpload_MissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.gz")

	up := NewHTTPUploader("https://example.com/upload", "t", "vm", "node")
	err := up.Upload(context.Background(), path, "evt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "opening report")
}

func TestUpload_NoTokenOmitsAuthHeader(t *testing.T) {
	var hadAuth bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadAuth = r.Header["Authorization"]
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	up := NewHTTPUploader(srv.URL, "", "vm", "node")
	require.NoError(t, up.Upload(context.Background(), writeReport(t, "r.gz", "x"), "evt"))
	assert.False(t, hadAuth)
}
