package vector

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------


func testK8sConfig() K8sConfig {
	return K8sConfig{
		DCGM: ExporterConfig{
			Enabled:        true,
			Port:           9400,
			Paths:          []string{"/metrics"},
			ScrapeInterval: 30,
		},
		AMD: ExporterConfig{
			Enabled:        true,
			Port:           5000,
			Paths:          []string{"/metrics"},
			ScrapeInterval: 60,
		},
		KSM: ExporterConfig{
			Enabled:        true,
			Port:           8080,
			Paths:          []string{"/metrics"},
			ScrapeInterval: 60,
		},
		Slurm: ExporterConfig{
			Enabled:        false,
			Port:           6817,
			Paths:          []string{"/metrics/jobs", "/metrics/jobs-users-accts", "/metrics/nodes", "/metrics/partitions", "/metrics/scheduler"},
			ScrapeInterval: 60,
		},
		CME: ExporterConfig{
			Enabled:        true,
			Port:           9500,
			Paths:          []string{"/metrics"},
			ScrapeInterval: 60,
		},
		CustomMetricsEnabled:       true,
		CustomMetricsDefaultPort:   9100,
		CustomMetricsDefaultPath:   "/metrics",
		CustomMetricsDefaultScrape: 30,
		LogsEnabled:                true,
		SinkEndpoint:               "https://cms-monitoring.example.com",
		NodeLabels: NodeLabels{
			VMID:         "test-vm-id",
			NodepoolID:   "test-nodepool-id",
			InstanceType: "test-instance-type",
			Hostname:     "test-node",
		},
	}
}

func testNodeLabels() NodeLabels {
	return NodeLabels{
		VMID:         "test-vm-id",
		NodepoolID:   "test-nodepool-id",
		InstanceType: "test-instance-type",
		Hostname:     "test-node",
	}
}

func buildAndParse(t *testing.T, pods []ClassifiedPod, cmData map[string]string, cfg K8sConfig) map[string]any {
	t.Helper()
	baseCfg := GenerateK8sBase(cfg)
	ApplyK8s(baseCfg, pods, cmData, cfg)
	// Round-trip through YAML to normalize types ([]string → []any, etc.)
	out, err := yaml.Marshal(baseCfg)
	require.NoError(t, err)
	var result map[string]any
	require.NoError(t, yaml.Unmarshal(out, &result))
	return result
}

func getSources(cfg map[string]any) map[string]any {
	return cfg["sources"].(map[string]any)
}

func getTransforms(cfg map[string]any) map[string]any {
	return cfg["transforms"].(map[string]any)
}

func getSinks(cfg map[string]any) map[string]any {
	return cfg["sinks"].(map[string]any)
}

// ---------------------------------------------------------------------------
// DCGM
// ---------------------------------------------------------------------------

func TestApplyDCGM(t *testing.T) {
	pods := []ClassifiedPod{{Name: "dcgm-1", IP: "10.0.0.1", Type: PodTypeDCGM}}
	cfg := buildAndParse(t, pods, nil, testK8sConfig())

	sources := getSources(cfg)
	assert.Contains(t, sources, "dcgm_exporter_scrape")
	src := sources["dcgm_exporter_scrape"].(map[string]any)
	assert.Equal(t, "prometheus_scrape", src["type"])

	eps := src["endpoints"].([]any)
	assert.Len(t, eps, 1)
	assert.Equal(t, "http://10.0.0.1:9400/metrics", eps[0])

	// Verify DCGM wired into enrich_node_metrics inputs.
	transforms := getTransforms(cfg)
	nodeTransform := transforms["enrich_node_metrics"].(map[string]any)
	inputs := nodeTransform["inputs"].([]any)
	assert.Contains(t, inputs, "dcgm_exporter_scrape")
}

func TestApplyDCGMDisabled(t *testing.T) {
	pods := []ClassifiedPod{{Name: "dcgm-1", IP: "10.0.0.1", Type: PodTypeDCGM}}
	k := testK8sConfig()
	k.DCGM.Enabled = false
	cfg := buildAndParse(t, pods, nil, k)

	assert.NotContains(t, getSources(cfg), "dcgm_exporter_scrape")
}

func TestApplyDCGMNoPod(t *testing.T) {
	cfg := buildAndParse(t, nil, nil, testK8sConfig())
	assert.NotContains(t, getSources(cfg), "dcgm_exporter_scrape")
}

// ---------------------------------------------------------------------------
// AMD
// ---------------------------------------------------------------------------

