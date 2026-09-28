package upgrade

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errInstaller = errors.New("installer exited 1")

// fakeScriptRunner stands in for the installer.
type fakeScriptRunner struct {
	calls [][]string
	// versionFile is where the real installer records what it moved to, and
	// records is what this one writes there. An empty records changes nothing,
	// standing in for an installer that reported success and did not.
	versionFile string
	records     string
	out         []byte
	err         error
	// deadline is the one the last run saw, for asserting the rollback window.
	deadline time.Time
}

func (f *fakeScriptRunner) Run(ctx context.Context, script string, args []string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{script}, args...))
	f.deadline, _ = ctx.Deadline()

	if f.err != nil {
		return f.out, f.err
	}

	if f.records != "" {
		if err := os.WriteFile(f.versionFile, []byte(f.records+"\n"), 0o600); err != nil {
			return nil, err
		}
	}

	return f.out, nil
}

// scriptHost is a fake VM: the installer copy the install left behind, and the
// version file it maintains.
type scriptHost struct {
	script      string
	versionFile string
	runner      *fakeScriptRunner
	verifier    *fakeVerifier
}

// newScriptHost lays out a host running installed, whose installer would move it
// to records.
func newScriptHost(t *testing.T, installed, records string) *scriptHost {
	t.Helper()

	root := t.TempDir()
	versionFile := filepath.Join(root, "VERSION")
	require.NoError(t, os.WriteFile(versionFile, []byte(installed+"\n"), 0o600))

	return &scriptHost{
		script:      filepath.Join(root, "crusoe_watch_agent.sh"),
		versionFile: versionFile,
		runner:      &fakeScriptRunner{versionFile: versionFile, records: records},
		verifier:    &fakeVerifier{},
	}
}

// executor wires a ScriptExecutor against this host.
func (h *scriptHost) executor() *ScriptExecutor {
	return NewScriptExecutor(ScriptConfig{
		Runner:      h.runner,
		Verifier:    h.verifier,
		Logger:      discardLogger(),
		Script:      h.script,
		VersionFile: h.versionFile,
	})
}

// installedVersion is what the version file records.
func (h *scriptHost) installedVersion(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile(h.versionFile)
	require.NoError(t, err)

	return strings.TrimSpace(string(data))
}

// scriptState is the record the service hands an executor.
func scriptState(target, rollback string) *State {
	return &State{Phase: PhaseInProgress, TargetVersion: target, RollbackVersion: rollback}
}

// ---------------------------------------------------------------------------
// Upgrade
// ---------------------------------------------------------------------------

// The executor's whole job on the way in is to name the version: the installer
// owns resolving, verifying and installing the release.
func TestScriptUpgradeRunsTheInstaller(t *testing.T) {
	h := newScriptHost(t, "v1.3", "v1.4")

	require.NoError(t, h.executor().Upgrade(context.Background(), scriptState("v1.4", "v1.3")))

	assert.Equal(t, [][]string{{h.script, "upgrade", "v1.4"}}, h.runner.calls)
	assert.Equal(t, 1, h.verifier.calls)
	assert.Equal(t, "v1.4", h.installedVersion(t))
}

func TestScriptUpgradeReportsAFailedInstaller(t *testing.T) {
	h := newScriptHost(t, "v1.3", "v1.4")
	h.runner.err = errInstaller
	h.runner.out = []byte("==> Moving v1.3 → v1.4\nERROR: Release signature does not verify.\n")

	err := h.executor().Upgrade(context.Background(), scriptState("v1.4", "v1.3"))

	require.ErrorIs(t, err, errInstaller)
	assert.Equal(t, 0, h.verifier.calls, "the health check ran over a failed install")
	assert.Equal(t, "v1.3", h.installedVersion(t))
}

// An installer that reports success without replacing anything has to surface as
// a failure, or the control plane would record the upgrade at a version the host
// is not running.
func TestScriptUpgradeRejectsAnUnchangedVersion(t *testing.T) {
	h := newScriptHost(t, "v1.3", "")

	err := h.executor().Upgrade(context.Background(), scriptState("v1.4", "v1.3"))

	assert.ErrorIs(t, err, errWrongVersionInstalled)
	assert.Equal(t, 0, h.verifier.calls)
}

