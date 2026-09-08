package command

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/upgrade"
)

// fakeHandoff records the handoffs posted to it and replays a scripted outcome.
type fakeHandoff struct {
	calls []*upgrade.Request
	// failures is how many times to fail before accepting, and failWith is the
	// error to fail with.
	failures int
	failWith error
	// view is what Status reports; nil means cwa-updater is idle.
	view *upgrade.StatusView
	// viewAfter withholds view for this many Status calls, so a test can have
	// cwa-updater record a result partway through the retry loop.
	viewAfter   int
	statusCalls int
}

func (f *fakeHandoff) Status(context.Context) (*upgrade.StatusView, error) {
	f.statusCalls++

	if f.view == nil || f.statusCalls <= f.viewAfter {
		return nil, upgrade.ErrNoState
	}

	return f.view, nil
}

func (f *fakeHandoff) Handoff(_ context.Context, req *upgrade.Request) (upgrade.Acceptance, error) {
	copied := *req
	f.calls = append(f.calls, &copied)

	if len(f.calls) <= f.failures {
		return upgrade.Acceptance{}, f.failWith
	}

	return upgrade.Acceptance{
		Status: upgrade.StatusAccepted, Phase: upgrade.PhasePending,
		CollectedCount: len(f.calls), ExpectedCount: 3,
	}, nil
}

// upgradeExecuteFor builds a handler with the retry delay removed so tests do
// not sleep, dispatched as execution exec-1 for agent-1.
func upgradeExecuteFor(t *testing.T, client Updater, opts ...UpgradeExecuteOption) (*UpgradeExecute, context.Context) {
	t.Helper()

	handler := NewUpgradeExecute(client, func() string { return "agent-1" }, opts...)
	handler.delay = 0

	return handler, context.WithValue(context.Background(), execIDKey{}, "exec-1")
}

func upgradeParams() map[string]string {
	return map[string]string{
		ParamTargetVersion:   "v2.1",
		ParamRollbackVersion: "v2.0",
	}
}

func TestUpgradeExecuteHandsOffTheUpgrade(t *testing.T) {
	t.Parallel()

	client := &fakeHandoff{}
	handler, ctx := upgradeExecuteFor(t, client)

	result, err := handler.Run(ctx, upgradeParams())
	require.NoError(t, err)

	require.Len(t, client.calls, 1)
	req := client.calls[0]
	assert.Equal(t, "v2.1", req.TargetVersion)
	assert.Equal(t, "v2.0", req.RollbackVersion)
	// The dispatcher's execution_id and this agent's id are what cwa-updater
	// collects, and what it reports the outcome against.
	assert.Equal(t, "exec-1", req.ExecutionID)
	assert.Equal(t, "agent-1", req.AgentID)
	assert.False(t, req.RequestedAt.IsZero())

	// The command result carries the acceptance, not an upgrade outcome.
	var acceptance upgrade.Acceptance
	require.NoError(t, json.Unmarshal([]byte(result), &acceptance))
	assert.Equal(t, upgrade.StatusAccepted, acceptance.Status)
	assert.Equal(t, 3, acceptance.ExpectedCount)
}

func TestUpgradeExecutePassesArtifactAndChecksumThrough(t *testing.T) {
	t.Parallel()

	client := &fakeHandoff{}
	handler, ctx := upgradeExecuteFor(t, client)

	params := upgradeParams()
	params[ParamArtifactURL] = "oci://ghcr.io/crusoecloud/charts/crusoe-watch-agent:2.1"
	params[ParamChecksum] = "sha256:abc"

	_, err := handler.Run(ctx, params)
	require.NoError(t, err)

	require.Len(t, client.calls, 1)
	assert.Equal(t, "oci://ghcr.io/crusoecloud/charts/crusoe-watch-agent:2.1", client.calls[0].ArtifactURL)
	assert.Equal(t, "sha256:abc", client.calls[0].Checksum)
}

func TestUpgradeExecuteDefaultsRollbackToTheRunningVersion(t *testing.T) {
	t.Parallel()

	client := &fakeHandoff{}
	handler, ctx := upgradeExecuteFor(t, client)

	_, err := handler.Run(ctx, map[string]string{ParamTargetVersion: "v2.1"})
	require.NoError(t, err)

	require.Len(t, client.calls, 1)
	assert.NotEmpty(t, client.calls[0].RollbackVersion)
}

