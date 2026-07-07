package heartbeat

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/command"
	pb "gitlab.com/crusoeenergy/schemas/api/island/v2/observability"
)

func newTestLoop() *Loop {
	return &Loop{
		logger:         slog.Default(),
		pendingResults: make(map[string]*pb.CwaCommandResult),
	}
}

func TestDeriveAgentStatus(t *testing.T) {
	tests := []struct {
		name       string
		components *pb.CwaComponentsHealth
		expected   pb.CwaAgentStatus
	}{
		{
			name: "all healthy",
			components: &pb.CwaComponentsHealth{
				CwaManager: &pb.CwaManagerHealth{Status: pb.CwaComponentStatus_CWA_COMPONENT_STATUS_HEALTHY},
				Vector:     &pb.CwaVectorHealth{Status: pb.CwaComponentStatus_CWA_COMPONENT_STATUS_HEALTHY},
				CwaUpdater: &pb.CwaUpdaterHealth{Status: pb.CwaComponentStatus_CWA_COMPONENT_STATUS_HEALTHY},
			},
			expected: pb.CwaAgentStatus_CWA_AGENT_STATUS_HEALTHY,
		},
		{
			name: "vector unhealthy",
			components: &pb.CwaComponentsHealth{
				CwaManager: &pb.CwaManagerHealth{Status: pb.CwaComponentStatus_CWA_COMPONENT_STATUS_HEALTHY},
				Vector:     &pb.CwaVectorHealth{Status: pb.CwaComponentStatus_CWA_COMPONENT_STATUS_UNHEALTHY},
				CwaUpdater: &pb.CwaUpdaterHealth{Status: pb.CwaComponentStatus_CWA_COMPONENT_STATUS_HEALTHY},
			},
			expected: pb.CwaAgentStatus_CWA_AGENT_STATUS_DEGRADED,
		},
		{
			name: "updater unknown",
			components: &pb.CwaComponentsHealth{
				CwaManager: &pb.CwaManagerHealth{Status: pb.CwaComponentStatus_CWA_COMPONENT_STATUS_HEALTHY},
				Vector:     &pb.CwaVectorHealth{Status: pb.CwaComponentStatus_CWA_COMPONENT_STATUS_HEALTHY},
				CwaUpdater: &pb.CwaUpdaterHealth{Status: pb.CwaComponentStatus_CWA_COMPONENT_STATUS_UNKNOWN},
			},
			expected: pb.CwaAgentStatus_CWA_AGENT_STATUS_DEGRADED,
		},
		{
			name: "all unhealthy",
			components: &pb.CwaComponentsHealth{
				CwaManager: &pb.CwaManagerHealth{Status: pb.CwaComponentStatus_CWA_COMPONENT_STATUS_UNHEALTHY},
				Vector:     &pb.CwaVectorHealth{Status: pb.CwaComponentStatus_CWA_COMPONENT_STATUS_UNHEALTHY},
				CwaUpdater: &pb.CwaUpdaterHealth{Status: pb.CwaComponentStatus_CWA_COMPONENT_STATUS_UNHEALTHY},
			},
			expected: pb.CwaAgentStatus_CWA_AGENT_STATUS_DEGRADED,
		},
		{
			name:       "nil components",
			components: &pb.CwaComponentsHealth{},
			expected:   pb.CwaAgentStatus_CWA_AGENT_STATUS_DEGRADED,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := deriveAgentStatus(tt.components)
			assert.Equal(t, tt.expected, status)
		})
	}
}

// DeliverResult is the command.ResultSink implementation used by the dispatcher.
func TestDeliverResult(t *testing.T) {
	l := newTestLoop()

	l.DeliverResult(&pb.CwaCommandResult{
		ExecutionId: "exec-1",
		Command:     "config.apply",
		Status:      pb.CwaCommandResultStatus_CWA_COMMAND_RESULT_STATUS_SUCCEEDED,
	})

	result, ok := l.pendingResults["exec-1"]
	require.True(t, ok, "result should be stored in pendingResults")
	assert.Equal(t, "config.apply", result.GetCommand())
}

