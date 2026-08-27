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