func TestUpgradeExecuteNotifiesOnlyAfterAcceptance(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		failures   int
		failWith   error
		wantNotify bool
	}{
		{name: "accepted", wantNotify: true},
		{
			name:       "refused outright",
			failures:   1,
			failWith:   fmt.Errorf("%w: agent_id is required", upgrade.ErrInvalidRequest),
			wantNotify: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			notified := ""
			client := &fakeHandoff{failures: tc.failures, failWith: tc.failWith}
			handler, ctx := upgradeExecuteFor(t, client,
				WithUpgradeNotifier(func(target string) { notified = target }))

			_, err := handler.Run(ctx, upgradeParams())
			if tc.wantNotify {
				require.NoError(t, err)
				assert.Equal(t, "v2.1", notified)
			} else {
				require.Error(t, err)
				assert.Empty(t, notified)
			}
		})
	}
}

func TestUpgradeExecuteRetriesAConflictUntilTheResultIsCleared(t *testing.T) {
	t.Parallel()

	// cwa-updater refuses while it still holds the previous round's result; the
	// heartbeat clears that alongside this command, so retrying is what works.
	client := &fakeHandoff{
		failures: 2,
		failWith: fmt.Errorf("%w: the result of the upgrade to v2.0 is unacknowledged", upgrade.ErrConflict),
	}
	handler, ctx := upgradeExecuteFor(t, client)

	_, err := handler.Run(ctx, upgradeParams())
	require.NoError(t, err)
	assert.Len(t, client.calls, 3)
}

func TestUpgradeExecuteRetriesAnUnreachableUpdater(t *testing.T) {
	t.Parallel()

	client := &fakeHandoff{
		failures: 1,
		failWith: fmt.Errorf("%w: connection refused", upgrade.ErrUpdaterUnavailable),
	}
	handler, ctx := upgradeExecuteFor(t, client)

	_, err := handler.Run(ctx, upgradeParams())
	require.NoError(t, err)
	assert.Len(t, client.calls, 2)
}

func TestUpgradeExecuteFailsFastOnARequestNeverAccepted(t *testing.T) {
	t.Parallel()

	client := &fakeHandoff{
		// More failures than attempts, so a retry would show up as a second call.
		failures: 5,
		failWith: fmt.Errorf("%w: target_version is required", upgrade.ErrInvalidRequest),
	}
	handler, ctx := upgradeExecuteFor(t, client)

	_, err := handler.Run(ctx, upgradeParams())
	require.ErrorIs(t, err, upgrade.ErrInvalidRequest)
	assert.Len(t, client.calls, 1)
}

func TestUpgradeExecuteGivesUpWhenTheCommandTimesOut(t *testing.T) {
	t.Parallel()

	client := &fakeHandoff{
		failures: 100,
		failWith: fmt.Errorf("%w: connection refused", upgrade.ErrUpdaterUnavailable),
	}
	handler, baseCtx := upgradeExecuteFor(t, client)
	handler.delay = time.Millisecond

	ctx, cancel := context.WithTimeout(baseCtx, 50*time.Millisecond)
	defer cancel()

	_, err := handler.Run(ctx, upgradeParams())
	require.ErrorIs(t, err, upgrade.ErrUpdaterUnavailable)
	// The reason names the attempts so a stuck handoff is diagnosable.
	assert.Contains(t, err.Error(), "attempts")
	assert.NotEmpty(t, client.calls)
}

// completedView is cwa-updater holding the result of a finished upgrade.
func completedView(status, toVersion string) *upgrade.StatusView {
	return &upgrade.StatusView{
		Status:        upgrade.StatusIdle,
		Phase:         upgrade.PhaseComplete,
		TargetVersion: toVersion,
		Result: &upgrade.Result{
			Status:      status,
			FromVersion: "v2.0",
			ToVersion:   toVersion,
			CompletedAt: time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC),
		},
	}
}

func TestUpgradeExecuteReportsAnUpgradeAlreadyApplied(t *testing.T) {
	t.Parallel()

	// The upgrade replaced the process that asked for it, so the coordinator
	// re-echoed this command to the replacement.
	client := &fakeHandoff{view: completedView(upgrade.ResultSucceeded, "v2.1")}
	handler, ctx := upgradeExecuteFor(t, client)

	result, err := handler.Run(ctx, upgradeParams())
	require.NoError(t, err)

	// Asking for the same upgrade twice would churn the whole DaemonSet.
	assert.Empty(t, client.calls)

	var reported upgrade.Result
	require.NoError(t, json.Unmarshal([]byte(result), &reported))
	assert.Equal(t, upgrade.ResultSucceeded, reported.Status)
	assert.Equal(t, "v2.1", reported.ToVersion)
}

// handedOffView is cwa-updater holding the result of a round this agent's
// execution was collected into.
func handedOffView(status, toVersion string) *upgrade.StatusView {
	view := completedView(status, toVersion)
	view.AgentExecutions = map[string]string{"agent-1": "exec-1"}

	return view
}

