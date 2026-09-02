// Package main is the entry point for cwa-updater.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/upgrade"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/version"
)

const (
	defaultPort = "8786"
	portEnv     = "CWA_UPDATER_PORT"

	healthPath  = "/health"
	statusPath  = "/status"
	upgradePath = "/upgrade"

	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 5 * time.Second

	defaultHandoffConfigMap = "cwa-upgrade-handoff"
	defaultAgentDaemonSet   = "crusoe-watch-agent"
	defaultAgentRelease     = "crusoe-watch-agent"
	defaultAgentChartRepo   = "oci://ghcr.io/crusoecloud/crusoe-watch-agent-v2/charts"
	defaultAgentChartName   = "crusoe-watch-agent"
	defaultAgentHealthSvc   = "crusoe-watch-agent"
	defaultAgentHealthPort  = 8787

	// maxRequestBytes caps the handoff body; a handoff is a few hundred bytes.
	maxRequestBytes = 16 << 10
)

// errMissingNamespace is fatal: without a namespace there is no handoff ConfigMap
// to read, and an updater that cannot recover its state must not report idle.
var errMissingNamespace = errors.New("POD_NAMESPACE is not set")

// healthResponse is the /health body. /health answers 200 in every upgrade
// state; the status field carries the state.
type healthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

// errorResponse carries a refusal reason.
type errorResponse struct {
	Error string `json:"error"`
}

// server holds the handler dependencies.
type server struct {
	logger  *slog.Logger
	upgrade *upgrade.Service
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	showVersion := flag.Bool("version", false, "print the cwa-updater version and exit")
	flag.Parse()

	if *showVersion {
		if _, err := os.Stdout.WriteString(version.Version + "\n"); err != nil {
			return fmt.Errorf("writing version: %w", err)
		}

		return nil
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	upgrades, err := buildUpgradeService(logger)
	if err != nil {
		return err
	}

	// Recover before serving, so the first /health and /status reads already
	// reflect any upgrade that was in flight when this process last stopped.
	if err := upgrades.Recover(ctx); err != nil {
		return fmt.Errorf("cwa-updater startup: %w", err)
	}

	// Drives the collection window, including one left open by a restart.
	go upgrades.Run(ctx)

	return serve(ctx, logger, &server{logger: logger, upgrade: upgrades})
}

// buildUpgradeService wires the Kubernetes-backed upgrade state. VM targets get
// a file-backed store when systemd and Docker packaging lands.
func buildUpgradeService(logger *slog.Logger) (*upgrade.Service, error) {
	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		return nil, errMissingNamespace
	}

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("building in-cluster config: %w", err)
	}

	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("building kubernetes client: %w", err)
	}

	configMap := getEnvOrDefault("CWA_UPDATER_HANDOFF_CONFIGMAP", defaultHandoffConfigMap)
	daemonSet := getEnvOrDefault("AGENT_DAEMONSET", defaultAgentDaemonSet)
	ackTimeout := minutesFromEnv(logger, "UPGRADE_ACK_TIMEOUT_MIN")

	logger.Info("upgrade state wired", "namespace", namespace,
		"handoff_configmap", configMap, "agent_daemonset", daemonSet, "ack_timeout", ackTimeout)

	return upgrade.New(upgrade.Config{
		Store:      upgrade.NewConfigMapStore(client, namespace, configMap),
		Counter:    upgrade.NewDaemonSetCounter(client, namespace, daemonSet),
		Executor:   buildExecutor(logger, client, namespace),
		Logger:     logger,
		AckTimeout: ackTimeout,
	}), nil
}

// buildExecutor wires the helm-backed executor and its post-upgrade health check.
func buildExecutor(logger *slog.Logger, client kubernetes.Interface, namespace string) upgrade.Executor {
	healthService := getEnvOrDefault("AGENT_HEALTH_SERVICE", defaultAgentHealthSvc)
	healthPort := portFromEnv(logger, "AGENT_HEALTH_PORT", defaultAgentHealthPort)
	rollbackWindow := minutesFromEnv(logger, "UPGRADE_ROLLBACK_WINDOW_MIN")

	cfg := upgrade.HelmConfig{
		Runner:         upgrade.NewHelmRunner(logger),
		Cosign:         upgrade.NewCosignRunner(logger),
		Verifier:       upgrade.NewEndpointVerifier(client, logger, namespace, healthService, healthPort),
		Logger:         logger,
		Namespace:      namespace,
		Release:        getEnvOrDefault("AGENT_RELEASE_NAME", defaultAgentRelease),
		ChartRepo:      getEnvOrDefault("AGENT_CHART_REPO", defaultAgentChartRepo),
		ChartName:      getEnvOrDefault("AGENT_CHART_NAME", defaultAgentChartName),
		RollbackWindow: rollbackWindow,
	}

	logger.Info("upgrade executor wired",
		"release", cfg.Release, "chart_repo", cfg.ChartRepo, "chart_name", cfg.ChartName,
		"health_service", healthService, "health_port", healthPort,
		"rollback_window", rollbackWindow)

	return upgrade.NewHelmExecutor(cfg)
}

