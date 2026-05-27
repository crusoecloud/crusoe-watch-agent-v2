// Package main is the entry point for cwa-manager.
package main

import (
	"context"
	"errors"
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
	pb "gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/proto/gen"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/vector"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/version"
)

var errUnknownGPUType = errors.New("unknown GPU type (use none, nvidia, amd)")

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
	gpuFlag := flag.String("gpu", "none", "GPU type for Vector config: none, nvidia, amd")
	enableCME := flag.Bool("cme", false, "include Crusoe Metrics Exporter in Vector config")
	dumpVectorCfg := flag.Bool("dump-vector-config", false, "print generated Vector config to stdout and exit")
	flag.Parse()

	gpuType, err := parseGPUType(*gpuFlag)
	if err != nil {
		return err
	}

	vmCfg := vector.VMConfig{GPUType: gpuType, EnableCME: *enableCME}

	if *dumpVectorCfg {
		out, genErr := vector.GenerateVM(vmCfg)
		if genErr != nil {
			return fmt.Errorf("generating vector config: %w", genErr)
		}

		if _, writeErr := os.Stdout.Write(out); writeErr != nil {
			return fmt.Errorf("writing vector config to stdout: %w", writeErr)
		}

		return nil
	}

	return runAgent(*coordAddr)
}

func runAgent(coordAddr string) error {
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

	// In K8s mode, start the Vector config watcher (pod + ConfigMap informers).
	if ident.InstallType == pb.InstallType_INSTALL_TYPE_KUBERNETES {
		startK8sWatcher(ctx, logger)
	}

	// TODO: Use TLS with JWT credentials once IMDS fetch is implemented.
	conn, err := grpc.NewClient(coordAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
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

	heartbeatLoop(ctx, coordAddr, ident, loop, resolver, logger)

	logger.Info("cwa-manager shutdown complete")

	return nil
}

func heartbeatLoop(
	ctx context.Context,
	coordAddr string,
	ident *identity.Identity,
	loop *heartbeat.Loop,
	resolver *identity.Resolver,
	logger *slog.Logger,
) {
	for ctx.Err() == nil {
		if err := registerIfNeeded(ctx, ident, loop, resolver, logger); err != nil {
			if ctx.Err() != nil {
				return
			}

			logger.Error("registration failed, retrying", "error", err, "retry_in", retryDelay)

			select {
			case <-ctx.Done():
			case <-time.After(retryDelay):
			}

			continue
		}

		logger.Info("starting heartbeat loop", "coordinator", coordAddr)

		if err := loop.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error("heartbeat stream failed, reconnecting", "error", err, "retry_in", retryDelay)

			select {
			case <-ctx.Done():
			case <-time.After(retryDelay):
			}
		}
	}
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

func parseGPUType(gpu string) (vector.GPUType, error) {
	switch gpu {
	case "none":
		return vector.GPUNone, nil
	case "nvidia":
		return vector.GPUNvidia, nil
	case "amd":
		return vector.GPUAMD, nil
	default:
		return vector.GPUNone, fmt.Errorf("%w: %q", errUnknownGPUType, gpu)
	}
}
