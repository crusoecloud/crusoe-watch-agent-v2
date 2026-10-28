package vector

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// cpTestConfig is a minimal cell-mode (CMKv2 CP) config.
func cpTestConfig() K8sConfig {
	return K8sConfig{
		CMKv2CPEnabled: true,
		CMLEndpoint:    "https://cms-monitoring.example.com",
		NodeLabels:     testNodeLabels(),
	}
}

// renderCP renders the CP-only pipeline and normalizes types via a YAML round-trip.
func renderCP(t *testing.T, cfg K8sConfig) map[string]any {
	t.Helper()
	baseCfg := GenerateK8sBase(cfg)
	out, err := yaml.Marshal(baseCfg)
	require.NoError(t, err)
	var result map[string]any
	require.NoError(t, yaml.Unmarshal(out, &result))
	return result
}

// TestCMKv2CP_AuditSourceMatchesLegacyAndPathEncoded: matches flat + path-encoded layouts, disjoint globs.
func TestCMKv2CP_AuditSourceMatchesLegacyAndPathEncoded(t *testing.T) {
	cfg := renderCP(t, cpTestConfig())

	sources := getSources(cfg)
	require.Contains(t, sources, "cmkv2_audit")
	src := sources["cmkv2_audit"].(map[string]any)
	assert.Equal(t, "file", src["type"])

	includes, ok := src["include"].([]any)
	require.True(t, ok, "include must be a list")
	globs := make([]string, 0, len(includes))
	for _, g := range includes {
		globs = append(globs, g.(string))
	}

	const base = "/var/lib/kubelet/pods/*/volumes/kubernetes.io~empty-dir/audit-logs"
	assert.Contains(t, globs, base+"/audit*.log", "legacy flat audit files")
	assert.Contains(t, globs, base+"/*/*/audit*.log", "Delta A path-encoded audit files")
}

// TestCMKv2CP_AttributionPrefersPathThenUIDMap: path-encoded tenant wins, else uid_map, else drop.
func TestCMKv2CP_AttributionPrefersPathThenUIDMap(t *testing.T) {
	cfg := renderCP(t, cpTestConfig())

	transforms := getTransforms(cfg)
	require.Contains(t, transforms, "cmkv2_attribute")
	xform := transforms["cmkv2_attribute"].(map[string]any)
	assert.Equal(t, "remap", xform["type"])

	vrl := xform["source"].(string)
	// Delta A: tenant parsed from the audit path (authoritative when present).
	assert.Contains(t, vrl, `/audit-logs/(?P<project>[^/]+)/(?P<cluster>[^/]+)/audit`)
	assert.Contains(t, vrl, ".crusoe_project_id = path_project")
	assert.Contains(t, vrl, ".crusoe_cluster_id = path_cluster")
	// Fallback to the pod-UID uid_map (legacy audit files + operational stdout).
	assert.Contains(t, vrl, `get_enrichment_table_record("uid_map"`)
	// Unattributable records are dropped.
	assert.Contains(t, vrl, "abort")
}

// TestCMKv2CP_AuditSummaryAddsSubresourceToMsg: summary folds objectRef.subresource into _msg.
func TestCMKv2CP_AuditSummaryAddsSubresourceToMsg(t *testing.T) {
	cfg := renderCP(t, cpTestConfig())
	transforms := getTransforms(cfg)

	require.Contains(t, transforms, "cmkv2_summarize")
	sum := transforms["cmkv2_summarize"].(map[string]any)
	assert.Equal(t, "remap", sum["type"])

	sumInputs := make([]string, 0)
	for _, in := range sum["inputs"].([]any) {
		sumInputs = append(sumInputs, in.(string))
	}
	assert.Equal(t, []string{"cmkv2_audit"}, sumInputs, "summary runs on the audit source only")

	vrl := sum["source"].(string)
	assert.Contains(t, vrl, `.log_source = "k8s_audit"`)
	assert.Contains(t, vrl, `sub = string(parsed.objectRef.subresource)`)
	assert.Contains(t, vrl, `r = r + "/" + sub`) // subresource folded into resource
	assert.Contains(t, vrl, `._msg =`)

	// Attribution now consumes the summarized audit only (operational has its own transform).
	attr := transforms["cmkv2_attribute"].(map[string]any)
	attrInputs := make([]string, 0)
	for _, in := range attr["inputs"].([]any) {
		attrInputs = append(attrInputs, in.(string))
	}
	assert.Equal(t, []string{"cmkv2_summarize"}, attrInputs)
}

// TestCMKv2CP_OperationalTagBuildsMsgFromNamespaceLabels: ns-label attribution, per-component log_source, _msg.
func TestCMKv2CP_OperationalTagBuildsMsgFromNamespaceLabels(t *testing.T) {
	cfg := renderCP(t, cpTestConfig())
	transforms := getTransforms(cfg)

	require.Contains(t, transforms, "cmkv2_tag_operational")
	op := transforms["cmkv2_tag_operational"].(map[string]any)
	assert.Equal(t, "remap", op["type"])

	opInputs := make([]string, 0)
	for _, in := range op["inputs"].([]any) {
		opInputs = append(opInputs, in.(string))
	}
	assert.Equal(t, []string{"cmkv2_operational"}, opInputs)

	vrl := op["source"].(string)
	assert.Contains(t, vrl, `.kubernetes.namespace_labels."crusoe.ai/cluster-id"`)
	assert.NotContains(t, vrl, "get_enrichment_table_record") // operational does NOT use the uid_map
	assert.Contains(t, vrl, `.log_source = "k8s_apiserver"`)
	assert.Contains(t, vrl, "._msg")

	// Sink consumes both audit (via attribute) and operational (via its tag transform).
	sink := getSinks(cfg)["cmkv2_cml"].(map[string]any)
	sinkInputs := make([]string, 0)
	for _, in := range sink["inputs"].([]any) {
		sinkInputs = append(sinkInputs, in.(string))
	}
	assert.Contains(t, sinkInputs, "cmkv2_attribute")
	assert.Contains(t, sinkInputs, "cmkv2_tag_operational")
}

// TestCMKv2CP_PipelineWiredEndToEnd: CP pipeline renders its sources, uid_map table, and sinks.
func TestCMKv2CP_PipelineWiredEndToEnd(t *testing.T) {
	cfg := renderCP(t, cpTestConfig())

	assert.Contains(t, getSources(cfg), "cmkv2_operational")
	assert.Contains(t, getSinks(cfg), "cmkv2_cml")

	// Internal metrics are exposed so drops are visible (component_discarded_events_total).
	assert.Contains(t, getSources(cfg), "internal_metrics")
	assert.Contains(t, getSinks(cfg), "internal_metrics_exporter")

	cpExporter, ok := getSinks(cfg)["internal_metrics_exporter"].(map[string]any)
	require.True(t, ok, "internal_metrics_exporter sink must be present")
	assert.Equal(t, "0.0.0.0:9598", cpExporter["address"])

	tables, ok := cfg["enrichment_tables"].(map[string]any)
	require.True(t, ok, "enrichment_tables must be present")
	assert.Contains(t, tables, "uid_map")
}