// A round that failed or rolled back still replaced the process that handed it
// off, so a re-echo of that command reports the recorded result. Handing off
// again would open a second round for a version the fleet has reported on.
func TestUpgradeExecuteReportsARoundItAlreadyHandedOff(t *testing.T) {
	t.Parallel()

	for _, status := range []string{upgrade.ResultRolledBack, upgrade.ResultFailed} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()

			client := &fakeHandoff{view: handedOffView(status, "v2.1")}
			handler, ctx := upgradeExecuteFor(t, client)

			result, err := handler.Run(ctx, upgradeParams())
			require.NoError(t, err)
			assert.Empty(t, client.calls)

			var reported upgrade.Result
			require.NoError(t, json.Unmarshal([]byte(result), &reported))
			assert.Equal(t, status, reported.Status)
		})
	}
}

// The result can land while this command is still retrying a conflict, and the
// heartbeat clears it as soon as it is reported. A retry into the cwa-updater
// left idle by that clearing would open a second round, so every attempt has to
// re-read the state rather than trusting the read it started with.
func TestUpgradeExecuteStopsRetryingOnceTheResultIsRecorded(t *testing.T) {
	t.Parallel()

	client := &fakeHandoff{
		failures:  10,
		failWith:  fmt.Errorf("%w: upgrade to v2.1 is already in_progress", upgrade.ErrConflict),
		view:      handedOffView(upgrade.ResultSucceeded, "v2.1"),
		viewAfter: 2,
	}
	handler, ctx := upgradeExecuteFor(t, client)

	result, err := handler.Run(ctx, upgradeParams())
	require.NoError(t, err)
	// Two conflicting attempts, then the recorded result settles it.
	assert.Len(t, client.calls, 2)

	var reported upgrade.Result
	require.NoError(t, json.Unmarshal([]byte(result), &reported))
	assert.Equal(t, "v2.1", reported.ToVersion)
}

// An execution_id is only absent outside a dispatch, and it must not match the
// zero entry of a round's map: that would report an unrelated agent's result.
func TestUpgradeExecuteWithoutAnExecutionIDDoesNotClaimARound(t *testing.T) {
	t.Parallel()

	view := completedView(upgrade.ResultRolledBack, "v2.1")
	view.AgentExecutions = map[string]string{}

	client := &fakeHandoff{view: view}
	handler := NewUpgradeExecute(client, func() string { return "agent-1" })
	handler.delay = 0

	_, err := handler.Run(context.Background(), upgradeParams())
	require.NoError(t, err)
	assert.Len(t, client.calls, 1)
}

func TestUpgradeExecuteHandsOffDespiteAnUnrelatedRecordedResult(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		view *upgrade.StatusView
	}{
		{
			name: "a different version succeeded",
			view: completedView(upgrade.ResultSucceeded, "v2.0"),
		},
		{
			// Nothing replaced this process, so it is a real attempt.
			name: "this version failed",
			view: completedView(upgrade.ResultFailed, "v2.1"),
		},
		{
			name: "this version rolled back",
			view: completedView(upgrade.ResultRolledBack, "v2.1"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			client := &fakeHandoff{view: tc.view}
			handler, ctx := upgradeExecuteFor(t, client)

			_, err := handler.Run(ctx, upgradeParams())
			require.NoError(t, err)
			assert.Len(t, client.calls, 1)
		})
	}
}

func TestUpgradeExecuteIsLongRunning(t *testing.T) {
	t.Parallel()

	handler, _ := upgradeExecuteFor(t, &fakeHandoff{})
	assert.Equal(t, LongRunning, handler.Timeout())
}

func TestDispatcherSuppliesTheExecutionIDToHandlers(t *testing.T) {
	t.Parallel()

	// upgrade.execute is only correlatable if the dispatcher passes this down.
	seen := make(chan string, 1)
	disp := NewDispatcher(newFakeSink(), nil, discardLogger())
	disp.Register("probe", &execIDHandler{seen: seen})

	disp.Dispatch(context.Background(), cmd("exec-42", "probe"))

	select {
	case id := <-seen:
		assert.Equal(t, "exec-42", id)
	case <-time.After(time.Second):
		t.Fatal("handler was never run")
	}
}

// execIDHandler reports the execution_id its context carried.
type execIDHandler struct {
	seen chan string
}

func (h *execIDHandler) Timeout() time.Duration { return Instant }

func (h *execIDHandler) Run(ctx context.Context, _ map[string]string) (string, error) {
	h.seen <- ExecutionIDFromContext(ctx)

	return "", nil
}

func TestExecutionIDFromContextOutsideADispatch(t *testing.T) {
	t.Parallel()

	assert.Empty(t, ExecutionIDFromContext(context.Background()))
}
