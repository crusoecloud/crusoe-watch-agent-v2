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

// vrlParseK8sAgentBody parses logfmt from kubernetes_logs events.
// Callers must set .log_source before this body runs.
const vrlParseK8sAgentBody = `
log_line = string(.message) ?? ""

# Use .timestamp as the default for ._time. The logfmt time field overrides it.
if exists(.timestamp) {
    ._time = .timestamp
}
` + vrlParseCwaManagerBody

// All agent binaries use the same logfmt handler. Parsers differ only in log_source.
const vrlParseCwaManagerLogsK8s = "\n.log_source = \"cwa-manager\"\n" + vrlParseK8sAgentBody

const vrlParseReportRunnerLogsK8s = "\n.log_source = \"cwa-report-runner\"\n" + vrlParseK8sAgentBody

const vrlParseCwaUpdaterLogsK8s = "\n.log_source = \"cwa-updater\"\n" + vrlParseK8sAgentBody

// vrlParseOperatorLogsK8s parses the GPU / network operator Deployment logs.
const vrlParseOperatorLogsK8s = `
# Three log formats across these Deployments; first match wins, and klog is
# tried before zap console, which is lenient enough to claim its lines.
# Anything else keeps its raw text as _msg with no level.
#   json         gpu-operator (zap), nv-ipam-controller (zapr)
#   klog         node-feature-discovery master + gc
#   zap console  network-operator controller

# The namespace minus its optional nvidia- prefix, so the prefixed and
# unprefixed installs of the same operator converge on one value.
.log_source = replace(to_string(.kubernetes.pod_namespace) ?? "", r'^nvidia-', "")

# Drop node-feature-discovery's ~75 hardware-capability labels, which
# kubernetes_logs copies onto every line. Other node labels stay.
if is_object(.kubernetes.node_labels) {
    .kubernetes.node_labels = filter(object!(.kubernetes.node_labels)) -> |key, _value| {
        !starts_with(key, "feature.node.kubernetes.io/")
    }
}

msg = to_string(.message) ?? ""
if msg != "" {
    ._msg = msg
}

matched = false

json_fields = object(parse_json(msg) ?? {}) ?? {}
if !is_empty(json_fields) {
    matched = true
    # Lift the parsed fields onto the event so they ship as payload.<field>.
    # Merging under the event keeps vector's metadata on a name collision.
    . = merge(json_fields, ., deep: true)
    # zap uses "msg"; logrus and the k8s libraries use "message".
    if exists(json_fields.msg) {
        ._msg = to_string(json_fields.msg) ?? msg
    } else if exists(json_fields.message) {
        ._msg = to_string(json_fields.message) ?? msg
    }
    # enrich_logs normalizes the enum and drops anything unrecognized.
    if exists(json_fields.level) {
        .level = to_string(json_fields.level) ?? ""
    } else if exists(json_fields.severity) {
        .level = to_string(json_fields.severity) ?? ""
    } else if exists(json_fields.error) || exists(json_fields.err) {
        # zapr omits the level key: Error() emits an error field and no v,
        # Info() emits v and no error field. The key is "error" or "err".
        .level = "error"
    } else if exists(json_fields.v) {
        # v is verbosity, not severity: V(0) is Info, higher is Debug.
        verbosity = to_int(json_fields.v) ?? 0
        .level = "debug"
        if verbosity == 0 {
            .level = "info"
        }
    }
}

if !matched {
    klog_fields = object(parse_klog(msg) ?? {})
    if !is_empty(klog_fields) {
        matched = true
        if exists(klog_fields.message) {
            klog_msg = to_string(klog_fields.message)
            ._msg = klog_msg
            # klog's structured form is: "message" key=value ... -- unwrap the
            # message, lift the trailing pairs to payload.<field>.
            unwrapped = object(parse_regex(klog_msg, r'^"(?P<msg>[^"]*)"(?P<fields>.*)$') ?? {})
            if !is_empty(unwrapped) {
                ._msg = unwrapped.msg
                klog_tail = to_string(unwrapped.fields)
                if match(klog_tail, r'\S+=') {
                    logfmt_fields = object(parse_logfmt(klog_tail) ?? {})
                    if !is_empty(logfmt_fields) {
                        . = merge(logfmt_fields, ., deep: true)
                    }
                }
            }
        }
        if exists(klog_fields.level) {
            .level = klog_fields.level
        }
        if exists(klog_fields.timestamp) {
            ._time = klog_fields.timestamp
        }
    }
}

if !matched {
    # Tab separated: <ts> <LEVEL> [logger] <message> [{fields}].
    zap = object(parse_regex(msg, r'^[^\t]+\t(?P<level>[A-Z]+)\t(?P<rest>.*)$') ?? {})
    if !is_empty(zap) {
        zap_level = downcase(to_string(zap.level))
        # Guard on a real level word so tabbed plain text is not claimed.
        if includes(["debug", "info", "warn", "warning", "error", "fatal"], zap_level) {
            .level = zap_level
            rest = to_string(zap.rest)
            # Lift the optional trailing {...} object to payload.<field>.
            zap_tail = object(parse_regex(rest, r'\t(?P<fields>\{.*\})$') ?? {})
            if !is_empty(zap_tail) {
                zap_fields = object(parse_json(to_string(zap_tail.fields)) ?? {}) ?? {}
                if !is_empty(zap_fields) {
                    . = merge(zap_fields, ., deep: true)
                }
            }
            # Drop the optional trailing field object, then the optional logger.
            rest = replace(rest, r'\t\{.*\}$', "")
            tail = object(parse_regex(rest, r'(?P<msg>[^\t]*)$') ?? {})
            if !is_empty(tail) {
                ._msg = tail.msg
            }
        }
    }
}

# The raw line still ships verbatim as payload.message, alongside the parsed
# payload.<field> and the scratch ._msg / ._time / .level / .log_source.
`

// vrlEnrichLogsK8s is the K8s version of the envelope assembly.
// crusoe_watch_version is populated from AGENT_VERSION (helm AppVersion).
const vrlEnrichLogsK8s = vrlEnrichLogsPrefix +
	`.crusoe_watch_version = "${AGENT_VERSION}"` +
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
