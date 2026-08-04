package vector

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestDetectGPU(t *testing.T) {
	dir := t.TempDir()
	assert.Equal(t, GPUNone, detectGPU(dir))

	require.NoError(t, os.Mkdir(filepath.Join(dir, "amdgpu"), 0o750))
	assert.Equal(t, GPUAMD, detectGPU(dir))

	// NVIDIA wins when both modules are present.
	require.NoError(t, os.Mkdir(filepath.Join(dir, "nvidia"), 0o750))
	assert.Equal(t, GPUNvidia, detectGPU(dir))
}

// parsedVM unmarshals generated VM config YAML into a map for assertion.
func parsedVM(t *testing.T, cfg VMConfig) map[string]any {
	t.Helper()

	out, err := GenerateVM(cfg)
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, yaml.Unmarshal(out, &m))

	return m
}

func sources(cfg map[string]any) map[string]any {
	return cfg["sources"].(map[string]any)
}

func transforms(cfg map[string]any) map[string]any {
	return cfg["transforms"].(map[string]any)
}

func sinks(cfg map[string]any) map[string]any {
	return cfg["sinks"].(map[string]any)
}

func TestGenerateVM_CPUOnly(t *testing.T) {
	cfg := parsedVM(t, VMConfig{GPUType: GPUNone})

	src := sources(cfg)
	assert.Contains(t, src, "host_metrics")
	assert.Contains(t, src, "internal_metrics")
	assert.Contains(t, src, "journald_logs")
	assert.Contains(t, src, "vector_internal_logs")
	assert.Contains(t, src, "cwa_manager_logs")
	assert.Contains(t, src, "report_runner_logs")
	assert.NotContains(t, src, "dcgm_metrics")
	assert.NotContains(t, src, "amd_metrics")
	assert.NotContains(t, src, "crusoe_infra_metrics")

	xf := transforms(cfg)
	labelInputs := xf["add_update_labels"].(map[string]any)["inputs"].([]any)
	assert.Equal(t, []any{"host_metrics"}, labelInputs)

	sk := sinks(cfg)
	assert.Contains(t, sk, "crusoe_ingest")
	assert.Contains(t, sk, "cms_gateway")
	assert.Contains(t, sk, "internal_metrics_exporter")
	assert.NotContains(t, sk, "cms_gateway_cme")
}

func TestGenerateVM_EndpointOverrides(t *testing.T) {
	cfg := parsedVM(t, VMConfig{
		EnableCME:       true,
		LogsEndpoint:    "https://logs.example.com",
		MetricsEndpoint: "https://metrics.example.com",
	})

	// Sink endpoints are derived from the per-kind base URLs as literals,
	// replacing the ${...} env placeholders (same derivation the installer
	// applies to cms_url). Logs and metrics can point at different bases.
	sk := sinks(cfg)
	assert.Equal(t, "https://logs.example.com/logs/ingest", sk["crusoe_ingest"].(map[string]any)["uri"])
	assert.Equal(t, "https://metrics.example.com/ingest", sk["cms_gateway"].(map[string]any)["endpoint"])
	// The CME sink shares the telemetry endpoint.
	assert.Equal(t, "https://metrics.example.com/ingest", sk["cms_gateway_cme"].(map[string]any)["endpoint"])
}

func TestGenerateVM_PartialEndpointOverride(t *testing.T) {
	cfg := parsedVM(t, VMConfig{LogsEndpoint: "https://logs.example.com"})

	// Only the overridden sink gets a literal; the other keeps its env placeholder.
	sk := sinks(cfg)
	assert.Equal(t, "https://logs.example.com/logs/ingest", sk["crusoe_ingest"].(map[string]any)["uri"])
	assert.Equal(t, "${TELEMETRY_INGRESS_ENDPOINT}", sk["cms_gateway"].(map[string]any)["endpoint"])
}