func TestApplyAMD(t *testing.T) {
	pods := []ClassifiedPod{{Name: "amd-1", IP: "10.0.0.2", Type: PodTypeAMD}}
	cfg := buildAndParse(t, pods, nil, testK8sConfig())

	sources := getSources(cfg)
	assert.Contains(t, sources, "amd_exporter_scrape")
	src := sources["amd_exporter_scrape"].(map[string]any)
	eps := src["endpoints"].([]any)
	assert.Equal(t, "http://10.0.0.2:5000/metrics", eps[0])

	// Filter transform exists.
	transforms := getTransforms(cfg)
	assert.Contains(t, transforms, "amd_allowed_filter")
	filter := transforms["amd_allowed_filter"].(map[string]any)
	assert.Equal(t, "filter", filter["type"])

	// AMD filter wired into enrich_node_metrics.
	nodeTransform := transforms["enrich_node_metrics"].(map[string]any)
	inputs := nodeTransform["inputs"].([]any)
	assert.Contains(t, inputs, "amd_allowed_filter")
}

// ---------------------------------------------------------------------------
// KSM
// ---------------------------------------------------------------------------

func TestApplyKSM(t *testing.T) {
	pods := []ClassifiedPod{{Name: "ksm-1", IP: "10.0.0.3", Type: PodTypeKSM}}
	cfg := buildAndParse(t, pods, nil, testK8sConfig())

	sources := getSources(cfg)
	assert.Contains(t, sources, "kube_state_metrics_scrape")

	transforms := getTransforms(cfg)
	assert.Contains(t, transforms, "enrich_kube_state_metrics")
	xform := transforms["enrich_kube_state_metrics"].(map[string]any)
	assert.Equal(t, "remap", xform["type"])
	assert.Contains(t, xform["source"], "kube-state-metrics")

	sinks := getSinks(cfg)
	assert.Contains(t, sinks, "kube_state_metrics_sink")
	sink := sinks["kube_state_metrics_sink"].(map[string]any)
	assert.Equal(t, "prometheus_remote_write", sink["type"])
	assert.Contains(t, sink["endpoint"].(string), "/cluster")
}

// ---------------------------------------------------------------------------
// Slurm
// ---------------------------------------------------------------------------

func TestApplySlurmDisabledByDefault(t *testing.T) {
	pods := []ClassifiedPod{{Name: "slurm-1", IP: "10.0.0.4", Type: PodTypeSlurm}}
	cfg := buildAndParse(t, pods, nil, testK8sConfig())

	assert.NotContains(t, getSources(cfg), "slurm_metrics_scrape")
}

func TestApplySlurmEnabled(t *testing.T) {
	pods := []ClassifiedPod{{Name: "slurm-1", IP: "10.0.0.4", Type: PodTypeSlurm}}
	k := testK8sConfig()
	k.Slurm.Enabled = true
	cfg := buildAndParse(t, pods, nil, k)

	sources := getSources(cfg)
	assert.Contains(t, sources, "slurm_metrics_scrape")
	src := sources["slurm_metrics_scrape"].(map[string]any)
	eps := src["endpoints"].([]any)
	assert.Len(t, eps, 5, "slurm has 5 scrape paths")

	sinks := getSinks(cfg)
	assert.Contains(t, sinks, "slurm_metrics_sink")
}

// ---------------------------------------------------------------------------
// CME
// ---------------------------------------------------------------------------

func TestApplyCME(t *testing.T) {
	pods := []ClassifiedPod{{Name: "cme-1", IP: "10.0.0.5", Type: PodTypeCME}}
	cfg := buildAndParse(t, pods, nil, testK8sConfig())

	sources := getSources(cfg)
	assert.Contains(t, sources, "crusoe_metrics_exporter_scrape")

	transforms := getTransforms(cfg)
	assert.Contains(t, transforms, "enrich_crusoe_metrics_exporter")
	xform := transforms["enrich_crusoe_metrics_exporter"].(map[string]any)
	assert.Contains(t, xform["source"], "vm_custom_infra")
	assert.Contains(t, xform["source"], "test-vm-id")

	sinks := getSinks(cfg)
	assert.Contains(t, sinks, "crusoe_metrics_exporter_sink")
	sink := sinks["crusoe_metrics_exporter_sink"].(map[string]any)
	assert.Contains(t, sink["endpoint"].(string), "/ingest")
}

