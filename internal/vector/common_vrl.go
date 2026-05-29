package vector

// Shared VRL (Vector Remap Language) constants used by both VM and K8s modes.
// Mode-specific VRL lives in vm_vrl.go and k8s_vrl.go.

// ---------------------------------------------------------------------------
// Log pipeline (shared)
// ---------------------------------------------------------------------------

// vrlParseJournaldBody is the shared journald log parser body used by both
// VM and K8s modes. It maps syslog PRIORITY to a level name and extracts
// structured fields from known logfmt emitters.
const vrlParseJournaldBody = `
.log_source = "journald"

# Map syslog PRIORITY (0–7) to canonical level names.
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
# Gate on ` + "`" + `^\S+=` + "`" + ` to avoid false positives. Validate extracted keys
# are proper identifiers. Everything else keeps its raw _msg.
logfmt_emitters = ["containerd", "dockerd", "etcd"]
syslog_id = string(.SYSLOG_IDENTIFIER) ?? ""

if includes(logfmt_emitters, syslog_id) && match(msg, r'^\S+=') {
    parsed, err = parse_logfmt(msg)
    if err == null && is_object(parsed) {
        structured_fields = {}
        for_each(object(parsed)) -> |key, value| {
            is_bare = (value == true) || (value == "true")
            if !is_bare && match(key, r'^[a-zA-Z_][a-zA-Z0-9_.-]*$') {
                structured_fields = set!(structured_fields, [key], value)
            }
        }

        if length(structured_fields) > 0 {
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
            for_each(structured_fields) -> |key, value| {
                if key != "msg" && key != "time" && key != "level" {
                    . = set!(., [key], value)
                }
            }
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
    for_each(object(parsed)) -> |key, value| {
        if key != "msg" && key != "time" && key != "level" {
            . = set!(., [key], value)
        }
    }
}

del(.message)
del(.timestamp)
`

// vrlEnrichLogsBody is the shared log enrichment body: deletes source_type,
// normalizes timestamps and messages, and maps log levels to a canonical enum.
// Mode-specific headers (agent metadata) are prepended by vrlEnrichLogs / vrlEnrichLogsK8s.
const vrlEnrichLogsBody = `
del(.source_type)

# Timestamp fallback: __REALTIME_TIMESTAMP (journald) → .timestamp (internal)
if !exists(._time) {
    if exists(.__REALTIME_TIMESTAMP) {
        ._time = .__REALTIME_TIMESTAMP
    } else if exists(.timestamp) {
        ._time = .timestamp
    }
}

# Message fallback
if !exists(._msg) && exists(.message) {
    ._msg = del(.message)
}

# Level normalization: map synonyms to canonical enum, drop unrecognized.
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
if exists(.level) {
    lvl = downcase(string!(.level))
    normalized = get(level_synonyms, [lvl]) ?? null
    if normalized != null {
        .level = normalized
    } else {
        del(.level)
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
