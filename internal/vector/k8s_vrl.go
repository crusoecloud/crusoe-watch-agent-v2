package vector

import (
	"fmt"
	"strings"
)

// K8s-specific VRL (Vector Remap Language) constants and builders.
// These are used by the K8s config generator for log and metric transforms.
// VM-mode VRL lives in vm_vrl.go; shared constants are not duplicated.

// ---------------------------------------------------------------------------
// Log pipeline (K8s mode)
// ---------------------------------------------------------------------------

const vrlFilterJournaldNoise = `
!((string(.SYSLOG_IDENTIFIER) ?? "") == "containerd" && contains(string(.message) ?? "", "as a uint from Cgroup file"))
`

// vrlParseJournaldLogsK8s is the K8s version of the journald log parser.
const vrlParseJournaldLogsK8s = vrlParseJournaldBody + `
# klog prefix parser. Without this, kubelet stdout is always level=info
# (journald PRIORITY is fixed at 6); parse_klog reads the leading severity
# letter.
klog_emitters = ["kubelet", "kube-proxy"]
if includes(klog_emitters, syslog_id) {
    parsed_klog, klog_err = parse_klog(msg)
    if klog_err == null && is_object(parsed_klog) {
        if exists(parsed_klog.message) {
            ._msg = string!(parsed_klog.message)
        }
        if exists(parsed_klog.level) {
            .level = string!(parsed_klog.level)
        }
        if exists(parsed_klog.timestamp) {
            ._time = parsed_klog.timestamp
        }
    }
}
`

// vrlUnwrapCRIAndParseBody unwraps the CRI log wrapper and applies shared logfmt parser body.
// Callers prepend a `.log_source = "..."` line to identify the emitter.
const vrlUnwrapCRIAndParseBody = `
msg = string(.message) ?? ""
log_line = msg

# Unwrap CRI log format: <timestamp> <stream> <tag> <log_line>
parsed_cri = parse_regex(msg, r'^(?P<cri_time>\S+) (?P<stream>\S+) \S+ (?P<log>.*)$') ?? null
if parsed_cri != null {
    cri_time, ts_err = parse_timestamp(string!(parsed_cri.cri_time), format: "%+")
    if ts_err == null {
        ._time = cri_time
    }
    log_line = string!(parsed_cri.log)
}
` + vrlParseCwaManagerBody

// All agent binaries share the logfmt handler, so their parsers differ only in log_source.
const vrlParseCwaManagerLogsK8s = "\n.log_source = \"cwa-manager\"\n" + vrlUnwrapCRIAndParseBody

const vrlParseReportRunnerLogsK8s = "\n.log_source = \"cwa-report-runner\"\n" + vrlUnwrapCRIAndParseBody

const vrlParseCwaUpdaterLogsK8s = "\n.log_source = \"cwa-updater\"\n" + vrlUnwrapCRIAndParseBody

// vrlEnrichLogsK8s is the K8s version of the envelope assembly. crusoe_watch_version
// is populated from AGENT_VERSION (helm AppVersion).
const vrlEnrichLogsK8s = vrlEnrichLogsPrefix +
	`{ "agent": "crusoe-watch-agent", "crusoe_watch_version": "${AGENT_VERSION}" }` +
	vrlEnrichLogsSuffix

// ---------------------------------------------------------------------------
// Metric transforms (K8s mode)
// ---------------------------------------------------------------------------

// vrlAddInternalLabelsK8s tags filtered internal metrics with cluster identity and version.
// Differs from VM mode: adds cluster_id for cluster identification. install_type is
// "kubernetes" here, set explicitly on the Vector container in the daemonset.
const vrlAddInternalLabelsK8s = `
.tags.cluster_id = "${CRUSOE_CLUSTER_ID}"
.tags.vm_id = "${VM_ID}"
.tags.crusoe_resource = "vm"
.tags.chart_version = "${AGENT_VERSION}"
.tags.install_type = "${INSTALL_TYPE:-unspecified}"
`

// vrlEnrichKSM tags kube-state-metrics with cluster identity.
const vrlEnrichKSM = `
.tags.cluster_id = "${CRUSOE_CLUSTER_ID}"
.tags.project_id = "${CRUSOE_PROJECT_ID}"
.tags.crusoe_resource = "cmk"
.tags.metrics_source = "kube-state-metrics"
`

// vrlEnrichSlurm tags slurm metrics with cluster identity.
const vrlEnrichSlurm = `
.tags.cluster_id = "${CRUSOE_CLUSTER_ID}"
.tags.project_id = "${CRUSOE_PROJECT_ID}"
.tags.crusoe_resource = "cmk"
.tags.metrics_source = "slurm-metrics"
`

// vrlAmdAllowlistFilter is a VRL condition that keeps only approved AMD GPU metrics.
const vrlAmdAllowlistFilter = `
metrics_allowlist = [
    "gpu_used_visible_vram",
    "gpu_free_visible_vram",
    "gpu_total_visible_vram",
    "gpu_gfx_activity",
    "gpu_power_usage",
    "gpu_umc_activity",
    "gpu_prof_tensor_active_percent",
    "pcie_bandwidth",
    "gpu_junction_temperature",
    "gpu_xgmi_link_rx",
    "gpu_xgmi_link_tx",
    "pcie_replay_count",
    "gpu_ecc_uncorrect_total",
    "gpu_ecc_correct_total",
    "gpu_afid_errors",
    "gpu_prof_occupancy_percent",
    "gpu_prof_sm_active",
]
includes(metrics_allowlist, .name)
`

