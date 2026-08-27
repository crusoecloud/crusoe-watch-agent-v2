package upgrade

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPhaseTerminal(t *testing.T) {
	for _, phase := range []Phase{PhaseComplete, PhaseRolledBack, PhaseFailed} {
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
	} {
		assert.Equal(t, want, (&State{Phase: phase}).HealthStatus(), phase)
	}
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
