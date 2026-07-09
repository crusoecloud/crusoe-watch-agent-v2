package main

import (
	"context"
	"log/slog"
	"os"
	"strconv"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/vector"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/watcher"
)

const defaultVectorConfigPath = "/etc/crusoe/vector/vector.yaml"

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

// startK8sWatcher creates an in-cluster Kubernetes client and launches the
// Vector config watcher in a background goroutine. If client creation fails,
// it logs the error and returns — the agent continues in degraded mode.
func startK8sWatcher(
	ctx context.Context,
	logger *slog.Logger,
	logsEndpoint, metricsEndpoint string,
	ingestionBlocked bool,
) *watcher.Watcher {
	nodeName, err := watcher.ResolveNodeName()
	if err != nil {
		logger.Error("failed to resolve node name, k8s watcher disabled", "error", err)

		return nil
	}

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		logger.Error("failed to create in-cluster config, k8s watcher disabled", "error", err)

		return nil
	}

	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		logger.Error("failed to create kubernetes client, k8s watcher disabled", "error", err)

		return nil
	}

	k8sCfg := buildK8sConfig()
	// Seed the persisted control-plane state so the first reconcile carries it.
	k8sCfg.LogsEndpoint = logsEndpoint
	k8sCfg.MetricsEndpoint = metricsEndpoint
	k8sCfg.IngestionBlocked = ingestionBlocked

	configWatcher := watcher.New(watcher.Config{
		NodeName:   nodeName,
		ConfigPath: getEnvOrDefault("VECTOR_CONFIG_PATH", defaultVectorConfigPath),
		K8sCfg:     k8sCfg,
		Logger:     logger,
	}, client)

	go func() {
		if runErr := configWatcher.Run(ctx); runErr != nil && ctx.Err() == nil {
			logger.Error("k8s watcher stopped", "error", runErr)
		}
	}()

	logger.Info("k8s watcher launched", "node", nodeName)

	return configWatcher
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
		SinkEndpoint:               os.Getenv("CMS_ENDPOINT"),
		Proxy:                      buildProxyConfig(),
	}
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