func TestGenerateVM_RateLimit(t *testing.T) {
	cfg := parsedVM(t, VMConfig{GPUType: GPUNvidia, EnableCME: true, RateLimits: map[string]int{RateLimitAll: 100}})

	// The RateLimitAll default caps every external forwarding sink.
	sk := sinks(cfg)
	for _, name := range []string{"crusoe_ingest", "cms_gateway", "cms_gateway_cme"} {
		req := sk[name].(map[string]any)["request"].(map[string]any)
		assert.Equal(t, 100, req["rate_limit_num"], name)
		assert.Equal(t, 60, req["rate_limit_duration_secs"], name)
	}

	// The local internal-metrics exporter has no request block and is untouched.
	assert.NotContains(t, sk["internal_metrics_exporter"].(map[string]any), "request")
}

func TestGenerateVM_RateLimitPerSink(t *testing.T) {
	cfg := parsedVM(t, VMConfig{
		GPUType:   GPUNvidia,
		EnableCME: true,
		RateLimits: map[string]int{
			RateLimitAll:    100,
			"crusoe_ingest": 10,
		},
	})

	sk := sinks(cfg)
	// The named sink gets its own cap; the rest fall back to the default.
	assert.Equal(t, 10, sk["crusoe_ingest"].(map[string]any)["request"].(map[string]any)["rate_limit_num"])
	assert.Equal(t, 100, sk["cms_gateway"].(map[string]any)["request"].(map[string]any)["rate_limit_num"])

	// With no default, only the named sink is capped.
	cfg = parsedVM(t, VMConfig{GPUType: GPUNvidia, EnableCME: true, RateLimits: map[string]int{"crusoe_ingest": 10}})
	sk = sinks(cfg)
	assert.Equal(t, 10, sk["crusoe_ingest"].(map[string]any)["request"].(map[string]any)["rate_limit_num"])
	assert.NotContains(t, sk["cms_gateway"].(map[string]any)["request"].(map[string]any), "rate_limit_num")
}

func TestGenerateVM_NoRateLimitByDefault(t *testing.T) {
	cfg := parsedVM(t, VMConfig{})

	// With no override, sinks carry no rate-limit cap (Vector default: unlimited).
	req := sinks(cfg)["crusoe_ingest"].(map[string]any)["request"].(map[string]any)
	assert.NotContains(t, req, "rate_limit_num")
}

func TestGenerateVM_IngestionBlocked(t *testing.T) {
	cfg := parsedVM(t, VMConfig{
		GPUType:          GPUNvidia,
		EnableCME:        true,
		LogsEndpoint:     "https://logs.example.com",
		MetricsEndpoint:  "https://metrics.example.com",
		IngestionBlocked: true,
	})

	// Only the local internal-metrics exporter survives a block.
	sk := sinks(cfg)
	assert.Len(t, sk, 1)
	assert.Contains(t, sk, "internal_metrics_exporter")

	// Sources and transforms keep running; only forwarding stops.
	src := sources(cfg)
	assert.Contains(t, src, "host_metrics")
	assert.Contains(t, src, "journald_logs")
	assert.Contains(t, src, "dcgm_metrics")

	xf := transforms(cfg)
	assert.Contains(t, xf, "enrich_logs")
	assert.Contains(t, xf, "add_update_labels")
}

func TestGenerateVM_NvidiaGPU(t *testing.T) {
	cfg := parsedVM(t, VMConfig{GPUType: GPUNvidia})

	src := sources(cfg)
	assert.Contains(t, src, "dcgm_metrics")
	dcgm := src["dcgm_metrics"].(map[string]any)
	assert.Equal(t, "prometheus_scrape", dcgm["type"])

	endpoints := dcgm["endpoints"].([]any)
	assert.Contains(t, endpoints, "http://localhost:${DCGM_EXPORTER_PORT:-9400}/metrics")

	xf := transforms(cfg)
	labelInputs := xf["add_update_labels"].(map[string]any)["inputs"].([]any)
	assert.Contains(t, labelInputs, "host_metrics")
	assert.Contains(t, labelInputs, "dcgm_metrics")
}

