package upgrade

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
	// failures[key] is how many more times that subcommand fails before it
	// starts succeeding, for exercising the retries.
	failures map[string]int
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{out: map[string][]byte{}, err: map[string]error{}, failures: map[string]int{}}
}

func (f *fakeRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)

	if len(args) == 0 {
		return nil, nil
	}

	// Most specific key first, so "upgrade" can be canned for every upgrade call
	// while "get metadata" stays distinct from a bare "get".
	for _, key := range []string{strings.Join(args[:min(2, len(args))], " "), args[0]} {
		if left := f.failures[key]; left > 0 {
			f.failures[key] = left - 1

			return nil, errHelm
		}

		if err, ok := f.err[key]; ok {
			return f.out[key], err
		}

		if out, ok := f.out[key]; ok {
			return out, nil
		}
	}

	return nil, nil
}

// ranSubcommands is the first argument of every recorded call, in order.
func (f *fakeRunner) ranSubcommands() []string {
	subcommands := make([]string, 0, len(f.calls))

	for _, call := range f.calls {
		if len(call) > 0 {
			subcommands = append(subcommands, call[0])
		}
	}

	return subcommands
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

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newHelmExecutor builds an executor whose signing key exists on disk and whose
// retries do not sleep. One fake runner backs both helm and cosign, so the
// recorded calls show the real order across the two binaries.
func newHelmExecutor(t *testing.T, runner Runner, verifier Verifier) *HelmExecutor {
	t.Helper()

	executor := NewHelmExecutor(HelmConfig{
		Runner:         runner,
		Cosign:         runner,
		Verifier:       verifier,
		Logger:         discardLogger(),
		Namespace:      "crusoe-system",
		Release:        "crusoe-watch-agent",
		ChartRepo:      "oci://ghcr.io/crusoecloud/crusoe-watch-agent-v2/charts",
		ChartName:      "crusoe-watch-agent",
		CosignKey:      writeCosignKey(t),
		RollbackWindow: 15 * time.Minute,
	})

	executor.backoff = backoff{attempts: 3, delay: time.Millisecond}

	return executor
}

// writeCosignKey stands in for the public key the image bakes in.
func writeCosignKey(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "cosign.pub")
	require.NoError(t, os.WriteFile(path, []byte("-----BEGIN PUBLIC KEY-----\n"), 0o600))

	return path
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

	require.NoError(t, newHelmExecutor(t, runner, verifier).Upgrade(context.Background(), upgradeState()))

	call, ok := runner.ran("upgrade")
	require.True(t, ok, "helm upgrade must run")

	joined := strings.Join(call, " ")
	assert.Contains(t, joined, "upgrade crusoe-watch-agent ")
	assert.Contains(t, joined, "--namespace crusoe-system")
	assert.Contains(t, joined, "--wait")
	assert.Equal(t, 1, verifier.calls, "pod health must be verified after the rollout")

	// The CMK add-on installs; cwa-updater only upgrades. --install would
	// half-succeed, since it holds no create permission on cluster-scoped resources.
	assert.NotContains(t, call, "--install")
}

// Verification comes first so an unsigned chart is never downloaded, and the
// apply reads what was downloaded rather than reaching for the registry again.
func TestHelmUpgradeVerifiesThenDownloadsThenApplies(t *testing.T) {
	runner := newFakeRunner()

	require.NoError(t, newHelmExecutor(t, runner, nil).Upgrade(context.Background(), upgradeState()))

	assert.Equal(t, []string{"verify", "pull", "upgrade"}, runner.ranSubcommands())

	verify, _ := runner.ran("verify")
	assert.Contains(t, strings.Join(verify, " "),
		"ghcr.io/crusoecloud/crusoe-watch-agent-v2/charts/crusoe-watch-agent:2.1.0")
	// cosign takes a bare registry reference; the oci:// scheme is helm's.
	assert.NotContains(t, strings.Join(verify, " "), ociScheme)
	// Transparency-log verification is what makes a leaked signing key
	// detectable, so it stays on.
	assert.NotContains(t, verify, "--insecure-ignore-tlog")

	applied, _ := runner.ran("upgrade")
	assert.Contains(t, strings.Join(applied, " "), "crusoe-watch-agent-2.1.0.tgz")
	assert.NotContains(t, strings.Join(applied, " "), ociScheme,
		"the apply must read the verified download, not re-resolve the tag")
}

