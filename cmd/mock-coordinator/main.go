// Package main implements a mock cwa-coordinator for local development.
// It accepts Register and HeartbeatStream RPCs and logs everything to stdout.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

	"github.com/google/uuid"
	"google.golang.org/grpc"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/command"
	pb "gitlab.com/crusoeenergy/schemas/api/island/v2/observability"
)

const defaultPort = "50051"

type coordinator struct {
	pb.UnimplementedCwaAgentServer
	logger *slog.Logger

	// testCmd, when non-nil, is echoed on every heartbeat until the agent
	// reports a result for it. Built from SEND_* env vars at startup.
	testCmd      *pb.CwaCommand
	commandAcked atomic.Bool
}

// buildTestCommand returns the single command to echo, selected by env var,
// or nil when none is set. Restart the mock to send another command.
// Endpoint vars combine into one config.apply: SEND_CONFIG_APPLY=<base>
// (both sink kinds), SEND_LOGS_ENDPOINT=<base>, SEND_METRICS_ENDPOINT=<base>.
// SEND_INGESTION_BLOCK=1 / SEND_INGESTION_UNBLOCK=1 send the parameterless
// block and unblock commands.
func buildTestCommand() *pb.CwaCommand {
	params := map[string]string{}

	for env, param := range map[string]string{
		"SEND_CONFIG_APPLY":     command.ParamIngestionEndpoint,
		"SEND_LOGS_ENDPOINT":    command.ParamLogsEndpoint,
		"SEND_METRICS_ENDPOINT": command.ParamMetricsEndpoint,
	} {
		if v := os.Getenv(env); v != "" {
			params[param] = v
		}
	}

	if len(params) > 0 {
		return &pb.CwaCommand{
			ExecutionId: "mock-config-apply-1",
			Command:     command.ConfigApplyCommand,
			Parameters:  params,
		}
	}

	if os.Getenv("SEND_INGESTION_BLOCK") != "" {
		return &pb.CwaCommand{ExecutionId: "mock-ingestion-block-1", Command: command.IngestionBlockCommand}
	}

	if os.Getenv("SEND_INGESTION_UNBLOCK") != "" {
		return &pb.CwaCommand{ExecutionId: "mock-ingestion-unblock-1", Command: command.IngestionUnblockCommand}
	}

	return nil
}

func (c *coordinator) RegisterCwaAgent(
	_ context.Context,
	req *pb.RegisterCwaAgentRequest,
) (*pb.RegisterCwaAgentResponse, error) {
	agentID := uuid.New().String()
	c.logger.Info("agent registered",
		"agent_id", agentID,
		"vm_id", req.GetVmId(),
		"install_type", req.GetInstallType().String(),
		"version", req.GetVersion(),
	)

	return &pb.RegisterCwaAgentResponse{AgentId: agentID}, nil
}

func (c *coordinator) CwaAgentHeartbeat(stream pb.CwaAgent_CwaAgentHeartbeatServer) error {
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("recv: %w", err)
		}

		c.logger.Info("heartbeat received",
			"agent_id", req.GetAgentId(),
			"status", req.GetAgentStatus().String(),
			"cwa_manager", req.GetComponents().GetCwaManager().GetStatus().String(),
			"vector", req.GetComponents().GetVector().GetStatus().String(),
			"cwa_updater", req.GetComponents().GetCwaUpdater().GetStatus().String(),
			"command_results", len(req.GetCommandResults()),
		)

		resp := &pb.CwaAgentHeartbeatResponse{}
		if testCmd := c.testCommand(req); testCmd != nil {
			resp.Commands = []*pb.CwaCommand{testCmd}
			c.logger.Info("echoing test command",
				"execution_id", testCmd.GetExecutionId(),
				"command", testCmd.GetCommand(),
				"parameters", testCmd.GetParameters(),
			)
		}

		if err := stream.Send(resp); err != nil {
			return fmt.Errorf("send: %w", err)
		}
	}
}

// testCommand returns the test command to echo, or nil once the agent has
// reported a result for it. Echoing until the result arrives (then stopping)
// exercises the agent's full ACK loop: dedup of the re-echoed command, result
// re-send, and pending-result pruning.
func (c *coordinator) testCommand(req *pb.CwaAgentHeartbeatRequest) *pb.CwaCommand {
	if c.testCmd == nil {
		return nil
	}

	for _, result := range req.GetCommandResults() {
		if result.GetExecutionId() != c.testCmd.GetExecutionId() {
			continue
		}

		if c.commandAcked.CompareAndSwap(false, true) {
			c.logger.Info("command result received",
				"execution_id", result.GetExecutionId(),
				"command", result.GetCommand(),
				"status", result.GetStatus().String(),
				"reason", result.GetReason(),
			)
		}
	}

	// Once acked, never echo again — the agent prunes the result as soon as
	// the echo stops, and re-echoing after that would re-execute the command.
	if c.commandAcked.Load() {
		return nil
	}

	return c.testCmd
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	port := os.Getenv("MOCK_COORDINATOR_PORT")
	if port == "" {
		port = defaultPort
	}

	lis, err := net.Listen("tcp", ":"+port)
	if err != nil {
		logger.Error("failed to listen", "error", err)
		os.Exit(1)
	}

	srv := grpc.NewServer()
	pb.RegisterCwaAgentServer(srv, &coordinator{
		logger:  logger,
		testCmd: buildTestCommand(),
	})

	// Shutdown on SIGINT/SIGTERM. Stop (not GracefulStop) because the
	// heartbeat stream never completes, so a graceful drain would hang until
	// the agent disconnects.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		logger.Info("shutting down", "signal", sig.String())
		srv.Stop()
	}()

	logger.Info("mock coordinator listening", "port", port)

	if err := srv.Serve(lis); err != nil {
		logger.Error("server error", "error", err)
		os.Exit(1)
	}
}
