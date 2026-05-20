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
	"syscall"

	"github.com/google/uuid"
	"google.golang.org/grpc"

	pb "gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/proto/gen"
)

const defaultPort = "50051"

type coordinator struct {
	pb.UnimplementedAgentServiceServer
	logger *slog.Logger
}

func (c *coordinator) Register(_ context.Context, req *pb.RegisterRequest) (*pb.RegisterResponse, error) {
	agentID := uuid.New().String()
	c.logger.Info("agent registered",
		"agent_id", agentID,
		"vm_id", req.GetVmId(),
		"install_type", req.GetInstallType().String(),
		"version", req.GetVersion(),
	)

	return &pb.RegisterResponse{AgentId: agentID}, nil
}

func (c *coordinator) HeartbeatStream(stream pb.AgentService_HeartbeatStreamServer) error {
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

		// Send empty response (no commands). Test commands can be added here later.
		if err := stream.Send(&pb.HeartbeatResponse{}); err != nil {
			return fmt.Errorf("send: %w", err)
		}
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
	pb.RegisterAgentServiceServer(srv, &coordinator{logger: logger})

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
