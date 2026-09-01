package upgrade

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errHelm = errors.New("helm exited 1")

// fakeRunner records every invocation and answers from canned per-subcommand
// responses, keyed by the first two args (e.g. "upgrade", "get metadata").
type fakeRunner struct {
	calls [][]string
	out   map[string][]byte
	err   map[string]error
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{out: map[string][]byte{}, err: map[string]error{}}
}

func (f *fakeRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)

	if len(args) == 0 {
		return nil, nil
	}

	// Most specific key first, so "upgrade" can be canned for every upgrade call
	// while "get metadata" stays distinct from a bare "get".
	for _, key := range []string{strings.Join(args[:min(2, len(args))], " "), args[0]} {
		if err, ok := f.err[key]; ok {
			return f.out[key], err
		}

		if out, ok := f.out[key]; ok {
			return out, nil
		}
	}

	return nil, nil
}

// ran reports the recorded invocation whose first arg matches subcommand.
func (f *fakeRunner) ran(subcommand string) ([]string, bool) {
	for _, call := range f.calls {
		if len(call) > 0 && call[0] == subcommand {
			return call, true
		}
	}

	return nil, false
}

// fakeVerifier stands in for the post-upgrade health check.
type fakeVerifier struct {
	calls int
	err   error
}

func (f *fakeVerifier) Verify(context.Context) error {
	f.calls++

	return f.err
}

func newHelmExecutor(runner Runner, verifier Verifier) *HelmExecutor {
	return NewHelmExecutor(HelmConfig{
		Runner:         runner,
		Verifier:       verifier,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Namespace:      "crusoe-system",
		Release:        "crusoe-watch-agent",
		ChartRepo:      "oci://ghcr.io/crusoecloud/crusoe-watch-agent-v2/charts",
		ChartName:      "crusoe-watch-agent",
		RollbackWindow: 15 * time.Minute,
	})
}

func upgradeState() *State {
	return &State{Phase: PhaseInProgress, TargetVersion: "v2.1.0", RollbackVersion: "v2.0.3"}
}

// ---------------------------------------------------------------------------
// Upgrade
// ---------------------------------------------------------------------------

func TestHelmUpgradeArgs(t *testing.T) {
	runner := newFakeRunner()
	verifier := &fakeVerifier{}

	require.NoError(t, newHelmExecutor(runner, verifier).Upgrade(context.Background(), upgradeState()))

	call, ok := runner.ran("upgrade")
	require.True(t, ok, "helm upgrade must run")

	joined := strings.Join(call, " ")
	assert.Contains(t, joined, "upgrade crusoe-watch-agent "+
		"oci://ghcr.io/crusoecloud/crusoe-watch-agent-v2/charts/crusoe-watch-agent")
	assert.Contains(t, joined, "--namespace crusoe-system")
	assert.Contains(t, joined, "--wait")
	assert.Equal(t, 1, verifier.calls, "pod health must be verified after the rollout")

	// The CMK add-on installs; cwa-updater only upgrades. --install would
	// half-succeed, since it holds no create permission on cluster-scoped resources.
	assert.NotContains(t, call, "--install")
}

// The pipeline publishes chart versions and OCI tags with the leading v
// stripped, so passing target_version through unchanged would never resolve.
func TestHelmUpgradeStripsVersionPrefix(t *testing.T) {
	runner := newFakeRunner()

	require.NoError(t, newHelmExecutor(runner, nil).Upgrade(context.Background(), upgradeState()))

	call, _ := runner.ran("upgrade")
	assert.Contains(t, strings.Join(call, " "), "--version 2.1.0")
}

// Without this the release falls back to the new chart's defaults and the agent
// comes back with the customer's configuration silently dropped.
func TestHelmUpgradePreservesReleaseValues(t *testing.T) {
	runner := newFakeRunner()

	require.NoError(t, newHelmExecutor(runner, nil).Upgrade(context.Background(), upgradeState()))

	call, _ := runner.ran("upgrade")
	assert.Contains(t, call, "--reset-then-reuse-values")
}

func TestHelmUpgradeFailsOnHelmError(t *testing.T) {
	runner := newFakeRunner()
	runner.err["upgrade"] = errHelm
	verifier := &fakeVerifier{}

	err := newHelmExecutor(runner, verifier).Upgrade(context.Background(), upgradeState())
	require.ErrorIs(t, err, errHelm)
	assert.Equal(t, 0, verifier.calls, "a failed rollout must not be verified")
}

// `--wait` only proves the readiness probe passed. An agent that never reaches
// the control plane cannot report the result, so the round has to fail.
func TestHelmUpgradeFailsWhenAgentsNeverReport(t *testing.T) {
	runner := newFakeRunner()

	err := newHelmExecutor(runner, &fakeVerifier{err: errAgentUnhealthy}).
		Upgrade(context.Background(), upgradeState())
	require.ErrorIs(t, err, errAgentUnhealthy)
}

