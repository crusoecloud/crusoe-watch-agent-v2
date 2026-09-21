// Package main is the entry point for cwa-manager.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/bugreport"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/command"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/health"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/heartbeat"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/identity"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/upgrade"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/vector"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/version"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/watcher"
	pb "gitlab.com/crusoeenergy/schemas/api/island/v2/observability"
)

var errMissingMonitoringToken = errors.New("CRUSOE_MONITORING_TOKEN is not set")

const (
	defaultCoordinatorAddr = "cwa-coordinator.crusoecloud.com:443"
	monitoringTokenEnv     = "CRUSOE_MONITORING_TOKEN"
	nodeNameEnv            = "NODE_NAME"
	retryDelay             = 30 * time.Second
	retryJitter            = 30 * time.Second

	defaultStateDir      = "/etc/crusoe"
	logsEndpointFile     = ".logs-endpoint"
	metricsEndpointFile  = ".metrics-endpoint"
	ingestionBlockedFile = ".ingestion-blocked"
	rateLimitFile        = ".rate-limit.json"
	commandStoreDir      = ".commands" // per-execution crash-recovery records
)

// bearerToken sends `authorization: Bearer <CRUSOE_MONITORING_TOKEN>` on every RPC.
type bearerToken struct{ token string }

func (b bearerToken) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}

func (bearerToken) RequireTransportSecurity() bool { return true }

func main() {
	if err := run(); err != nil {
		slog.Error("fatal error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	coordAddr := flag.String("coordinator", defaultCoordinatorAddr, "cwa-coordinator gRPC address")
	dumpVectorCfg := flag.Bool("dump-vector-config", false, "print generated Vector config to stdout and exit")
	flag.Parse()

	// The agent owns VM Vector config generation.
	vmCfg := vector.VMConfig{
		GPUType:   vector.DetectGPU(),
		EnableCME: getEnvBool("CME_ENABLED", true),
	}

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

	return runAgent(*coordAddr, vmCfg)
}

func runAgent(coordAddr string, vmCfg vector.VMConfig) error {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	logger.Info("cwa-manager starting", "version", version.Version)

	resolver := identity.NewResolver()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	ident, err := resolver.Resolve(ctx)
	if err != nil {
		return fmt.Errorf("resolving identity: %w", err)
	}

	logger.Info("identity resolved", "vm_id", ident.VMID, "install_type", ident.InstallType.String(),
		"agent_id", ident.AgentID, "region", ident.Region)

	vmCfg.DockerMode = ident.InstallType == pb.CwaInstallType_CWA_INSTALL_TYPE_DOCKER

	deps := buildDeps(ident, vmCfg)

	// On K8s, build the in-cluster client once and share it between the config watcher and the bug-report generator.
	var k8sRT *k8sRuntime
	if ident.InstallType == pb.CwaInstallType_CWA_INSTALL_TYPE_KUBERNETES {
		k8sRT = newK8sRuntime(logger)
	}

	if configWatcher := startDataPlane(ctx, deps, k8sRT, logger); configWatcher != nil {
		deps.Watcher = configWatcher
	}

	token := os.Getenv(monitoringTokenEnv)
	if token == "" {
		return errMissingMonitoringToken
	}

	conn, err := grpc.NewClient(
		coordAddr,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})),
		grpc.WithPerRPCCredentials(bearerToken{token: token}),
	)
	if err != nil {
		return fmt.Errorf("connecting to coordinator: %w", err)
	}

	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			logger.Warn("error closing connection", "error", closeErr)
		}
	}()

	loop := heartbeat.NewLoop(ident, newHealthCollector(ident, vmCfg.GPUType, logger), conn, logger)
	wireCommands(loop, deps, k8sRT, coordAddr, token, ident, logger)

	// Layer 2: recover commands a hard stop interrupted, before the loop starts.
	command.RecoverInterrupted(deps.Store, loop, logger)

	startHealthServer(ctx, ident.InstallType, loop, logger)

	heartbeatLoop(ctx, coordAddr, ident, loop, resolver, logger)
	logger.Info("cwa-manager shutdown complete")

	return nil
}