// ---------------------------------------------------------------------------
// Dynamic VRL builders (depend on per-node labels)
// ---------------------------------------------------------------------------

// buildNodeMetricsTransformVRL generates VRL that tags host metrics with node identity.
func buildNodeMetricsTransformVRL(labels NodeLabels) string {
	vrl := fmt.Sprintf(`.tags.nodepool = "%s"
.tags.cluster_id = "${CRUSOE_CLUSTER_ID}"
.tags.vm_id = "${VM_ID}"
.tags.vm_instance_type = "%s"
.tags.node = "%s"
`, labels.NodepoolID, labels.InstanceType, labels.Hostname)

	if labels.PodID != "" {
		vrl += fmt.Sprintf("if \"%s\" != \"\" { .tags.pod_id = \"%s\" }\n", labels.PodID, labels.PodID)
	}

	vrl += `.tags.crusoe_resource = "vm"` + "\n"
	vrl += `.tags.metrics_source = "node-metrics"` + "\n"

	return vrl
}

// vrlEnrichCME tags Crusoe Metrics Exporter metrics.
const vrlEnrichCME = `.tags.cluster_id = "${CRUSOE_CLUSTER_ID}"
.tags.project_id = "${CRUSOE_PROJECT_ID}"
.tags.vm_id = "${VM_ID}"
.tags.crusoe_resource = "vm_custom_infra"
.tags.metrics_source = "crusoe-metrics-exporter"
`

// buildCustomMetricsFilterVRL generates VRL lines that apply allowlist or droplist filtering.
func buildCustomMetricsFilterVRL(deploymentCfg map[string]any) []string {
	var lines []string

	if allowlist, found := toStringSlice(deploymentCfg["allowlist"]); found && len(allowlist) > 0 {
		quoted := make([]string, len(allowlist))
		for i, metric := range allowlist {
			quoted[i] = fmt.Sprintf("%q", metric)
		}
		lines = append(lines, fmt.Sprintf("allowed_metrics = [%s]", strings.Join(quoted, ", ")))
		lines = append(lines, "if !includes(allowed_metrics, .name) { abort }")
	} else if droplist, found := toStringSlice(deploymentCfg["droplist"]); found && len(droplist) > 0 {
		quoted := make([]string, len(droplist))
		for i, metric := range droplist {
			quoted[i] = fmt.Sprintf("%q", metric)
		}
		lines = append(lines, fmt.Sprintf("dropped_metrics = [%s]", strings.Join(quoted, ", ")))
		lines = append(lines, "if includes(dropped_metrics, .name) { abort }")
	}

	return lines
}

// buildCustomMetricsLabelVRL generates VRL lines that drop/add labels per deployment config.
func buildCustomMetricsLabelVRL(deploymentCfg map[string]any) []string {
	var lines []string

	if dropLabels, found := toStringSlice(deploymentCfg["dropLabels"]); found {
		for _, label := range dropLabels {
			lines = append(lines, fmt.Sprintf("del(.tags.%s)", label))
		}
	}

	if addLabels, isSlice := deploymentCfg["addLabels"].([]any); isSlice {
		for _, entry := range addLabels {
			if labelMap, isMap := entry.(map[string]any); isMap {
				for key, val := range labelMap {
					lines = append(lines, fmt.Sprintf(".tags.%s = %q", key, fmt.Sprint(val)))
				}
			}
		}
	}

	return lines
}

// buildCustomMetricsEnrichVRL generates VRL lines that tag custom metrics with node identity.
func buildCustomMetricsEnrichVRL(pod ClassifiedPod, labels NodeLabels) []string {
	if pod.AppID != "" {
		return []string{
			`.tags.crusoe_resource = "custom_internal"`,
			`.tags.cluster_id = "${CRUSOE_CLUSTER_ID}"`,
			fmt.Sprintf(".tags.app_id = %q", pod.AppID),
			fmt.Sprintf(".tags.pod_ip = %q", pod.IP),
			fmt.Sprintf(".tags.pod_name = %q", pod.Name),
		}
	}

	lines := []string{
		fmt.Sprintf(".tags.nodepool = %q", labels.NodepoolID),
		`.tags.cluster_id = "${CRUSOE_CLUSTER_ID}"`,
		`.tags.vm_id = "${VM_ID}"`,
		fmt.Sprintf(".tags.vm_instance_type = %q", labels.InstanceType),
	}
	if labels.PodID != "" {
		lines = append(lines, fmt.Sprintf(`if %q != "" { .tags.pod_id = %q }`, labels.PodID, labels.PodID))
	}
	lines = append(lines,
		`.tags.crusoe_resource = "custom_metrics"`,
		`.tags.metrics_source = "custom-metrics"`,
		fmt.Sprintf(".tags.pod_ip = %q", pod.IP),
		fmt.Sprintf(".tags.pod_name = %q", pod.Name),
	)

	return lines
}

// buildCustomMetricsTransformVRL generates per-pod VRL that applies allowlist/droplist
// filtering and tag enrichment.
func buildCustomMetricsTransformVRL(deploymentCfg map[string]any, pod ClassifiedPod, labels NodeLabels) string {
	var lines []string
	lines = append(lines, buildCustomMetricsFilterVRL(deploymentCfg)...)
	lines = append(lines, buildCustomMetricsLabelVRL(deploymentCfg)...)
	lines = append(lines, buildCustomMetricsEnrichVRL(pod, labels)...)

	return strings.Join(lines, "\n")
}
