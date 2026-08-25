package health

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/version"
)

// fakeReporter is a Reporter the test controls.
type fakeReporter struct {
	last time.Time
}

func (f fakeReporter) LastHeartbeat() time.Time { return f.last }

func newTestServer(last time.Time) *Server {
	return NewServer(":0", fakeReporter{last: last}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func callHealth(t *testing.T, srv *Server, method string) (*httptest.ResponseRecorder, serverResponse) {
	t.Helper()

	recorder := httptest.NewRecorder()
	srv.handleHealth(recorder, httptest.NewRequest(method, HealthPath, nil))

	var body serverResponse
	if recorder.Header().Get("Content-Type") == "application/json" {
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	}

	return recorder, body
}

// Neither probe may depend on the control plane: an agent that has not reached
// cwa-coordinator is still serving, and restarting or de-registering it would not
// help. The unreached state shows up as status, not as a failure code.
func TestHealthIsOKBeforeAnyHeartbeat(t *testing.T) {
	recorder, body := callHealth(t, newTestServer(time.Time{}), http.MethodGet)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, StatusStarting, body.Status)
	assert.Equal(t, version.Version, body.Version)
	assert.Nil(t, body.LastHeartbeat)
}

// last_heartbeat is what cwa-updater's post-upgrade poll reads to tell an agent
// that is merely listening from one that is reaching the control plane.
func TestHealthReportsHeartbeat(t *testing.T) {
	last := time.Now().Truncate(time.Second)

	recorder, body := callHealth(t, newTestServer(last), http.MethodGet)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, StatusHealthy, body.Status)
	require.NotNil(t, body.LastHeartbeat)
	assert.True(t, last.Equal(*body.LastHeartbeat))
}

func TestHealthRejectsNonGET(t *testing.T) {
	recorder, _ := callHealth(t, newTestServer(time.Now()), http.MethodPost)

	assert.Equal(t, http.StatusMethodNotAllowed, recorder.Code)
}

func TestServerRunStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	srv := NewServer("127.0.0.1:0", fakeReporter{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	done := make(chan error, 1)

	go func() { done <- srv.Run(ctx) }()

	// Give the listener a moment to bind before shutting it down.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}
