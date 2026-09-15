#!/usr/bin/env bash
# render.sh — substitute placeholders in templated sources using values from
# dependencies.yaml. Called from the release pipeline AFTER checking out the
# tagged commit, never from live main. The output goes to a working dir; the
# release pipeline uploads it but never commits it back to the repo.
#
# Usage:
#   render.sh vm      <release-version> <out-dir>
#   render.sh k8s     <release-version> <out-dir>
#   render.sh updater <release-version> <out-dir>
#
# The <release-version> is what gets stamped into AGENT_VERSION (VM) or
# version/appVersion (K8s, updater). It's the value computed by
# compute-next-version.sh.

set -euo pipefail

die() { echo "ERROR: $*" >&2; exit 1; }

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DEPS_FILE="${REPO_ROOT}/dependencies.yaml"

[[ -f "$DEPS_FILE" ]] || die "dependencies.yaml not found at ${DEPS_FILE}"

MODE="${1:?usage: render.sh <vm|k8s|updater> <release-version> <out-dir>}"
RELEASE_VERSION="${2:?usage: render.sh <vm|k8s|updater> <release-version> <out-dir>}"
OUT_DIR="${3:?usage: render.sh <vm|k8s|updater> <release-version> <out-dir>}"

mkdir -p "$OUT_DIR"

