package upgrade

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Both binaries are resolved from PATH; the cwa-updater image ships them.
const (
	helmBinary   = "helm"
	cosignBinary = "cosign"
)

// defaultCosignKey is where the image keeps the public half of the release
// pipeline's chart signing key. Baked into the image rather than configured, so
// no cluster-side value can point verification at a different key.
const defaultCosignKey = "/etc/cwa/cosign.pub"

// ociScheme prefixes helm's OCI references. cosign takes the same reference without it.
const ociScheme = "oci://"

// defaultRollbackWindow is upgrade.rollbackWindowMin; the chart overrides it.
const defaultRollbackWindow = 15 * time.Minute

// helmNotFound is what helm prints for a release that does not exist.
const helmNotFound = "release: not found"

// errMissingCosignKey means the image shipped without its verification key. The
// round fails closed: an unverifiable chart is never applied.
var errMissingCosignKey = errors.New("chart signing key is missing")

// errNothingDeployed means the agent release has no revision to restore.
var errNothingDeployed = errors.New("agent release is not installed")

// The schedule for the registry calls an upgrade makes before it changes
// anything: a registry that is briefly unreachable is the likeliest way a round
// fails, and retrying costs nothing but time inside the rollback window.
const (
	fetchAttempts = 5
	fetchDelay    = 2 * time.Second
	fetchMaxDelay = 30 * time.Second
	fetchJitter   = time.Second
)

// Runner runs one CLI invocation and returns its combined output.
type Runner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// Verifier confirms the agents are healthy after a rolling update.
type Verifier interface {
	Verify(ctx context.Context) error
}

// ExecRunner runs one external binary.
type ExecRunner struct {
	Binary string
	Logger *slog.Logger
}

// NewHelmRunner returns a Runner for the helm CLI.
func NewHelmRunner(logger *slog.Logger) ExecRunner {
	return ExecRunner{Binary: helmBinary, Logger: logger}
}

// NewCosignRunner returns a Runner for the cosign CLI.
func NewCosignRunner(logger *slog.Logger) ExecRunner {
	return ExecRunner{Binary: cosignBinary, Logger: logger}
}

// Run executes the binary, folding stderr into the output: both helm and cosign
// report failures there, and that text becomes the reason on the upgrade result
// the control plane sees.
func (r ExecRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	if r.Logger != nil {
		r.Logger.Info("running "+r.Binary, "args", args)
	}

	// Binary is one of this package's two constants, and args are assembled by
	// the executor rather than taken from a request.
	//nolint:gosec // G204: neither the binary nor its arguments are caller-supplied
	out, err := exec.CommandContext(ctx, r.Binary, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, errorMessage(out))
	}

	return out, nil
}

// errorMessage reduces a command's combined output to the part worth reporting:
// its first Error: line onwards.
func errorMessage(out []byte) string {
	text := strings.TrimSpace(string(out))

	if i := strings.Index(text, "Error:"); i >= 0 {
		return text[i:]
	}

	return text
}

// HelmConfig wires a HelmExecutor.
type HelmConfig struct {
	Runner Runner
	// Cosign runs the signature check on the chart before anything is applied.
	Cosign Runner
	// Verifier is the post-upgrade agent health check. A nil Verifier trusts
	// `--wait` alone, which only proves the readiness probe passed.
	Verifier Verifier
	Logger   *slog.Logger

	// CosignKey is the public key the chart signature is checked against.
	// Empty uses defaultCosignKey, the path baked into the image.
	CosignKey string

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
	cosign   Runner
	verifier Verifier
	logger   *slog.Logger

	namespace      string
	release        string
	chartRepo      string
	chartName      string
	cosignKey      string
	rollbackWindow time.Duration
	// backoff paces the registry calls; tests shorten it.
	backoff backoff
}

// NewHelmExecutor returns an Executor backed by the helm CLI.
func NewHelmExecutor(cfg HelmConfig) *HelmExecutor {
	if cfg.RollbackWindow <= 0 {
		cfg.RollbackWindow = defaultRollbackWindow
	}

	if cfg.CosignKey == "" {
		cfg.CosignKey = defaultCosignKey
	}

	return &HelmExecutor{
		runner:         cfg.Runner,
		cosign:         cfg.Cosign,
		verifier:       cfg.Verifier,
		logger:         cfg.Logger,
		namespace:      cfg.Namespace,
		release:        cfg.Release,
		chartRepo:      cfg.ChartRepo,
		chartName:      cfg.ChartName,
		cosignKey:      cfg.CosignKey,
		rollbackWindow: cfg.RollbackWindow,
		backoff: backoff{
			attempts: fetchAttempts,
			delay:    fetchDelay,
			maxDelay: fetchMaxDelay,
			jitter:   fetchJitter,
		},
	}
}