// wireCommands gives the loop its command surface: the bug-report uploader, the
// cwa-updater client, and the dispatcher.
func wireCommands(
	loop *heartbeat.Loop, deps command.Deps, k8sRT *k8sRuntime,
	coordAddr, token string, ident *identity.Identity, logger *slog.Logger,
) {
	uploadURL := "https://" + strings.TrimSuffix(coordAddr, ":443") + "/upload"
	uploader := bugreport.NewHTTPUploader(uploadURL, token, ident.VMID, os.Getenv(nodeNameEnv))

	// cwa-updater owns the upgrade; cwa-manager hands its own off and reports back.
	updater := upgrade.NewClient(
		getEnvOrDefault(upgrade.HostEnv, upgrade.DefaultHost),
		getEnvOrDefault(upgrade.PortEnv, upgrade.DefaultPort),
	)

	loop.SetUpgradeStatus(updater)
	loop.SetDispatcher(buildDispatcher(loop, deps, k8sRT, uploader, updater, ident, logger))
}

// startHealthServer serves cwa-manager's own /health in the background.
func startHealthServer(ctx context.Context, installType pb.CwaInstallType, loop *heartbeat.Loop, logger *slog.Logger) {
	// On K8s, bind all interfaces so kubelet and cwa-updater can reach the pod.
	host := "127.0.0.1"
	if installType == pb.CwaInstallType_CWA_INSTALL_TYPE_KUBERNETES {
		host = ""
	}

	addr := host + ":" + getEnvOrDefault(health.PortEnv, health.DefaultPort)
	srv := health.NewServer(addr, loop, logger)

	go func() {
		if err := srv.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error("health server stopped", "error", err)
		}
	}()
}

// newHealthCollector builds the heartbeat health collector, polling the report-runner
// only on hosts where it's deployed and passing nil to omit it elsewhere.
func newHealthCollector(ident *identity.Identity, gpu vector.GPUType, logger *slog.Logger) *health.Collector {
	if !reportRunnerExpected(ident.InstallType, gpu) {
		return health.NewCollector(logger, ident.InstallType, nil)
	}

	socket := getEnvOrDefault(bugreport.EnvSocketPath, bugreport.DefaultSocketPath)
	reportDir := getEnvOrDefault(bugreport.EnvReportDir, bugreport.DefaultReportDir)

	return health.NewCollector(logger, ident.InstallType, bugreport.NewRunnerClient(socket, reportDir))
}

// reportRunnerExpected reports whether the report-runner bug-report collector is deployed on this host.
func reportRunnerExpected(installType pb.CwaInstallType, gpu vector.GPUType) bool {
	switch installType {
	case pb.CwaInstallType_CWA_INSTALL_TYPE_SYSTEMD:
		return gpu == vector.GPUNvidia
	case pb.CwaInstallType_CWA_INSTALL_TYPE_DOCKER:
		return gpu == vector.GPUNvidia || gpu == vector.GPUAMD
	case pb.CwaInstallType_CWA_INSTALL_TYPE_KUBERNETES, pb.CwaInstallType_CWA_INSTALL_TYPE_UNSPECIFIED:
		return false
	default:
		return false
	}
}

// buildDeps assembles command.Deps, restoring the last-applied endpoints and
// blocked state so a restart doesn't revert to defaults.
func buildDeps(ident *identity.Identity, vmCfg vector.VMConfig) command.Deps {
	stateDir := getEnvOrDefault("CWA_STATE_DIR", defaultStateDir)
	logsPath := filepath.Join(stateDir, logsEndpointFile)
	metricsPath := filepath.Join(stateDir, metricsEndpointFile)
	blockedPath := filepath.Join(stateDir, ingestionBlockedFile)
	rateLimitPath := filepath.Join(stateDir, rateLimitFile)

	vmCfg.LogsEndpoint = command.LoadEndpoint(logsPath)
	vmCfg.MetricsEndpoint = command.LoadEndpoint(metricsPath)
	vmCfg.IngestionBlocked = command.LoadIngestionBlocked(blockedPath)
	vmCfg.RateLimits = command.LoadRateLimits(rateLimitPath)

	return command.Deps{
		InstallType:        ident.InstallType,
		VMCfg:              vmCfg,
		VMConfigPath:       getEnvOrDefault("VECTOR_CONFIG_PATH", defaultVectorConfigPath),
		Store:              command.NewExecStore(filepath.Join(stateDir, commandStoreDir)),
		LogsStatePath:      logsPath,
		MetricsStatePath:   metricsPath,
		BlockedStatePath:   blockedPath,
		RateLimitStatePath: rateLimitPath,
	}
}

