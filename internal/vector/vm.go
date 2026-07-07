// VM-specific Vector config generation.

package vector

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// GPUType determines which GPU metrics source to include.
type GPUType int

const (
	GPUNone   GPUType = iota // CPU-only VM
	GPUNvidia                // NVIDIA GPU — scrapes DCGM exporter on ${DCGM_EXPORTER_PORT}
	GPUAMD                   // AMD GPU — scrapes AMD exporter on ${AMD_EXPORTER_PORT}
)

// VMConfig controls which sections appear in the generated VM Vector config.
type VMConfig struct {
	GPUType   GPUType
	EnableCME bool // Include Crusoe Metrics Exporter source/sink

	// LogsEndpoint and MetricsEndpoint, when non-empty, are control-plane
	// overrides (from config.apply): the corresponding sinks get literal
	// endpoints derived from these base URLs (<base>/logs/ingest for logs,
	// <base>/ingest for metrics — the same derivation the installer applies
	// to cms_url). They may differ, so logs and metrics can be redirected
	// independently. When empty the config keeps the
	// ${LOGS_INGRESS_ENDPOINT} / ${TELEMETRY_INGRESS_ENDPOINT} env-var
	// placeholders that Vector resolves at runtime.
	LogsEndpoint    string
	MetricsEndpoint string
}

// GenerateVMBase returns the static VM base config: data_dir, api, all sources
// except GPU/CME, all log and metrics transforms, and all standard sinks.
// This is written to /etc/vector-base/vector.yaml at startup.
func GenerateVMBase() map[string]any {
	baseCfg := defaultBaseConfig("/var/lib/vector")
	sources := ensureMap(baseCfg, "sources")
	transforms := ensureMap(baseCfg, "transforms")
	sinks := ensureMap(baseCfg, "sinks")

	// Sources
	sources["host_metrics"] = hostMetricsSource()
	sources["internal_metrics"] = internalMetricsSource()
	sources["journald_logs"] = journaldSource()
	sources["vector_internal_logs"] = map[string]any{"type": "internal_logs"}
	sources["cwa_manager_logs"] = cwaManagerLogsSource()

	// Log transforms
	transforms["parse_journald_logs"] = remapTransform([]string{"journald_logs"}, vrlParseJournaldLogs)
	transforms["parse_internal_logs"] = remapTransform([]string{"vector_internal_logs"}, vrlParseInternalLogs)
	transforms["parse_cwa_manager_logs"] = remapTransform([]string{"cwa_manager_logs"}, vrlParseCwaManagerLogs)
	transforms["enrich_logs"] = remapTransform(
		[]string{"parse_journald_logs", "parse_internal_logs", "parse_cwa_manager_logs"}, vrlEnrichLogs)

	// Metrics transforms (add_update_labels starts with host_metrics only; ApplyVM wires GPU)
	transforms["add_update_labels"] = remapTransform([]string{"host_metrics"}, vrlAddUpdateLabels)
	transforms["filter_internal_metrics"] = filterTransform([]string{"internal_metrics"}, vrlFilterInternalMetrics)
	transforms["add_internal_labels"] = remapTransform([]string{"filter_internal_metrics"}, vrlAddInternalLabels)

	// Sinks
	sinks["crusoe_ingest"] = logsSink()
	sinks["cms_gateway"] = metricsRemoteWriteSink([]string{"add_update_labels", "add_internal_labels"})
	sinks["internal_metrics_exporter"] = internalMetricsExporterSink()

	return baseCfg
}

// ApplyVM overlays dynamic VM-specific configuration onto baseCfg.
// Adds GPU source (wired into add_update_labels) and CME pipeline if enabled.
func ApplyVM(baseCfg map[string]any, cfg VMConfig) {
	sources := ensureMap(baseCfg, "sources")
	transforms := ensureMap(baseCfg, "transforms")
	sinks := ensureMap(baseCfg, "sinks")

	switch cfg.GPUType {
	case GPUNvidia:
		sources["dcgm_metrics"] = prometheusScrapeSource("http://localhost:${DCGM_EXPORTER_PORT:-9400}/metrics")
		wireIntoTransform(transforms, "add_update_labels", "dcgm_metrics")
	case GPUAMD:
		sources["amd_metrics"] = prometheusScrapeSource("http://localhost:${AMD_EXPORTER_PORT:-5000}/metrics")
		transforms["amd_allowed_filter"] = filterTransform([]string{"amd_metrics"}, vrlAmdAllowlistFilter)
		wireIntoTransform(transforms, "add_update_labels", "amd_allowed_filter")
	case GPUNone:
		// no GPU source
	}

	if cfg.EnableCME {
		sources["crusoe_infra_metrics"] = prometheusScrapeSource(
			"http://localhost:${CRUSOE_METRICS_EXPORTER_PORT:-9500}/metrics")
		transforms["enrich_crusoe_infra_metrics"] = remapTransform(
			[]string{"crusoe_infra_metrics"}, vrlEnrichCMEMetrics,
		)
		sinks["cms_gateway_cme"] = metricsRemoteWriteSink([]string{"enrich_crusoe_infra_metrics"})
	}

	applyEndpointOverrides(sinks, cfg)
}

