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
}

func (c *corruptStore) Load(context.Context) (*State, error) {
	if c.cleared {
		return nil, ErrNoState
	}

	return nil, fmt.Errorf("%w: invalid character 'x'", ErrCorruptState)
}

func (c *corruptStore) Save(context.Context, *State) error { return nil }

func (c *corruptStore) Clear(context.Context) error {
	c.cleared = true

	return nil
}

func newTestService(store Store) *Service {
	return New(Config{Store: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
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
