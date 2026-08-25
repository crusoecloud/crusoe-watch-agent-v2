package upgrade

import (
	"errors"
	"fmt"
	"time"
)

// Phase is the persisted position of an upgrade in its lifecycle. The two-phase
// write (PhasePending, then PhaseInProgress) is what lets a restart tell a clean
// resume from a crash mid-execution: pending means nothing was touched yet.
type Phase string

// Persisted phases.
const (
	PhasePending           Phase = "pending"
	PhaseInProgress        Phase = "in_progress"
	PhaseRollingBack       Phase = "rolling_back"
	PhaseComplete          Phase = "complete"
	PhaseRolledBack        Phase = "rolled_back"
	PhaseFailed            Phase = "failed"
	PhaseCollectionTimeout Phase = "collection_timeout"
)

// Known reports whether the phase is one this build understands.
func (p Phase) Known() bool {
	switch p {
	case PhasePending, PhaseInProgress, PhaseRollingBack,
		PhaseComplete, PhaseRolledBack, PhaseFailed, PhaseCollectionTimeout:
		return true
	}

	return false
}

// Terminal reports whether the phase is an end state, held until cwa-manager
// acknowledges the result with DELETE /status.
func (p Phase) Terminal() bool {
	switch p {
	case PhaseComplete, PhaseRolledBack, PhaseFailed, PhaseCollectionTimeout:
		return true
	case PhasePending, PhaseInProgress, PhaseRollingBack:
		return false
	}

	return false
}

// /health status values. A successful upgrade reports idle: only rolled_back and
// failed are sticky, and the transient states are never seen by the control plane
// because cwa-manager has exited by then.
const (
	StatusIdle        = "idle"
	StatusInProgress  = "in_progress"
	StatusRollingBack = "rolling_back"
	StatusRolledBack  = "rolled_back"
	StatusFailed      = "failed"
	StatusError       = "error"
)

// Upgrade result statuses, as reported to the control plane in last_upgrade_result.
const (
	ResultSucceeded  = "succeeded"
	ResultRolledBack = "rolled_back"
	ResultFailed     = "failed"
)

// StatusAccepted is the POST /upgrade response status for a persisted handoff.
const StatusAccepted = "accepted"

// ErrConflict marks a request the current upgrade state refuses.
var ErrConflict = errors.New("conflicts with current upgrade state")

// ErrInvalidRequest marks a handoff that cannot be executed or acknowledged.
var ErrInvalidRequest = errors.New("invalid upgrade request")

// Request is the POST /upgrade body. On Kubernetes every DaemonSet pod sends
// one, carrying the same upgrade but its own agent_id and execution_id.
//
//nolint:tagliatelle // snake_case is the handoff wire contract
type Request struct {
	ExecutionID     string    `json:"execution_id"`
	AgentID         string    `json:"agent_id"`
	TargetVersion   string    `json:"target_version"`
	RollbackVersion string    `json:"rollback_version"`
	ArtifactURL     string    `json:"artifact_url,omitempty"`
	Checksum        string    `json:"checksum,omitempty"`
	RequestedAt     time.Time `json:"requested_at,omitempty"`
}

// Validate rejects a handoff cwa-updater could not act on. Checksum is not
// required: on Kubernetes the OCI registry digest covers artifact integrity, so
// the control plane omits it.
func (r *Request) Validate() error {
	switch {
	case r.ExecutionID == "":
		return fmt.Errorf("%w: execution_id is required", ErrInvalidRequest)
	case r.AgentID == "":
		return fmt.Errorf("%w: agent_id is required", ErrInvalidRequest)
	case r.TargetVersion == "":
		return fmt.Errorf("%w: target_version is required", ErrInvalidRequest)
	case r.RollbackVersion == "":
		return fmt.Errorf("%w: rollback_version is required", ErrInvalidRequest)
	}

	return nil
}

// Acceptance is the POST /upgrade response. It confirms the handoff is durable,
// which is all cwa-manager needs before it exits; it is not an upgrade result.
//
//nolint:tagliatelle // snake_case is the handoff wire contract
type Acceptance struct {
	Status         string `json:"status"`
	Phase          Phase  `json:"phase"`
	CollectedCount int    `json:"collected_count"`
	ExpectedCount  int    `json:"expected_count"`
}

// StatusView is the GET /status body: the /health status plus everything
// cwa-manager needs to close the acknowledgement loop after an upgrade, namely
// its own execution_id (by agent_id) and the result to report.
//
//nolint:tagliatelle // snake_case is the handoff wire contract
type StatusView struct {
	Status          string            `json:"status"`
	Phase           Phase             `json:"phase"`
	TargetVersion   string            `json:"target_version,omitempty"`
	RollbackVersion string            `json:"rollback_version,omitempty"`
	RequestedAt     time.Time         `json:"requested_at"`
	AgentExecutions map[string]string `json:"agent_executions,omitempty"`
	ExpectedCount   int               `json:"expected_count,omitempty"`
	Result          *Result           `json:"last_upgrade_result,omitempty"`
}

// State is the durable upgrade record: the handoff ConfigMap on Kubernetes, a local file on VM targets.
//
//nolint:tagliatelle // matches the StatusView wire contract above
type State struct {
	Phase           Phase  `json:"phase"`
	TargetVersion   string `json:"target_version"`
	RollbackVersion string `json:"rollback_version"`
	ArtifactURL     string `json:"artifact_url,omitempty"`
	Checksum        string `json:"checksum,omitempty"`
	// AgentExecutions maps agent_id to execution_id, one entry per agent that handed off this upgrade.
	AgentExecutions map[string]string `json:"agent_executions"`
	// ExpectedCount is how many handoffs to collect.
	ExpectedCount   int       `json:"expected_count"`
	CollectDeadline time.Time `json:"collect_deadline"`
	RequestedAt     time.Time `json:"requested_at"`
	Result          *Result   `json:"result,omitempty"`
}

// Result is a terminal upgrade outcome, served until cwa-manager acknowledges it.
//
//nolint:tagliatelle // matches the StatusView wire contract above
type Result struct {
	Status      string    `json:"status"`
	FromVersion string    `json:"from_version"`
	ToVersion   string    `json:"to_version"`
	Reason      string    `json:"reason,omitempty"`
	CompletedAt time.Time `json:"timestamp"`
}

// HealthStatus maps the persisted phase onto a /health status value.
func (s *State) HealthStatus() string {
	switch s.Phase {
	case PhasePending, PhaseInProgress:
		return StatusInProgress
	case PhaseRollingBack:
		return StatusRollingBack
	case PhaseRolledBack:
		return StatusRolledBack
	case PhaseFailed, PhaseCollectionTimeout:
		// A window that never filled is a failed upgrade to the control plane:
		// nothing was installed, and the round has to be dispatched again.
		return StatusFailed
	case PhaseComplete:
		return StatusIdle
	}

	return StatusIdle
}

// CollectionComplete reports whether every expected agent has handed off.
func (s *State) CollectionComplete() bool {
	return s.ExpectedCount > 0 && len(s.AgentExecutions) >= s.ExpectedCount
}

// clone returns a deep copy, so a caller can stage a change and commit it only once it is durably persisted.
func (s *State) clone() *State {
	copied := *s

	copied.AgentExecutions = make(map[string]string, len(s.AgentExecutions))
	for agentID, execID := range s.AgentExecutions {
		copied.AgentExecutions[agentID] = execID
	}

	if s.Result != nil {
		result := *s.Result
		copied.Result = &result
	}

	return &copied
}