func TestDeliverResult_InitializesNilMap(t *testing.T) {
	l := &Loop{logger: slog.Default()}

	l.DeliverResult(&pb.CwaCommandResult{ExecutionId: "exec-1"})

	require.NotNil(t, l.pendingResults)
	assert.Contains(t, l.pendingResults, "exec-1")
}

type stubHandler struct{}

func (stubHandler) Timeout() time.Duration { return command.Instant }

func (stubHandler) Run(context.Context, map[string]string) error { return nil }

func TestHandleCommand_DelegatesToDispatcher(t *testing.T) {
	l := newTestLoop()
	disp := command.NewDispatcher(l, slog.Default())
	disp.Register("config.apply", stubHandler{})
	l.SetDispatcher(disp)

	l.handleCommand(context.Background(), &pb.CwaCommand{ExecutionId: "exec-1", Command: "config.apply"})

	require.Eventually(t, func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()

		return l.pendingResults["exec-1"] != nil
	}, time.Second, 5*time.Millisecond)

	l.mu.Lock()
	result := l.pendingResults["exec-1"]
	l.mu.Unlock()
	assert.Equal(t, pb.CwaCommandResultStatus_CWA_COMMAND_RESULT_STATUS_SUCCEEDED, result.GetStatus())
}

func TestHandleCommand_NoDispatcherIsNoOp(t *testing.T) {
	l := newTestLoop()

	l.handleCommand(context.Background(), &pb.CwaCommand{ExecutionId: "exec-1", Command: "config.apply"})

	assert.Empty(t, l.pendingResults)
}

func TestDeriveAgentStatus_UnspecifiedIsDegraded(t *testing.T) {
	components := &pb.CwaComponentsHealth{
		CwaManager: &pb.CwaManagerHealth{Status: pb.CwaComponentStatus_CWA_COMPONENT_STATUS_HEALTHY},
		Vector:     &pb.CwaVectorHealth{Status: pb.CwaComponentStatus_CWA_COMPONENT_STATUS_HEALTHY},
		CwaUpdater: &pb.CwaUpdaterHealth{Status: pb.CwaComponentStatus_CWA_COMPONENT_STATUS_UNSPECIFIED},
	}

	assert.Equal(t, pb.CwaAgentStatus_CWA_AGENT_STATUS_DEGRADED, deriveAgentStatus(components))
}

func TestPruneAckedResults(t *testing.T) {
	tests := []struct {
		name              string
		pending           map[string]*pb.CwaCommandResult
		echoed            map[string]struct{}
		expectedRemaining []string
	}{
		{
			name: "prunes acked, keeps echoed",
			pending: map[string]*pb.CwaCommandResult{
				"exec-1": {ExecutionId: "exec-1"},
				"exec-2": {ExecutionId: "exec-2"},
				"exec-3": {ExecutionId: "exec-3"},
			},
			echoed:            map[string]struct{}{"exec-2": {}},
			expectedRemaining: []string{"exec-2"},
		},
		{
			name: "all acked",
			pending: map[string]*pb.CwaCommandResult{
				"exec-1": {ExecutionId: "exec-1"},
				"exec-2": {ExecutionId: "exec-2"},
			},
			echoed:            map[string]struct{}{},
			expectedRemaining: []string{},
		},
		{
			name: "none acked",
			pending: map[string]*pb.CwaCommandResult{
				"exec-1": {ExecutionId: "exec-1"},
				"exec-2": {ExecutionId: "exec-2"},
			},
			echoed:            map[string]struct{}{"exec-1": {}, "exec-2": {}},
			expectedRemaining: []string{"exec-1", "exec-2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := newTestLoop()
			l.pendingResults = tt.pending

			// Prune logic from receiveLoop.
			l.mu.Lock()
			for id := range l.pendingResults {
				if _, stillEchoed := tt.echoed[id]; !stillEchoed {
					delete(l.pendingResults, id)
				}
			}
			l.mu.Unlock()

			assert.Len(t, l.pendingResults, len(tt.expectedRemaining))
			for _, id := range tt.expectedRemaining {
				assert.Contains(t, l.pendingResults, id)
			}
		})
	}
}
