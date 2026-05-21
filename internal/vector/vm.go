// Package vector generates Vector configuration YAML for VM.
package vector

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// GPUType determines which GPU metrics source to include.
type GPUType int

const (
	GPUNone   GPUType = iota // CPU-only VM
	GPUNvidia                // NVIDIA GPU — scrapes DCGM exporter on port 9400
	GPUAMD                   // AMD GPU — scrapes AMD exporter on ${AMD_EXPORTER_PORT}
)

const (
	scrapeIntervalSecs  = 60
	scrapeTimeoutSecs   = 50
	logBatchMaxBytes    = 100000
	metricBatchMaxBytes = 500000
	requestTimeoutSecs  = 15
	diskBufferMaxSize   = 268435488 // 256 MiB
)

// VMConfig controls which sections appear in the generated VM Vector config.
type VMConfig struct {
	GPUType   GPUType
	EnableCME bool // Include Crusoe Metrics Exporter source/sink
}

// vectorConfig is the top-level Vector configuration structure.
// Field order is preserved in YAML output via struct ordering.
// GenerateVM returns Vector configuration YAML for VM mode.
func GenerateVM(cfg VMConfig) ([]byte, error) {
	vecCfg := map[string]any{
		"data_dir": "/var/lib/vector",
		"api": map[string]any{
			"enabled": true,
			"address": "127.0.0.1:8686",
		},
		"sources":    vmSources(cfg),
		"transforms": vmTransforms(cfg),
		"sinks":      vmSinks(cfg),
	}

	out, err := yaml.Marshal(vecCfg)
	if err != nil {
		return nil, fmt.Errorf("marshaling vector config: %w", err)
	}

	return out, nil
}

// ---------- sources ----------

func vmSources(cfg VMConfig) map[string]any {
	sources := map[string]any{
		"host_metrics":         hostMetricsSource(),
		"internal_metrics":     internalMetricsSource(),
		"journald_logs":        journaldSource(),
		"vector_internal_logs": map[string]any{"type": "internal_logs"},
	}

	switch cfg.GPUType {
	case GPUNvidia:
		sources["dcgm_metrics"] = prometheusScrapeSource("http://localhost:9400/metrics")
	case GPUAMD:
		sources["amd_metrics"] = prometheusScrapeSource("http://localhost:${AMD_EXPORTER_PORT}/metrics")
	case GPUNone:
		// no GPU source
	}

	if cfg.EnableCME {
		sources["crusoe_infra_metrics"] = prometheusScrapeSource("http://localhost:9500/metrics")
	}

	return sources
}

func hostMetricsSource() map[string]any {
	return map[string]any{
		"type":       "host_metrics",
		"collectors": []string{"cpu", "disk", "host", "memory", "network", "process"},
		"network": map[string]any{
			"devices": map[string]any{
				"excludes": []string{"lo*"},
				"includes": []string{"ens*"},
			},
		},
		"process": map[string]any{
			"processes": map[string]any{
				"includes": []string{"vector"},
			},
		},
		"scrape_interval_secs": scrapeIntervalSecs,
	}
}

func internalMetricsSource() map[string]any {
	return map[string]any{
		"type":                 "internal_metrics",
		"scrape_interval_secs": scrapeIntervalSecs,
	}
}

func journaldSource() map[string]any {
	return map[string]any{
		"type":              "journald",
		"journal_directory": "/var/log/journal",
		"since_now":         true,
		"exclude_units": []string{
			"crusoe-watch-agent.service",
			"crusoe-watch-agent-native.service",
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

// ---------- transforms ----------

func vmTransforms(cfg VMConfig) map[string]any {
	metricsLabelInputs := []string{"host_metrics"}

	switch cfg.GPUType {
	case GPUNvidia:
		metricsLabelInputs = append(metricsLabelInputs, "dcgm_metrics")
	case GPUAMD:
		metricsLabelInputs = append(metricsLabelInputs, "amd_metrics")
	case GPUNone:
		// host_metrics only
	}

	transforms := map[string]any{
		// Log pipeline
		"parse_journald_logs": remapTransform([]string{"journald_logs"}, vrlParseJournaldLogs),
		"parse_internal_logs": remapTransform([]string{"vector_internal_logs"}, vrlParseInternalLogs),
		"enrich_logs":         remapTransform([]string{"parse_journald_logs", "parse_internal_logs"}, vrlEnrichLogs),

		// Metrics pipeline
		"add_update_labels":       remapTransform(metricsLabelInputs, vrlAddUpdateLabels),
		"filter_internal_metrics": filterTransform([]string{"internal_metrics"}, vrlFilterInternalMetrics),
		"add_internal_labels":     remapTransform([]string{"filter_internal_metrics"}, vrlAddInternalLabels),
	}

	if cfg.EnableCME {
		transforms["enrich_crusoe_infra_metrics"] = remapTransform(
			[]string{"crusoe_infra_metrics"}, vrlEnrichCMEMetrics,
		)
	}

	return transforms
}

func remapTransform(inputs []string, source string) map[string]any {
	return map[string]any{
		"type":   "remap",
		"inputs": inputs,
		"source": source,
	}
}

func filterTransform(inputs []string, condition string) map[string]any {
	return map[string]any{
		"type":      "filter",
		"inputs":    inputs,
		"condition": condition,
	}
}

// ---------- sinks ----------

func vmSinks(cfg VMConfig) map[string]any {
	sinks := map[string]any{
		"crusoe_ingest":             logsSink(),
		"cms_gateway":               metricsRemoteWriteSink([]string{"add_update_labels", "add_internal_labels"}),
		"internal_metrics_exporter": internalMetricsExporterSink(),
	}

	if cfg.EnableCME {
		sinks["cms_gateway_cme"] = metricsRemoteWriteSink([]string{"enrich_crusoe_infra_metrics"})
	}

	return sinks
}

func logsSink() map[string]any {
	return map[string]any{
		"type":        "http",
		"inputs":      []string{"enrich_logs"},
		"uri":         "${LOGS_INGRESS_ENDPOINT}",
		"framing":     map[string]any{"method": "newline_delimited"},
		"compression": "snappy",
		"healthcheck": map[string]any{"enabled": false},
		"request": map[string]any{
			"headers":      map[string]any{"X-Crusoe-Vm-Id": "${VM_ID}"},
			"timeout_secs": requestTimeoutSecs,
		},
		"auth": map[string]any{
			"strategy": "bearer",
			"token":    "${CRUSOE_AUTH_TOKEN}", // TODO: Replace with JWT once IMDS fetch is implemented.
		},
		"encoding": map[string]any{"codec": "json"},
		"batch":    map[string]any{"max_bytes": logBatchMaxBytes},
		"tls":      tlsConfig(),
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
		"tls":         tlsConfig(),
	}
}

// internalMetricsExporterSink exposes Vector's internal metrics on port 9598
// so cwa-manager can scrape component_errors_total for health assessment.
func internalMetricsExporterSink() map[string]any {
	return map[string]any{
		"type":    "prometheus_exporter",
		"inputs":  []string{"internal_metrics"},
		"address": "127.0.0.1:9598",
	}
}

func diskBufferConfig() map[string]any {
	return map[string]any{
		"type":      "disk",
		"max_size":  diskBufferMaxSize,
		"when_full": "block",
	}
}

func tlsConfig() map[string]any {
	return map[string]any{
		"verify_certificate": true,
		"verify_hostname":    true,
		"alpn_protocols":     []string{"h2", "http/1.1"},
	}
}