// A chart that fails the signature check must not be downloaded, let alone applied.
func TestHelmUpgradeStopsWhenSignatureFails(t *testing.T) {
	runner := newFakeRunner()
	runner.err["verify"] = errHelm

	err := newHelmExecutor(t, runner, nil).Upgrade(context.Background(), upgradeState())
	require.ErrorIs(t, err, errHelm)

	assert.Equal(t, []string{"verify", "verify", "verify"}, runner.ranSubcommands(),
		"only the signature check should have run, and it should have been retried")
}

// An image without its key cannot check anything, so it must refuse rather than
// apply a chart whose signature it never read.
func TestHelmUpgradeFailsClosedWithoutSigningKey(t *testing.T) {
	runner := newFakeRunner()
	executor := newHelmExecutor(t, runner, nil)
	executor.cosignKey = filepath.Join(t.TempDir(), "absent.pub")

	err := executor.Upgrade(context.Background(), upgradeState())
	require.ErrorIs(t, err, errMissingCosignKey)

	assert.Empty(t, runner.calls, "no command should run without a key to verify against")
}

// A registry that is briefly unreachable is the likeliest way a round fails, and
// retrying the fetch is safe precisely because nothing has been applied yet.
func TestHelmUpgradeRetriesTheDownload(t *testing.T) {
	runner := newFakeRunner()
	runner.failures["pull"] = 2

	require.NoError(t, newHelmExecutor(t, runner, nil).Upgrade(context.Background(), upgradeState()))

	assert.Equal(t, []string{"verify", "pull", "pull", "pull", "upgrade"}, runner.ranSubcommands())
}

// The pipeline publishes chart versions and OCI tags with the leading v
// stripped, so passing target_version through unchanged would never resolve.
func TestHelmUpgradeStripsVersionPrefix(t *testing.T) {
	runner := newFakeRunner()

	require.NoError(t, newHelmExecutor(t, runner, nil).Upgrade(context.Background(), upgradeState()))

	call, _ := runner.ran("pull")
	assert.Contains(t, strings.Join(call, " "), "--version 2.1.0")
}

// Without this the release falls back to the new chart's defaults and the agent
// comes back with the customer's configuration silently dropped.
func TestHelmUpgradePreservesReleaseValues(t *testing.T) {
	runner := newFakeRunner()

	require.NoError(t, newHelmExecutor(t, runner, nil).Upgrade(context.Background(), upgradeState()))

	call, _ := runner.ran("upgrade")
	assert.Contains(t, call, "--reset-then-reuse-values")
}

func TestHelmUpgradeFailsOnHelmError(t *testing.T) {
	runner := newFakeRunner()
	runner.err["upgrade"] = errHelm
	verifier := &fakeVerifier{}

	err := newHelmExecutor(t, runner, verifier).Upgrade(context.Background(), upgradeState())
	require.ErrorIs(t, err, errHelm)
	assert.Equal(t, 0, verifier.calls, "a failed rollout must not be verified")
}

// `--wait` only proves the readiness probe passed. An agent that never reaches
// the control plane cannot report the result, so the round has to fail.
func TestHelmUpgradeFailsWhenAgentsNeverReport(t *testing.T) {
	runner := newFakeRunner()

	err := newHelmExecutor(t, runner, &fakeVerifier{err: errAgentUnhealthy}).
		Upgrade(context.Background(), upgradeState())
	require.ErrorIs(t, err, errAgentUnhealthy)
}

// ---------------------------------------------------------------------------
// Rollback
// ---------------------------------------------------------------------------

func TestHelmRollbackRestoresPreviousRevision(t *testing.T) {
	runner := newFakeRunner()
	runner.out["get metadata"] = []byte(`{"chart":"crusoe-watch-agent-2.1.0","version":"2.1.0"}`)

	require.NoError(t, newHelmExecutor(t, runner, nil).Rollback(context.Background(), upgradeState()))

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

	require.NoError(t, newHelmExecutor(t, runner, nil).Rollback(context.Background(), upgradeState()))

	_, ok := runner.ran("rollback")
	assert.False(t, ok, "a release still on rollback_version needs no rollback")
}

func TestHelmRollbackSkipsWhenReleaseAbsent(t *testing.T) {
	runner := newFakeRunner()
	runner.out["get metadata"] = []byte("Error: release: not found")
	runner.err["get metadata"] = errHelm

	require.NoError(t, newHelmExecutor(t, runner, nil).Rollback(context.Background(), upgradeState()))

	_, ok := runner.ran("rollback")
	assert.False(t, ok)
}