# Parse a single top-level scalar from dependencies.yaml: `key: "value"` or
# `key: value`. Comments and blank lines are ignored.
deps_get() {
    local key="$1"
    awk -v k="$key" '
        /^[[:space:]]*#/ { next }
        /^[[:space:]]*$/ { next }
        {
            line = $0
            sub(/[[:space:]]*#.*$/, "", line)
            n = index(line, ":")
            if (n == 0) next
            name = substr(line, 1, n-1)
            val  = substr(line, n+1)
            gsub(/^[[:space:]]+|[[:space:]]+$/, "", name)
            gsub(/^[[:space:]]+|[[:space:]]+$/, "", val)
            sub(/^"/, "", val); sub(/"$/, "", val)
            sub(/^'\''/, "", val); sub(/'\''$/, "", val)
            if (name == k) { print val; exit }
        }
    ' "$DEPS_FILE"
}

# Look up each known component; missing keys are fatal because that would mean
# the source has a placeholder with no corresponding pin.
load_deps() {
    CWA_MANAGER=$(deps_get cwa-manager)
    CWA_UPDATER=$(deps_get cwa-updater)
    REPORT_RUNNER=$(deps_get report-runner)
    VECTOR=$(deps_get vector)
    CRUSOE_METRICS_EXPORTER=$(deps_get crusoe-metrics-exporter)
    AMD_EXPORTER=$(deps_get amd-exporter)
    DCGM_RELEASE=$(deps_get dcgm-exporter-release)
    DCGM_2004=$(deps_get dcgm-exporter-ubuntu2004)
    DCGM_2204=$(deps_get dcgm-exporter-ubuntu2204)
    DCGM_2404=$(deps_get dcgm-exporter-ubuntu2404)
    TOKEN_JOB=$(deps_get token-job)
    for var in CWA_MANAGER CWA_UPDATER REPORT_RUNNER VECTOR CRUSOE_METRICS_EXPORTER AMD_EXPORTER \
               DCGM_RELEASE DCGM_2004 DCGM_2204 DCGM_2404 TOKEN_JOB; do
        [[ -n "${!var}" ]] || die "missing pin in dependencies.yaml for ${var}"
    done

    # Native mode clones dcgm-exporter-release while docker mode pulls the per-OS image
    # tags, so the tags must be that release plus a base-OS suffix or the two install
    # modes ship different exporters.
    for var in DCGM_2004 DCGM_2204 DCGM_2404; do
        [[ "${!var}" == "${DCGM_RELEASE}"* ]] \
            || die "${var}=${!var} is not built on dcgm-exporter-release=${DCGM_RELEASE}"
    done
}

# Replace @@KEY@@ tokens in $1 (in place). Uses ASCII-1 as the sed delimiter so
# version strings containing `/` or `.` are safe.
substitute_file() {
    local file="$1"; shift
    local d=$'\x01'
    while [[ $# -gt 0 ]]; do
        local key="$1" val="$2"; shift 2
        sed -i.bak "s${d}@@${key}@@${d}${val}${d}g" "$file"
        rm -f "${file}.bak"
    done
}

# Replace a placeholder line in $1 with the contents of file $3. Used for values
# that span lines, which substitute_file's sed cannot carry.
splice_file() {
    local file="$1" key="$2" src="$3"

    [[ -f "$src" ]] || die "cannot splice @@${key}@@: ${src} not found"

    awk -v token="@@${key}@@" -v src="$src" '
        index($0, token) { while ((getline line < src) > 0) print line; next }
        { print }
    ' "$file" > "${file}.spliced"
    mv "${file}.spliced" "$file"
}

# Assert no render-time placeholders remain. EXEC_START/EXEC_STOP are
# install-time markers the rendered VM script substitutes when it writes
# systemd unit files at install time, not release-time placeholders.
RUNTIME_TOKENS_RE='@@(EXEC_START|EXEC_STOP)@@'

assert_no_placeholders() {
    local file="$1"
    local leftover
    leftover=$(grep -nE '@@[A-Z0-9_]+@@' "$file" | grep -vE "$RUNTIME_TOKENS_RE" || true)
    if [[ -n "$leftover" ]]; then
        echo "$leftover" >&2
        die "unrendered placeholders remaining in ${file}"
    fi
}

render_vm() {
    local agent_version="$RELEASE_VERSION"
    local src="${REPO_ROOT}/vm/crusoe_watch_agent.sh"
    local dst="${OUT_DIR}/crusoe_watch_agent.sh"

    [[ -f "$src" ]] || die "VM script not found: ${src}"
    cp "$src" "$dst"
    chmod 0755 "$dst"

    substitute_file "$dst" \
        AGENT_VERSION                       "$agent_version" \
        CWA_MANAGER_VERSION                 "$CWA_MANAGER" \
        CWA_UPDATER_VERSION                 "$CWA_UPDATER" \
        REPORT_RUNNER_VERSION               "$REPORT_RUNNER" \
        VECTOR_VERSION                      "$VECTOR" \
        CRUSOE_METRICS_EXPORTER_VERSION     "$CRUSOE_METRICS_EXPORTER" \
        AMD_EXPORTER_VERSION                "$AMD_EXPORTER" \
        DCGM_EXPORTER_RELEASE               "$DCGM_RELEASE" \
        DCGM_EXPORTER_UBUNTU2004_VERSION    "$DCGM_2004" \
        DCGM_EXPORTER_UBUNTU2204_VERSION    "$DCGM_2204" \
        DCGM_EXPORTER_UBUNTU2404_VERSION    "$DCGM_2404"

    # The installer verifies its release bundle against this key. Splicing it
    # from ci/cosign.pub keeps the installer and the signing job on one key.
    splice_file "$dst" COSIGN_PUBLIC_KEY "${REPO_ROOT}/ci/cosign.pub"

    assert_no_placeholders "$dst"

    # Compose files contain only runtime ${VAR} placeholders filled at install
    # time via the .env that the rendered script writes. They ship as-is.
    mkdir -p "${OUT_DIR}/docker" "${OUT_DIR}/systemctl" "${OUT_DIR}/config"
    cp "${REPO_ROOT}/vm/docker/"*.yaml      "${OUT_DIR}/docker/"
    cp "${REPO_ROOT}/vm/systemctl/"*.service "${OUT_DIR}/systemctl/"
    cp "${REPO_ROOT}/vm/config/"*           "${OUT_DIR}/config/"

    # VERSION asset — fetched by `do_upgrade` from the GitHub Release URL.
    echo "$agent_version" > "${OUT_DIR}/VERSION"

    echo "Rendered VM release ${agent_version} -> ${OUT_DIR}"
}

render_k8s() {
    # Chart.yaml versions must be valid SemVer 2. Strip a leading "v" so that
    # k8s/v0.9 stamps as 0.9. The release pipeline passes whatever
    # compute-next-version produced; trust it but normalize here.
    local chart_version="${RELEASE_VERSION#v}"
    local cwa_manager_version="$CWA_MANAGER"

    [[ -n "$chart_version" ]] || die "release version must not be empty"

    rm -rf "${OUT_DIR}/helm-chart"
    cp -r "${REPO_ROOT}/k8s/helm-chart" "${OUT_DIR}/helm-chart"

    local chart="${OUT_DIR}/helm-chart/Chart.yaml"
    local values="${OUT_DIR}/helm-chart/values.yaml"

    substitute_file "$chart" \
        CHART_VERSION       "$chart_version" \
        CHART_APP_VERSION   "$chart_version"
    substitute_file "$values" \
        CWA_MANAGER_VERSION             "$cwa_manager_version" \
        REPORT_RUNNER_VERSION           "$REPORT_RUNNER" \
        VECTOR_VERSION                  "$VECTOR" \
        TOKEN_JOB_VERSION               "$TOKEN_JOB" \
        CRUSOE_METRICS_EXPORTER_VERSION "$CRUSOE_METRICS_EXPORTER"

    assert_no_placeholders "$chart"
    assert_no_placeholders "$values"

    echo "Rendered K8s release ${RELEASE_VERSION} (chart ${chart_version}) -> ${OUT_DIR}/helm-chart"
}

# cwa-updater ships as its own chart so it never upgrades itself, and as its own
# release mode so it is installed and versioned independently of the agent.
render_updater() {
    local chart_version="${RELEASE_VERSION#v}"

    [[ -n "$chart_version" ]] || die "release version must not be empty"

    rm -rf "${OUT_DIR}/cwa-updater-chart"
    cp -r "${REPO_ROOT}/k8s/cwa-updater-chart" "${OUT_DIR}/cwa-updater-chart"

    local chart="${OUT_DIR}/cwa-updater-chart/Chart.yaml"
    local values="${OUT_DIR}/cwa-updater-chart/values.yaml"

    substitute_file "$chart" \
        CHART_VERSION       "$chart_version" \
        CHART_APP_VERSION   "$chart_version"
    substitute_file "$values" \
        CWA_UPDATER_VERSION "$CWA_UPDATER"

    assert_no_placeholders "$chart"
    assert_no_placeholders "$values"

    echo "Rendered cwa-updater release ${RELEASE_VERSION} (chart ${chart_version}) -> ${OUT_DIR}/cwa-updater-chart"
}

load_deps

case "$MODE" in
    vm)      render_vm ;;
    k8s)     render_k8s ;;
    updater) render_updater ;;
    *)       die "unknown mode: ${MODE} (expected vm, k8s or updater)" ;;
esac
