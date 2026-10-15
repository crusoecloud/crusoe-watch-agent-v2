// VRL sources for the CMKv2 control-plane (CP) pipeline (matches the k8s_vrl.go / vm_vrl.go split).
package vector

// vrlCMKv2AuditSummarize parses the audit JSON and builds _msg (incl. objectRef.subresource).
const vrlCMKv2AuditSummarize = `
.payload = .
.log_source = "k8s_audit"
parsed, perr = parse_json(.message)
if perr == null && is_object(parsed) {
    .payload.parsed = parsed
}
u = string(parsed.user.username) ?? "?"
v = string(parsed.verb) ?? "?"
r = string(parsed.objectRef.resource) ?? "?"
sub = string(parsed.objectRef.subresource) ?? ""
if sub != "" {
    r = r + "/" + sub
}
d = string(parsed.annotations."authorization.k8s.io/decision") ?? "?"
._msg = "User: " + u + "; " + v + " " + r + "; decision=" + d
if exists(parsed.stageTimestamp) {
    ._time = parsed.stageTimestamp
}
.level = "info"
del(.message)
`

// vrlCMKv2OperationalTag tags CP stdout from ns labels: log_source + json/klog/logrus parse, drops unattributable.
const vrlCMKv2OperationalTag = `
cid = .kubernetes.namespace_labels."crusoe.ai/cluster-id"
oid = .kubernetes.namespace_labels."crusoe.ai/org-id"
pid = .kubernetes.namespace_labels."crusoe.ai/project-id"
del(.kubernetes.namespace_labels)
del(.kubernetes.node_labels)
del(.kubernetes.pod_labels)
del(.kubernetes.pod_annotations)
del(.kubernetes.pod_owner)
del(.kubernetes.container_image)
del(.kubernetes.container_image_id)
del(.kubernetes.container_id)
del(.kubernetes.pod_ip)
del(.kubernetes.pod_ips)
.payload = .
.crusoe_cluster_id = string(cid) ?? ""
.crusoe_org_id = string(oid) ?? ""
.crusoe_project_id = string(pid) ?? ""
container = string(.kubernetes.container_name) ?? ""
if container == "kube-apiserver" {
    .log_source = "k8s_apiserver"
} else if container == "etcd" {
    .log_source = "k8s_etcd"
} else if container == "kube-scheduler" {
    .log_source = "k8s_scheduler"
} else if container == "kube-controller-manager" {
    .log_source = "k8s_controller_manager"
} else if container == "konnectivity-server" {
    .log_source = "k8s_konnectivity"
} else {
    .log_source = "k8s_operational"
}
parsed_json, perr = parse_json(.message)
if perr == null && is_object(parsed_json) {
    .payload.parsed = parsed_json
    if exists(parsed_json.apiVersion) && parsed_json.apiVersion == "audit.k8s.io/v1" {
        u = string(parsed_json.user.username) ?? "?"
        v = string(parsed_json.verb) ?? "?"
        r = string(parsed_json.objectRef.resource) ?? "?"
        sub = string(parsed_json.objectRef.subresource) ?? ""
        if sub != "" {
            r = r + "/" + sub
        }
        d = string(parsed_json.annotations."authorization.k8s.io/decision") ?? "?"
        ._msg = "User: " + u + "; " + v + " " + r + "; decision=" + d
        if exists(parsed_json.stageTimestamp) { ._time = parsed_json.stageTimestamp }
        .level = "info"
    } else {
        ._msg = .message
        if exists(parsed_json.msg) { ._msg = parsed_json.msg }
        if exists(parsed_json.message) { ._msg = parsed_json.message }
        if exists(parsed_json.ts) { ._time = parsed_json.ts }
        if exists(parsed_json.timestamp) { ._time = parsed_json.timestamp }
        .level = "unknown"
        if exists(parsed_json.level) { .level = parsed_json.level }
    }
} else {
    klog, kerr = parse_klog(.message)
    if kerr == null {
        .payload.parsed = klog
        ._msg = .message
        if exists(.payload.parsed.msg) { ._msg = .payload.parsed.msg }
        if exists(.payload.parsed.message) { ._msg = .payload.parsed.message }
        if exists(.payload.parsed.timestamp) { ._time = .payload.parsed.timestamp }
        .level = "unknown"
        if exists(.payload.parsed.level) { .level = .payload.parsed.level }
    } else {
        tsv, tsverr = parse_regex(.message, r'^(?P<ts>[^\t]+)\t(?P<lvl>[^\t]+)\t(?P<msg>[^\t]*)(?:\t(?P<rest>.*))?$')
        if tsverr == null {
            .payload.parsed.timestamp = tsv.ts
            .payload.parsed.level = tsv.lvl
            .payload.parsed.msg = tsv.msg
            ._msg = tsv.msg
            ._time = tsv.ts
            .level = tsv.lvl
            rj, rjerr = parse_json(tsv.rest)
            if rjerr == null && is_object(rj) { .payload.parsed.fields = rj }
        } else {
            lkv, lkverr = parse_key_value(.message)
            if lkverr == null {
                .payload.parsed = lkv
                ._msg = .message
                if exists(.payload.parsed.msg) { ._msg = .payload.parsed.msg }
                if exists(.payload.parsed.time) { ._time = .payload.parsed.time }
                .level = "unknown"
                if exists(.payload.parsed.level) { .level = .payload.parsed.level }
            } else {
                ._msg = .message
                .level = "unknown"
            }
        }
    }
}
del(.kubernetes)
del(.message)
if .crusoe_cluster_id == "" || .crusoe_project_id == "" || .crusoe_org_id == "" {
    abort
}
`

// vrlCMKv2Attribute attributes each record: path-encoded project/cluster when present, else uid_map.
const vrlCMKv2Attribute = `
uid = ""
path_project = ""
path_cluster = ""
if exists(.file) {
    uid = parse_regex(.file, r'/kubelet/pods/(?P<uid>[^/]+)/').uid ?? ""
    p = parse_regex(.file, r'/audit-logs/(?P<project>[^/]+)/(?P<cluster>[^/]+)/audit[^/]*\.log$') ?? null
    if p != null {
        path_project = p.project
        path_cluster = p.cluster
    }
} else if exists(.kubernetes.pod_uid) {
    uid = to_string(.kubernetes.pod_uid) ?? ""
}

row = get_enrichment_table_record("uid_map", {"uid": uid}) ?? null

if path_project != "" && path_cluster != "" {
    # Path-encoded attribution is authoritative; org still comes from the map for now.
    .crusoe_project_id = path_project
    .crusoe_cluster_id = path_cluster
    if row == null {
        abort
    }
    .crusoe_org_id = row.org_id
} else if row != null {
    .crusoe_cluster_id = row.cluster_id
    .crusoe_project_id = row.project_id
    .crusoe_org_id = row.org_id
} else {
    # Unattributable -> drop (abort); DLQ + per-reason drop metric is a follow-up.
    abort
}
`