// An unreadable release state is not the same as "nothing to roll back":
// guessing here could downgrade a healthy release, so it must surface as failed.
func TestHelmRollbackFailsWhenReleaseStateUnreadable(t *testing.T) {
	runner := newFakeRunner()
	runner.out["get metadata"] = []byte("Error: Kubernetes cluster unreachable")
	runner.err["get metadata"] = errHelm

	err := newHelmExecutor(t, runner, nil).Rollback(context.Background(), upgradeState())
	require.ErrorIs(t, err, errHelm)

	_, ok := runner.ran("rollback")
	assert.False(t, ok)
}

func TestHelmRollbackFailsOnHelmError(t *testing.T) {
	runner := newFakeRunner()
	runner.out["get metadata"] = []byte(`{"version":"2.1.0"}`)
	runner.err["rollback"] = errHelm

	require.ErrorIs(t,
		newHelmExecutor(t, runner, nil).Rollback(context.Background(), upgradeState()), errHelm)
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
func TestErrorMessageKeepsOnlyTheError(t *testing.T) {
	out := []byte("Pulled: ghcr.io/example/charts/agent:2.0\n" +
		"Digest: sha256:abc123\n" +
		"Error: UPGRADE FAILED: cannot patch \"crusoe-monitoring\"")

	assert.Equal(t, `Error: UPGRADE FAILED: cannot patch "crusoe-monitoring"`, errorMessage(out))

	// Output with no Error: line is reported as-is rather than discarded.
	assert.Equal(t, "something unexpected", errorMessage([]byte("  something unexpected\n")))
}

func TestChartRefTrimsRepoSlash(t *testing.T) {
	executor := NewHelmExecutor(HelmConfig{
		ChartRepo: "oci://mirror.internal/charts/",
		ChartName: "crusoe-watch-agent",
	})

	assert.Equal(t, "oci://mirror.internal/charts/crusoe-watch-agent", executor.chartRef())
}

// ---------------------------------------------------------------------------
// Retries
// ---------------------------------------------------------------------------

// fastBackoff keeps the schedule's shape without the waiting.
var fastBackoff = backoff{attempts: 4, delay: time.Millisecond, maxDelay: 2 * time.Millisecond}

func TestRetrySucceedsOnceTheRegistryAnswers(t *testing.T) {
	calls := 0

	require.NoError(t, fastBackoff.retry(context.Background(), discardLogger(), "the download",
		func(context.Context) error {
			if calls++; calls < 3 {
				return errHelm
			}

			return nil
		}))

	assert.Equal(t, 3, calls)
}

// The error the caller sees has to name what failed, because it becomes the
// reason on the upgrade result the control plane reports.
func TestRetryGivesUpAfterTheLastAttempt(t *testing.T) {
	calls := 0

	err := fastBackoff.retry(context.Background(), discardLogger(), "the download",
		func(context.Context) error {
			calls++

			return errHelm
		})

	require.ErrorIs(t, err, errHelm)
	assert.Equal(t, fastBackoff.attempts, calls)
	assert.Contains(t, err.Error(), "the download failed after 4 attempts")
}

// Once the rollback window is spent there is no point retrying: the round is
// over either way, and the caller needs the deadline error, not a retry summary.
func TestRetryStopsWhenTheWindowIsSpent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := 0

	err := fastBackoff.retry(ctx, discardLogger(), "the download", func(context.Context) error {
		calls++

		return errHelm
	})

	require.ErrorIs(t, err, errHelm)
	assert.Equal(t, 1, calls, "a cancelled round must not be retried")
	assert.NotContains(t, err.Error(), "attempts")
}

// A cancelled command surfaces the context error rather than a retry summary,
// even when the deadline lands while the command is running.
func TestRetryStopsOnAContextErrorFromTheCommand(t *testing.T) {
	calls := 0

	err := fastBackoff.retry(context.Background(), discardLogger(), "the download",
		func(context.Context) error {
			calls++

			return context.DeadlineExceeded
		})

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, 1, calls)
}

func TestJitterStaysInRange(t *testing.T) {
	for range 100 {
		assert.GreaterOrEqual(t, jitter(time.Second), time.Duration(0))
		assert.Less(t, jitter(time.Second), time.Second)
	}

	assert.Zero(t, jitter(0), "an unset jitter adds nothing")
}
