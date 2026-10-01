package upgrade

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Where the VM installer leaves the copy of itself that cwa-updater drives, and
// where it records the version it installed. Both are written by
// vm/crusoe_watch_agent.sh, so these paths are a contract with it.
const (
	defaultInstallerPath    = "/etc/crusoe/crusoe_watch_agent/crusoe_watch_agent.sh"
	defaultAgentVersionFile = "/etc/crusoe/crusoe_watch_agent/VERSION"
)

// Where configure-updater's values reach the installer. Both are contracts with
// vm/crusoe_watch_agent.sh.
const (
	installerReleaseBaseEnv     = "CWA_RELEASE_BASE_URL"
	installerDownloadBackoffEnv = "CWA_DOWNLOAD_BACKOFF_MAX_SEC"
)

// The installer subcommands an upgrade and a rollback map onto. Both take the
// release to move to as their operand.
const (
	upgradeSubcommand  = "upgrade"
	rollbackSubcommand = "rollback"
)

// installLogLines is how much of the installer's output is carried into the
// reported failure reason. It logs every step, so what failed is at the end.
const installLogLines = 20

// killGracePeriod is how long the installer's process group has to unwind after
// the SIGTERM.
const killGracePeriod = 10 * time.Second

var errWrongVersionInstalled = errors.New("the installer left the wrong version installed")

// ScriptExecutorFields are the config fields the VM executor reads.
func ScriptExecutorFields() []string {
	return []string{FieldDownloadURLBase, FieldRollbackTimeoutMin, FieldDownloadBackoffMaxSec}
}

// ScriptRunner runs one installer invocation and returns its combined output.
// env carries "KEY=value" overrides, added to the parent environment.
type ScriptRunner interface {
	Run(ctx context.Context, script string, args, env []string) ([]byte, error)
}

// ExecScriptRunner runs the installer as a child process.
type ExecScriptRunner struct {
	Logger *slog.Logger
}

// NewScriptRunner returns a ScriptRunner backed by the installer itself.
func NewScriptRunner(logger *slog.Logger) ExecScriptRunner {
	return ExecScriptRunner{Logger: logger}
}

// Run executes the installer, folding stderr into the output: the installer
// reports its failures there, and that text becomes the reason on the upgrade
// result the control plane sees.
//
// Stdin is left unset, which gives the child /dev/null. handle_token falls
// through to an interactive read when no token is on disk, and an upgrade has to
// fail fast rather than block until the window expires.
//
// The installer shells out to apt-get, wget and systemctl, so cancellation runs
// it in its own process group and signals the group.
func (r ExecScriptRunner) Run(ctx context.Context, script string, args, env []string) ([]byte, error) {
	if r.Logger != nil {
		r.Logger.Info("running the installer", "script", script, "args", args, "env", env)
	}

	cmd := exec.CommandContext(ctx, script, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}

			return fmt.Errorf("signalling the installer process group: %w", err)
		}

		return nil
	}
	// Caps how long a descendant holding the output pipe can delay Wait.
	cmd.WaitDelay = killGracePeriod

	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, lastLines(out, installLogLines))
	}

	return out, nil
}

// lastLines reduces a command's output to its final lines.
func lastLines(out []byte, limit int) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")

	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}

	return strings.Join(lines, "\n")
}

// ScriptConfig wires a ScriptExecutor.
type ScriptConfig struct {
	// Runner runs the installer.
	Runner ScriptRunner
	// Verifier is the post-upgrade agent health check. A nil Verifier trusts the
	// installer alone, which only proves its services started.
	Verifier Verifier
	Logger   *slog.Logger

	// Script is the installer copy on this host. Empty uses defaultInstallerPath.
	Script string
	// VersionFile is where the installer records the version it installed. Empty
	// uses defaultAgentVersionFile.
	VersionFile string
	// RollbackWindow bounds the whole attempt. Zero uses defaultRollbackWindow;
	// a configure-updater value takes precedence.
	RollbackWindow time.Duration
	// Settings is the live configure-updater config, read per run. Nil keeps
	// RollbackWindow.
	Settings *Holder
}

// ScriptExecutor upgrades a VM agent by running the installer already on the
// host, which is the same script an operator runs by hand.
//
// It deliberately reimplements none of the mechanism: the installer owns
// resolving the release, verifying its signature and checksums, replacing
// binaries, writing unit files and restarting services. cwa-updater's job is to
// say which version to move to, and to judge whether what came back is healthy.
type ScriptExecutor struct {
	runner   ScriptRunner
	verifier Verifier
	logger   *slog.Logger

	script         string
	versionFile    string
	rollbackWindow time.Duration
	settings       *Holder
}