// ---------------------------------------------------------------------------
// Custom metrics
// ---------------------------------------------------------------------------

func TestApplyCustomMetrics(t *testing.T) {
	pods := []ClassifiedPod{
		{Name: "svc-x-1", IP: "10.2.0.1", Type: PodTypeCustom, Port: 9100, Path: "/metrics", DeploymentName: "svc"},
		{Name: "svc-y-2", IP: "10.2.0.2", Type: PodTypeCustom, Port: 9200, Path: "/m", DeploymentName: "svc"},
	}
	cfg := buildAndParse(t, pods, nil, testK8sConfig())

	sources := getSources(cfg)
	assert.Contains(t, sources, "svc_x_1_scrape")
	assert.Contains(t, sources, "svc_y_2_scrape")

	transforms := getTransforms(cfg)
	assert.Contains(t, transforms, "svc_x_1_transform")
	xform := transforms["svc_x_1_transform"].(map[string]any)
	assert.Equal(t, true, xform["drop_on_abort"])

	sinks := getSinks(cfg)
	assert.Contains(t, sinks, "svc_x_1_sink")
	assert.Contains(t, sinks, "svc_y_2_sink")
}

func TestApplyCustomMetricsWithAllowlist(t *testing.T) {
	pods := []ClassifiedPod{
		{Name: "svc-x-1", IP: "10.2.0.1", Type: PodTypeCustom, Port: 9100, Path: "/metrics", DeploymentName: "svc"},
	}
	cmData := map[string]string{
		"custom-metrics-config.yaml": "svc:\n  scrape_interval_secs: 15\n  allowlist:\n    - foo_total\n    - bar_total\n",
	}
	cfg := buildAndParse(t, pods, cmData, testK8sConfig())

	sources := getSources(cfg)
	src := sources["svc_x_1_scrape"].(map[string]any)
	assert.Equal(t, 15, src["scrape_interval_secs"])

	transforms := getTransforms(cfg)
	xform := transforms["svc_x_1_transform"].(map[string]any)
	source := xform["source"].(string)
	assert.Contains(t, source, "foo_total")
	assert.Contains(t, source, "allowed_metrics")
}

func TestApplyCustomMetricsWithAppID(t *testing.T) {
	pods := []ClassifiedPod{
		{Name: "managed-1", IP: "10.2.0.1", Type: PodTypeCustom, Port: 9100, Path: "/metrics", AppID: "my-app"},
	}
	cfg := buildAndParse(t, pods, nil, testK8sConfig())

	sinks := getSinks(cfg)
	sink := sinks["managed_1_sink"].(map[string]any)
	assert.Contains(t, sink["endpoint"].(string), "/custom/my-app")

	transforms := getTransforms(cfg)
	xform := transforms["managed_1_transform"].(map[string]any)
	assert.Contains(t, xform["source"], "custom_internal")
	assert.Contains(t, xform["source"], "my-app")
}

func TestApplyCustomMetricsDisabled(t *testing.T) {
	pods := []ClassifiedPod{
		{Name: "svc-x-1", IP: "10.2.0.1", Type: PodTypeCustom, Port: 9100, Path: "/metrics"},
	}
	k := testK8sConfig()
	k.CustomMetricsEnabled = false
	cfg := buildAndParse(t, pods, nil, k)

	assert.NotContains(t, getSources(cfg), "svc_x_1_scrape")
}

// ---------------------------------------------------------------------------
// Logs
// ---------------------------------------------------------------------------

func TestApplyLogs(t *testing.T) {
	cfg := buildAndParse(t, nil, nil, testK8sConfig())

	sources := getSources(cfg)
	assert.Contains(t, sources, "journald_logs")
	assert.Contains(t, sources, "vector_internal_logs")
	assert.Contains(t, sources, "cwa_manager_logs")

	// cwa-manager logs read from container log files.
	cwaLogs := sources["cwa_manager_logs"].(map[string]any)
	assert.Equal(t, "file", cwaLogs["type"])

	transforms := getTransforms(cfg)
	assert.Contains(t, transforms, "filter_journald_noise")
	assert.Contains(t, transforms, "parse_journald_logs")
	assert.Contains(t, transforms, "parse_internal_logs")
	assert.Contains(t, transforms, "parse_cwa_manager_logs")
	assert.Contains(t, transforms, "enrich_logs")

	// Enrich logs converges three parsers (journald + internal + cwa-manager).
	enrich := transforms["enrich_logs"].(map[string]any)
	inputs := enrich["inputs"].([]any)
	assert.Len(t, inputs, 3)
	assert.Contains(t, inputs, "parse_cwa_manager_logs")

	sinks := getSinks(cfg)
	assert.Contains(t, sinks, "crusoe_ingest")
	sink := sinks["crusoe_ingest"].(map[string]any)
	assert.Equal(t, "http", sink["type"])
	assert.Contains(t, sink["uri"].(string), "/logs/ingest")
}