// startDataPlane writes the VM Vector config at startup, or on K8s launches
// the watcher (returned for command wiring) that regenerates it continuously.
func startDataPlane(
	ctx context.Context, deps command.Deps, k8sRT *k8sRuntime, logger *slog.Logger,
) *watcher.Watcher {
	switch deps.InstallType {
	case pb.CwaInstallType_CWA_INSTALL_TYPE_KUBERNETES:
		if k8sRT == nil {
			return nil
		}

		return k8sRT.startWatcher(
			ctx, logger, deps.VMCfg.LogsEndpoint, deps.VMCfg.MetricsEndpoint,
			deps.VMCfg.IngestionBlocked, deps.VMCfg.RateLimits,
		)
	case pb.CwaInstallType_CWA_INSTALL_TYPE_DOCKER, pb.CwaInstallType_CWA_INSTALL_TYPE_SYSTEMD:
		if err := writeVMConfig(deps.VMCfg, deps.VMConfigPath); err != nil {
			// Degraded, not fatal: Vector keeps its previous config.
			logger.Error("failed to write vector config at startup", "error", err)
		}
	case pb.CwaInstallType_CWA_INSTALL_TYPE_UNSPECIFIED:
	}

	return nil
}

// buildDispatcher wires the command dispatcher with all command handlers.
func buildDispatcher(
	loop *heartbeat.Loop, deps command.Deps, k8sRT *k8sRuntime, uploader command.Uploader,
	updater command.Updater, ident *identity.Identity, logger *slog.Logger,
) *command.Dispatcher {
	disp := command.NewDispatcher(loop, deps.Store, logger)
	disp.Register(command.ConfigApplyCommand, command.NewConfigApply(deps))
	disp.Register(command.ConfigGetCommand, command.NewConfigGet(deps))
	disp.Register(command.IngestionBlockCommand, command.NewIngestionBlock(deps, true))
	disp.Register(command.IngestionUnblockCommand, command.NewIngestionBlock(deps, false))
	disp.Register(command.RateLimitSetCommand, command.NewRateLimitSet(deps))

	// Registered only where cwa-updater is deployed.
	if deps.InstallType == pb.CwaInstallType_CWA_INSTALL_TYPE_KUBERNETES {
		disp.Register(command.UpgradeExecuteCommand, command.NewUpgradeExecute(
			updater,
			func() string { return ident.AgentID },
			command.WithUpgradeNotifier(loop.MarkUpgradeHandedOff),
		))
	}

	// report.bug is only registered when a platform generator could be built;
	// otherwise the agent runs degraded and the command is acked as FAILED.
	if gen, opts := buildGenerator(deps, k8sRT, logger); gen != nil {
		disp.Register(command.ReportBugCommand, command.NewReportBug(gen, uploader, opts...))
	}

	return disp
}

// retryWait returns retryDelay plus a random jitter in [0, retryJitter) so a mass
// disconnect does not cause the whole fleet to reconnect and heartbeat in lockstep.
func retryWait() time.Duration {
	jitter, err := rand.Int(rand.Reader, big.NewInt(int64(retryJitter)))
	if err != nil {
		return retryDelay
	}

	return retryDelay + time.Duration(jitter.Int64())
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

			wait := retryWait()
			logger.Error("registration failed, retrying", "error", err, "retry_in", wait)

			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}

			continue
		}

		logger.Info("starting heartbeat loop", "coordinator", coordAddr)

		if err := loop.Run(ctx); err != nil && ctx.Err() == nil {
			wait := retryWait()
			logger.Error("heartbeat stream failed, reconnecting", "error", err, "retry_in", wait)

			select {
			case <-ctx.Done():
			case <-time.After(wait):
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

// writeVMConfig generates the VM Vector config and atomically writes it.
func writeVMConfig(vmCfg vector.VMConfig, path string) error {
	out, err := vector.GenerateVM(vmCfg)
	if err != nil {
		return fmt.Errorf("generating vector config: %w", err)
	}

	if err := vector.WriteConfigFile(path, out); err != nil {
		return fmt.Errorf("writing vector config: %w", err)
	}

	return nil
}
