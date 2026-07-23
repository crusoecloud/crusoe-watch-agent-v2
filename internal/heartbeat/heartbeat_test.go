package heartbeat

import (
	"context"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/command"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/health"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/identity"
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
	disp := command.NewDispatcher(l, nil, slog.Default())
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

// fakeStream is a minimal CwaAgent heartbeat client stream for exercising
// shutdown(): it records sent heartbeats and CloseSend. The embedded interface
// is nil — only Send and CloseSend are called during shutdown.
type fakeStream struct {
	pb.CwaAgent_CwaAgentHeartbeatClient

	mu     sync.Mutex
	sent   []*pb.CwaAgentHeartbeatRequest
	closed bool
}

func (f *fakeStream) Send(req *pb.CwaAgentHeartbeatRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.sent = append(f.sent, req)

	return nil
}

func (f *fakeStream) CloseSend() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.closed = true

	return nil
}

func (f *fakeStream) lastSent() *pb.CwaAgentHeartbeatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.sent) == 0 {
		return nil
	}

	return f.sent[len(f.sent)-1]
}

func (f *fakeStream) closeSendCalled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.closed
}

// blockingHandler is a long-running Handler that blocks until its context is
// cancelled — used to hold a command in flight while shutdown interrupts it.
type blockingHandler struct {
	startOnce sync.Once
	started   chan struct{}
}

func (blockingHandler) Timeout() time.Duration { return command.LongRunning }

func (h *blockingHandler) Run(ctx context.Context, _ map[string]string) error {
	h.startOnce.Do(func() { close(h.started) })
	<-ctx.Done()

	return ctx.Err()
}

// newShutdownLoop builds a Loop with the real collaborators shutdown() touches
// (health, identity) so sendHeartbeat can run.
func newShutdownLoop() *Loop {
	return &Loop{
		identity:       &identity.Identity{AgentID: "agent-test"},
		health:         health.NewCollector(slog.Default(), pb.CwaInstallType_CWA_INSTALL_TYPE_SYSTEMD),
		logger:         slog.Default(),
		pendingResults: make(map[string]*pb.CwaCommandResult),
	}
}

func TestShutdown_FlushesPendingResultsInFinalHeartbeat(t *testing.T) {
	l := newShutdownLoop()
	l.pendingResults["exec-1"] = &pb.CwaCommandResult{
		ExecutionId: "exec-1",
		Command:     "config.apply",
		Status:      pb.CwaCommandResultStatus_CWA_COMMAND_RESULT_STATUS_SUCCEEDED,
	}

	stream := &fakeStream{}
	cancelCalled := false

	l.shutdown(context.Background(), stream, func() { cancelCalled = true })

	req := stream.lastSent()
	require.NotNil(t, req, "shutdown must send a final heartbeat")
	require.Len(t, req.GetCommandResults(), 1)
	assert.Equal(t, "exec-1", req.GetCommandResults()[0].GetExecutionId())
	assert.True(t, stream.closeSendCalled(), "shutdown must close the send side")
	assert.False(t, cancelCalled, "a flush that completes within grace must not hit the abort path")
}

func TestShutdown_InterruptsInflightAndFlushesInterrupted(t *testing.T) {
	l := newShutdownLoop()

	disp := command.NewDispatcher(l, nil, slog.Default())
	h := &blockingHandler{started: make(chan struct{})}
	disp.Register("report.bug", h)
	l.SetDispatcher(disp)

	disp.Dispatch(context.Background(), &pb.CwaCommand{ExecutionId: "exec-1", Command: "report.bug"})
	<-h.started // command is in flight

	stream := &fakeStream{}
	l.shutdown(context.Background(), stream, func() {})

	req := stream.lastSent()
	require.NotNil(t, req, "shutdown must send a final heartbeat")
	require.Len(t, req.GetCommandResults(), 1, "the interrupted command's result must be flushed")
	result := req.GetCommandResults()[0]
	assert.Equal(t, "exec-1", result.GetExecutionId())
	assert.Equal(t, pb.CwaCommandResultStatus_CWA_COMMAND_RESULT_STATUS_INTERRUPTED, result.GetStatus())
	assert.True(t, stream.closeSendCalled())
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

// TestShutdownBudgetFitsPlatformGrace guards the coupling between the heartbeat
// shutdown budget (commandDrainGrace + heartbeatFlushGrace) and the post-SIGTERM
// grace each deploy target gives cwa-manager before SIGKILL. If the budget grows
// past any platform grace, shutdown would be killed mid-flush and command results
// lost. This fails the moment the two drift, so a future bump can't create that
// SIGKILL trap silently.
func TestShutdownBudgetFitsPlatformGrace(t *testing.T) {
	budget := commandDrainGrace + heartbeatFlushGrace

	cases := []struct {
		name  string
		grace time.Duration
	}{
		{"k8s daemonset", k8sGrace(t)},
		{"docker compose", extractSeconds(t,
			"../../vm/docker/docker-compose-cwa-manager.yaml", `stop_grace_period:\s*(\d+)s`)},
		{"systemd unit", extractSeconds(t,
			"../../vm/systemctl/cwa-manager.service", `TimeoutStopSec=(\d+)`)},
	}

	for _, tc := range cases {
		require.GreaterOrEqualf(t, tc.grace, budget,
			"%s grace (%s) must be >= shutdown budget (%s); raise the platform value or lower the constants",
			tc.name, tc.grace, budget)
	}
}

// k8sGrace is terminationGracePeriodSeconds minus the preStop sleep, since the
// sleep consumes part of the window before cwa-manager sees SIGTERM.
func k8sGrace(t *testing.T) time.Duration {
	const path = "../../k8s/helm-chart/templates/daemonset.yaml"
	total := extractSeconds(t, path, `terminationGracePeriodSeconds:\s*(\d+)`)
	preStop := extractSeconds(t, path, `sleep\s+(\d+)`)

	return total - preStop
}

func extractSeconds(t *testing.T, path, pattern string) time.Duration {
	data, err := os.ReadFile(path)
	require.NoError(t, err)

	m := regexp.MustCompile(pattern).FindSubmatch(data)
	require.Lenf(t, m, 2, "pattern %q not found in %s", pattern, path)

	secs, err := strconv.Atoi(string(m[1]))
	require.NoError(t, err)

	return time.Duration(secs) * time.Second
}
