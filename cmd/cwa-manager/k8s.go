package main

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/bugreport"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/bugreport/k8sexec"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/command"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/vector"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/watcher"
)

const defaultVectorConfigPath = "/etc/crusoe/shared/vector.yaml"

// hostSysModuleDir is the host sysfs cwa-manager mounts for K8s GPU detection.
const hostSysModuleDir = "/host/sys/module"

// defaultDriverNamespace is where the NVIDIA GPU Operator runs its driver pods.
const defaultDriverNamespace = "nvidia-gpu-operator"

// Default exporter ports and scrape intervals matching production v1 deployment.
const (
	defaultDCGMPort            = 9400
	defaultDCGMScrape          = 30
	defaultAMDPort             = 5000
	defaultAMDScrape           = 60
	defaultKSMPort             = 8080
	defaultKSMScrape           = 60
	defaultSlurmPort           = 6817
	defaultSlurmScrape         = 60
	defaultCMEPort             = 9500
	defaultCMEScrape           = 60
	defaultCustomMetricsPort   = 9100
	defaultCustomMetricsScrape = 30
)

// k8sRuntime holds the in-cluster client built once at startup and shared by the
// Vector config watcher and the bug-report generator (both need the API and node identity).
type k8sRuntime struct {
	client   kubernetes.Interface
	restCfg  *rest.Config
	nodeName string
}

// newK8sRuntime builds the in-cluster client and resolves the node name.
// It returns nil on any failure so the agent can continue in degraded mode.
func newK8sRuntime(logger *slog.Logger) *k8sRuntime {
	nodeName, err := watcher.ResolveNodeName()
	if err != nil {
		logger.Error("failed to resolve node name, k8s runtime disabled", "error", err)

		return nil
	}

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		logger.Error("failed to create in-cluster config, k8s runtime disabled", "error", err)

		return nil
	}

	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		logger.Error("failed to create kubernetes client, k8s runtime disabled", "error", err)

		return nil
	}

	return &k8sRuntime{client: client, restCfg: restCfg, nodeName: nodeName}
}

// startWatcher launches the Vector config watcher in a background goroutine.
func (r *k8sRuntime) startWatcher(
	ctx context.Context,
	logger *slog.Logger,
	logsEndpoint, metricsEndpoint string,
	ingestionBlocked bool,
	rateLimits map[string]int,
) *watcher.Watcher {
	k8sCfg := buildK8sConfig()
	// Seed the persisted control-plane state so the first reconcile carries it.
	k8sCfg.LogsEndpoint = logsEndpoint
	k8sCfg.MetricsEndpoint = metricsEndpoint
	k8sCfg.IngestionBlocked = ingestionBlocked
	k8sCfg.RateLimits = rateLimits

	configWatcher := watcher.New(watcher.Config{
		NodeName:   r.nodeName,
		ConfigPath: getEnvOrDefault("VECTOR_CONFIG_PATH", defaultVectorConfigPath),
		K8sCfg:     k8sCfg,
		Logger:     logger,
	}, r.client)

	go func() {
		if runErr := configWatcher.Run(ctx); runErr != nil && ctx.Err() == nil {
			logger.Error("k8s watcher stopped", "error", runErr)
		}
	}()

	logger.Info("k8s watcher launched", "node", r.nodeName)

	return configWatcher
}

// buildK8sGenerator wires the report.bug generator for K8s: operator-exec into the driver
// pod for GPU Operator NVIDIA nodes, and the bundled-driver host report-runner (over a
// unix socket) for AMD and GB200/GB300 nodes. The router picks per node at collection time.
func (r *k8sRuntime) buildK8sGenerator() command.Generator {
	gpu := vector.DetectGPUAt(hostSysModuleDir)
	outputDir := getEnvOrDefault(bugreport.EnvReportDir, bugreport.DefaultReportDir)
	driverNS := getEnvOrDefault("NVIDIA_DRIVER_NAMESPACE", defaultDriverNamespace)

	exec := k8sexec.NewExecGenerator(r.client, r.restCfg, outputDir, r.nodeName, driverNS)
	runner := bugreport.NewRunnerClient(
		getEnvOrDefault(bugreport.EnvSocketPath, bugreport.DefaultSocketPath), outputDir)

	return k8sexec.NewK8sGenerator(r.client, exec, runner, r.nodeName, gpu)
}