func TestGenerateVM_AMDGPU(t *testing.T) {
	cfg := parsedVM(t, VMConfig{GPUType: GPUAMD})

	src := sources(cfg)
	assert.Contains(t, src, "amd_metrics")
	assert.NotContains(t, src, "dcgm_metrics")

	amd := src["amd_metrics"].(map[string]any)
	endpoints := amd["endpoints"].([]any)
	assert.Contains(t, endpoints, "http://localhost:${AMD_EXPORTER_PORT:-5000}/metrics")

	xf := transforms(cfg)
	assert.Contains(t, xf, "amd_allowed_filter")
	filter := xf["amd_allowed_filter"].(map[string]any)
	assert.Equal(t, []any{"amd_metrics"}, filter["inputs"].([]any))

	labelInputs := xf["add_update_labels"].(map[string]any)["inputs"].([]any)
	assert.Contains(t, labelInputs, "host_metrics")
	assert.Contains(t, labelInputs, "amd_allowed_filter")
	assert.NotContains(t, labelInputs, "amd_metrics")
}

func TestGenerateVM_CME(t *testing.T) {
	t.Run("cpu with cme", func(t *testing.T) {
		cfg := parsedVM(t, VMConfig{GPUType: GPUNone, EnableCME: true})

		src := sources(cfg)
		assert.Contains(t, src, "crusoe_infra_metrics")

		xf := transforms(cfg)
		assert.Contains(t, xf, "enrich_crusoe_infra_metrics")
		cme := xf["enrich_crusoe_infra_metrics"].(map[string]any)
		assert.Equal(t, []any{"crusoe_infra_metrics"}, cme["inputs"].([]any))

		sk := sinks(cfg)
		assert.Contains(t, sk, "cms_gateway_cme")
	})

	t.Run("gpu with cme", func(t *testing.T) {
		cfg := parsedVM(t, VMConfig{GPUType: GPUNvidia, EnableCME: true})

		src := sources(cfg)
		assert.Contains(t, src, "dcgm_metrics")
		assert.Contains(t, src, "crusoe_infra_metrics")

		sk := sinks(cfg)
		assert.Contains(t, sk, "cms_gateway")
		assert.Contains(t, sk, "cms_gateway_cme")
	})
}

func TestGenerateVM_InternalMetricsExporterAlwaysPresent(t *testing.T) {
	for _, gpu := range []GPUType{GPUNone, GPUNvidia, GPUAMD} {
		cfg := parsedVM(t, VMConfig{GPUType: gpu})

		sk := sinks(cfg)
		exporter := sk["internal_metrics_exporter"].(map[string]any)
		assert.Equal(t, "prometheus_exporter", exporter["type"])
		assert.Equal(t, "127.0.0.1:9598", exporter["address"])
	}
}

func TestGenerateVM_CommonTransformsPresent(t *testing.T) {
	cfg := parsedVM(t, VMConfig{GPUType: GPUNone})
	xf := transforms(cfg)

	for _, name := range []string{
		"parse_journald_logs",
		"parse_internal_logs",
		"parse_cwa_manager_logs",
		"parse_report_runner_logs",
		"enrich_logs",
		"add_update_labels",
		"filter_internal_metrics",
		"add_internal_labels",
	} {
		assert.Contains(t, xf, name)
	}
}

func TestGenerateVM_LogsSinkConfig(t *testing.T) {
	cfg := parsedVM(t, VMConfig{GPUType: GPUNone})
	sk := sinks(cfg)

	logs := sk["crusoe_ingest"].(map[string]any)
	assert.Equal(t, "http", logs["type"])
	assert.Equal(t, "${LOGS_INGRESS_ENDPOINT}", logs["uri"])
	assert.Equal(t, "snappy", logs["compression"])

	auth := logs["auth"].(map[string]any)
	assert.Equal(t, "bearer", auth["strategy"])
	assert.Equal(t, "${CRUSOE_MONITORING_TOKEN}", auth["token"])
}

func TestGenerateVM_MetricsSinkConfig(t *testing.T) {
	cfg := parsedVM(t, VMConfig{GPUType: GPUNone})
	sk := sinks(cfg)

	metrics := sk["cms_gateway"].(map[string]any)
	assert.Equal(t, "prometheus_remote_write", metrics["type"])
	assert.Equal(t, "${TELEMETRY_INGRESS_ENDPOINT}", metrics["endpoint"])
	assert.Equal(t, "cri:vm/${VM_ID}", metrics["tenant_id"])

	inputs := metrics["inputs"].([]any)
	assert.Contains(t, inputs, "add_update_labels")
	assert.Contains(t, inputs, "add_internal_labels")
}

