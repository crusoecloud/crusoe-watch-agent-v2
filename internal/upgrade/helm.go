package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// helmBinary is resolved from PATH; the cwa-updater image ships it.
const helmBinary = "helm"

// defaultRollbackWindow is upgrade.rollbackWindowMin; the chart overrides it.
const defaultRollbackWindow = 15 * time.Minute

// helmNotFound is what helm prints for a release that does not exist.
const helmNotFound = "release: not found"

// errNothingDeployed means the agent release has no revision to restore.
var errNothingDeployed = errors.New("agent release is not installed")

// Runner runs one helm invocation and returns its combined output.
type Runner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// Verifier confirms the agents are healthy after a rolling update.
type Verifier interface {
	Verify(ctx context.Context) error
}

// ExecRunner runs the helm binary.
type ExecRunner struct{ Logger *slog.Logger }

// Run executes helm, folding stderr into the output: helm reports failures there,
// and that text becomes the reason on the upgrade result the control plane sees.
func (r ExecRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	if r.Logger != nil {
		r.Logger.Info("running helm", "args", args)
	}

	out, err := exec.CommandContext(ctx, helmBinary, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, helmMessage(out))
	}

	return out, nil
}

// helmMessage reduces helm's combined output to the part worth reporting: its first Error: line onwards.
func helmMessage(out []byte) string {
	text := strings.TrimSpace(string(out))

	if i := strings.Index(text, "Error:"); i >= 0 {
		return text[i:]
	}

	return text
}

// HelmConfig wires a HelmExecutor.
type HelmConfig struct {
	Runner Runner
	// Verifier is the post-upgrade agent health check. A nil Verifier trusts
	// `--wait` alone, which only proves the readiness probe passed.
	Verifier Verifier
	Logger   *slog.Logger

	Namespace string
	// Release is the agent Helm release this upgrades.
	Release string
	// ChartRepo is the OCI repo holding the agent chart, a pull-through mirror in
	// clusters without outbound access; ChartName is the chart within it.
	ChartRepo string
	ChartName string
	// RollbackWindow bounds the whole attempt. Zero uses defaultRollbackWindow.
	RollbackWindow time.Duration
}

// HelmExecutor upgrades the agent release with helm and restores the previous revision on failure.
type HelmExecutor struct {
	runner   Runner
	verifier Verifier
	logger   *slog.Logger

	namespace      string
	release        string
	chartRepo      string
	chartName      string
	rollbackWindow time.Duration
}

// NewHelmExecutor returns an Executor backed by the helm CLI.
func NewHelmExecutor(cfg HelmConfig) *HelmExecutor {
	if cfg.RollbackWindow <= 0 {
		cfg.RollbackWindow = defaultRollbackWindow
	}

	return &HelmExecutor{
		runner:         cfg.Runner,
		verifier:       cfg.Verifier,
		logger:         cfg.Logger,
		namespace:      cfg.Namespace,
		release:        cfg.Release,
		chartRepo:      cfg.ChartRepo,
		chartName:      cfg.ChartName,
		rollbackWindow: cfg.RollbackWindow,
	}
}

// Upgrade installs TargetVersion and confirms the agents that came back are
// reaching the control plane. The rollout and the health check share one deadline.
func (e *HelmExecutor) Upgrade(ctx context.Context, state *State) error {
	ctx, cancel := context.WithTimeout(ctx, e.rollbackWindow)
	defer cancel()

	// No --install: the CMK add-on owns installation, and cwa-updater holds no
	// create permission on the chart's cluster-scoped resources.
	_, err := e.runner.Run(ctx,
		"upgrade", e.release, e.chartRef(),
		"--version", chartVersion(state.TargetVersion),
		"--namespace", e.namespace,
		"--reset-then-reuse-values", // Without this every value falls back to the new chart
		"--wait",
		"--timeout", remaining(ctx).String(),
	)
	if err != nil {
		return fmt.Errorf("upgrading %s to %s: %w", e.release, state.TargetVersion, err)
	}

	if e.verifier == nil {
		return nil
	}

	if err := e.verifier.Verify(ctx); err != nil {
		return fmt.Errorf("verifying agents after upgrade to %s: %w", state.TargetVersion, err)
	}

	return nil
}

// Rollback restores the revision the release was on before the upgrade.
//
// It is a no-op when TargetVersion is not the deployed version.
func (e *HelmExecutor) Rollback(ctx context.Context, state *State) error {
	ctx, cancel := context.WithTimeout(ctx, e.rollbackWindow)
	defer cancel()

	deployed, err := e.deployedVersion(ctx)

	switch {
	case errors.Is(err, errNothingDeployed):
		e.logger.Warn("agent release is not installed; nothing to roll back", "release", e.release)

		return nil
	case err != nil:
		// Rolling back blind could downgrade a healthy release, so an unreadable
		// release state has to surface as a failure instead.
		return err
	case deployed != chartVersion(state.TargetVersion):
		e.logger.Info("target version is not deployed; nothing to roll back",
			"deployed", deployed, "target_version", state.TargetVersion)

		return nil
	}

	// No revision argument: helm restores the immediately previous revision,
	// which is the one rollback_version was installed from.
	if _, err := e.runner.Run(ctx,
		"rollback", e.release,
		"--namespace", e.namespace,
		"--wait",
		"--timeout", remaining(ctx).String(),
	); err != nil {
		return fmt.Errorf("rolling %s back to %s: %w", e.release, state.RollbackVersion, err)
	}

	e.logger.Info("agent release rolled back",
		"release", e.release, "rollback_version", state.RollbackVersion)

	return nil
}

// metadata is the subset of `helm get metadata -o json` this needs: version is
// the chart version, which is what target_version resolves to.
type metadata struct {
	Version string `json:"version"`
}

// deployedVersion is the chart version of the release's current revision,
// whatever its status: a failed `helm upgrade --wait` still leaves the new
// revision current, and that is exactly the case a rollback must act on.
func (e *HelmExecutor) deployedVersion(ctx context.Context) (string, error) {
	out, err := e.runner.Run(ctx,
		"get", "metadata", e.release, "--namespace", e.namespace, "-o", "json")
	if err != nil {
		if strings.Contains(string(out), helmNotFound) {
			return "", errNothingDeployed
		}

		return "", fmt.Errorf("reading release metadata for %s: %w", e.release, err)
	}

	var meta metadata
	if err := json.Unmarshal(out, &meta); err != nil {
		return "", fmt.Errorf("parsing release metadata for %s: %w", e.release, err)
	}

	return meta.Version, nil
}

// chartRef is the OCI reference helm pulls the agent chart from.
func (e *HelmExecutor) chartRef() string {
	return strings.TrimSuffix(e.chartRepo, "/") + "/" + e.chartName
}

// chartVersion is target_version as a chart version. The control plane sends
// agent versions as vX.Y.Z; the release pipeline publishes chart versions and
// OCI tags with the v stripped, so `--version` would never match otherwise.
func chartVersion(version string) string {
	return strings.TrimPrefix(version, "v")
}

// remaining is how much of the rollback window is left, truncated to whole
// seconds so the timeout reads cleanly in logs and in the reported reason.
func remaining(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return defaultRollbackWindow
	}

	if left := time.Until(deadline); left > time.Second {
		return left.Truncate(time.Second)
	}

	return time.Second
}