// Upgrade installs TargetVersion and confirms the agents that came back are
// reaching the control plane. The rollout and the health check share one deadline.
//
// It runs in three steps so that only the last one can leave the cluster
// changed: the signature check and the download are pure registry work and are
// retried, while the apply happens once against an already-verified local chart.
func (e *HelmExecutor) Upgrade(ctx context.Context, state *State) error {
	ctx, cancel := context.WithTimeout(ctx, e.rollbackWindow)
	defer cancel()

	version := chartVersion(state.TargetVersion)

	// Both of these name the chart in their own errors, so the reason the control
	// plane ends up reporting stays readable without another layer of wrapping.
	if err := e.verifySignature(ctx, version); err != nil {
		return err
	}

	chart, err := e.pull(ctx, version)
	if err != nil {
		return err
	}

	// The chart is only needed for the apply below; the pod's /tmp is an emptyDir
	// that a restart would clear anyway, so this just keeps it from accumulating.
	defer func() {
		if err := os.RemoveAll(filepath.Dir(chart)); err != nil {
			e.logger.Warn("removing the downloaded chart", "error", err)
		}
	}()

	// No --install: the CMK add-on owns installation, and cwa-updater holds no
	// create permission on the chart's cluster-scoped resources.
	if _, err := e.runner.Run(ctx,
		"upgrade", e.release, chart,
		"--namespace", e.namespace,
		"--reset-then-reuse-values", // Without this every value falls back to the new chart
		"--wait",
		"--timeout", remaining(ctx).String(),
	); err != nil {
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

// verifySignature proves the published chart is the one the release pipeline
// signed, before any of it is downloaded or applied.
func (e *HelmExecutor) verifySignature(ctx context.Context, version string) error {
	// Checked up front so a missing key reports itself instead of being retried
	// as a run of identical cosign failures.
	if _, err := os.Stat(e.cosignKey); err != nil {
		return fmt.Errorf("%w at %s: %w", errMissingCosignKey, e.cosignKey, err)
	}

	reference := strings.TrimPrefix(e.chartRef(), ociScheme) + ":" + version

	if err := e.backoff.retry(ctx, e.logger, "the signature check on chart "+version,
		func(ctx context.Context) error {
			if _, err := e.cosign.Run(ctx, "verify", "--key", e.cosignKey, reference); err != nil {
				return fmt.Errorf("cosign verify: %w", err)
			}

			return nil
		}); err != nil {
		return err
	}

	e.logger.Info("chart signature verified", "reference", reference)

	return nil
}

// pull downloads the chart and reports the path to it. Downloading separately
// from the apply means the retries above never re-run a partial upgrade.
func (e *HelmExecutor) pull(ctx context.Context, version string) (string, error) {
	dir, err := os.MkdirTemp("", "cwa-chart-")
	if err != nil {
		return "", fmt.Errorf("creating a directory for the chart: %w", err)
	}

	if err := e.backoff.retry(ctx, e.logger, "the download of chart "+version,
		func(ctx context.Context) error {
			if _, err := e.runner.Run(ctx,
				"pull", e.chartRef(), "--version", version, "--destination", dir); err != nil {
				return fmt.Errorf("helm pull: %w", err)
			}

			return nil
		}); err != nil {
		if err := os.RemoveAll(dir); err != nil {
			e.logger.Warn("removing the chart directory", "error", err)
		}

		return "", err
	}

	// helm names the file after the chart and version it packaged.
	return filepath.Join(dir, fmt.Sprintf("%s-%s.tgz", e.chartName, version)), nil
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

// backoff is an exponential retry schedule.
type backoff struct {
	attempts int
	delay    time.Duration
	maxDelay time.Duration
	// jitter is the upper bound on the random amount added to each wait, so a
	// fleet-wide upgrade does not retry against the registry in lockstep.
	jitter time.Duration
}

// retry calls command until it succeeds, ctx ends, or the attempts run out, and
// reports its last error. Only the caller's deadline stops it early: an error
// cosign or helm returned cannot be classified as permanent from the outside, so
// every failure is retried. A tampered chart simply fails all of the attempts.
func (b backoff) retry(
	ctx context.Context, logger *slog.Logger, what string, command func(context.Context) error,
) error {
	delay := b.delay

	var err error

	for attempt := 1; attempt <= b.attempts; attempt++ {
		if err = command(ctx); err == nil {
			return nil
		}

		// The round is over; nothing after this could succeed either.
		if ctx.Err() != nil || isContextErr(err) {
			return err
		}

		if attempt == b.attempts {
			break
		}

		wait := delay + jitter(b.jitter)

		logger.Warn("retrying "+what,
			"attempt", attempt, "attempts", b.attempts, "wait", wait, "error", err)

		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}

		if delay *= 2; delay > b.maxDelay {
			delay = b.maxDelay
		}
	}

	return fmt.Errorf("%s failed after %d attempts: %w", what, b.attempts, err)
}

// jitter returns a random duration in [0, limit).
func jitter(limit time.Duration) time.Duration {
	if limit <= 0 {
		return 0
	}

	n, err := rand.Int(rand.Reader, big.NewInt(int64(limit)))
	if err != nil {
		return 0
	}

	return time.Duration(n.Int64())
}