func TestK8sLogsEnvelopeContract(t *testing.T) {
	// Standardized envelope: raw event under payload, identity under crusoe,
	// _msg/_time/level/log_source at the top level. Scratch (._*) never ships.
	cfg := buildAndParse(t, nil, nil, testK8sConfig())
	transforms := getTransforms(cfg)

	enrich := transforms["enrich_logs"].(map[string]any)["source"].(string)
	assert.Contains(t, enrich, ".payload = raw")
	assert.Contains(t, enrich, `.crusoe = { "agent": "crusoe-watch-agent"`)
	// K8s envelope uses chart_version (not agent_version — that's the VM field name).
	assert.Contains(t, enrich, `"chart_version": "${AGENT_VERSION}"`)
	assert.NotContains(t, enrich, `"agent_version"`)
	assert.NotContains(t, enrich, "cluster_id")
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

	journald := transforms["parse_journald_logs"].(map[string]any)["source"].(string)
	// Drop-nothing: must not delete the raw message/timestamp.
	assert.NotContains(t, journald, "del(.message)")
	assert.NotContains(t, journald, "del(.timestamp)")
	assert.Contains(t, journald, `.log_source = "journald"`)
	assert.Contains(t, journald, `.level = "info"`)
	assert.Contains(t, journald, ".level = string!(parsed_klog.level)")

	cwaManager := transforms["parse_cwa_manager_logs"].(map[string]any)["source"].(string)
	assert.Contains(t, cwaManager, `.log_source = "cwa-manager"`)
	assert.Contains(t, cwaManager, ".level = downcase(string!(parsed.level))")
	assert.NotContains(t, cwaManager, "del(.message)")
	assert.NotContains(t, cwaManager, "del(.timestamp)")

	internal := transforms["parse_internal_logs"].(map[string]any)["source"].(string)
	assert.Contains(t, internal, `.log_source = "crusoe-watch-agent"`)
}

func TestK8sLogsSinkUserAgent(t *testing.T) {
	cfg := buildAndParse(t, nil, nil, testK8sConfig())
	sinks := getSinks(cfg)

	sink := sinks["crusoe_ingest"].(map[string]any)
	headers := sink["request"].(map[string]any)["headers"].(map[string]any)
	assert.Equal(t, "CrusoeWatchAgent/CMK-${AGENT_VERSION}", headers["User-Agent"])
}

func TestApplyLogsDisabled(t *testing.T) {
	k := testK8sConfig()
	k.LogsEnabled = false
	cfg := buildAndParse(t, nil, nil, k)

	assert.NotContains(t, getSources(cfg), "journald_logs")
	assert.NotContains(t, getSinks(cfg), "crusoe_ingest")
}

// ---------------------------------------------------------------------------
// Node metrics transform
// ---------------------------------------------------------------------------

func TestNodeMetricsTransformHasNodeLabels(t *testing.T) {
	cfg := buildAndParse(t, nil, nil, testK8sConfig())

	transforms := getTransforms(cfg)
	nodeTransform := transforms["enrich_node_metrics"].(map[string]any)
	source := nodeTransform["source"].(string)
	assert.Contains(t, source, "test-vm-id")
	assert.Contains(t, source, "test-nodepool-id")
	assert.Contains(t, source, "test-instance-type")
	assert.Contains(t, source, "test-node")
	assert.Contains(t, source, "node-metrics")
}

// ---------------------------------------------------------------------------
// Base config sink endpoint
// ---------------------------------------------------------------------------

func TestNodeMetricsSinkEndpointSet(t *testing.T) {
	cfg := buildAndParse(t, nil, nil, testK8sConfig())
	sinks := getSinks(cfg)
	sink := sinks["cms_gateway_node_metrics"].(map[string]any)
	assert.Equal(t, "https://cms-monitoring.example.com/ingest", sink["endpoint"])
}

