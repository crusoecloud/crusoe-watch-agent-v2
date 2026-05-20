// Package main is the entry point for cwa-manager.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/health"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/heartbeat"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/identity"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/version"
)

const (
	defaultCoordinatorAddr = "localhost:50051"
	retryDelay             = 30 * time.Second
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	coordAddr := flag.String("coordinator", defaultCoordinatorAddr, "cwa-coordinator gRPC address")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	logger.Info("cwa-manager starting", "version", version.Version)

	resolver := identity.NewResolver()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	ident, err := resolver.Resolve(ctx)
	if err != nil {
		return fmt.Errorf("resolving identity: %w", err)
	}

	logger.Info("identity resolved",
		"vm_id", ident.VMID,
		"install_type", ident.InstallType.String(),
		"agent_id", ident.AgentID,
	)

	conn, err := grpc.NewClient(*coordAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("connecting to coordinator: %w", err)
	}

	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			logger.Warn("error closing connection", "error", closeErr)
		}
	}()

	hc := health.NewCollector(logger, ident.InstallType)
	loop := heartbeat.NewLoop(ident, hc, conn, logger)

	for ctx.Err() == nil {
		if err := registerIfNeeded(ctx, ident, loop, resolver, logger); err != nil {
			if ctx.Err() != nil {
				break
			}

			logger.Error("registration failed, retrying", "error", err, "retry_in", retryDelay)
			time.Sleep(retryDelay)

			continue
		}

		logger.Info("starting heartbeat loop", "coordinator", *coordAddr)

		if err := loop.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error("heartbeat stream failed, reconnecting", "error", err, "retry_in", retryDelay)
			time.Sleep(retryDelay)
		}
	}

	logger.Info("cwa-manager shutdown complete")

	return nil
}

func registerIfNeeded(
	ctx context.Context,
	ident *identity.Identity,
	loop *heartbeat.Loop,
	resolver *identity.Resolver,
	logger *slog.Logger,
) error {
	if ident.AgentID != "" {
		return nil
	}

	logger.Info("no agent_id found, registering with coordinator")

	agentID, err := loop.Register(ctx)
	if err != nil {
		return fmt.Errorf("registration: %w", err)
	}

	ident.AgentID = agentID
	logger.Info("registered", "agent_id", agentID)

	if persistErr := resolver.PersistAgentID(agentID); persistErr != nil {
		logger.Warn("failed to persist agent_id (will re-register on restart)", "error", persistErr)
	}

	return nil
}
