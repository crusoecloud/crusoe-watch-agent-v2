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

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/command"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/health"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/identity"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/version"
	pb "gitlab.com/crusoeenergy/schemas/api/island/v2/observability"
)

const (
	heartbeatInterval = 30 * time.Second
	shutdownGrace     = 5 * time.Second
)

func capabilities() []string {
	return []string{
		"heartbeat",
		"config_apply",
		"ingestion_block",
		"ingestion_unblock",
	}
}

// Loop manages the bidirectional heartbeat stream.
type Loop struct {
	identity   *identity.Identity
	health     *health.Collector
	client     pb.CwaAgentClient
	logger     *slog.Logger
	dispatcher *command.Dispatcher
	mu         sync.Mutex
	// pendingResults holds command results that are re-sent on every heartbeat
	// until the coordinator stops echoing the corresponding command.
	pendingResults map[string]*pb.CwaCommandResult
}

// SetDispatcher wires the command dispatcher. The loop is constructed before
// handlers so they can be registered against a loop that already exists.
func (l *Loop) SetDispatcher(d *command.Dispatcher) {
	l.dispatcher = d
}

// DeliverResult implements command.ResultSink. It stores a finished command
// result for inclusion in the next heartbeat, re-sent until the coordinator acks.
func (l *Loop) DeliverResult(result *pb.CwaCommandResult) {
	l.mu.Lock()
	if l.pendingResults == nil {
		l.pendingResults = make(map[string]*pb.CwaCommandResult)
	}

	l.pendingResults[result.GetExecutionId()] = result
	l.mu.Unlock()
}

// NewLoop creates a heartbeat Loop.
func NewLoop(id *identity.Identity, hc *health.Collector, conn grpc.ClientConnInterface, logger *slog.Logger) *Loop {
	return &Loop{
		identity: id,
		health:   hc,
		client:   pb.NewCwaAgentClient(conn),
		logger:   logger,
	}
}

// Register calls the RegisterCwaAgent RPC to obtain an agent_id.
func (l *Loop) Register(ctx context.Context) (string, error) {
	req := &pb.RegisterCwaAgentRequest{
		VmId:           l.identity.VMID,
		InstallType:    l.identity.InstallType,
		Version:        version.Agent(),
		CapabilityList: capabilities(),
		Location:       l.identity.Region,
	}
	if l.identity.ProjectID != "" {
		req.ProjectId = &l.identity.ProjectID
	}

	resp, err := l.client.RegisterCwaAgent(ctx, req)
	if err != nil {
		return "", fmt.Errorf("register RPC: %w", err)
	}

	return resp.GetAgentId(), nil
}

// Run opens the heartbeat stream and sends heartbeats every 30 seconds.
// It blocks until ctx is cancelled or the stream errors out.
func (l *Loop) Run(ctx context.Context) error {
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()

	stream, err := l.client.CwaAgentHeartbeat(streamCtx)
	if err != nil {
		return fmt.Errorf("opening heartbeat stream: %w", err)
	}

	// Receive goroutine: reads commands from coordinator.
	// streamCancel ensures this goroutine exits when Run returns.
	errCh := make(chan error, 1)

	go func() {
		errCh <- l.receiveLoop(ctx, stream)
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
			l.shutdown(ctx, stream)

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

func (l *Loop) sendHeartbeat(ctx context.Context, stream pb.CwaAgent_CwaAgentHeartbeatClient) error {
	l.mu.Lock()
	results := make([]*pb.CwaCommandResult, 0, len(l.pendingResults))
	for _, r := range l.pendingResults {
		results = append(results, r)
	}
	l.mu.Unlock()

	components := l.health.Collect(ctx)

	req := &pb.CwaAgentHeartbeatRequest{
		AgentId:           l.identity.AgentID,
		InstallType:       l.identity.InstallType,
		Version:           version.Agent(),
		CapabilityList:    capabilities(),
		AgentStatus:       deriveAgentStatus(components),
		Components:        components,
		LastUpgradeResult: nil, // TODO: Populate from cwa-updater persistence store on startup.
		CommandResults:    results,
		Location:          l.identity.Region,
	}

	if err := stream.Send(req); err != nil {
		return fmt.Errorf("stream send: %w", err)
	}

	l.logger.Info("heartbeat sent", "agent_id", req.GetAgentId())

	return nil
}

func (l *Loop) receiveLoop(ctx context.Context, stream pb.CwaAgent_CwaAgentHeartbeatClient) error {
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
			l.handleCommand(ctx, cmd)
		}
	}
}

// TODO: Set CWA_AGENT_STATUS_UPGRADE_IN_PROGRESS when upgrade dispatch is implemented.
func deriveAgentStatus(c *pb.CwaComponentsHealth) pb.CwaAgentStatus {
	healthy := pb.CwaComponentStatus_CWA_COMPONENT_STATUS_HEALTHY

	if c.GetCwaManager().GetStatus() == healthy &&
		c.GetVector().GetStatus() == healthy &&
		c.GetCwaUpdater().GetStatus() == healthy {

		return pb.CwaAgentStatus_CWA_AGENT_STATUS_HEALTHY
	}

	return pb.CwaAgentStatus_CWA_AGENT_STATUS_DEGRADED
}

func (l *Loop) handleCommand(ctx context.Context, cmd *pb.CwaCommand) {
	if l.dispatcher == nil {
		l.logger.Warn("no dispatcher configured, ignoring command",
			"command", cmd.GetCommand(), "execution_id", cmd.GetExecutionId())

		return
	}

	l.dispatcher.Dispatch(ctx, cmd)
}

// shutdown handles graceful termination: it interrupts any in-flight commands
// (marking them INTERRUPTED), flushes a final heartbeat carrying those results
// within a short grace window, then closes the stream.
func (l *Loop) shutdown(ctx context.Context, stream pb.CwaAgent_CwaAgentHeartbeatClient) {
	if l.dispatcher != nil {
		l.dispatcher.Interrupt()
	}

	// ctx is already cancelled here; detach so the final flush gets its own
	// grace window.
	graceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()

	if err := l.sendHeartbeat(graceCtx, stream); err != nil {
		l.logger.Warn("error sending final heartbeat", "error", err)
	}

	if closeErr := stream.CloseSend(); closeErr != nil {
		l.logger.Warn("error closing heartbeat stream", "error", closeErr)
	}
}