// ---------------------------------------------------------------------------
// Rollback
// ---------------------------------------------------------------------------

func TestHelmRollbackRestoresPreviousRevision(t *testing.T) {
	runner := newFakeRunner()
	runner.out["get metadata"] = []byte(`{"chart":"crusoe-watch-agent-2.1.0","version":"2.1.0"}`)

	require.NoError(t, newHelmExecutor(runner, nil).Rollback(context.Background(), upgradeState()))

	call, ok := runner.ran("rollback")
	require.True(t, ok, "helm rollback must run when the target is deployed")

	joined := strings.Join(call, " ")
	assert.Contains(t, joined, "rollback crusoe-watch-agent")
	assert.Contains(t, joined, "--namespace crusoe-system")
	// No revision argument: helm restores the immediately previous revision.
	assert.NotContains(t, joined, "--revision")
}

// A round that failed before helm mutated the release — a chart pull failure, or
// a crash between the in_progress write and the helm call — must not roll a
// healthy release back a revision it was never moved off.
func TestHelmRollbackSkipsWhenTargetNeverDeployed(t *testing.T) {
	runner := newFakeRunner()
	runner.out["get metadata"] = []byte(`{"chart":"crusoe-watch-agent-2.0.3","version":"2.0.3"}`)

	require.NoError(t, newHelmExecutor(runner, nil).Rollback(context.Background(), upgradeState()))

	_, ok := runner.ran("rollback")
	assert.False(t, ok, "a release still on rollback_version needs no rollback")
}

func TestHelmRollbackSkipsWhenReleaseAbsent(t *testing.T) {
	runner := newFakeRunner()
	runner.out["get metadata"] = []byte("Error: release: not found")
	runner.err["get metadata"] = errHelm

	require.NoError(t, newHelmExecutor(runner, nil).Rollback(context.Background(), upgradeState()))

	_, ok := runner.ran("rollback")
	assert.False(t, ok)
}

// An unreadable release state is not the same as "nothing to roll back":
// guessing here could downgrade a healthy release, so it must surface as failed.
func TestHelmRollbackFailsWhenReleaseStateUnreadable(t *testing.T) {
	runner := newFakeRunner()
	runner.out["get metadata"] = []byte("Error: Kubernetes cluster unreachable")
	runner.err["get metadata"] = errHelm

	err := newHelmExecutor(runner, nil).Rollback(context.Background(), upgradeState())
	require.ErrorIs(t, err, errHelm)

	_, ok := runner.ran("rollback")
	assert.False(t, ok)
}

func TestHelmRollbackFailsOnHelmError(t *testing.T) {
	runner := newFakeRunner()
	runner.out["get metadata"] = []byte(`{"version":"2.1.0"}`)
	runner.err["rollback"] = errHelm

	require.ErrorIs(t,
		newHelmExecutor(runner, nil).Rollback(context.Background(), upgradeState()), errHelm)
}

// ---------------------------------------------------------------------------
// Window arithmetic
// ---------------------------------------------------------------------------

// The rollout and the health check share the rollback window, so helm must be
// handed what is left of it rather than the full window each time.
func TestRemainingTracksDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	left := remaining(ctx)
	assert.Positive(t, left)
	assert.LessOrEqual(t, left, 10*time.Minute)
	// Whole seconds: the value reaches the customer-visible upgrade reason.
	assert.Zero(t, left%time.Second)

	// A window already spent still yields a usable timeout: helm reads a
	// non-positive one as "no timeout", which would hang the round forever.
	expired, cancelExpired := context.WithTimeout(context.Background(), -time.Second)
	defer cancelExpired()
	assert.Equal(t, time.Second, remaining(expired))

	assert.Equal(t, defaultRollbackWindow, remaining(context.Background()))
}

// The reason reaches the customer-facing upgrade audit log, so helm's progress
// chatter has to be stripped down to what actually failed.
func TestHelmMessageKeepsOnlyTheError(t *testing.T) {
	out := []byte("Pulled: ghcr.io/example/charts/agent:2.0\n" +
		"Digest: sha256:abc123\n" +
		"Error: UPGRADE FAILED: cannot patch \"crusoe-monitoring\"")

	assert.Equal(t, `Error: UPGRADE FAILED: cannot patch "crusoe-monitoring"`, helmMessage(out))

	// Output with no Error: line is reported as-is rather than discarded.
	assert.Equal(t, "something unexpected", helmMessage([]byte("  something unexpected\n")))
}

func TestChartRefTrimsRepoSlash(t *testing.T) {
	executor := NewHelmExecutor(HelmConfig{
		ChartRepo: "oci://mirror.internal/charts/",
		ChartName: "crusoe-watch-agent",
	})

	assert.Equal(t, "oci://mirror.internal/charts/crusoe-watch-agent", executor.chartRef())
}
