package vector

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

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

func TestGenerateVM_NvidiaGPU(t *testing.T) {
	cfg := parsedVM(t, VMConfig{GPUType: GPUNvidia})

	src := sources(cfg)
	assert.Contains(t, src, "dcgm_metrics")
	dcgm := src["dcgm_metrics"].(map[string]any)
	assert.Equal(t, "prometheus_scrape", dcgm["type"])

	endpoints := dcgm["endpoints"].([]any)
	assert.Contains(t, endpoints, "http://localhost:9400/metrics")

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
	assert.Contains(t, endpoints, "http://localhost:${AMD_EXPORTER_PORT}/metrics")

	xf := transforms(cfg)
	labelInputs := xf["add_update_labels"].(map[string]any)["inputs"].([]any)
	assert.Contains(t, labelInputs, "host_metrics")
	assert.Contains(t, labelInputs, "amd_metrics")
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
	assert.Equal(t, "${CRUSOE_AUTH_TOKEN}", auth["token"])
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