func TestGenerateVM_InternalMetricsVersionLabel(t *testing.T) {
	cfg := parsedVM(t, VMConfig{GPUType: GPUNone})

	// The agent version rides on Vector's internal metrics (incl. build_info)
	// as a tag. VM/Docker uses agent_version, never helm_version.
	src := transforms(cfg)["add_internal_labels"].(map[string]any)["source"].(string)
	assert.Contains(t, src, `.tags.vm_id = "${VM_ID}"`)
	assert.Contains(t, src, `.tags.agent_version = "${AGENT_VERSION}"`)
	assert.NotContains(t, src, "helm_version")
}

func TestGenerateVM_ValidYAML(t *testing.T) {
	variants := []VMConfig{
		{GPUType: GPUNone},
		{GPUType: GPUNvidia},
		{GPUType: GPUAMD},
		{GPUType: GPUNone, EnableCME: true},
		{GPUType: GPUNvidia, EnableCME: true},
	}

	for _, vc := range variants {
		out, err := GenerateVM(vc)
		require.NoError(t, err)
		assert.NotEmpty(t, out)

		var cfg map[string]any
		assert.NoError(t, yaml.Unmarshal(out, &cfg), "variant %+v should produce valid YAML", vc)
	}
}

func TestGenerateVM_JournaldExcludesAgentUnits(t *testing.T) {
	cfg := parsedVM(t, VMConfig{GPUType: GPUNone})
	src := sources(cfg)

	journald := src["journald_logs"].(map[string]any)
	units := journald["exclude_units"].([]any)
	assert.Contains(t, units, "crusoe-watch-agent.service")
	assert.Contains(t, units, "crusoe-watch-agent-native.service")
	assert.Contains(t, units, "cwa-manager.service")
	assert.Contains(t, units, "cwa-report-runner.service")
}

func TestGenerateVM_CwaManagerLogs(t *testing.T) {
	cfg := parsedVM(t, VMConfig{GPUType: GPUNone})
	src := sources(cfg)

	// Dedicated cwa-manager journald source exists.
	cwaLogs := src["cwa_manager_logs"].(map[string]any)
	assert.Equal(t, "journald", cwaLogs["type"])
	includeUnits := cwaLogs["include_units"].([]any)
	assert.Contains(t, includeUnits, "cwa-manager.service")

	// Transform is wired into enrich_logs.
	xf := transforms(cfg)
	parseCwa := xf["parse_cwa_manager_logs"].(map[string]any)
	assert.Equal(t, []any{"cwa_manager_logs"}, parseCwa["inputs"].([]any))

	enrichInputs := xf["enrich_logs"].(map[string]any)["inputs"].([]any)
	assert.Contains(t, enrichInputs, "parse_cwa_manager_logs")
}

func TestGenerateVM_ReportRunnerLogs(t *testing.T) {
	cfg := parsedVM(t, VMConfig{GPUType: GPUNone})
	src := sources(cfg)

	// Dedicated report-runner journald source exists.
	runnerLogs := src["report_runner_logs"].(map[string]any)
	assert.Equal(t, "journald", runnerLogs["type"])
	includeUnits := runnerLogs["include_units"].([]any)
	assert.Contains(t, includeUnits, "cwa-report-runner.service")

	// Transform is wired into enrich_logs.
	xf := transforms(cfg)
	parseRunner := xf["parse_report_runner_logs"].(map[string]any)
	assert.Equal(t, []any{"report_runner_logs"}, parseRunner["inputs"].([]any))

	enrichInputs := xf["enrich_logs"].(map[string]any)["inputs"].([]any)
	assert.Contains(t, enrichInputs, "parse_report_runner_logs")

	// Shares cwa-manager's logfmt body, differing only in log_source.
	source := parseRunner["source"].(string)
	assert.Contains(t, source, `.log_source = "cwa-report-runner"`)
	assert.Contains(t, source, ".level = downcase(string!(parsed.level))")
}

