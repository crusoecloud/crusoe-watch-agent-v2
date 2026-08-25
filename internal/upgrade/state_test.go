package upgrade

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Every declared phase must be Known, or recovering a state this build wrote
// itself would report it as corrupt.
func TestPhaseKnown(t *testing.T) {
	for _, phase := range []Phase{
		PhasePending, PhaseInProgress, PhaseRollingBack,
		PhaseComplete, PhaseRolledBack, PhaseFailed, PhaseCollectionTimeout,
	} {
		assert.True(t, phase.Known(), phase)
	}

	for _, phase := range []Phase{"", "bogus"} {
		assert.False(t, phase.Known(), phase)
	}
}

func TestPhaseTerminal(t *testing.T) {
	for _, phase := range []Phase{PhaseComplete, PhaseRolledBack, PhaseFailed, PhaseCollectionTimeout} {
		assert.True(t, phase.Terminal(), phase)
	}

	for _, phase := range []Phase{PhasePending, PhaseInProgress, PhaseRollingBack} {
		assert.False(t, phase.Terminal(), phase)
	}
}

// A successful upgrade reports idle: only rolled_back and failed are sticky.
func TestStateHealthStatus(t *testing.T) {
	for phase, want := range map[Phase]string{
		PhasePending:     StatusInProgress,
		PhaseInProgress:  StatusInProgress,
		PhaseRollingBack: StatusRollingBack,
		PhaseComplete:    StatusIdle,
		PhaseRolledBack:  StatusRolledBack,
		PhaseFailed:      StatusFailed,
		// Nothing was installed, so the round reads as failed to the control plane.
		PhaseCollectionTimeout: StatusFailed,
	} {
		assert.Equal(t, want, (&State{Phase: phase}).HealthStatus(), phase)
	}
}

func TestRequestValidate(t *testing.T) {
	valid := Request{
		ExecutionID: "cmd-1", AgentID: "agent-1",
		TargetVersion: "v2.1.0", RollbackVersion: "v2.0.3",
	}
	assert.NoError(t, valid.Validate())

	// Kubernetes omits the checksum: the OCI digest covers artifact integrity.
	assert.NoError(t, (&Request{
		ExecutionID: "cmd-1", AgentID: "agent-1",
		TargetVersion: "v2.1.0", RollbackVersion: "v2.0.3", Checksum: "",
	}).Validate())

	for field, req := range map[string]Request{
		"execution_id":     {AgentID: "a", TargetVersion: "v2.1.0", RollbackVersion: "v2.0.3"},
		"agent_id":         {ExecutionID: "c", TargetVersion: "v2.1.0", RollbackVersion: "v2.0.3"},
		"target_version":   {ExecutionID: "c", AgentID: "a", RollbackVersion: "v2.0.3"},
		"rollback_version": {ExecutionID: "c", AgentID: "a", TargetVersion: "v2.1.0"},
	} {
		err := req.Validate()
		assert.ErrorIs(t, err, ErrInvalidRequest, field)
		assert.Contains(t, err.Error(), field)
	}
}

func TestStateCollectionComplete(t *testing.T) {
	state := &State{
		ExpectedCount:   2,
		AgentExecutions: map[string]string{"agent-1": "cmd-1"},
	}
	assert.False(t, state.CollectionComplete())

	state.AgentExecutions["agent-2"] = "cmd-2"
	assert.True(t, state.CollectionComplete())

	// An unsnapshotted window is never complete, however many have handed off.
	assert.False(t, (&State{AgentExecutions: state.AgentExecutions}).CollectionComplete())
}

func TestStateCloneIsIndependent(t *testing.T) {
	state := &State{
		Phase:           PhasePending,
		AgentExecutions: map[string]string{"agent-1": "cmd-1"},
		Result:          &Result{Status: ResultFailed},
	}

	copied := state.clone()
	copied.Phase = PhaseFailed
	copied.AgentExecutions["agent-2"] = "cmd-2"
	copied.Result.Status = ResultSucceeded

	assert.Equal(t, PhasePending, state.Phase)
	assert.Len(t, state.AgentExecutions, 1)
	assert.Equal(t, ResultFailed, state.Result.Status)
}
