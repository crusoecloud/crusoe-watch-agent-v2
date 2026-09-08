package heartbeat

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/upgrade"
	pb "gitlab.com/crusoeenergy/schemas/api/island/v2/observability"
)

// fakeUpdater serves a scripted /status and counts acknowledgements.
type fakeUpdater struct {
	view    *upgrade.StatusView
	err     error
	polls   int
	acks    int
	ackErr  error
	ackErrs int // fail this many acknowledgements before succeeding
}

func (f *fakeUpdater) Status(context.Context) (*upgrade.StatusView, error) {
	f.polls++

	if f.err != nil {
		return nil, f.err
	}

	return f.view, nil
}

func (f *fakeUpdater) Acknowledge(context.Context) error {
	f.acks++

	if f.acks <= f.ackErrs {
		return f.ackErr
	}

	return nil
}

func reporterFor(client UpdaterStatus) *upgradeReporter {
	return &upgradeReporter{
		client: client,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// rolledBackView is a terminal state cwa-updater is holding for a manager.
func rolledBackView() *upgrade.StatusView {
	return &upgrade.StatusView{
		Status:          upgrade.StatusRolledBack,
		Phase:           upgrade.PhaseRolledBack,
		TargetVersion:   "v2.1",
		RollbackVersion: "v2.0",
		AgentExecutions: map[string]string{"agent-1": "exec-1"},
		ExpectedCount:   1,
		Result: &upgrade.Result{
			Status:      upgrade.ResultRolledBack,
			FromVersion: "v2.0",
			ToVersion:   "v2.1",
			Reason:      "agents did not come back healthy",
			CompletedAt: time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC),
		},
	}
}

func TestReporterReportsATerminalResult(t *testing.T) {
	t.Parallel()

	client := &fakeUpdater{view: rolledBackView()}
	reporter := reporterFor(client)

	result := reporter.Result(context.Background())
	require.NotNil(t, result)

	assert.Equal(t, pb.CwaUpgradeStatus_CWA_UPGRADE_STATUS_ROLLED_BACK, result.GetStatus())
	assert.Equal(t, "v2.0", result.GetFromVersion())
	assert.Equal(t, "v2.1", result.GetToVersion())
	assert.Equal(t, "agents did not come back healthy", result.GetReason())
	assert.True(t, rolledBackView().Result.CompletedAt.Equal(result.GetTimestamp().AsTime()))
}

func TestReporterMapsEachResultStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status string
		want   pb.CwaUpgradeStatus
	}{
		{"succeeded", upgrade.ResultSucceeded, pb.CwaUpgradeStatus_CWA_UPGRADE_STATUS_SUCCEEDED},
		{"rolled back", upgrade.ResultRolledBack, pb.CwaUpgradeStatus_CWA_UPGRADE_STATUS_ROLLED_BACK},
		{"failed", upgrade.ResultFailed, pb.CwaUpgradeStatus_CWA_UPGRADE_STATUS_FAILED},
		// A status this build does not recognise must not read as success.
		{"unrecognised", "something-new", pb.CwaUpgradeStatus_CWA_UPGRADE_STATUS_FAILED},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			view := rolledBackView()
			view.Result.Status = tc.status

			result := reporterFor(&fakeUpdater{view: view}).Result(context.Background())
			require.NotNil(t, result)
			assert.Equal(t, tc.want, result.GetStatus())
		})
	}
}

func TestReporterReportsNothingWhenIdle(t *testing.T) {
	t.Parallel()

	client := &fakeUpdater{err: upgrade.ErrNoState}
	reporter := reporterFor(client)

	assert.Nil(t, reporter.Result(context.Background()))
	assert.False(t, reporter.InProgress())
}