func TestGenerateVM_LogsEnvelopeContract(t *testing.T) {
	// Standardized envelope: raw event under payload, identity under crusoe,
	// _msg/_time/level/log_source at the top level. Scratch (._*) never ships.
	cfg := parsedVM(t, VMConfig{GPUType: GPUNone})
	xf := transforms(cfg)

	enrich := xf["enrich_logs"].(map[string]any)["source"].(string)
	assert.Contains(t, enrich, ".payload = raw")
	assert.Contains(t, enrich, `.crusoe = { "agent": "crusoe-watch-agent"`)
	assert.Contains(t, enrich, `"crusoe_watch_version": "${AGENT_VERSION}"`)
	// Version tag is unified across modes as crusoe_watch_version; legacy
	// per-mode names must not leak. cluster_id is a K8s-only field.
	assert.NotContains(t, enrich, `"agent_version"`)
	assert.NotContains(t, enrich, `"chart_version"`)
	assert.NotContains(t, enrich, `"cluster_id"`)
	assert.Contains(t, enrich, ".log_source = cwa_log_source")
	assert.NotContains(t, enrich, ".crusoe.log_source")
	assert.NotContains(t, enrich, "._payload")
	assert.NotContains(t, enrich, "._crusoe")
	// Scratch is pulled out before wrapping, so it never lands in payload.
	assert.Contains(t, enrich, "cwa_level = del(.level)")
	assert.Contains(t, enrich, "cwa_log_source = del(.log_source)")
	// No flattening of agent metadata to the top level.
	assert.NotContains(t, enrich, `.agent = "crusoe-watch-agent"`)
	assert.NotContains(t, enrich, ".host = get_hostname")
	assert.Contains(t, enrich, "._msg = parsed_msg")
	assert.Contains(t, enrich, "._msg = .payload.message")
	assert.Contains(t, enrich, "._time = parsed_time")

	journald := xf["parse_journald_logs"].(map[string]any)["source"].(string)
	// Drop-nothing: must not delete the raw message/timestamp.
	assert.NotContains(t, journald, "del(.message)")
	assert.NotContains(t, journald, "del(.timestamp)")
	assert.Contains(t, journald, `.log_source = "journald"`)
	assert.Contains(t, journald, `.level = "info"`)

	cwaManager := xf["parse_cwa_manager_logs"].(map[string]any)["source"].(string)
	assert.Contains(t, cwaManager, `.log_source = "cwa-manager"`)
	assert.Contains(t, cwaManager, ".level = downcase(string!(parsed.level))")
	assert.NotContains(t, cwaManager, "del(.message)")
	assert.NotContains(t, cwaManager, "del(.timestamp)")

	internal := xf["parse_internal_logs"].(map[string]any)["source"].(string)
	assert.Contains(t, internal, `.log_source = "crusoe-watch-agent"`)
}

func TestGenerateVM_LogsSinkUserAgent(t *testing.T) {
	cfg := parsedVM(t, VMConfig{GPUType: GPUNone})
	sk := sinks(cfg)

	logs := sk["crusoe_ingest"].(map[string]any)
	headers := logs["request"].(map[string]any)["headers"].(map[string]any)
	assert.Equal(t, "CrusoeWatchAgent/VM-${AGENT_VERSION}", headers["User-Agent"])
}

func TestGenerateVM_HostMetricsCollectors(t *testing.T) {
	cfg := parsedVM(t, VMConfig{GPUType: GPUNone})
	src := sources(cfg)

	hm := src["host_metrics"].(map[string]any)
	collectors := hm["collectors"].([]any)

	expected := []string{"cpu", "disk", "host", "memory", "network", "process"}
	for _, c := range expected {
		assert.Contains(t, collectors, c)
	}
}

func TestGenerateVM_APIEnabled(t *testing.T) {
	cfg := parsedVM(t, VMConfig{GPUType: GPUNone})

	api := cfg["api"].(map[string]any)
	assert.Equal(t, true, api["enabled"])
	assert.Equal(t, "127.0.0.1:8686", api["address"])
}

func TestGenerateVM_TLSConfig(t *testing.T) {
	cfg := parsedVM(t, VMConfig{GPUType: GPUNone})
	sk := sinks(cfg)

	for _, sinkName := range []string{"crusoe_ingest", "cms_gateway"} {
		sink := sk[sinkName].(map[string]any)
		tls := sink["tls"].(map[string]any)
		assert.Equal(t, true, tls["verify_certificate"])
		assert.Equal(t, true, tls["verify_hostname"])
	}
}
