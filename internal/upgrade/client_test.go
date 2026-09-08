package upgrade

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recorded is what the fake cwa-updater saw.
type recorded struct {
	method string
	path   string
	body   Request
}

// newFakeUpdater serves handler and returns a Client pointed at it, plus a
// pointer to the last request it recorded.
func newFakeUpdater(t *testing.T, handler http.HandlerFunc) (*Client, *recorded) {
	t.Helper()

	last := &recorded{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last.method, last.path = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&last.body)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	parsed, err := url.Parse(srv.URL)
	require.NoError(t, err)

	return NewClient(parsed.Hostname(), parsed.Port()), last
}

// validRequest is a handoff that passes Validate.
func validRequest() *Request {
	return &Request{
		ExecutionID:     "exec-1",
		AgentID:         "agent-1",
		TargetVersion:   "v2.1",
		RollbackVersion: "v2.0",
	}
}

func TestClientHandoffPostsTheRequestAndDecodesAcceptance(t *testing.T) {
	t.Parallel()

	client, last := newFakeUpdater(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(Acceptance{
			Status: StatusAccepted, Phase: PhasePending, CollectedCount: 2, ExpectedCount: 3,
		})
	})

	acceptance, err := client.Handoff(context.Background(), validRequest())
	require.NoError(t, err)

	assert.Equal(t, http.MethodPost, last.method)
	assert.Equal(t, upgradeRoute, last.path)
	assert.Equal(t, "exec-1", last.body.ExecutionID)
	assert.Equal(t, "agent-1", last.body.AgentID)
	assert.Equal(t, "v2.1", last.body.TargetVersion)

	assert.Equal(t, StatusAccepted, acceptance.Status)
	assert.Equal(t, 2, acceptance.CollectedCount)
	assert.Equal(t, 3, acceptance.ExpectedCount)
}

func TestClientHandoffValidatesBeforeCallingTheServer(t *testing.T) {
	t.Parallel()

	client, last := newFakeUpdater(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	_, err := client.Handoff(context.Background(), &Request{ExecutionID: "exec-1"})
	require.ErrorIs(t, err, ErrInvalidRequest)

	// A handoff cwa-updater would reject anyway must not reach it.
	assert.Empty(t, last.method)
}

func TestClientHandoffMapsRefusalsOntoSentinels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{
			name:   "conflict is retryable",
			status: http.StatusConflict,
			body:   ErrConflict.Error() + ": upgrade to v2.0 is already in_progress",
			want:   ErrConflict,
		},
		{
			name:   "bad request is not",
			status: http.StatusBadRequest,
			body:   ErrInvalidRequest.Error() + ": agent_id is required",
			want:   ErrInvalidRequest,
		},
		{
			name:   "server error is unavailable",
			status: http.StatusInternalServerError,
			body:   "persisting upgrade state: etcdserver: request timed out",
			want:   ErrUpdaterUnavailable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			client, _ := newFakeUpdater(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": tc.body})
			})

			_, err := client.Handoff(context.Background(), validRequest())
			require.ErrorIs(t, err, tc.want)
			// cwa-updater's own reason survives, and is not repeated.
			assert.Contains(t, err.Error(), tc.body)
			assert.NotContains(t, err.Error(), tc.want.Error()+": "+tc.want.Error())
		})
	}
}

func TestClientHandoffReportsAnUnreachableUpdater(t *testing.T) {
	t.Parallel()

	// Nothing is listening on this port.
	client := NewClient("127.0.0.1", "1")

	_, err := client.Handoff(context.Background(), validRequest())
	require.ErrorIs(t, err, ErrUpdaterUnavailable)
}

func TestClientStatusDecodesTheView(t *testing.T) {
	t.Parallel()

	completed := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	client, last := newFakeUpdater(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(StatusView{
			Status:          StatusRolledBack,
			Phase:           PhaseRolledBack,
			TargetVersion:   "v2.1",
			RollbackVersion: "v2.0",
			AgentExecutions: map[string]string{"agent-1": "exec-1"},
			ExpectedCount:   1,
			Result: &Result{
				Status:      ResultRolledBack,
				FromVersion: "v2.0",
				ToVersion:   "v2.1",
				Reason:      "agents did not come back healthy",
				CompletedAt: completed,
			},
		})
	})

	view, err := client.Status(context.Background())
	require.NoError(t, err)

	assert.Equal(t, http.MethodGet, last.method)
	assert.Equal(t, statusRoute, last.path)

	assert.Equal(t, PhaseRolledBack, view.Phase)
	assert.Equal(t, map[string]string{"agent-1": "exec-1"}, view.AgentExecutions)
	require.NotNil(t, view.Result)
	assert.Equal(t, ResultRolledBack, view.Result.Status)
	assert.Equal(t, "agents did not come back healthy", view.Result.Reason)
	assert.True(t, completed.Equal(view.Result.CompletedAt))
}

func TestClientStatusReportsIdleAsErrNoState(t *testing.T) {
	t.Parallel()

	client, _ := newFakeUpdater(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	view, err := client.Status(context.Background())
	require.ErrorIs(t, err, ErrNoState)
	assert.Nil(t, view)
}

func TestClientAcknowledgeDeletesTheStatus(t *testing.T) {
	t.Parallel()

	client, last := newFakeUpdater(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	require.NoError(t, client.Acknowledge(context.Background()))
	assert.Equal(t, http.MethodDelete, last.method)
	assert.Equal(t, statusRoute, last.path)
}

func TestClientAcknowledgeRefusedWhileAnUpgradeRuns(t *testing.T) {
	t.Parallel()

	client, _ := newFakeUpdater(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": ErrConflict.Error() + ": upgrade to v2.1 is in_progress",
		})
	})

	err := client.Acknowledge(context.Background())
	require.ErrorIs(t, err, ErrConflict)
}

func TestClientHonoursContextCancellation(t *testing.T) {
	t.Parallel()

	client, _ := newFakeUpdater(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.Status(ctx)
	require.ErrorIs(t, err, ErrUpdaterUnavailable)
	require.ErrorIs(t, err, context.Canceled)
}
