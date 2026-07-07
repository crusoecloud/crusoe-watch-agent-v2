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

// testExecutionID identifies the single config.apply command this mock echoes
// when any SEND_* env var is set.
const testExecutionID = "mock-config-apply-1"

type coordinator struct {
	pb.UnimplementedCwaAgentServer
	logger *slog.Logger

	// testParams, when non-nil, makes the mock echo a config.apply command
	// with these parameters on every heartbeat until the agent reports a
	// result for it.
	testParams   map[string]string
	commandAcked atomic.Bool
}

// buildTestParams assembles config.apply parameters from env vars:
// SEND_CONFIG_APPLY=<base> (both sink kinds), SEND_LOGS_ENDPOINT=<base>,
// SEND_METRICS_ENDPOINT=<base>. Returns nil when none are set.
func buildTestParams() map[string]string {
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

	if len(params) == 0 {
		return nil
	}

	return params
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
			c.logger.Info("echoing config.apply command",
				"execution_id", testExecutionID,
				"parameters", c.testParams,
			)
		}

		if err := stream.Send(resp); err != nil {
			return fmt.Errorf("send: %w", err)
		}
	}
}

// testCommand returns the config.apply command to echo, or nil once the agent
// has reported a result for it. Echoing until the result arrives (then
// stopping) exercises the agent's full ACK loop: dedup of the re-echoed
// command, result re-send, and pending-result pruning.
func (c *coordinator) testCommand(req *pb.CwaAgentHeartbeatRequest) *pb.CwaCommand {
	if c.testParams == nil {
		return nil
	}

	for _, result := range req.GetCommandResults() {
		if result.GetExecutionId() != testExecutionID {
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

	return &pb.CwaCommand{
		ExecutionId: testExecutionID,
		Command:     command.ConfigApplyCommand,
		Parameters:  c.testParams,
	}
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
		logger:     logger,
		testParams: buildTestParams(),
	})

	// Graceful shutdown on SIGINT/SIGTERM.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		logger.Info("shutting down", "signal", sig.String())
		srv.GracefulStop()
	}()

	logger.Info("mock coordinator listening", "port", port)

	if err := srv.Serve(lis); err != nil {
		logger.Error("server error", "error", err)
		os.Exit(1)
	}
}
