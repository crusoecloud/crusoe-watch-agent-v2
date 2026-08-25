package upgrade

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errStore = errors.New("store unavailable")

// memStore is an in-memory Store.
type memStore struct {
	state   *State
	cleared bool
}

func (m *memStore) Load(context.Context) (*State, error) {
	if m.state == nil {
		return nil, ErrNoState
	}

	return m.state.clone(), nil
}

func (m *memStore) Save(_ context.Context, state *State) error {
	m.state = state.clone()

	return nil
}

func (m *memStore) Clear(context.Context) error {
	m.state = nil
	m.cleared = true

	return nil
}

// failingStore fails every read.
type failingStore struct{}

func (failingStore) Load(context.Context) (*State, error) { return nil, errStore }
func (failingStore) Save(context.Context, *State) error   { return nil }
func (failingStore) Clear(context.Context) error          { return nil }

// corruptStore holds a record that cannot be parsed, until it is cleared.
type corruptStore struct {
	cleared bool
	saved   bool
}

func (c *corruptStore) Load(context.Context) (*State, error) {
	if c.cleared {
		return nil, ErrNoState
	}

	return nil, fmt.Errorf("%w: invalid character 'x'", ErrCorruptState)
}

func (c *corruptStore) Save(context.Context, *State) error {
	c.saved = true

	return nil
}

func (c *corruptStore) Clear(context.Context) error {
	c.cleared = true

	return nil
}

// savingFailsStore accepts reads but refuses to persist.
type savingFailsStore struct{ memStore }

func (s *savingFailsStore) Save(context.Context, *State) error { return errStore }

// fakeCounter reports a settable ready count.
type fakeCounter struct {
	ready int
	err   error
}

func (f *fakeCounter) ReadyCount(context.Context) (int, error) {
	if f.err != nil {
		return 0, f.err
	}

	return f.ready, nil
}

// fakeExecutor records that it ran and returns a canned result.
type fakeExecutor struct {
	calls  int
	result *Result
	err    error
}

func (f *fakeExecutor) Execute(context.Context, *State) (*Result, error) {
	f.calls++

	return f.result, f.err
}