func TestScriptUpgradeReportsAFailedHealthCheck(t *testing.T) {
	h := newScriptHost(t, "v1.3", "v1.4")
	h.verifier.err = errAgentUnhealthy

	err := h.executor().Upgrade(context.Background(), scriptState("v1.4", "v1.3"))

	assert.ErrorIs(t, err, errAgentUnhealthy)
}

// A nil Verifier trusts the installer alone.
func TestScriptUpgradeWithoutAVerifier(t *testing.T) {
	h := newScriptHost(t, "v1.3", "v1.4")

	executor := NewScriptExecutor(ScriptConfig{
		Runner:      h.runner,
		Logger:      discardLogger(),
		Script:      h.script,
		VersionFile: h.versionFile,
	})

	require.NoError(t, executor.Upgrade(context.Background(), scriptState("v1.4", "v1.3")))
}

// ---------------------------------------------------------------------------
// Rollback
// ---------------------------------------------------------------------------

// The rollback names the version to move back to, and is a separate subcommand
// so an upgrade's refusal to move backwards still holds everywhere else.
func TestScriptRollbackRunsTheInstaller(t *testing.T) {
	h := newScriptHost(t, "v1.4", "v1.3")

	require.NoError(t, h.executor().Rollback(context.Background(), scriptState("v1.4", "v1.3")))

	assert.Equal(t, [][]string{{h.script, "rollback", "v1.3"}}, h.runner.calls)
	assert.Equal(t, "v1.3", h.installedVersion(t))
}

// An upgrade interrupted before the installer changed anything leaves the host on
// rollback_version already, so reinstalling it would be churn.
func TestScriptRollbackIsANoOpOnTheRollbackVersion(t *testing.T) {
	h := newScriptHost(t, "v1.3", "v1.3")

	require.NoError(t, h.executor().Rollback(context.Background(), scriptState("v1.4", "v1.3")))

	assert.Empty(t, h.runner.calls)
}

// Reinstalling blind could replace a host that was never touched, so an
// unreadable version record is a failure rather than an assumption.
func TestScriptRollbackFailsWithoutAVersionRecord(t *testing.T) {
	h := newScriptHost(t, "v1.4", "v1.3")
	require.NoError(t, os.Remove(h.versionFile))

	err := h.executor().Rollback(context.Background(), scriptState("v1.4", "v1.3"))

	require.Error(t, err)
	assert.Empty(t, h.runner.calls)
}

// A rollback that does not land is reported, not assumed.
func TestScriptRollbackRejectsAnUnchangedVersion(t *testing.T) {
	h := newScriptHost(t, "v1.4", "")

	err := h.executor().Rollback(context.Background(), scriptState("v1.4", "v1.3"))

	assert.ErrorIs(t, err, errWrongVersionInstalled)
}

// ---------------------------------------------------------------------------
// Defaults and helpers
// ---------------------------------------------------------------------------

// The paths are a contract with vm/crusoe_watch_agent.sh: it writes both, and
// neither may live on the /run tmpfs, where a reboot would drop them.
func TestScriptExecutorDefaults(t *testing.T) {
	executor := NewScriptExecutor(ScriptConfig{})

	assert.Equal(t, defaultInstallerPath, executor.script)
	assert.Equal(t, defaultAgentVersionFile, executor.versionFile)
	assert.NotContains(t, executor.script, "/run/")
	assert.NotContains(t, executor.versionFile, "/run/")
	assert.Equal(t, defaultRollbackWindow, executor.rollbackWindow)
}

// The window bounds the installer run and the health check together: an installer
// that hangs must not hold the round open past it.
func TestScriptUpgradeIsBoundedByTheRollbackWindow(t *testing.T) {
	h := newScriptHost(t, "v1.3", "v1.4")

	executor := NewScriptExecutor(ScriptConfig{
		Runner:         h.runner,
		Logger:         discardLogger(),
		Script:         h.script,
		VersionFile:    h.versionFile,
		RollbackWindow: time.Minute,
	})

	require.NoError(t, executor.Upgrade(context.Background(), scriptState("v1.4", "v1.3")))
	assert.WithinDuration(t, time.Now().Add(time.Minute), h.runner.deadline, 5*time.Second)
}

func TestLastLinesKeepsTheEnd(t *testing.T) {
	assert.Equal(t, "c\nd", lastLines([]byte("a\nb\nc\nd\n"), 2))
	assert.Equal(t, "a\nb", lastLines([]byte("  a\nb  "), 5))
}
