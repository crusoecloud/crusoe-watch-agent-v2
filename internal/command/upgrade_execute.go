package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/upgrade"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/version"
)

// UpgradeExecuteCommand hands this agent's upgrade to cwa-updater.
const UpgradeExecuteCommand = "upgrade.execute"

// upgrade.execute parameters. Only target_version is required.
const (
	ParamTargetVersion   = "target_version"
	ParamRollbackVersion = "rollback_version"
	ParamArtifactURL     = "artifact_url"
	ParamChecksum        = "checksum"
)

// handoffRetryDelay is the wait between handoff attempts; the command timeout
// bounds how many there are.
const handoffRetryDelay = 10 * time.Second

// Updater is the cwa-updater surface this handler needs. *upgrade.Client implements it.
type Updater interface {
	Handoff(ctx context.Context, req *upgrade.Request) (upgrade.Acceptance, error)
	Status(ctx context.Context) (*upgrade.StatusView, error)
}

// UpgradeExecute is the upgrade.execute handler. A SUCCEEDED result means the
// handoff is durable, not that the agent is upgraded: cwa-updater collects every
// agent's handoff, then upgrades them at once, replacing the pod running this
// command. The outcome arrives as last_upgrade_result on a later heartbeat.
type UpgradeExecute struct {
	client    Updater
	agentID   func() string
	onHandoff func(targetVersion string)
	delay     time.Duration
}

// UpgradeExecuteOption customizes an UpgradeExecute handler.
type UpgradeExecuteOption func(*UpgradeExecute)

// WithUpgradeNotifier registers a hook called once the handoff is accepted, so
// the heartbeat can report the agent as upgrading.
func WithUpgradeNotifier(notify func(targetVersion string)) UpgradeExecuteOption {
	return func(u *UpgradeExecute) { u.onHandoff = notify }
}

// NewUpgradeExecute creates an upgrade.execute handler. agentID is a function
// because registration may only assign one after the handler is built.
func NewUpgradeExecute(client Updater, agentID func() string, opts ...UpgradeExecuteOption) *UpgradeExecute {
	handler := &UpgradeExecute{
		client:  client,
		agentID: agentID,
		delay:   handoffRetryDelay,
	}

	for _, opt := range opts {
		opt(handler)
	}

	return handler
}

// Timeout returns the long-running class: cwa-updater refuses a handoff while it
// still holds the previous round's result, which a heartbeat has to clear first.
func (u *UpgradeExecute) Timeout() time.Duration { return LongRunning }

// Run hands the upgrade to cwa-updater.
func (u *UpgradeExecute) Run(ctx context.Context, params map[string]string) (string, error) {
	req := &upgrade.Request{
		ExecutionID:     ExecutionIDFromContext(ctx),
		AgentID:         u.agentID(),
		TargetVersion:   params[ParamTargetVersion],
		RollbackVersion: params[ParamRollbackVersion],
		ArtifactURL:     params[ParamArtifactURL],
		Checksum:        params[ParamChecksum],
		RequestedAt:     time.Now().UTC(),
	}

	// The version running here is what a rollback would restore, so an omitted
	// rollback target is not worth failing on.
	if req.RollbackVersion == "" {
		req.RollbackVersion = version.Agent()
	}

	return u.handoff(ctx, req)
}

// applied returns the upgrade result cwa-updater already holds for this command.
func (u *UpgradeExecute) applied(ctx context.Context, req *upgrade.Request) *upgrade.Result {
	view, err := u.client.Status(ctx)
	if err != nil || view == nil || view.Result == nil {
		return nil
	}

	// Guarded on a non-empty execution_id: an unset one matches a round's zero
	// entry, which would report a result this command never asked for.
	if req.ExecutionID != "" && view.AgentExecutions[req.AgentID] == req.ExecutionID {
		return view.Result
	}

	if view.Result.Status == upgrade.ResultSucceeded && view.Result.ToVersion == req.TargetVersion {
		return view.Result
	}

	return nil
}

// handoff posts the handoff, retrying until the command times out, and returns
// the result payload. Every attempt first reads what cwa-updater holds.
func (u *UpgradeExecute) handoff(ctx context.Context, req *upgrade.Request) (string, error) {
	var (
		attempts int
		lastErr  error
	)

	for ctx.Err() == nil {
		attempts++

		if applied := u.applied(ctx, req); applied != nil {
			return encode(applied, "the recorded upgrade result")
		}

		acceptance, err := u.client.Handoff(ctx, req)
		if err == nil {
			if u.onHandoff != nil {
				u.onHandoff(req.TargetVersion)
			}

			return encode(acceptance, "the handoff acceptance")
		}

		lastErr = err

		if errors.Is(err, upgrade.ErrInvalidRequest) {
			break
		}

		select {
		case <-ctx.Done():
		case <-time.After(u.delay):
		}
	}

	if lastErr == nil {
		lastErr = ctx.Err()
	}

	return "", fmt.Errorf("handing off the upgrade to %s after %d attempts: %w",
		req.TargetVersion, attempts, lastErr)
}

// encode renders a command's result payload; what names the value for the error.
func encode(payload any, what string) (string, error) {
	out, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encoding %s: %w", what, err)
	}

	return string(out), nil
}