func newTestService(store Store) *Service {
	return New(Config{Store: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

// newCollectService wires a service with a full collection window.
func newCollectService(store Store, counter ReadyCounter, executor Executor) *Service {
	return New(Config{
		Store:          store,
		Counter:        counter,
		Executor:       executor,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		AckTimeout:     15 * time.Minute,
		ScaleDownGrace: 30 * time.Second,
	})
}

func handoff(agentID, execID string) *Request {
	return &Request{
		ExecutionID:     execID,
		AgentID:         agentID,
		TargetVersion:   "v2.1.0",
		RollbackVersion: "v2.0.3",
	}
}

func terminalState() *State {
	return &State{
		Phase:           PhaseRolledBack,
		TargetVersion:   "v2.1.0",
		RollbackVersion: "v2.0.3",
		AgentExecutions: map[string]string{"agent-1": "cmd-1", "agent-2": "cmd-2"},
		ExpectedCount:   2,
		RequestedAt:     time.Now(),
		Result:          &Result{Status: ResultRolledBack, Reason: "health check never passed"},
	}
}

// ---------------------------------------------------------------------------
// Startup recovery
// ---------------------------------------------------------------------------

func TestRecoverIdle(t *testing.T) {
	svc := newTestService(&memStore{})

	require.NoError(t, svc.Recover(context.Background()))
	assert.Equal(t, StatusIdle, svc.HealthStatus())

	_, err := svc.Status()
	assert.ErrorIs(t, err, ErrNoState)
}

// A terminal result survives the restart and is served until acknowledged, so a
// cwa-manager that restarted before reporting it can still close the loop.
func TestRecoverTerminalServesResult(t *testing.T) {
	svc := newTestService(&memStore{state: terminalState()})

	require.NoError(t, svc.Recover(context.Background()))
	assert.Equal(t, StatusRolledBack, svc.HealthStatus())

	status, err := svc.Status()
	require.NoError(t, err)
	assert.Equal(t, StatusRolledBack, status.Status)
	assert.Equal(t, PhaseRolledBack, status.Phase)
	assert.Equal(t, "v2.1.0", status.TargetVersion)
	assert.Equal(t, "cmd-2", status.AgentExecutions["agent-2"])
	require.NotNil(t, status.Result)
	assert.Equal(t, ResultRolledBack, status.Result.Status)
}

// An upgrade still in flight when the process stopped is served as-is; resuming
// or rolling it back needs the executor.
func TestRecoverInFlightIsServed(t *testing.T) {
	svc := newTestService(&memStore{state: &State{
		Phase:           PhaseInProgress,
		TargetVersion:   "v2.1.0",
		RollbackVersion: "v2.0.3",
		AgentExecutions: map[string]string{"agent-1": "cmd-1"},
		ExpectedCount:   1,
	}})

	require.NoError(t, svc.Recover(context.Background()))
	assert.Equal(t, StatusInProgress, svc.HealthStatus())

	status, err := svc.Status()
	require.NoError(t, err)
	assert.Equal(t, PhaseInProgress, status.Phase)
	assert.Nil(t, status.Result)
}

// An updater that cannot reach its store must fail loudly rather than report idle
// and let the control plane dispatch an upgrade to it. The restart retries the
// read, which is what makes exiting the right answer here.
func TestRecoverStoreFailure(t *testing.T) {
	require.ErrorIs(t, newTestService(failingStore{}).Recover(context.Background()), errStore)
}

// A record that cannot be parsed must not be fatal: every restart would re-read
// the same bytes, so the crash loop could never clear itself.
func TestRecoverCorruptStateStartsAndReportsError(t *testing.T) {
	svc := newTestService(&corruptStore{})

	require.NoError(t, svc.Recover(context.Background()))
	assert.Equal(t, StatusError, svc.HealthStatus())

	_, err := svc.Status()
	require.ErrorIs(t, err, ErrCorruptState)
	// The parse failure reaches the caller, so the reason is not guesswork.
	assert.Contains(t, err.Error(), "invalid character 'x'")
}

// A phase this build does not recognise is corrupt too: it is neither terminal
// nor resumable, so serving it would wedge the updater with no way back to idle.
func TestRecoverUnknownPhaseIsCorrupt(t *testing.T) {
	svc := newTestService(&memStore{state: &State{Phase: "bogus", TargetVersion: "v2.1.0"}})

	require.NoError(t, svc.Recover(context.Background()))
	assert.Equal(t, StatusError, svc.HealthStatus())

	_, err := svc.Status()
	require.ErrorIs(t, err, ErrCorruptState)
	assert.Contains(t, err.Error(), "bogus")
}

// An unreadable record leaves s.state nil, but the record itself is still there
// and may hold an upgrade that was in flight. Opening a window over it would
// overwrite it and risk executing the same upgrade twice.
func TestAcceptRefusesWhileCorrupt(t *testing.T) {
	ctx := context.Background()
	store := &corruptStore{}
	svc := newCollectService(store, &fakeCounter{ready: 3}, &fakeExecutor{})
	require.NoError(t, svc.Recover(ctx))

	_, err := svc.Accept(ctx, handoff("agent-1", "cmd-1"))
	require.ErrorIs(t, err, ErrConflict)
	assert.ErrorIs(t, err, ErrCorruptState)
	assert.False(t, store.saved, "a refused handoff must not overwrite the record")

	// Clearing it is what makes the updater genuinely idle and able to collect again.
	require.NoError(t, svc.ClearStatus(ctx))

	accepted, err := svc.Accept(ctx, handoff("agent-1", "cmd-1"))
	require.NoError(t, err)
	assert.Equal(t, StatusAccepted, accepted.Status)
	assert.Equal(t, 3, accepted.ExpectedCount)
}

// DELETE /status is the self-service recovery path: it wipes the bad record and
// returns the updater to idle without anyone editing the ConfigMap by hand.
func TestClearStatusRecoversFromCorruptState(t *testing.T) {
	ctx := context.Background()
	store := &corruptStore{}
	svc := newTestService(store)
	require.NoError(t, svc.Recover(ctx))

	require.NoError(t, svc.ClearStatus(ctx))
	assert.True(t, store.cleared)
	assert.Equal(t, StatusIdle, svc.HealthStatus())

	_, err := svc.Status()
	assert.ErrorIs(t, err, ErrNoState)
}

// ---------------------------------------------------------------------------
// Status view
// ---------------------------------------------------------------------------

// The served view must not alias live state.
func TestStatusIsACopy(t *testing.T) {
	svc := newTestService(&memStore{state: terminalState()})
	require.NoError(t, svc.Recover(context.Background()))

	status, err := svc.Status()
	require.NoError(t, err)

	status.AgentExecutions["agent-3"] = "cmd-3"
	status.Result.Status = ResultSucceeded

	fresh, _ := svc.Status()
	assert.Len(t, fresh.AgentExecutions, 2)
	assert.Equal(t, ResultRolledBack, fresh.Result.Status)
}

// ---------------------------------------------------------------------------
// Status acknowledgement
// ---------------------------------------------------------------------------

func TestClearStatusClearsTerminalResult(t *testing.T) {
	ctx := context.Background()
	store := &memStore{state: terminalState()}
	svc := newTestService(store)
	require.NoError(t, svc.Recover(ctx))

	require.NoError(t, svc.ClearStatus(ctx))
	assert.True(t, store.cleared)
	assert.Equal(t, StatusIdle, svc.HealthStatus())

	_, err := svc.Status()
	assert.ErrorIs(t, err, ErrNoState)

	// Clearing an already-idle updater is a no-op, not an error.
	require.NoError(t, svc.ClearStatus(ctx))
}

// Clearing mid-upgrade would drop the record of a running upgrade.
func TestClearStatusRefusesWhileRunning(t *testing.T) {
	ctx := context.Background()
	store := &memStore{state: &State{Phase: PhaseInProgress, TargetVersion: "v2.1.0"}}
	svc := newTestService(store)
	require.NoError(t, svc.Recover(ctx))

	require.ErrorIs(t, svc.ClearStatus(ctx), ErrConflict)
	assert.False(t, store.cleared)
	assert.NotNil(t, store.state)
}

// ---------------------------------------------------------------------------
// Acceptance
// ---------------------------------------------------------------------------

// The first handoff opens the window and snapshots numberReady as the target.
func TestAcceptOpensCollectionWindow(t *testing.T) {
	ctx := context.Background()
	store := &memStore{}
	svc := newCollectService(store, &fakeCounter{ready: 3}, &fakeExecutor{})

	accepted, err := svc.Accept(ctx, handoff("agent-1", "cmd-1"))
	require.NoError(t, err)
	assert.Equal(t, StatusAccepted, accepted.Status)
	assert.Equal(t, PhasePending, accepted.Phase)
	assert.Equal(t, 1, accepted.CollectedCount)
	assert.Equal(t, 3, accepted.ExpectedCount)

	// Durable before the response: cwa-manager exits on this 200.
	require.NotNil(t, store.state)
	assert.Equal(t, PhasePending, store.state.Phase)
	assert.Equal(t, "cmd-1", store.state.AgentExecutions["agent-1"])
	assert.False(t, store.state.CollectDeadline.IsZero())
	assert.Equal(t, StatusInProgress, svc.HealthStatus())
}

func TestAcceptRejectsInvalidRequest(t *testing.T) {
	svc := newCollectService(&memStore{}, &fakeCounter{ready: 1}, &fakeExecutor{})

	req := handoff("agent-1", "")
	_, err := svc.Accept(context.Background(), req)
	require.ErrorIs(t, err, ErrInvalidRequest)

	req = handoff("", "cmd-1")
	_, err = svc.Accept(context.Background(), req)
	require.ErrorIs(t, err, ErrInvalidRequest)
}

// Every pod POSTs its own execution_id; a retry from one must not double-count.
func TestAcceptCollectsOnePerAgent(t *testing.T) {
	ctx := context.Background()
	svc := newCollectService(&memStore{}, &fakeCounter{ready: 3}, &fakeExecutor{})

	_, err := svc.Accept(ctx, handoff("agent-1", "cmd-1"))
	require.NoError(t, err)

	_, err = svc.Accept(ctx, handoff("agent-1", "cmd-1"))
	require.NoError(t, err)

	accepted, err := svc.Accept(ctx, handoff("agent-2", "cmd-2"))
	require.NoError(t, err)
	assert.Equal(t, 2, accepted.CollectedCount)
}

// A ready count below the agent that just handed off would close the window
// before its peers could POST.
func TestAcceptFloorsExpectedCountAtOne(t *testing.T) {
	svc := newCollectService(&memStore{}, &fakeCounter{ready: 0}, &fakeExecutor{})

	accepted, err := svc.Accept(context.Background(), handoff("agent-1", "cmd-1"))
	require.NoError(t, err)
	assert.Equal(t, 1, accepted.ExpectedCount)
}

func TestAcceptRejectsMismatchedTarget(t *testing.T) {
	ctx := context.Background()
	svc := newCollectService(&memStore{}, &fakeCounter{ready: 2}, &fakeExecutor{})

	_, err := svc.Accept(ctx, handoff("agent-1", "cmd-1"))
	require.NoError(t, err)

	other := handoff("agent-2", "cmd-2")
	other.TargetVersion = "v2.2.0"

	_, err = svc.Accept(ctx, other)
	require.ErrorIs(t, err, ErrConflict)
}

// An unacknowledged result must not be overwritten by a new round. agent-3 was
// never collected, so this is a fresh handoff rather than a retry.
func TestAcceptRefusesUnacknowledgedResult(t *testing.T) {
	ctx := context.Background()
	svc := newCollectService(&memStore{state: terminalState()}, &fakeCounter{ready: 1}, &fakeExecutor{})
	require.NoError(t, svc.Recover(ctx))

	_, err := svc.Accept(ctx, handoff("agent-3", "cmd-3"))
	require.ErrorIs(t, err, ErrConflict)
}

// Spec — Agent Architecture: "a duplicate POST for the same target_version while
// an upgrade is in progress returns 200 OK without starting a parallel upgrade".
// cwa-manager retries a POST whose response it lost, and it treats a non-200 as a
// failed upgrade, so a refusal here would fail a round that actually succeeded.
func TestAcceptDuplicateAfterWindowClosesSucceeds(t *testing.T) {
	ctx := context.Background()

	for phase, state := range map[Phase]*State{
		PhaseInProgress:  {Phase: PhaseInProgress, TargetVersion: "v2.1.0"},
		PhaseRollingBack: {Phase: PhaseRollingBack, TargetVersion: "v2.1.0"},
		PhaseComplete:    {Phase: PhaseComplete, TargetVersion: "v2.1.0", Result: &Result{Status: ResultSucceeded}},
	} {
		state.RollbackVersion = "v2.0.3"
		state.ExpectedCount = 2
		state.AgentExecutions = map[string]string{"agent-1": "cmd-1"}

		store := &memStore{state: state}
		svc := newCollectService(store, &fakeCounter{ready: 2}, &fakeExecutor{})
		require.NoError(t, svc.Recover(ctx))

		accepted, err := svc.Accept(ctx, handoff("agent-1", "cmd-1"))
		require.NoError(t, err, phase)
		assert.Equal(t, StatusAccepted, accepted.Status, phase)
		assert.Equal(t, phase, accepted.Phase, phase)

		// Answered from state, so a retry never rewrites a closed round.
		assert.Equal(t, phase, store.state.Phase, phase)
		assert.Len(t, store.state.AgentExecutions, 1, phase)

		// An agent that missed the window is still refused: telling it "accepted"
		// would let it exit with no execution_id to recover.
		_, err = svc.Accept(ctx, handoff("agent-2", "cmd-2"))
		require.ErrorIs(t, err, ErrConflict, phase)
	}
}

// Same agent, new execution_id: that is a re-dispatch, not a retry, and the
// closed window cannot take it.
func TestAcceptRefusesNewExecutionAfterWindowCloses(t *testing.T) {
	ctx := context.Background()
	svc := newCollectService(&memStore{state: &State{
		Phase:           PhaseInProgress,
		TargetVersion:   "v2.1.0",
		RollbackVersion: "v2.0.3",
		AgentExecutions: map[string]string{"agent-1": "cmd-1"},
		ExpectedCount:   1,
	}}, &fakeCounter{ready: 1}, &fakeExecutor{})
	require.NoError(t, svc.Recover(ctx))

	_, err := svc.Accept(ctx, handoff("agent-1", "cmd-2"))
	require.ErrorIs(t, err, ErrConflict)
}

// A handoff that could not be persisted must not be reported as accepted.
func TestAcceptFailsWhenStoreRefuses(t *testing.T) {
	svc := newCollectService(&savingFailsStore{}, &fakeCounter{ready: 1}, &fakeExecutor{})

	_, err := svc.Accept(context.Background(), handoff("agent-1", "cmd-1"))
	require.ErrorIs(t, err, errStore)
	assert.Equal(t, StatusIdle, svc.HealthStatus())
}

// ---------------------------------------------------------------------------
// Collection window
// ---------------------------------------------------------------------------

// Every expected agent handed off, so the upgrade runs behind an in_progress
// write and its result is held for acknowledgement.
func TestCollectionCompleteExecutes(t *testing.T) {
	ctx := context.Background()
	store := &memStore{}
	executor := &fakeExecutor{result: &Result{
		Status: ResultSucceeded, FromVersion: "v2.0.3", ToVersion: "v2.1.0",
	}}
	svc := newCollectService(store, &fakeCounter{ready: 2}, executor)

	_, err := svc.Accept(ctx, handoff("agent-1", "cmd-1"))
	require.NoError(t, err)
	_, err = svc.Accept(ctx, handoff("agent-2", "cmd-2"))
	require.NoError(t, err)

	svc.collectTick(ctx)

	assert.Equal(t, 1, executor.calls)
	assert.Equal(t, PhaseComplete, store.state.Phase)
	require.NotNil(t, store.state.Result)
	assert.Equal(t, ResultSucceeded, store.state.Result.Status)
	// A successful upgrade reports idle; the result is still served on /status.
	assert.Equal(t, StatusIdle, svc.HealthStatus())
}

// Nothing runs until the last agent has handed off.
func TestCollectionWaitsForEveryAgent(t *testing.T) {
	ctx := context.Background()
	executor := &fakeExecutor{result: &Result{Status: ResultSucceeded}}
	svc := newCollectService(&memStore{}, &fakeCounter{ready: 3}, executor)

	_, err := svc.Accept(ctx, handoff("agent-1", "cmd-1"))
	require.NoError(t, err)

	svc.collectTick(ctx)

	assert.Equal(t, 0, executor.calls)
	assert.Equal(t, StatusInProgress, svc.HealthStatus())
}

// Pods that become ready after the snapshot were never dispatched this round,
// so they must not raise the bar.
func TestCollectionIgnoresScaleUp(t *testing.T) {
	ctx := context.Background()
	counter := &fakeCounter{ready: 1}
	executor := &fakeExecutor{result: &Result{Status: ResultSucceeded}}
	svc := newCollectService(&memStore{}, counter, executor)

	_, err := svc.Accept(ctx, handoff("agent-1", "cmd-1"))
	require.NoError(t, err)

	counter.ready = 5
	svc.collectTick(ctx)

	assert.Equal(t, 1, executor.calls)
}

// A drop only counts once it has held for the grace period, so a pod bouncing
// through NotReady does not shrink the round.
func TestCollectionScaleDownNeedsGrace(t *testing.T) {
	ctx := context.Background()
	store := &memStore{}
	counter := &fakeCounter{ready: 3}
	executor := &fakeExecutor{result: &Result{Status: ResultSucceeded}}
	svc := newCollectService(store, counter, executor)

	_, err := svc.Accept(ctx, handoff("agent-1", "cmd-1"))
	require.NoError(t, err)
	_, err = svc.Accept(ctx, handoff("agent-2", "cmd-2"))
	require.NoError(t, err)

	// Third pod goes NotReady, but the drop is still inside the grace window.
	counter.ready = 2
	svc.collectTick(ctx)
	assert.Equal(t, 3, store.state.ExpectedCount)
	assert.Equal(t, 0, executor.calls)

	// It bounces to a different value: the clock restarts.
	counter.ready = 1
	svc.collectTick(ctx)
	assert.Equal(t, 3, store.state.ExpectedCount)

	// Back to a steady 2 that outlives the grace period.
	counter.ready = 2
	svc.collectTick(ctx)
	svc.readyFloorSince = time.Now().Add(-time.Hour)
	svc.collectTick(ctx)

	assert.Equal(t, 2, store.state.ExpectedCount)
	assert.Equal(t, 1, executor.calls)
}

// The window is bounded: an agent that never POSTs must not wedge the round.
func TestCollectionTimesOut(t *testing.T) {
	ctx := context.Background()
	store := &memStore{}
	executor := &fakeExecutor{result: &Result{Status: ResultSucceeded}}
	svc := newCollectService(store, &fakeCounter{ready: 3}, executor)

	_, err := svc.Accept(ctx, handoff("agent-1", "cmd-1"))
	require.NoError(t, err)

	svc.state.CollectDeadline = time.Now().Add(-time.Minute)
	svc.collectTick(ctx)

	assert.Equal(t, 0, executor.calls)
	// The ConfigMap records why nothing was installed.
	assert.Equal(t, PhaseCollectionTimeout, store.state.Phase)
	require.NotNil(t, store.state.Result)
	assert.Equal(t, ResultFailed, store.state.Result.Status)
	assert.Contains(t, store.state.Result.Reason, "1 of 3")
	// The control plane sees a failed round it can reschedule.
	assert.Equal(t, StatusFailed, svc.HealthStatus())

	// Held until acknowledged, then the updater is free for the next round.
	require.NoError(t, svc.ClearStatus(ctx))
	assert.Equal(t, StatusIdle, svc.HealthStatus())
}

// An executor failure is still a result the agents can acknowledge.
func TestCollectionExecutorFailureIsTerminal(t *testing.T) {
	ctx := context.Background()
	store := &memStore{}
	svc := newCollectService(store, &fakeCounter{ready: 1}, &fakeExecutor{err: errStore})

	_, err := svc.Accept(ctx, handoff("agent-1", "cmd-1"))
	require.NoError(t, err)

	svc.collectTick(ctx)

	assert.Equal(t, PhaseFailed, store.state.Phase)
	require.NotNil(t, store.state.Result)
	assert.Equal(t, ResultFailed, store.state.Result.Status)
	assert.Equal(t, StatusFailed, svc.HealthStatus())
}

// The stand-in executor terminates the round rather than leaving it in_progress.
func TestUnimplementedExecutorReportsFailure(t *testing.T) {
	result, err := UnimplementedExecutor{}.Execute(context.Background(), &State{
		TargetVersion: "v2.1.0", RollbackVersion: "v2.0.3",
	})
	require.NoError(t, err)
	assert.Equal(t, ResultFailed, result.Status)
	assert.Equal(t, "v2.1.0", result.ToVersion)
}

// An idle updater must not poll the API server.
func TestCollectTickIdleDoesNotQuery(t *testing.T) {
	counter := &fakeCounter{err: errStore}
	svc := newCollectService(&memStore{}, counter, &fakeExecutor{})

	svc.collectTick(context.Background())

	assert.Equal(t, StatusIdle, svc.HealthStatus())
}

// A window left open by a restart is picked back up.
func TestCollectionResumesAfterRestart(t *testing.T) {
	ctx := context.Background()
	store := &memStore{state: &State{
		Phase:           PhasePending,
		TargetVersion:   "v2.1.0",
		RollbackVersion: "v2.0.3",
		AgentExecutions: map[string]string{"agent-1": "cmd-1", "agent-2": "cmd-2"},
		ExpectedCount:   2,
		CollectDeadline: time.Now().Add(time.Minute),
	}}
	executor := &fakeExecutor{result: &Result{Status: ResultSucceeded}}
	svc := newCollectService(store, &fakeCounter{ready: 2}, executor)

	require.NoError(t, svc.Recover(ctx))
	svc.collectTick(ctx)

	assert.Equal(t, 1, executor.calls)
	assert.Equal(t, PhaseComplete, store.state.Phase)
}
