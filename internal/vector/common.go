package vector

// Shared constants, source/transform/sink builders, and utilities used by
// both VM and K8s Vector config generators.

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
