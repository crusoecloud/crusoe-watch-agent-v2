package upgrade

import (
	"context"
	"time"
)

// unimplementedReason is reported as the upgrade result until the Helm executor
// lands. It is deliberately a terminal failure rather than a silent no-op: the
// control plane must see that the round did not install anything.
const unimplementedReason = "upgrade execution is not implemented in this build"

// UnimplementedExecutor closes a collected upgrade without touching the cluster.
// It stands in for the Helm executor so the collection window resolves instead
// of hanging, and so the agents that handed off get a result to acknowledge.
type UnimplementedExecutor struct{}

// Execute reports the upgrade as failed, leaving the cluster untouched.
func (UnimplementedExecutor) Execute(_ context.Context, state *State) (*Result, error) {
	return &Result{
		Status:      ResultFailed,
		FromVersion: state.RollbackVersion,
		ToVersion:   state.TargetVersion,
		Reason:      unimplementedReason,
		CompletedAt: time.Now().UTC(),
	}, nil
}
