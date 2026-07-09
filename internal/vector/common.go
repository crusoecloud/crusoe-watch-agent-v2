package vector

// Shared constants, source/transform/sink builders, and utilities used by
// both VM and K8s Vector config generators.

import (
	"fmt"
	"os"
	"path/filepath"
)

// configDirPerm is the mode for created Vector config directories.
const configDirPerm = 0o750

// WriteConfigFile atomically writes a generated Vector config to path via a
// temp file + rename, so Vector's --watch-config never observes a partially
// written file. Used by the K8s watcher and the VM config.apply handler.
func WriteConfigFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, configDirPerm); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".vector-config-*.yaml")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}

	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)

		return fmt.Errorf("writing temp file: %w", err)
	}

	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)

		return fmt.Errorf("closing temp file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)

		return fmt.Errorf("renaming config: %w", err)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const (
	scrapeIntervalSecs  = 60
	scrapeTimeoutSecs   = 50
	logBatchMaxBytes    = 100000
	metricBatchMaxBytes = 500000
	requestTimeoutSecs  = 15
	diskBufferMaxSize   = 268435488 // 256 MiB
)

// ---------------------------------------------------------------------------
// Base config
// ---------------------------------------------------------------------------

// defaultBaseConfig returns a minimal base config with data_dir and api block.
func defaultBaseConfig(dataDir string) map[string]any {
	return map[string]any{
		"data_dir": dataDir,
		"api": map[string]any{
			"enabled": true,
			"address": "127.0.0.1:8686",
		},
	}
}

// ensureMap returns the map at baseCfg[key], creating it if absent.
func ensureMap(baseCfg map[string]any, key string) map[string]any {
	if m, ok := baseCfg[key].(map[string]any); ok {
		return m
	}
	m := map[string]any{}
	baseCfg[key] = m

	return m
}

// ---------------------------------------------------------------------------
// Source builders
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Transform helpers
// ---------------------------------------------------------------------------

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

func wireIntoTransform(transforms map[string]any, transformName, inputName string) {
	transform, exists := transforms[transformName].(map[string]any)
	if !exists {
		return
	}
	var inputs []string
	if existing, ok := transform["inputs"].([]string); ok {
		inputs = existing
	}
	for _, name := range inputs {
		if name == inputName {
			return
		}
	}
	transform["inputs"] = append(inputs, inputName)
}

// ---------------------------------------------------------------------------
// Sink helpers
// ---------------------------------------------------------------------------

// internalMetricsExporterSinkName is the one sink that survives an ingestion block.
const internalMetricsExporterSinkName = "internal_metrics_exporter"

// removeExternalSinks deletes every sink except the local internal-metrics exporter.
func removeExternalSinks(sinks map[string]any) {
	for name := range sinks {
		if name != internalMetricsExporterSinkName {
			delete(sinks, name)
		}
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

// tlsConfig returns the TLS settings for a sink. When the sink routes through a
// proxy, h2 is stripped from the ALPN list: the proxy terminates HTTP/1.1 and
// re-establishing h2 through it breaks the connection.
func tlsConfig(viaProxy bool) map[string]any {
	alpn := []string{"h2", "http/1.1"}
	if viaProxy {
		alpn = []string{"http/1.1"}
	}

	return map[string]any{
		"verify_certificate": true,
		"verify_hostname":    true,
		"alpn_protocols":     alpn,
	}
}
