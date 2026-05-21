// Package heartbeat implements the bidirectional streaming heartbeat loop
// between cwa-manager and cwa-coordinator.
package heartbeat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/health"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/identity"
	pb "gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/proto/gen"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/version"
)

const (
	heartbeatInterval = 30 * time.Second
)

// Loop manages the bidirectional heartbeat stream.
type Loop struct {
	identity *identity.Identity
	health   *health.Collector
	client   pb.AgentServiceClient
	logger   *slog.Logger
	mu       sync.Mutex
	// pendingResults holds command results that are re-sent on every heartbeat
	// until the coordinator stops echoing the corresponding command.
	pendingResults map[string]*pb.CommandResult
}

// NewLoop creates a heartbeat Loop.
func NewLoop(id *identity.Identity, hc *health.Collector, conn grpc.ClientConnInterface, logger *slog.Logger) *Loop {
	return &Loop{
		identity: id,
		health:   hc,
		client:   pb.NewAgentServiceClient(conn),
		logger:   logger,
	}
}

// Register calls the Register RPC to obtain an agent_id.
func (l *Loop) Register(ctx context.Context) (string, error) {
	resp, err := l.client.Register(ctx, &pb.RegisterRequest{
		VmId:           l.identity.VMID,
		InstallType:    l.identity.InstallType,
		Version:        version.Version,
		CapabilityList: []string{"heartbeat"}, // TODO: dynamic CapabilityList
	})
	if err != nil {
		return "", fmt.Errorf("register RPC: %w", err)
	}

	return resp.GetAgentId(), nil
}

// Run opens the HeartbeatStream and sends heartbeats every 30 seconds.
// It blocks until ctx is cancelled or the stream errors out.
func (l *Loop) Run(ctx context.Context) error {
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()

	stream, err := l.client.HeartbeatStream(streamCtx)
	if err != nil {
		return fmt.Errorf("opening heartbeat stream: %w", err)
	}

	// Receive goroutine: reads commands from coordinator.
	// streamCancel ensures this goroutine exits when Run returns.
	errCh := make(chan error, 1)

	go func() {
		errCh <- l.receiveLoop(stream)
	}()

	// Send loop: tick every 30s.
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	// Send first heartbeat immediately.
	if err := l.sendHeartbeat(ctx, stream); err != nil {
		return fmt.Errorf("sending initial heartbeat: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			// TODO: Graceful shutdown — wait for in-flight commands to complete,
			// send a final heartbeat with results (or interrupted status), then exit.
			if closeErr := stream.CloseSend(); closeErr != nil {
				l.logger.Warn("error closing heartbeat stream", "error", closeErr)
			}

			return fmt.Errorf("context done: %w", ctx.Err())
		case err := <-errCh:
			return fmt.Errorf("heartbeat receive loop: %w", err)
		case <-ticker.C:
			if err := l.sendHeartbeat(ctx, stream); err != nil {
				return fmt.Errorf("sending heartbeat: %w", err)
			}
		}
	}
}

func (l *Loop) sendHeartbeat(ctx context.Context, stream pb.AgentService_HeartbeatStreamClient) error {
	l.mu.Lock()
	results := make([]*pb.CommandResult, 0, len(l.pendingResults))
	for _, r := range l.pendingResults {
		results = append(results, r)
	}
	l.mu.Unlock()

	components := l.health.Collect(ctx)

	req := &pb.HeartbeatRequest{
		AgentId:           l.identity.AgentID,
		InstallType:       l.identity.InstallType,
		CapabilityList:    []string{"heartbeat"}, // TODO: dynamic CapabilityList
		AgentStatus:       deriveAgentStatus(components),
		Components:        components,
		LastUpgradeResult: nil, // TODO: Populate from cwa-updater persistence store on startup.
		CommandResults:    results,
	}

	if err := stream.Send(req); err != nil {
		return fmt.Errorf("stream send: %w", err)
	}

	l.logger.Info("heartbeat sent", "agent_id", req.GetAgentId())

	return nil
}

func (l *Loop) receiveLoop(stream pb.AgentService_HeartbeatStreamClient) error {
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("stream closed by coordinator: %w", io.EOF)
		}

		if err != nil {
			return fmt.Errorf("stream recv: %w", err)
		}

		// Build set of execution_ids the coordinator is still echoing.
		echoedIDs := make(map[string]struct{}, len(resp.GetCommands()))
		for _, cmd := range resp.GetCommands() {
			echoedIDs[cmd.GetExecutionId()] = struct{}{}
		}

		l.mu.Lock()
		// Prune results for commands no longer echoed — coordinator has acked them.
		for id := range l.pendingResults {
			if _, stillEchoed := echoedIDs[id]; !stillEchoed {
				l.logger.Debug("command acked by coordinator, dropping result", "execution_id", id)
				delete(l.pendingResults, id)
			}
		}
		l.mu.Unlock()

		// Handle new commands (ones we haven't already processed).
		for _, cmd := range resp.GetCommands() {
			l.mu.Lock()
			_, alreadyHandled := l.pendingResults[cmd.GetExecutionId()]
			l.mu.Unlock()

			if alreadyHandled {
				continue
			}

			l.logger.Info("received command",
				"execution_id", cmd.GetExecutionId(),
				"command", cmd.GetCommand(),
				"parameters", cmd.GetParameters(),
			)
			l.handleCommand(cmd)
		}
	}
}

// TODO: Set AGENT_STATUS_UPGRADE_IN_PROGRESS when upgrade dispatch is implemented.
func deriveAgentStatus(c *pb.ComponentsHealth) pb.AgentStatus {
	healthy := pb.ComponentStatus_COMPONENT_STATUS_HEALTHY

	if c.GetCwaManager().GetStatus() == healthy &&
		c.GetVector().GetStatus() == healthy &&
		c.GetCwaUpdater().GetStatus() == healthy {

		return pb.AgentStatus_AGENT_STATUS_HEALTHY
	}

	return pb.AgentStatus_AGENT_STATUS_DEGRADED
}

func (l *Loop) handleCommand(cmd *pb.Command) {
	// TODO: Real command dispatch with per-command timeouts (instant: 30s, long-running: 10min).
	// TODO: Persistence store — write {execution_id, status: "in_progress"} before execution, scan on startup.
	result := &pb.CommandResult{
		ExecutionId: cmd.GetExecutionId(),
		Command:     cmd.GetCommand(),
		Status:      pb.CommandResultStatus_COMMAND_RESULT_STATUS_SUCCEEDED,
		Reason:      "acknowledged (no-op)",
	}

	l.mu.Lock()
	if l.pendingResults == nil {
		l.pendingResults = make(map[string]*pb.CommandResult)
	}
	l.pendingResults[cmd.GetExecutionId()] = result
	l.mu.Unlock()
}
