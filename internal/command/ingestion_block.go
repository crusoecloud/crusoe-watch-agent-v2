package command

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// Command names for pausing and restoring ingestion. Neither takes parameters.
const (
	IngestionBlockCommand   = "ingestion.block"
	IngestionUnblockCommand = "ingestion.unblock"
)

// IngestionBlock handles ingestion.block (blocked=true) and ingestion.unblock
// (blocked=false): it persists the desired state, then regenerates the Vector
// config with external sinks stripped or restored.
type IngestionBlock struct {
	deps    Deps
	blocked bool
}

// NewIngestionBlock creates a handler that moves ingestion to the given state.
func NewIngestionBlock(deps Deps, blocked bool) *IngestionBlock {
	return &IngestionBlock{deps: deps, blocked: blocked}
}

// Timeout returns the instant class (block/unblock are 30s commands).
func (b *IngestionBlock) Timeout() time.Duration { return Instant }

// Run persists the blocked state first then applies it to the running data plane.
func (b *IngestionBlock) Run(_ context.Context, _ map[string]string) error {
	if b.deps.BlockedStatePath != "" {
		if err := persistBlocked(b.deps.BlockedStatePath, b.blocked); err != nil {
			return err
		}
	}

	return b.deps.apply(
		func(w Reloader) { w.SetIngestionBlocked(b.blocked) },
		LoadEndpoint(b.deps.LogsStatePath), LoadEndpoint(b.deps.MetricsStatePath), b.blocked,
		LoadRateLimits(b.deps.RateLimitStatePath),
	)
}

// persistBlocked records the blocked state as marker-file presence: block writes, unblock removes.
func persistBlocked(path string, blocked bool) error {
	if blocked {
		if err := os.WriteFile(path, []byte("blocked\n"), stateFilePerm); err != nil {
			return fmt.Errorf("persisting blocked state: %w", err)
		}

		return nil
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clearing blocked state: %w", err)
	}

	return nil
}