func TestReporterReportsNothingWhileTheUpgradeIsStillRunning(t *testing.T) {
	t.Parallel()

	client := &fakeUpdater{view: &upgrade.StatusView{
		Status:        upgrade.StatusInProgress,
		Phase:         upgrade.PhaseInProgress,
		TargetVersion: "v2.1",
	}}
	reporter := reporterFor(client)

	assert.Nil(t, reporter.Result(context.Background()))
	// A non-terminal phase is what upgrade-in-progress is derived from.
	assert.True(t, reporter.InProgress())
}

func TestReporterSurvivesAnUnreachableUpdater(t *testing.T) {
	t.Parallel()

	client := &fakeUpdater{err: errors.New("connection refused")}
	reporter := reporterFor(client)

	assert.Nil(t, reporter.Result(context.Background()))
	assert.False(t, reporter.InProgress())
}

func TestReporterAcknowledgesOnlyAfterASecondHeartbeatIsSent(t *testing.T) {
	t.Parallel()

	client := &fakeUpdater{view: rolledBackView()}
	reporter := reporterFor(client)
	reporter.StreamOpened()

	require.NotNil(t, reporter.Result(context.Background()))
	// Reading the result must not clear it: the heartbeat has not gone out yet.
	assert.Equal(t, 0, client.acks)

	reporter.Sent(context.Background())
	// A send only buffers, so the first one is not evidence the coordinator has it.
	assert.Equal(t, 0, client.acks)
	require.NotNil(t, reporter.Result(context.Background()))

	// The next send on the same stream shows the first one went out.
	reporter.Sent(context.Background())
	assert.Equal(t, 1, client.acks)

	// Once acknowledged there is nothing left to report.
	client.view = nil
	client.err = upgrade.ErrNoState
	assert.Nil(t, reporter.Result(context.Background()))
}

func TestReporterKeepsTheResultWhenTheStreamDiesAfterOneSend(t *testing.T) {
	t.Parallel()

	client := &fakeUpdater{view: rolledBackView()}
	reporter := reporterFor(client)
	reporter.StreamOpened()

	require.NotNil(t, reporter.Result(context.Background()))
	reporter.Sent(context.Background())

	// The stream broke with the heartbeat still buffered, so the result has to be
	// reported again before cwa-updater may drop it.
	reporter.StreamOpened()
	require.NotNil(t, reporter.Result(context.Background()))
	reporter.Sent(context.Background())
	assert.Equal(t, 0, client.acks)

	reporter.Sent(context.Background())
	assert.Equal(t, 1, client.acks)
}

func TestReporterKeepsReportingUntilTheAcknowledgementLands(t *testing.T) {
	t.Parallel()

	client := &fakeUpdater{
		view:    rolledBackView(),
		ackErrs: 1,
		ackErr:  errors.New("connection refused"),
	}
	reporter := reporterFor(client)
	reporter.StreamOpened()

	require.NotNil(t, reporter.Result(context.Background()))
	reporter.Sent(context.Background())
	reporter.Sent(context.Background())

	// The acknowledgement failed, so the result rides the next heartbeat too.
	require.NotNil(t, reporter.Result(context.Background()))
	reporter.Sent(context.Background())
	assert.Equal(t, 2, client.acks)
}

func TestReporterDoesNotAcknowledgeWithNothingReported(t *testing.T) {
	t.Parallel()

	client := &fakeUpdater{err: upgrade.ErrNoState}
	reporter := reporterFor(client)

	require.Nil(t, reporter.Result(context.Background()))
	reporter.Sent(context.Background())
	reporter.Sent(context.Background())

	// Clearing state nobody reported would drop the next round's result.
	assert.Equal(t, 0, client.acks)
}

func TestReporterThrottlesPollingBetweenHeartbeats(t *testing.T) {
	t.Parallel()

	client := &fakeUpdater{err: upgrade.ErrNoState}
	reporter := reporterFor(client)

	for range statusPollTicks {
		reporter.Result(context.Background())
	}

	// The first heartbeat polls (a restart may be a completed upgrade), then one
	// more when the interval comes round; the ticks in between do not.
	assert.Equal(t, 2, client.polls)
}