// NewScriptExecutor returns an Executor backed by the host's installer.
func NewScriptExecutor(cfg ScriptConfig) *ScriptExecutor {
	if cfg.RollbackWindow <= 0 {
		cfg.RollbackWindow = defaultRollbackWindow
	}

	if cfg.Script == "" {
		cfg.Script = defaultInstallerPath
	}

	if cfg.VersionFile == "" {
		cfg.VersionFile = defaultAgentVersionFile
	}

	return &ScriptExecutor{
		runner:         cfg.Runner,
		verifier:       cfg.Verifier,
		logger:         cfg.Logger,
		script:         cfg.Script,
		versionFile:    cfg.VersionFile,
		rollbackWindow: cfg.RollbackWindow,
		settings:       cfg.Settings,
	}
}

// Upgrade moves the host to TargetVersion, then waits for the agent to come back
// and reach the control plane. Both steps share one deadline, the rollback window.
func (e *ScriptExecutor) Upgrade(ctx context.Context, state *State) error {
	ctx, cancel := context.WithTimeout(ctx, e.window())
	defer cancel()

	if err := e.run(ctx, upgradeSubcommand, state.TargetVersion); err != nil {
		return err
	}

	if e.verifier == nil {
		return nil
	}

	if err := e.verifier.Verify(ctx); err != nil {
		return fmt.Errorf("verifying the agent after the upgrade to %s: %w", state.TargetVersion, err)
	}

	return nil
}

// Rollback moves the host back to RollbackVersion.
//
// It is a no-op when the host already records that version, which is the case
// when an upgrade was interrupted before the installer changed anything.
func (e *ScriptExecutor) Rollback(ctx context.Context, state *State) error {
	ctx, cancel := context.WithTimeout(ctx, e.window())
	defer cancel()

	installed, err := e.installedVersion()
	if err != nil {
		// Reinstalling blind could replace a host that was never touched, so an
		// unreadable version record has to surface as a failure instead.
		return err
	}

	if installed == state.RollbackVersion {
		e.logger.Info("the host already records the rollback version; nothing to roll back",
			"rollback_version", state.RollbackVersion)

		return nil
	}

	if err := e.run(ctx, rollbackSubcommand, state.RollbackVersion); err != nil {
		return err
	}

	e.logger.Info("agent rolled back",
		"rollback_version", state.RollbackVersion, "from", installed)

	return nil
}

// run drives the installer to one version and confirms the host recorded it.
func (e *ScriptExecutor) run(ctx context.Context, subcommand, version string) error {
	if _, err := e.runner.Run(ctx, e.script, []string{subcommand, version}, e.env()); err != nil {
		return fmt.Errorf("running %s %s: %w", subcommand, version, err)
	}

	installed, err := e.installedVersion()
	if err != nil {
		return err
	}

	if installed != version {
		return fmt.Errorf("%w: moved to %s but %s records %s",
			errWrongVersionInstalled, version, e.versionFile, installed)
	}

	e.logger.Info("installer completed", "subcommand", subcommand, "version", version)

	return nil
}

// window is the deadline one attempt gets, preferring a delivered value.
func (e *ScriptExecutor) window() time.Duration {
	return attemptWindow(e.settings, e.rollbackWindow)
}

// env carries the configure-updater overrides the installer reads. Empty leaves
// it on its own defaults.
func (e *ScriptExecutor) env() []string {
	cfg := e.settings.Get()

	var vars []string

	if cfg.DownloadURLBase != "" {
		vars = append(vars, installerReleaseBaseEnv+"="+cfg.DownloadURLBase)
	}

	if cfg.DownloadBackoffMaxSec > 0 {
		vars = append(vars,
			installerDownloadBackoffEnv+"="+strconv.Itoa(cfg.DownloadBackoffMaxSec))
	}

	return vars
}

// installedVersion is the version the installer last recorded on this host.
func (e *ScriptExecutor) installedVersion() (string, error) {
	data, err := os.ReadFile(e.versionFile)
	if err != nil {
		return "", fmt.Errorf("reading the installed version from %s: %w", e.versionFile, err)
	}

	return strings.TrimSpace(string(data)), nil
}
