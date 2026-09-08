package heartbeat

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/upgrade"
	pb "gitlab.com/crusoeenergy/schemas/api/island/v2/observability"
)

// statusPollTicks is how many heartbeats apart /status is read while nothing is
// in flight. Every agent polls the one cwa-updater, so reading it every tick
// would scale with the node count. An upgrade under way is exempt: see pollDue.
const statusPollTicks = 10

// updaterCallTimeout bounds a read or acknowledgement. Both run on the heartbeat
// send path, so a missed call is simply retried on a later tick.
const updaterCallTimeout = 3 * time.Second

// updaterCallBudget is the most one heartbeat can spend on cwa-updater: a read
// and an acknowledgement.
const updaterCallBudget = 2 * updaterCallTimeout

// UpdaterStatus is the cwa-updater surface the reporter needs.
// *upgrade.Client implements it.
type UpdaterStatus interface {
	Status(ctx context.Context) (*upgrade.StatusView, error)
	Acknowledge(ctx context.Context) error
}

// upgradeReporter turns the result cwa-updater persisted into the heartbeat's
// last_upgrade_result, then acknowledges it so the next round may open. The
// result is read back rather than remembered in process: an upgrade replaces the
// pod that handed it off. A nil *upgradeReporter is a working no-op.
type upgradeReporter struct {
	client UpdaterStatus
	logger *slog.Logger

	mu sync.Mutex
	// pending is the result waiting to be reported, held until acknowledged.
	pending *pb.CwaUpgradeResult
	// reported means an earlier send on the current stream carried pending.
	reported   bool
	inProgress bool
	ticks      int
}

// Result is the last_upgrade_result for the next heartbeat, nil when there is
// nothing to report.
func (r *upgradeReporter) Result(ctx context.Context) *pb.CwaUpgradeResult {
	if r == nil {
		return nil
	}

	r.refresh(ctx)

	r.mu.Lock()
	defer r.mu.Unlock()

	return r.pending
}

// InProgress reports whether this agent is mid-upgrade.
func (r *upgradeReporter) InProgress() bool {
	if r == nil {
		return false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return r.inProgress
}

// MarkHandedOff records that this manager handed an upgrade off, so the
// heartbeat reports it as upgrading for whatever time this process has left.
func (r *upgradeReporter) MarkHandedOff(targetVersion string) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.inProgress = true
	r.logger.Info("upgrade handed off to cwa-updater", "target_version", targetVersion)
}

// StreamOpened clears the marker before a stream's first send: a fresh stream
// carries none of the previous one's messages.
func (r *upgradeReporter) StreamOpened() {
	if r == nil {
		return
	}

	r.mu.Lock()
	r.reported = false
	r.mu.Unlock()
}

// Sent releases the result an earlier send on this stream carried. A send only
// buffers, so the first one merely marks it reported and a later one confirms it:
// a stream dying mid-flight costs a duplicate report rather than the result.
func (r *upgradeReporter) Sent(ctx context.Context) {
	if r == nil {
		return
	}

	r.mu.Lock()
	reported, alreadySent := r.pending, r.reported
	r.reported = r.pending != nil
	r.mu.Unlock()

	if reported == nil || !alreadySent || shutdownFlush(ctx) {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, updaterCallTimeout)
	defer cancel()

	if err := r.client.Acknowledge(ctx); err != nil {
		// Not fatal: the result stays pending and rides the next heartbeat.
		r.logger.Warn("acknowledging the upgrade result with cwa-updater", "error", err)

		return
	}

	r.mu.Lock()
	r.pending, r.inProgress, r.reported = nil, false, false
	r.mu.Unlock()

	r.logger.Info("upgrade result reported and acknowledged",
		"status", reported.GetStatus(), "to_version", reported.GetToVersion())
}

// refresh reads /status when a read is due and nothing is already waiting.
func (r *upgradeReporter) refresh(ctx context.Context) {
	if shutdownFlush(ctx) {
		return
	}

	r.mu.Lock()
	skip := r.pending != nil || !r.pollDue()
	r.mu.Unlock()

	if skip {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, updaterCallTimeout)
	defer cancel()

	view, err := r.client.Status(ctx)
	if err != nil && !errors.Is(err, upgrade.ErrNoState) {
		r.logger.Debug("reading the upgrade status from cwa-updater", "error", err)

		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// ErrNoState: idle with nothing recorded.
	if view == nil {
		r.inProgress = false

		return
	}

	r.inProgress = !view.Phase.Terminal()

	if view.Result == nil {
		return
	}

	r.pending = upgradeResult(view.Result)
	r.logger.Info("upgrade result read from cwa-updater",
		"status", view.Result.Status, "from_version", view.Result.FromVersion,
		"to_version", view.Result.ToVersion, "reason", view.Result.Reason)
}

// shutdownFlush reports whether ctx belongs to the final heartbeat, whose grace
// window exists to flush command results rather than to settle an upgrade.
func shutdownFlush(ctx context.Context) bool {
	deadline, ok := ctx.Deadline()

	return ok && time.Until(deadline) < updaterCallBudget
}

// pollDue reports whether this heartbeat should read /status. Callers hold r.mu.
func (r *upgradeReporter) pollDue() bool {
	r.ticks++

	// An upgrade in flight is read every tick to report the result faster.
	return r.inProgress || r.ticks == 1 || r.ticks%statusPollTicks == 0
}

// upgradeResult maps cwa-updater's result onto the heartbeat field. Whichever
// manager reads a result reports it, without checking agent_executions first: an
// unacknowledged result blocks the next round, so declining to claim one would
// wedge upgrades for the whole cluster.
func upgradeResult(result *upgrade.Result) *pb.CwaUpgradeResult {
	status := pb.CwaUpgradeStatus_CWA_UPGRADE_STATUS_FAILED

	switch result.Status {
	case upgrade.ResultSucceeded:
		status = pb.CwaUpgradeStatus_CWA_UPGRADE_STATUS_SUCCEEDED
	case upgrade.ResultRolledBack:
		status = pb.CwaUpgradeStatus_CWA_UPGRADE_STATUS_ROLLED_BACK
	}

	out := &pb.CwaUpgradeResult{
		Status:      status,
		FromVersion: result.FromVersion,
		ToVersion:   result.ToVersion,
		Reason:      result.Reason,
	}

	if !result.CompletedAt.IsZero() {
		out.Timestamp = timestamppb.New(result.CompletedAt)
	}

	return out
}