func buildK8sConfig() vector.K8sConfig {
	return vector.K8sConfig{
		DCGM: vector.ExporterConfig{
			Enabled:        getEnvBool("DCGM_ENABLED", true),
			Port:           getEnvInt("DCGM_PORT", defaultDCGMPort),
			Paths:          []string{"/metrics"},
			ScrapeInterval: getEnvInt("DCGM_SCRAPE_INTERVAL", defaultDCGMScrape),
		},
		AMD: vector.ExporterConfig{
			Enabled:        getEnvBool("AMD_ENABLED", true),
			Port:           getEnvInt("AMD_PORT", defaultAMDPort),
			Paths:          []string{"/metrics"},
			ScrapeInterval: getEnvInt("AMD_SCRAPE_INTERVAL", defaultAMDScrape),
		},
		KSM: vector.ExporterConfig{
			Enabled:        getEnvBool("KSM_ENABLED", true),
			Port:           getEnvInt("KSM_PORT", defaultKSMPort),
			Paths:          []string{"/metrics"},
			ScrapeInterval: getEnvInt("KSM_SCRAPE_INTERVAL", defaultKSMScrape),
		},
		Slurm: vector.ExporterConfig{
			Enabled:        getEnvBool("SLURM_ENABLED", false),
			Port:           getEnvInt("SLURM_PORT", defaultSlurmPort),
			Paths:          slurmDefaultPaths(),
			ScrapeInterval: getEnvInt("SLURM_SCRAPE_INTERVAL", defaultSlurmScrape),
		},
		CME: vector.ExporterConfig{
			Enabled:        getEnvBool("CME_ENABLED", true),
			Port:           getEnvInt("CME_PORT", defaultCMEPort),
			Paths:          []string{"/metrics"},
			ScrapeInterval: getEnvInt("CME_SCRAPE_INTERVAL", defaultCMEScrape),
		},
		CustomMetricsEnabled:       getEnvBool("CUSTOM_METRICS_ENABLED", true),
		CustomMetricsDefaultPort:   getEnvInt("CUSTOM_METRICS_DEFAULT_PORT", defaultCustomMetricsPort),
		CustomMetricsDefaultPath:   getEnvOrDefault("CUSTOM_METRICS_DEFAULT_PATH", "/metrics"),
		CustomMetricsDefaultScrape: getEnvInt("CUSTOM_METRICS_DEFAULT_SCRAPE", defaultCustomMetricsScrape),
		LogsEnabled:                getEnvBool("LOGS_ENABLED", true),
		OperatorLogNamespaces:      operatorLogNamespaces(),
		SinkEndpoint:               os.Getenv("CMS_ENDPOINT"),
		Proxy:                      buildProxyConfig(),
	}
}

// operatorLogNamespaces reads OPERATOR_LOG_NAMESPACES, a comma-separated list set by the
// chart. Both namespace names are listed per operator so either NVIDIA install layout
// matches; an explicitly empty value disables operator log collection.
func operatorLogNamespaces() []string {
	raw, set := os.LookupEnv("OPERATOR_LOG_NAMESPACES")
	if !set {
		return []string{"nvidia-gpu-operator", "gpu-operator", "nvidia-network-operator", "network-operator"}
	}

	var namespaces []string
	for _, namespace := range strings.Split(raw, ",") {
		if namespace = strings.TrimSpace(namespace); namespace != "" {
			namespaces = append(namespaces, namespace)
		}
	}

	return namespaces
}

func slurmDefaultPaths() []string {
	return []string{
		"/metrics/jobs",
		"/metrics/jobs-users-accts",
		"/metrics/nodes",
		"/metrics/partitions",
		"/metrics/scheduler",
	}
}

func buildProxyConfig() vector.ProxyConfig {
	httpProxy := os.Getenv("HTTP_PROXY")
	httpsProxy := os.Getenv("HTTPS_PROXY")

	if httpProxy == "" && httpsProxy == "" {
		return vector.ProxyConfig{}
	}

	return vector.ProxyConfig{
		Enabled: true,
		HTTP:    httpProxy,
		HTTPS:   httpsProxy,
		NoProxy: os.Getenv("NO_PROXY"),
	}
}

// ---------------------------------------------------------------------------
// Env var helpers
// ---------------------------------------------------------------------------

func getEnvOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return def
}

func getEnvBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}

	return v == "true" || v == "1"
}

func getEnvInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}

	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}

	return n
}
