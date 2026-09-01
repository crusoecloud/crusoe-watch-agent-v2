package upgrade

import (
	"context"
	"errors"
)

// ErrNoState is returned by Store.Load when no upgrade is recorded, the idle case.
var ErrNoState = errors.New("no upgrade state recorded")

// ErrCorruptState is returned by Store.Load when a record exists but cannot be
// read. Unlike a read failure it never resolves on retry, so the service serves
// the error rather than crashing, and DELETE /status is the recovery path.
var ErrCorruptState = errors.New("upgrade state is corrupt")

// Store is cwa-updater's durable state: the handoff ConfigMap on Kubernetes, a
// local file on VM targets. Load returns ErrNoState when nothing is recorded.
type Store interface {
	Load(ctx context.Context) (*State, error)
	Save(ctx context.Context, state *State) error
	Clear(ctx context.Context) error
}

// ReadyCounter reports how many agents are ready to hand off. That count is the
// number of execution_ids cwa-updater must collect before it may upgrade.
type ReadyCounter interface {
	ReadyCount(ctx context.Context) (int, error)
}

// Executor installs a version and restores one. It reports only whether the
// step worked; Service owns every phase write and builds the Result, so a crash
// between the two can never leave the persisted phase disagreeing with reality.
type Executor interface {
	// Upgrade installs state.TargetVersion and confirms the agents came back.
	Upgrade(ctx context.Context, state *State) error
	// Rollback restores state.RollbackVersion. It is called both when Upgrade
	// fails and when a restart finds an upgrade interrupted mid-execution, so it
	// must tolerate a target version that was never deployed.
	Rollback(ctx context.Context, state *State) error
}