// applyEndpointOverrides replaces the ${...} env-var placeholders in the logs
// and metrics sinks with literal endpoints derived from the control-plane
// base URLs. An empty override leaves that sink's placeholder untouched.
func applyEndpointOverrides(sinks map[string]any, cfg VMConfig) {
	if cfg.LogsEndpoint != "" {
		if s, ok := sinks["crusoe_ingest"].(map[string]any); ok {
			s["uri"] = cfg.LogsEndpoint + "/logs/ingest"
		}
	}

	if cfg.MetricsEndpoint != "" {
		// Both the node-metrics sink and the CME sink target the telemetry endpoint.
		for _, name := range []string{"cms_gateway", "cms_gateway_cme"} {
			if s, ok := sinks[name].(map[string]any); ok {
				s["endpoint"] = cfg.MetricsEndpoint + "/ingest"
			}
		}
	}
}

// GenerateVM returns complete Vector configuration YAML for VM mode.
// It creates a base config, applies the VM overlay, and marshals to YAML.
func GenerateVM(cfg VMConfig) ([]byte, error) {
	baseCfg := GenerateVMBase()
	ApplyVM(baseCfg, cfg)

	out, err := yaml.Marshal(baseCfg)
	if err != nil {
		return nil, fmt.Errorf("marshaling vector config: %w", err)
	}

	return out, nil
}

// ---------------------------------------------------------------------------
// VM-only sources
// ---------------------------------------------------------------------------

func journaldSource() map[string]any {
	return map[string]any{
		"type":              "journald",
		"journal_directory": "/var/log/journal",
		"since_now":         true,
		"exclude_units": []string{
			"crusoe-watch-agent.service",
			"crusoe-watch-agent-native.service",
			"cwa-manager.service",
		},
	}
}

func cwaManagerLogsSource() map[string]any {
	return map[string]any{
		"type":              "journald",
		"journal_directory": "/var/log/journal",
		"since_now":         true,
		"include_units": []string{
			"cwa-manager.service",
		},
	}
}

func prometheusScrapeSource(endpoint string) map[string]any {
	return map[string]any{
		"type":                 "prometheus_scrape",
		"endpoints":            []string{endpoint},
		"scrape_interval_secs": scrapeIntervalSecs,
		"scrape_timeout_secs":  scrapeTimeoutSecs,
	}
}

// ---------------------------------------------------------------------------
// VM-only sinks
// ---------------------------------------------------------------------------

func logsSink() map[string]any {
	return map[string]any{
		"type":        "http",
		"inputs":      []string{"enrich_logs"},
		"uri":         "${LOGS_INGRESS_ENDPOINT}",
		"framing":     map[string]any{"method": "newline_delimited"},
		"compression": "snappy",
		"healthcheck": map[string]any{"enabled": false},
		"request": map[string]any{
			"headers": map[string]any{
				"X-Crusoe-Vm-Id": "${VM_ID}",
				"User-Agent":     "CrusoeWatchAgent/VM-${AGENT_VERSION}",
			},
			"timeout_secs": requestTimeoutSecs,
		},
		"auth": map[string]any{
			"strategy": "bearer",
			"token":    "${CRUSOE_AUTH_TOKEN}", // TODO: Replace with JWT once IMDS fetch is implemented.
		},
		"encoding": map[string]any{"codec": "json"},
		"batch":    map[string]any{"max_bytes": logBatchMaxBytes},
		"tls":      tlsConfig(false),
	}
}

func metricsRemoteWriteSink(inputs []string) map[string]any {
	return map[string]any{
		"type":        "prometheus_remote_write",
		"inputs":      inputs,
		"endpoint":    "${TELEMETRY_INGRESS_ENDPOINT}",
		"tenant_id":   "cri:vm/${VM_ID}",
		"auth":        map[string]any{"strategy": "bearer", "token": "${CRUSOE_AUTH_TOKEN}"}, // TODO: Replace with JWT
		"healthcheck": map[string]any{"enabled": false},
		"request":     map[string]any{"concurrency": "adaptive", "timeout_secs": requestTimeoutSecs},
		"batch":       map[string]any{"max_bytes": metricBatchMaxBytes, "aggregate": false},
		"buffer":      diskBufferConfig(),
		"compression": "snappy",
		"tls":         tlsConfig(false),
	}
}
