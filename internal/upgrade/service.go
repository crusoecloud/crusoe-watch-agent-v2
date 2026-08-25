package upgrade

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Collection window defaults. AckTimeout is upgrade_ack_timeout_min; the chart
// overrides it. The interval and grace are fixed by Spec — Agent Deployment.
const (
	defaultAckTimeout      = 15 * time.Minute
	defaultCollectInterval = 5 * time.Second
	defaultScaleDownGrace  = 30 * time.Second
)

// collectOutcome is what one pass over the collection window decided.
type collectOutcome int

const (
	collectWaiting collectOutcome = iota
	collectComplete
	collectTimedOut
)

// Config wires a Service.
type Config struct {
	Store    Store
	Counter  ReadyCounter
	Executor Executor
	Logger   *slog.Logger

	// AckTimeout bounds the collection window. Zero uses defaultAckTimeout.
	AckTimeout time.Duration
	// CollectInterval is how often numberReady is re-queried while collecting.
	CollectInterval time.Duration
	// ScaleDownGrace is how long a lower numberReady must hold before
	// ExpectedCount follows it down.
	ScaleDownGrace time.Duration
}

// Service owns cwa-updater's upgrade state: what it recovered at startup, the
// handoffs it collects, what it serves on /health and /status, and clearing a
// result once cwa-manager has reported it.
type Service struct {
	store    Store
	counter  ReadyCounter
	executor Executor
	logger   *slog.Logger

	ackTimeout      time.Duration
	collectInterval time.Duration
	scaleDownGrace  time.Duration

	mu sync.Mutex
	// state mirrors the store, which stays authoritative; nil means idle.
	state *State
	// corrupt is non-nil when the persisted record could not be read.
	corrupt error
	// readyFloor is the reduced ready count currently serving out its grace
	// period, and readyFloorSince is when it was first seen. In memory only: a
	// restart restarts the grace period, which errs toward waiting.
	readyFloor      int
	readyFloorSince time.Time
}

// New returns a Service. Call Recover before serving, and Run to drive collection.
func New(cfg Config) *Service {
	svc := &Service{
		store:           cfg.Store,
		counter:         cfg.Counter,
		executor:        cfg.Executor,
		logger:          cfg.Logger,
		ackTimeout:      cfg.AckTimeout,
		collectInterval: cfg.CollectInterval,
		scaleDownGrace:  cfg.ScaleDownGrace,
	}

	if svc.ackTimeout <= 0 {
		svc.ackTimeout = defaultAckTimeout
	}

	if svc.collectInterval <= 0 {
		svc.collectInterval = defaultCollectInterval
	}

	if svc.scaleDownGrace <= 0 {
		svc.scaleDownGrace = defaultScaleDownGrace
	}

	return svc
}

// Recover loads any upgrade persisted by a previous cwa-updater process, so the
// first /health and /status reads reflect it rather than reporting idle. A
// terminal result is held until cwa-manager acknowledges it with DELETE /status;
// a collection window still open is resumed by Run. A record that cannot be
// read is not fatal, but a store that cannot be reached is.
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

// ---------------------------------------------------------------------------
// Acceptance
// ---------------------------------------------------------------------------