func TestReporterStopsPollingWhileAResultIsPending(t *testing.T) {
	t.Parallel()

	client := &fakeUpdater{view: rolledBackView()}
	reporter := reporterFor(client)

	for range statusPollTicks * 2 {
		require.NotNil(t, reporter.Result(context.Background()))
	}

	assert.Equal(t, 1, client.polls)
}

func TestReporterReportsUpgradingAfterAHandoff(t *testing.T) {
	t.Parallel()

	client := &fakeUpdater{err: upgrade.ErrNoState}
	reporter := reporterFor(client)

	require.False(t, reporter.InProgress())

	reporter.MarkHandedOff("v2.1")
	assert.True(t, reporter.InProgress())
}

func TestNilReporterIsANoOp(t *testing.T) {
	t.Parallel()

	// Targets with no cwa-updater wired leave the field nil.
	var reporter *upgradeReporter

	assert.Nil(t, reporter.Result(context.Background()))
	assert.False(t, reporter.InProgress())
	reporter.MarkHandedOff("v2.1")
	reporter.StreamOpened()
	reporter.Sent(context.Background())
}

func TestCapabilitiesAdvertiseUpgradeOnlyWhereTheUpdaterRuns(t *testing.T) {
	t.Parallel()

	assert.Contains(t, capabilities(pb.CwaInstallType_CWA_INSTALL_TYPE_KUBERNETES), "auto_upgrade")
	// No cwa-updater is deployed on VM targets yet, so claiming the capability
	// would invite an upgrade nothing can carry out.
	assert.NotContains(t, capabilities(pb.CwaInstallType_CWA_INSTALL_TYPE_SYSTEMD), "auto_upgrade")
	assert.NotContains(t, capabilities(pb.CwaInstallType_CWA_INSTALL_TYPE_DOCKER), "auto_upgrade")
}

func TestReporterSkipsCwaUpdaterOnTheShutdownFlush(t *testing.T) {
	t.Parallel()

	client := &fakeUpdater{view: rolledBackView()}
	reporter := reporterFor(client)
	reporter.StreamOpened()

	// The real shutdown window, so the guard is checked against the grace the
	// final heartbeat actually gets rather than an arbitrarily short one.
	ctx, cancel := context.WithTimeout(context.Background(), heartbeatFlushGrace)
	defer cancel()

	assert.Nil(t, reporter.Result(ctx))
	assert.Equal(t, 0, client.polls)

	// A result carried by an earlier heartbeat: the flush must not spend its
	// window acknowledging that either, so cwa-updater keeps it for the next
	// process to report.
	require.NotNil(t, reporter.Result(context.Background()))
	reporter.Sent(context.Background())
	reporter.Sent(ctx)
	assert.Equal(t, 0, client.acks)
}

func TestReporterPollsEveryHeartbeatWhileAnUpgradeIsInFlight(t *testing.T) {
	t.Parallel()

	client := &fakeUpdater{view: &upgrade.StatusView{
		Status:        upgrade.StatusInProgress,
		Phase:         upgrade.PhaseInProgress,
		TargetVersion: "v2.1",
	}}
	reporter := reporterFor(client)

	// The replacement manager's first heartbeat lands while helm is still waiting
	// on it, so the throttle must not hold the result back once it is written.
	for range statusPollTicks {
		require.Nil(t, reporter.Result(context.Background()))
	}

	assert.Equal(t, statusPollTicks, client.polls)

	// The tick the result lands on reports it, rather than one minutes later.
	client.view = rolledBackView()
	require.NotNil(t, reporter.Result(context.Background()))
}

func TestReporterOmitsAnUnsetTimestamp(t *testing.T) {
	t.Parallel()

	view := rolledBackView()
	view.Result.CompletedAt = time.Time{}

	result := reporterFor(&fakeUpdater{view: view}).Result(context.Background())
	require.NotNil(t, result)
	assert.Nil(t, result.GetTimestamp())
}
