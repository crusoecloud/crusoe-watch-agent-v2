package upgrade

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// versionLabel holds the agent chart's appVersion, which a release stamps equal to the chart version.
const versionLabel = "app.kubernetes.io/version"

// defaultRolloutInterval is how often the agent DaemonSet is re-read while a rollout is awaited.
const defaultRolloutInterval = 5 * time.Second

// errRolloutPending means the agent DaemonSet has not finished moving to the target version.
var errRolloutPending = errors.New("rollout is not complete")

// errRollbackUnavailable is what AddonExecutor.Rollback returns: the control
// plane restores a version through the add-on manager.
var errRollbackUnavailable = errors.New("rollback is not available to the add-on executor")

// AddonExecutorFields are the config fields the add-on executor reads.
func AddonExecutorFields() []string {
	return []string{FieldRollbackTimeoutMin}
}

// AddonConfig wires an AddonExecutor.
type AddonConfig struct {
	Client kubernetes.Interface
	// Verifier is the post-rollout agent health check. A nil Verifier trusts
	// the DaemonSet's readiness alone.
	Verifier Verifier
	Logger   *slog.Logger

	Namespace string
	// DaemonSet is the agent DaemonSet the add-on manager rolls.
	DaemonSet string
	// RolloutWindow bounds the wait for the rollout. Zero uses
	// defaultRollbackWindow; a configure-updater value takes precedence.
	RolloutWindow time.Duration
	// HealthWindow bounds the health check after the rollout. Zero uses defaultHealthWindow.
	HealthWindow time.Duration
	// Settings is the live configure-updater config, read per run. Nil keeps RolloutWindow.
	Settings *Holder
}

// AddonExecutor watches an upgrade the CMK add-on manager performs and reports
// whether the agents came back. It changes nothing on the cluster.
type AddonExecutor struct {
	client   kubernetes.Interface
	verifier Verifier
	logger   *slog.Logger

	namespace     string
	daemonSet     string
	rolloutWindow time.Duration
	healthWindow  time.Duration
	interval      time.Duration
	settings      *Holder
}

// NewAddonExecutor returns an Executor that observes the agent DaemonSet.
func NewAddonExecutor(cfg AddonConfig) *AddonExecutor {
	if cfg.RolloutWindow <= 0 {
		cfg.RolloutWindow = defaultRollbackWindow
	}

	if cfg.HealthWindow <= 0 {
		cfg.HealthWindow = defaultHealthWindow
	}

	return &AddonExecutor{
		client:        cfg.Client,
		verifier:      cfg.Verifier,
		logger:        cfg.Logger,
		namespace:     cfg.Namespace,
		daemonSet:     cfg.DaemonSet,
		rolloutWindow: cfg.RolloutWindow,
		healthWindow:  cfg.HealthWindow,
		interval:      defaultRolloutInterval,
		settings:      cfg.Settings,
	}
}

// Upgrade waits for the agent DaemonSet to finish rolling to TargetVersion and
// confirms the agents that came back are reaching the control plane.
func (e *AddonExecutor) Upgrade(ctx context.Context, state *State) error {
	rolloutCtx, cancelRollout := context.WithTimeout(ctx, attemptWindow(e.settings, e.rolloutWindow))
	err := e.awaitRollout(rolloutCtx, chartVersion(state.TargetVersion))

	cancelRollout()

	if err != nil {
		return err
	}

	if e.verifier == nil {
		return nil
	}

	verifyCtx, cancel := context.WithTimeout(ctx, e.healthWindow)
	defer cancel()

	if err := e.verifier.Verify(verifyCtx); err != nil {
		return fmt.Errorf("verifying agents after upgrade to %s: %w", state.TargetVersion, err)
	}

	return nil
}

// Rollback is not available: see errRollbackUnavailable.
func (e *AddonExecutor) Rollback(context.Context, *State) error {
	return errRollbackUnavailable
}

// Observes implements Observer.
func (e *AddonExecutor) Observes() bool { return true }

// awaitRollout polls until the DaemonSet has rolled to version or ctx ends. The
// last reason it was not done is wrapped into the error, so the upgrade result
// reports what held the rollout back rather than the deadline that ended it.
func (e *AddonExecutor) awaitRollout(ctx context.Context, version string) error {
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()

	var last error

	for {
		err := e.rolledOut(ctx, version)
		if err == nil {
			e.logger.Info("add-on rollout complete", "daemonset", e.daemonSet, "version", version)

			return nil
		}

		// A cut-off read must not displace the reason the rollout was pending.
		if !isContextErr(err) || last == nil {
			last = err
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("add-on rollout to %s did not complete: %w", version, last)
		case <-ticker.C:
		}
	}
}

// rolledOut reports whether the DaemonSet carries version and every pod has
// been replaced and is ready.
func (e *AddonExecutor) rolledOut(ctx context.Context, version string) error {
	daemonSet, err := e.client.AppsV1().DaemonSets(e.namespace).Get(ctx, e.daemonSet, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading daemonset %s/%s: %w", e.namespace, e.daemonSet, err)
	}

	if got := daemonSet.Labels[versionLabel]; got != version {
		return fmt.Errorf("%w: daemonset %s is at version %q", errRolloutPending, e.daemonSet, got)
	}

	status := daemonSet.Status
	if status.ObservedGeneration < daemonSet.Generation ||
		status.UpdatedNumberScheduled < status.DesiredNumberScheduled ||
		status.NumberReady < status.DesiredNumberScheduled {

		return fmt.Errorf("%w: %d of %d pods updated, %d ready", errRolloutPending,
			status.UpdatedNumberScheduled, status.DesiredNumberScheduled, status.NumberReady)
	}

	return nil
}