// Accept records one agent's handoff. The first POST opens the collection
// window and snapshots how many agents are expected to hand off; later POSTs
// add to the map. The write is durable before this returns: cwa-manager exits
// on the 200 and can never be asked again.
func (s *Service) Accept(ctx context.Context, req *Request) (Acceptance, error) {
	if err := req.Validate(); err != nil {
		return Acceptance{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// An unreadable record still holds an upgrade that may have been in flight.
	// Opening a window over it would overwrite it and risk a second execution.
	if s.corrupt != nil {
		return Acceptance{}, fmt.Errorf("%w: %w", ErrConflict, s.corrupt)
	}

	if s.state == nil {
		return s.openCollection(ctx, req)
	}

	// Handle duplicate from an agent already collected.
	if s.state.TargetVersion == req.TargetVersion &&
		s.state.AgentExecutions[req.AgentID] == req.ExecutionID {

		return acceptance(s.state), nil
	}

	if err := s.admit(req); err != nil {
		return Acceptance{}, err
	}

	staged := s.state.clone()
	staged.AgentExecutions[req.AgentID] = req.ExecutionID

	if err := s.commit(ctx, staged); err != nil {
		return Acceptance{}, err
	}

	s.logger.Info("handoff collected",
		"agent_id", req.AgentID, "execution_id", req.ExecutionID,
		"collected", len(staged.AgentExecutions), "expected", staged.ExpectedCount)

	return acceptance(staged), nil
}

// openCollection starts a new upgrade round from the first handoff received.
func (s *Service) openCollection(ctx context.Context, req *Request) (Acceptance, error) {
	expected, err := s.counter.ReadyCount(ctx)
	if err != nil {
		return Acceptance{}, fmt.Errorf("snapshotting ready agents: %w", err)
	}

	// The agent that just POSTed is itself proof of one ready agent, so a stale
	// zero must not close the window before anyone else can hand off.
	if expected < 1 {
		s.logger.Warn("ready count below the agent that handed off; using 1", "ready", expected)

		expected = 1
	}

	requestedAt := req.RequestedAt
	if requestedAt.IsZero() {
		requestedAt = time.Now().UTC()
	}

	staged := &State{
		Phase:           PhasePending,
		TargetVersion:   req.TargetVersion,
		RollbackVersion: req.RollbackVersion,
		ArtifactURL:     req.ArtifactURL,
		Checksum:        req.Checksum,
		AgentExecutions: map[string]string{req.AgentID: req.ExecutionID},
		ExpectedCount:   expected,
		CollectDeadline: time.Now().UTC().Add(s.ackTimeout),
		RequestedAt:     requestedAt,
	}

	if err := s.commit(ctx, staged); err != nil {
		return Acceptance{}, err
	}

	s.logger.Info("collection window opened",
		"target_version", staged.TargetVersion, "rollback_version", staged.RollbackVersion,
		"expected", expected, "deadline", staged.CollectDeadline)

	return acceptance(staged), nil
}

// admit rejects a handoff that does not belong to the open collection window.
func (s *Service) admit(req *Request) error {
	if s.state.Phase.Terminal() {
		return fmt.Errorf("%w: the result of the upgrade to %s is unacknowledged",
			ErrConflict, s.state.TargetVersion)
	}

	if s.state.Phase != PhasePending {
		return fmt.Errorf("%w: upgrade to %s is already %s; the collection window is closed",
			ErrConflict, s.state.TargetVersion, s.state.Phase)
	}

	if s.state.TargetVersion != req.TargetVersion {
		return fmt.Errorf("%w: collecting for %s, not %s",
			ErrConflict, s.state.TargetVersion, req.TargetVersion)
	}

	return nil
}

func acceptance(state *State) Acceptance {
	return Acceptance{
		Status:         StatusAccepted,
		Phase:          state.Phase,
		CollectedCount: len(state.AgentExecutions),
		ExpectedCount:  state.ExpectedCount,
	}
}

// ---------------------------------------------------------------------------
// Collection window
// ---------------------------------------------------------------------------

// Run drives the collection window until ctx is cancelled: it re-queries how
// many agents are ready, follows a sustained scale-down, and closes the window
// once every expected agent has handed off or the deadline passes. Running it
// as a loop rather than a goroutine per handoff is what lets a cwa-updater that
// restarted mid-collection pick the window back up.
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(s.collectInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.collectTick(ctx)
		}
	}
}

// collectTick advances the collection window one step.
func (s *Service) collectTick(ctx context.Context) {
	if !s.collecting() {
		return
	}

	ready, err := s.counter.ReadyCount(ctx)
	if err != nil {
		// Not fatal: the deadline still bounds the window.
		s.logger.Warn("re-querying ready agents", "error", err)
	} else {
		s.applyScaleDown(ctx, ready)
	}

	switch s.outcome() {
	case collectComplete:
		s.execute(ctx)
	case collectTimedOut:
		s.abort(ctx)
	case collectWaiting:
	}
}

// collecting reports whether a collection window is open, so an idle updater
// does not poll the API server.
func (s *Service) collecting() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.state != nil && s.state.Phase == PhasePending
}

// applyScaleDown follows a drop in ready agents that has held for the grace
// period. ExpectedCount never moves up: agents that became ready after the
// snapshot were not in scope when the control plane dispatched this round and
// will never hand off, so they are left to the next one.
func (s *Service) applyScaleDown(ctx context.Context, ready int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state == nil || s.state.Phase != PhasePending || ready >= s.state.ExpectedCount {
		s.readyFloor, s.readyFloorSince = 0, time.Time{}

		return
	}

	// A different lower count restarts the clock: only a value that holds still
	// is a real scale-down rather than a pod bouncing.
	if ready != s.readyFloor || s.readyFloorSince.IsZero() {
		s.readyFloor, s.readyFloorSince = ready, time.Now()

		return
	}

	if time.Since(s.readyFloorSince) < s.scaleDownGrace {
		return
	}

	staged := s.state.clone()
	staged.ExpectedCount = ready

	if err := s.commit(ctx, staged); err != nil {
		s.logger.Error("persisting reduced expected count", "error", err)

		return
	}

	s.logger.Info("expected handoffs reduced after sustained scale-down",
		"expected", ready, "grace", s.scaleDownGrace)

	s.readyFloor, s.readyFloorSince = 0, time.Time{}
}

