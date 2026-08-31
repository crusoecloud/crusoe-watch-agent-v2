package vector

// Shared VRL (Vector Remap Language) constants used by both VM and K8s modes.
// Mode-specific VRL lives in vm_vrl.go and k8s_vrl.go.

// ---------------------------------------------------------------------------
// Log pipeline (shared)
// ---------------------------------------------------------------------------

// vrlParseJournaldBody is the shared journald log parser body used by both
// VM and K8s modes. It maps syslog PRIORITY to a level name and extracts
// parsed msg/level/time from known logfmt emitters.
const vrlParseJournaldBody = `
.log_source = "journald"

# Map syslog PRIORITY (0–7) to a canonical level.
priority = to_string(.PRIORITY) ?? ""
if priority == "0" {
    .level = "emergency"
} else if priority == "1" {
    .level = "alert"
} else if priority == "2" {
    .level = "critical"
} else if priority == "3" {
    .level = "error"
} else if priority == "4" {
    .level = "warning"
} else if priority == "5" {
    .level = "notice"
} else if priority == "6" {
    .level = "info"
} else if priority == "7" {
    .level = "debug"
}

msg = string(.message) ?? ""
._msg = msg

# Only parse logfmt from known emitters (containerd, dockerd, etcd).
# Gate on ` + "`" + `^\S+=` + "`" + ` to avoid false positives.
logfmt_emitters = ["containerd", "dockerd", "etcd"]
syslog_id = string(.SYSLOG_IDENTIFIER) ?? ""

if includes(logfmt_emitters, syslog_id) && match(msg, r'^\S+=') {
    parsed, err = parse_logfmt(msg)
    if err == null && is_object(parsed) {
        structured_fields = object(parsed)
        if exists(structured_fields.msg) {
            ._msg = string!(structured_fields.msg)
        }
        if exists(structured_fields.time) {
            parsed_time, ts_err = parse_timestamp(string!(structured_fields.time), format: "%+")
            if ts_err == null {
                ._time = parsed_time
            }
        }
        if exists(structured_fields.level) {
            .level = downcase(string!(structured_fields.level))
        }
    }
}
`

// vrlParseInternalLogs tags Vector's own logs with a source identifier.
const vrlParseInternalLogs = `
.log_source = "crusoe-watch-agent"
if exists(.metadata.level) {
    .level = downcase(string!(.metadata.level))
}
`

// vrlParseCwaManagerBody is the shared logfmt parser for cwa-manager logs.
// cwa-manager uses Go slog.TextHandler, so output is always well-formed logfmt.
// Callers must set log_line before this body.
const vrlParseCwaManagerBody = `
._msg = log_line

parsed, err = parse_logfmt(log_line)
if err == null && is_object(parsed) {
    if exists(parsed.msg) { ._msg = string!(parsed.msg) }
    if exists(parsed.time) {
        parsed_time, ts_err = parse_timestamp(string!(parsed.time), format: "%+")
        if ts_err == null { ._time = parsed_time }
    }
    if exists(parsed.level) { .level = downcase(string!(parsed.level)) }
}
`

// vrlEnrichLogsPrefix and vrlEnrichLogsSuffix assemble the standardized
// envelope: the raw event verbatim under `payload`, identity fields at the
// top level (the mode-specific literal is spliced between the two), and
// `_msg`/`_time`/`level`/`log_source` at the top level.
const vrlEnrichLogsPrefix = `
parsed_msg = null
if exists(._msg) {
    parsed_msg = del(._msg)
}
parsed_time = null
if exists(._time) {
    parsed_time = del(._time)
}
cwa_level = null
if exists(.level) {
    cwa_level = del(.level)
}
cwa_log_source = null
if exists(.log_source) {
    cwa_log_source = del(.log_source)
}

raw = .
. = {}
.payload = raw
`

const vrlEnrichLogsSuffix = `

if cwa_log_source != null {
    .log_source = cwa_log_source
}

if parsed_msg != null {
    ._msg = parsed_msg
} else if exists(.payload.message) {
    ._msg = .payload.message
}

if parsed_time != null {
    ._time = parsed_time
} else if exists(.payload.__REALTIME_TIMESTAMP) {
    ._time = .payload.__REALTIME_TIMESTAMP
} else if exists(.payload.timestamp) {
    ._time = .payload.timestamp
}

# Normalize level to canonical enum (mirrors GCP Cloud Logging SEVERITY_TRANSLATIONS).
# Unrecognized values are omitted.
level_synonyms = {
    "emergency": "emergency", "emerg": "emergency",
    "alert": "alert", "a": "alert",
    "critical": "critical", "crit": "critical", "fatal": "critical", "c": "critical", "f": "critical",
    "error": "error", "err": "error", "severe": "error", "e": "error",
    "warning": "warning", "warn": "warning", "w": "warning",
    "notice": "notice", "n": "notice",
    "info": "info", "information": "info", "i": "info",
    "debug": "debug", "trace": "debug", "trace_int": "debug",
    "fine": "debug", "finer": "debug", "finest": "debug", "config": "debug", "d": "debug"
}
if cwa_level != null {
    lvl = downcase(string!(cwa_level))
    normalized = get(level_synonyms, [lvl]) ?? null
    if normalized != null {
        .level = normalized
    }
}
`

// ---------------------------------------------------------------------------
// Metrics pipeline (shared)
// ---------------------------------------------------------------------------

// vrlFilterInternalMetrics is a filter condition (not a remap source) that
// whitelists Vector's internal metrics to a curated set.
const vrlFilterInternalMetrics = `
includes(
    [
        "buffer_byte_size",
        "buffer_discarded_events_total",
        "buffer_events",
        "buffer_received_events_total",
        "buffer_sent_events_total",
        "build_info",
        "component_discarded_events_total",
        "component_errors_total",
        "component_received_events_total",
        "component_sent_events_total",
        "config_reloaded",
        "connection_established_total",
        "connection_send_errors_total",
        "http_client_requests_sent_total",
        "http_client_responses_total",
        "internal_metrics_cardinality",
        "open_connections",
        "open_files",
        "uptime_seconds",
    ],
    .name,
)
`
