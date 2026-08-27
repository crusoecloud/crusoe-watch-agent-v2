package upgrade

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

// Config wires a Service.
type Config struct {
	Store  Store
	Logger *slog.Logger
}

// Service owns cwa-updater's upgrade state: what it recovered at startup, what
// it serves on /health and /status, and clearing a result once cwa-manager has
// reported it. Accepting handoffs and executing upgrades build on this.
type Service struct {
	store  Store
	logger *slog.Logger

	mu sync.Mutex
	// state mirrors the store, which stays authoritative; nil means idle.
	state *State
	// corrupt is non-nil when the persisted record could not be read.
	corrupt error
}

// New returns a Service. Call Recover before serving.
func New(cfg Config) *Service {
	return &Service{store: cfg.Store, logger: cfg.Logger}
}

// Recover loads any upgrade persisted by a previous cwa-updater process, so the
// first /health and /status reads reflect it rather than reporting idle. A
// terminal result is held until cwa-manager acknowledges it with DELETE /status;
// an upgrade still in flight is served as-is. A record that cannot be read is
// not fatal, but a store that cannot be reached is.
func (s *Service) Recover(ctx context.Context) error {
	state, err := s.store.Load(ctx)

	switch {
	case errors.Is(err, ErrNoState):
		s.logger.Info("no upgrade state persisted; idle")

		return nil
	case errors.Is(err, ErrCorruptState):
		s.markCorrupt(err)

		return nil
	case err != nil:
		// A read that failed may well succeed on the next attempt, so exiting
		// and letting the restart retry is the recovery path here.
		return fmt.Errorf("loading upgrade state: %w", err)
	}

	if !state.Phase.Known() {
		s.markCorrupt(fmt.Errorf("%w: unrecognised phase %q", ErrCorruptState, state.Phase))

		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.state = state
	s.logger.Info("recovered upgrade state",
		"phase", state.Phase, "target_version", state.TargetVersion,
		"collected", len(state.AgentExecutions), "expected", state.ExpectedCount)

	return nil
}

// markCorrupt records an unreadable persisted record.
func (s *Service) markCorrupt(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.corrupt = err
	s.logger.Error("persisted upgrade state is unreadable; clear it with DELETE /status", "error", err)
}

// HealthStatus is the /health status value for the current state.
func (s *Service) HealthStatus() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case s.corrupt != nil:
		return StatusError
	case s.state == nil:
		return StatusIdle
	}

	return s.state.HealthStatus()
}

// Status is the GET /status view. ErrNoState means idle with nothing to report,
// which the handler answers 204; ErrCorruptState carries the parse failure, so
// the caller learns why rather than seeing a refused connection.
func (s *Service) Status() (StatusView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.corrupt != nil {
		return StatusView{}, s.corrupt
	}

	if s.state == nil {
		return StatusView{}, ErrNoState
	}

	// Served from a copy: the caller must not be able to reach into live state.
	snapshot := s.state.clone()

	return StatusView{
		Status:          snapshot.HealthStatus(),
		Phase:           snapshot.Phase,
		TargetVersion:   snapshot.TargetVersion,
		RollbackVersion: snapshot.RollbackVersion,
		RequestedAt:     snapshot.RequestedAt,
		AgentExecutions: snapshot.AgentExecutions,
		ExpectedCount:   snapshot.ExpectedCount,
		Result:          snapshot.Result,
	}, nil
}

// ClearStatus drops an acknowledged terminal result. It refuses while an upgrade
// is still running, so an in-flight execution can never lose its record.
func (s *Service) ClearStatus(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// An unreadable record is cleared unconditionally.
	if s.corrupt != nil {
		if err := s.store.Clear(ctx); err != nil {
			return fmt.Errorf("clearing corrupt upgrade state: %w", err)
		}

		s.logger.Warn("corrupt upgrade state cleared", "error", s.corrupt)
		s.corrupt = nil

		return nil
	}

	if s.state == nil {
		return nil
	}

	if !s.state.Phase.Terminal() {
		return fmt.Errorf("%w: upgrade to %s is %s", ErrConflict, s.state.TargetVersion, s.state.Phase)
	}

	if err := s.store.Clear(ctx); err != nil {
		return fmt.Errorf("clearing upgrade state: %w", err)
	}

	s.logger.Info("upgrade result acknowledged and cleared",
		"target_version", s.state.TargetVersion, "phase", s.state.Phase)
	s.state = nil

	return nil
}