// outcome reports what the open collection window has reached.
func (s *Service) outcome() collectOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state == nil || s.state.Phase != PhasePending {
		return collectWaiting
	}

	switch {
	case s.state.CollectionComplete():
		return collectComplete
	case time.Now().After(s.state.CollectDeadline):
		return collectTimedOut
	default:
		return collectWaiting
	}
}

// execute closes the window and runs the upgrade. The in_progress write lands
// first, so a crash from here on is not mistaken for a clean resume.
func (s *Service) execute(ctx context.Context) {
	state, ok := s.transition(ctx, PhaseInProgress)
	if !ok {
		return
	}

	s.logger.Info("collection complete; executing upgrade",
		"target_version", state.TargetVersion, "collected", len(state.AgentExecutions))

	result, err := s.executor.Execute(ctx, state)
	if err != nil {
		result = &Result{
			Status:      ResultFailed,
			FromVersion: state.RollbackVersion,
			ToVersion:   state.TargetVersion,
			Reason:      err.Error(),
			CompletedAt: time.Now().UTC(),
		}
	}

	s.finish(ctx, result)
}

// abort ends a window that never filled. The persisted phase is
// collection_timeout so the ConfigMap records why nothing was installed; the
// result handed to the control plane is a plain failure, which the agents that
// did hand off acknowledge in their next heartbeat.
func (s *Service) abort(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state == nil || s.state.Phase != PhasePending {
		return
	}

	staged := s.state.clone()
	staged.Phase = PhaseCollectionTimeout
	staged.Result = &Result{
		Status:      ResultFailed,
		FromVersion: staged.RollbackVersion,
		ToVersion:   staged.TargetVersion,
		Reason: fmt.Sprintf("collected %d of %d execution_ids before the %s acknowledgement timeout",
			len(staged.AgentExecutions), staged.ExpectedCount, s.ackTimeout),
		CompletedAt: time.Now().UTC(),
	}

	if err := s.commit(ctx, staged); err != nil {
		s.logger.Error("persisting collection timeout", "error", err)

		return
	}

	s.logger.Error("collection window timed out; upgrade aborted",
		"target_version", staged.TargetVersion,
		"collected", len(staged.AgentExecutions), "expected", staged.ExpectedCount)
}

// transition moves an open window to phase, returning a snapshot of the state
// it committed. The false return means another path already closed the window.
func (s *Service) transition(ctx context.Context, phase Phase) (*State, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state == nil || s.state.Phase != PhasePending {
		return nil, false
	}

	staged := s.state.clone()
	staged.Phase = phase

	if err := s.commit(ctx, staged); err != nil {
		s.logger.Error("marking upgrade in progress", "error", err)

		return nil, false
	}

	return staged.clone(), true
}

// finish records a terminal result, which is then served until acknowledged.
func (s *Service) finish(ctx context.Context, result *Result) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state == nil {
		return
	}

	staged := s.state.clone()
	staged.Phase = terminalPhase(result.Status)
	staged.Result = result

	if err := s.commit(ctx, staged); err != nil {
		s.logger.Error("persisting upgrade result", "error", err)

		return
	}

	s.logger.Info("upgrade finished",
		"result", result.Status, "target_version", staged.TargetVersion, "reason", result.Reason)
}

// terminalPhase is the phase that holds a result until it is acknowledged.
func terminalPhase(result string) Phase {
	switch result {
	case ResultSucceeded:
		return PhaseComplete
	case ResultRolledBack:
		return PhaseRolledBack
	default:
		return PhaseFailed
	}
}

// commit persists a staged change and adopts it only once the store accepted
// it, so in-memory state can never claim more than what survives a restart.
// Callers hold s.mu.
func (s *Service) commit(ctx context.Context, staged *State) error {
	if err := s.store.Save(ctx, staged); err != nil {
		return fmt.Errorf("persisting upgrade state: %w", err)
	}

	s.state = staged

	return nil
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

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
