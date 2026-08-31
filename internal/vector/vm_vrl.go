package vector

// VM-specific VRL (Vector Remap Language) constants.
// Shared VRL lives in common_vrl.go; K8s-specific VRL in k8s_vrl.go.

// ---------------------------------------------------------------------------
// Log pipeline (VM mode)
// ---------------------------------------------------------------------------

// vrlParseJournaldLogs is the VM version: the shared body.
const vrlParseJournaldLogs = vrlParseJournaldBody

// vrlParseCwaManagerLogs is the VM version: journald source → logfmt body.
const vrlParseCwaManagerLogs = `
.log_source = "cwa-manager"
log_line = string(.message) ?? ""
` + vrlParseCwaManagerBody

// vrlParseReportRunnerLogs is the VM version: journald source → logfmt body.
// report-runner shares cwa-manager's logfmt handler, so it reuses the same body.
const vrlParseReportRunnerLogs = `
.log_source = "cwa-report-runner"
log_line = string(.message) ?? ""
` + vrlParseCwaManagerBody

// vrlEnrichLogs is the VM version of the envelope assembly.
const vrlEnrichLogs = vrlEnrichLogsPrefix +
	`.crusoe_watch_version = "${AGENT_VERSION}"` +
	vrlEnrichLogsSuffix

// ---------------------------------------------------------------------------
// Metrics pipeline (VM mode)
// ---------------------------------------------------------------------------

// vrlAddUpdateLabels replaces the auto-populated hostname tag with vm_id
// on host metrics (and GPU metrics when present).
const vrlAddUpdateLabels = `
del(.tags.Hostname)
.tags.vm_id = "${VM_ID}"
.tags.crusoe_resource = "vm"
`

// vrlAddInternalLabels tags filtered internal metrics with vm_id, agent version,
// and install type (docker | systemd). install_type is set by the installer in .env.
const vrlAddInternalLabels = `
.tags.vm_id = "${VM_ID}"
.tags.crusoe_resource = "vm"
.tags.agent_version = "${AGENT_VERSION}"
.tags.install_type = "${INSTALL_TYPE:-unspecified}"
`

// vrlEnrichCMEMetrics tags Crusoe Metrics Exporter metrics with a distinct
// resource type so they route separately from standard VM metrics.
const vrlEnrichCMEMetrics = `
.tags.vm_id = "${VM_ID}"
.tags.crusoe_resource = "vm_custom_infra"
.tags.agent_version = "${AGENT_VERSION}"
`