func TestK8sEndpointOverrides(t *testing.T) {
	pods := []ClassifiedPod{{Name: "ksm-1", IP: "10.0.0.3", Type: PodTypeKSM}}
	k := testK8sConfig()
	k.LogsEndpoint = "https://logs.example.com"
	k.MetricsEndpoint = "https://metrics.example.com"
	cfg := buildAndParse(t, pods, nil, k)

	// Control-plane overrides win over SinkEndpoint, per sink kind: every
	// metrics-derived endpoint uses the metrics base, logs use the logs base.
	sinks := getSinks(cfg)
	nodeMetrics := sinks["cms_gateway_node_metrics"].(map[string]any)
	assert.Equal(t, "https://metrics.example.com/ingest", nodeMetrics["endpoint"])
	ksm := sinks["kube_state_metrics_sink"].(map[string]any)
	assert.Equal(t, "https://metrics.example.com/cluster", ksm["endpoint"])
	logs := sinks["crusoe_ingest"].(map[string]any)
	assert.Equal(t, "https://logs.example.com/logs/ingest", logs["uri"])
}

func TestK8sEndpointOverride_PartialFallsBackToSinkEndpoint(t *testing.T) {
	k := testK8sConfig()
	k.LogsEndpoint = "https://logs.example.com"
	cfg := buildAndParse(t, nil, nil, k)

	sinks := getSinks(cfg)
	logs := sinks["crusoe_ingest"].(map[string]any)
	assert.Equal(t, "https://logs.example.com/logs/ingest", logs["uri"])
	nodeMetrics := sinks["cms_gateway_node_metrics"].(map[string]any)
	assert.Equal(t, "https://cms-monitoring.example.com/ingest", nodeMetrics["endpoint"])
}

func TestNodeMetricsSinkProxyWhenEnabled(t *testing.T) {
	k := testK8sConfig()
	k.Proxy = ProxyConfig{Enabled: true, HTTP: "http://proxy:8080"}
	cfg := buildAndParse(t, nil, nil, k)

	sinks := getSinks(cfg)
	sink := sinks["cms_gateway_node_metrics"].(map[string]any)
	assert.Contains(t, sink, "proxy")

	// h2 must be stripped from ALPN when routing through the proxy.
	tls := sink["tls"].(map[string]any)
	assert.Equal(t, []any{"http/1.1"}, tls["alpn_protocols"])
}

// ---------------------------------------------------------------------------
// Full pipeline (DCGM + KSM + custom + logs)
// ---------------------------------------------------------------------------

func TestFullK8sPipeline(t *testing.T) {
	pods := []ClassifiedPod{
		{Name: "dcgm-1", IP: "10.0.0.1", Type: PodTypeDCGM},
		{Name: "ksm-1", IP: "10.0.0.3", Type: PodTypeKSM},
		{Name: "svc-x-1", IP: "10.2.0.1", Type: PodTypeCustom, Port: 9100, Path: "/metrics", DeploymentName: "svc"},
	}
	cfg := buildAndParse(t, pods, nil, testK8sConfig())

	// Verify we can marshal the whole thing without error.
	_, err := yaml.Marshal(cfg)
	require.NoError(t, err)

	sources := getSources(cfg)
	assert.Contains(t, sources, "dcgm_exporter_scrape")
	assert.Contains(t, sources, "kube_state_metrics_scrape")
	assert.Contains(t, sources, "svc_x_1_scrape")
	assert.Contains(t, sources, "journald_logs")

	sinks := getSinks(cfg)
	assert.Contains(t, sinks, "kube_state_metrics_sink")
	assert.Contains(t, sinks, "svc_x_1_sink")
	assert.Contains(t, sinks, "crusoe_ingest")
	assert.Contains(t, sinks, "cms_gateway_node_metrics")
}

// ---------------------------------------------------------------------------
// SanitizeName
// ---------------------------------------------------------------------------

func TestSanitizeName(t *testing.T) {
	assert.Equal(t, "pod_name_with_weird__chars", SanitizeName("pod.name/with:weird- chars"))
	assert.Equal(t, "ABC123__", SanitizeName("ABC123_-"))
}

// ---------------------------------------------------------------------------
// ExporterConfig.BuildEndpoints
// ---------------------------------------------------------------------------

func TestBuildEndpoints(t *testing.T) {
	ec := ExporterConfig{Port: 8080, Paths: []string{"/a", "/b"}}
	eps := ec.BuildEndpoints("10.0.0.1")
	assert.Equal(t, []string{"http://10.0.0.1:8080/a", "http://10.0.0.1:8080/b"}, eps)
}