// minutesFromEnv reads a minute-valued setting. Zero leaves the consumer on its own default.
func minutesFromEnv(logger *slog.Logger, key string) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return 0
	}

	minutes, err := strconv.Atoi(raw)
	if err != nil || minutes <= 0 {
		logger.Warn("ignoring invalid value", "key", key, "value", raw)

		return 0
	}

	return time.Duration(minutes) * time.Minute
}

// portFromEnv reads a port, falling back to def when it is unset or unusable.
func portFromEnv(logger *slog.Logger, key string, def int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}

	port, err := strconv.Atoi(raw)
	if err != nil || port <= 0 || port > math.MaxUint16 {
		logger.Warn("ignoring invalid port", "key", key, "value", raw)

		return def
	}

	return port
}

func serve(ctx context.Context, logger *slog.Logger, srv *server) error {
	port := getEnvOrDefault(portEnv, defaultPort)

	mux := http.NewServeMux()
	mux.HandleFunc(healthPath, srv.handleHealth)
	mux.HandleFunc(statusPath, srv.handleStatus)
	mux.HandleFunc(upgradePath, srv.handleUpgrade)

	httpSrv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()

		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutting down", "error", err)
		}
	}()

	logger.Info("cwa-updater listening", "addr", httpSrv.Addr, "version", version.Version)

	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving cwa-updater: %w", err)
	}

	return nil
}

// handleHealth reports the upgrade state and the running version. Always 200, so
// a long upgrade never trips the liveness probe.
func (s *server) handleHealth(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	s.respond(writer, http.StatusOK, healthResponse{
		Status:  s.upgrade.HealthStatus(),
		Version: version.Version,
	})
}

// handleStatus serves the persisted upgrade state, and clears it once cwa-manager
// has reported the result. 204 means idle with nothing to report.
func (s *server) handleStatus(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		status, err := s.upgrade.Status()

		switch {
		case errors.Is(err, upgrade.ErrNoState):
			writer.WriteHeader(http.StatusNoContent)
		case err != nil:
			s.respondError(writer, err)
		default:
			s.respond(writer, http.StatusOK, status)
		}
	case http.MethodDelete:
		if err := s.upgrade.ClearStatus(request.Context()); err != nil {
			s.respondError(writer, err)

			return
		}

		writer.WriteHeader(http.StatusNoContent)
	default:
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleUpgrade takes one agent's handoff. The 200 is what tells cwa-manager it
// may exit, so it is only written once the handoff is durably persisted.
func (s *server) handleUpgrade(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	var req upgrade.Request

	body := http.MaxBytesReader(writer, request.Body, maxRequestBytes)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		s.respondError(writer, fmt.Errorf("%w: %w", upgrade.ErrInvalidRequest, err))

		return
	}

	acceptance, err := s.upgrade.Accept(request.Context(), &req)
	if err != nil {
		s.respondError(writer, err)

		return
	}

	s.respond(writer, http.StatusOK, acceptance)
}

// respondError maps a service error onto its status code. A refusal the control
// plane should retry differs from one it should not, so the codes must be exact.
func (s *server) respondError(writer http.ResponseWriter, err error) {
	status := http.StatusInternalServerError

	switch {
	case errors.Is(err, upgrade.ErrInvalidRequest):
		status = http.StatusBadRequest
	case errors.Is(err, upgrade.ErrConflict):
		status = http.StatusConflict
	}

	s.logger.Warn("refusing request", "status", status, "error", err)
	s.respond(writer, status, errorResponse{Error: err.Error()})
}

func (s *server) respond(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)

	if err := json.NewEncoder(writer).Encode(body); err != nil {
		s.logger.Error("writing response", "error", err)
	}
}

func getEnvOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return def
}
