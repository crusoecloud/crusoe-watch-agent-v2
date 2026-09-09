#!/usr/bin/env bash
# Crusoe Watch Agent 2.0 — VM installer.
#
# Supports Docker (default) and native (--no-docker) installation modes.
# Auto-detects GPU type (NVIDIA / AMD / CPU-only).

set -euo pipefail

###############################################################################
# Constants & defaults
###############################################################################
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Version pins are stamped at release time from dependencies.yaml.
AGENT_VERSION="@@AGENT_VERSION@@"
CWA_MANAGER_VERSION="@@CWA_MANAGER_VERSION@@"
REPORT_RUNNER_VERSION="@@REPORT_RUNNER_VERSION@@"
VECTOR_VERSION="@@VECTOR_VERSION@@"
CME_VERSION="@@CRUSOE_METRICS_EXPORTER_VERSION@@"
AMD_EXPORTER_VERSION="@@AMD_EXPORTER_VERSION@@"
for v in AGENT_VERSION CWA_MANAGER_VERSION REPORT_RUNNER_VERSION VECTOR_VERSION CME_VERSION AMD_EXPORTER_VERSION; do
    case "${!v}" in @@*@@) printf -v "$v" '%s' "dev" ;; esac
done

CMS_BASE_URL="https://cms-monitoring.crusoecloud.com"

# GITHUB_LATEST_RELEASE_URL — used only by `do_upgrade` to fetch the newest available release.
if [[ "$AGENT_VERSION" == "dev" ]]; then
    GITHUB_RELEASE_URL="https://github.com/crusoecloud/crusoe-watch-agent-v2/releases/latest/download"
else
    GITHUB_RELEASE_URL="https://github.com/crusoecloud/crusoe-watch-agent-v2/releases/download/vm/${AGENT_VERSION}"
fi
GITHUB_LATEST_RELEASE_URL="https://github.com/crusoecloud/crusoe-watch-agent-v2/releases/latest/download"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/crusoe/crusoe_watch_agent"
SECRETS_DIR="/etc/crusoe/secrets"
ENV_FILE="${CONFIG_DIR}/.env"
VECTOR_CONFIG="/etc/crusoe/shared/vector.yaml"
SYSTEMCTL_DIR="/etc/systemd/system"

DCGM_EXPORTER_PORT=9400
AMD_EXPORTER_PORT=5000
CME_PORT=9500

# dcgm-exporter Docker image by Ubuntu version.
declare -A DCGM_EXPORTER_VERSION_MAP=(
  ["20.04"]="@@DCGM_EXPORTER_UBUNTU2004_VERSION@@"
  ["22.04"]="@@DCGM_EXPORTER_UBUNTU2204_VERSION@@"
  ["24.04"]="@@DCGM_EXPORTER_UBUNTU2404_VERSION@@"
)
for k in "${!DCGM_EXPORTER_VERSION_MAP[@]}"; do
    case "${DCGM_EXPORTER_VERSION_MAP[$k]}" in @@*@@) DCGM_EXPORTER_VERSION_MAP[$k]="dev" ;; esac
done

###############################################################################
# Configurable via flags
###############################################################################
INSTALL_MODE="docker"   # "docker" or "native"
MONITORING_TOKEN=""
INGRESS_URL=""
DCGM_EXPORTER_SKIP="false"   # set in dcgm_setup; leaves an existing Docker exporter untouched
DCGM_REINSTALLED="false"     # set in dcgm_apt_install; the hostengine restarted, so exporters must too

###############################################################################
# Helpers
###############################################################################
status()     { echo "==> $1"; }
error_exit() { echo "ERROR: $1" >&2; exit 1; }

command_exists() { command -v "$1" &>/dev/null; }
service_exists() { systemctl list-unit-files --no-legend "$1" 2>/dev/null | grep -q .; }

# Returns 0 (true) if $1 < $2 using version sort.
version_lt() {
    local a="$1" b="$2"
    [ "$a" != "$b" ] && [ "$(printf '%s\n%s\n' "$a" "$b" | sort -V | tail -n1)" = "$b" ]
}

get_ubuntu_version() {
    lsb_release -r -s
}

read_vm_id() {
    dmidecode -s system-uuid
}

detect_gpu() {
    if command_exists nvidia-smi && nvidia-smi &>/dev/null; then
        echo "nvidia"
    elif [[ -d /sys/module/amdgpu ]]; then
        echo "amd"
    else
        echo "none"
    fi
}

# Place an asset from the release bundle, which fetch_bundle has already
# verified and pointed SCRIPT_DIR at. A miss is a packaging bug.
copy_asset() {
    local asset_path="$1"
    local dest="$2"

    # Callers name assets by their repo path; the bundle mirrors vm/.
    local src="${SCRIPT_DIR}/${asset_path#vm/}"
    [[ -f "$src" ]] || error_exit "${asset_path} is missing from the release bundle."

    cp "$src" "$dest"
}

# Public half of the release signing key, whose private half lives in CI.
# Spliced in from ci/cosign.pub at render time so there is only one copy to rotate.
cosign_public_key() {
    cat <<'PUBKEY'
@@COSIGN_PUBLIC_KEY@@
PUBKEY
}

# Download the release bundle for this install mode, verify it, and extract it.
# SCRIPT_DIR is repointed at the result so copy_asset and install_release_binary
# resolve every asset from a bundle whose signature has already been checked.
fetch_bundle() {
    # A repo checkout already has everything laid out next to the script.
    if [[ -d "${SCRIPT_DIR}/systemctl" ]]; then
        status "Running from a checkout; using local assets."
        return
    fi

    # The agent bundle (cwa-updater ships separately).
    local bundle
    if [[ "$INSTALL_MODE" == "docker" ]]; then
        bundle="cwa-docker.tar.gz"
    else
        bundle="cwa-native-$(dpkg --print-architecture).tar.gz"
    fi

    fetch_verified "$GITHUB_RELEASE_URL" "$bundle"

    status "Extracting ${bundle}..."
    tar -xzf "${DOWNLOAD_DIR}/${bundle}" -C "$DOWNLOAD_DIR" \
        || error_exit "Failed to extract ${bundle}."

    SCRIPT_DIR="$DOWNLOAD_DIR"
}

# Download $2 and the signed manifest from release URL $1 into a fresh temp
# directory, verify $2 against the manifest, and leave the path in DOWNLOAD_DIR.
# Every artifact this installer fetches from a Crusoe release comes through here.
fetch_verified() {
    local base_url="$1" name="$2"

    DOWNLOAD_DIR=$(mktemp -d)
    trap 'rm -rf "${DOWNLOAD_DIR}"' EXIT

    status "Downloading ${name}..."
    local f
    for f in "$name" SHA256SUMS SHA256SUMS.sig; do
        wget -q -O "${DOWNLOAD_DIR}/${f}" "${base_url}/${f}" \
            || error_exit "Failed to download ${base_url}/${f}"
    done

    verify_download "$DOWNLOAD_DIR" "$name"
}

# Check the release signature over SHA256SUMS in directory $1, then $2's digest
# against it. Both must pass before $2 is extracted or executed. $1 must already
# hold SHA256SUMS and SHA256SUMS.sig from the same release as $2.
verify_download() {
    local dir="$1" name="$2"

    ensure_openssl

    cosign_public_key > "${dir}/cosign.pub"
    grep -q "BEGIN PUBLIC KEY" "${dir}/cosign.pub" \
        || error_exit "This installer carries no signing key; it was not produced by the release pipeline."
    base64 -d < "${dir}/SHA256SUMS.sig" > "${dir}/SHA256SUMS.der" \
        || error_exit "Release signature is malformed."

    status "Verifying the signature on ${name}..."
    openssl dgst -sha256 \
        -verify "${dir}/cosign.pub" \
        -signature "${dir}/SHA256SUMS.der" \
        "${dir}/SHA256SUMS" > /dev/null \
        || error_exit "Release signature does not verify. Refusing to continue."

    # Exact filename match, not a regex.
    local digest_line
    digest_line=$(awk -v n="$name" '$2 == n { print; found = 1 } END { exit !found }' \
        "${dir}/SHA256SUMS") \
        || error_exit "${name} is not listed in SHA256SUMS. Refusing to continue."

    ( cd "$dir" && printf '%s\n' "$digest_line" | sha256sum -c --status - ) \
        || error_exit "${name} does not match its signed checksum. Refusing to continue."
}

###############################################################################
# Validation
###############################################################################
require_root() {
    [[ $EUID -eq 0 ]] || error_exit "Must run as root."
}

validate_flags() {
    if [[ "$GPU_TYPE" == "amd" && "$INSTALL_MODE" == "native" ]]; then
        error_exit "AMD GPU is not supported in native mode. Remove --no-docker."
    fi
}

validate_os_support() {
    case "$GPU_TYPE" in
        nvidia)
            if [[ -z "${DCGM_EXPORTER_VERSION_MAP[$UBUNTU_VERSION]:-}" ]]; then
                error_exit "Ubuntu ${UBUNTU_VERSION} is not supported for NVIDIA GPUs. Supported: ${!DCGM_EXPORTER_VERSION_MAP[*]}."
            fi
            ;;
        amd)
            if version_lt "$UBUNTU_VERSION" "22.04"; then
                error_exit "Ubuntu ${UBUNTU_VERSION} is not supported for AMD GPUs. Requires 22.04 or newer."
            fi
            ;;
    esac
}

validate_nvidia_deps() {
    if ! command_exists nvidia-smi; then
        error_exit "nvidia-smi not found. NVIDIA drivers must be installed."
    fi
    # nvidia-ctk is required for Docker GPU containers (DCGM exporter).
    if [[ "$INSTALL_MODE" == "docker" ]] && ! command_exists nvidia-ctk; then
        error_exit "nvidia-ctk not found. Install the NVIDIA Container Toolkit."
    fi
}

validate_amd_deps() {
    local rocm_ver=""
    if command_exists apt; then
        rocm_ver=$(apt show rocm-libs -a 2>/dev/null | sed -En 's/^Installed: ([0-9]+\.[0-9]+\.[0-9]+).*/\1/p' | head -n1)
        if [[ -z "$rocm_ver" ]]; then
            rocm_ver=$(apt show rocm-libs -a 2>/dev/null | sed -En 's/^Version: ([0-9]+\.[0-9]+\.[0-9]+).*/\1/p' | head -n1)
        fi
    fi
    if [[ -z "$rocm_ver" ]] && [[ -f /opt/rocm/.info/version ]]; then
        rocm_ver=$(sed -En 's/^ROCM_VERSION=([0-9]+\.[0-9]+\.[0-9]+).*/\1/p' /opt/rocm/.info/version | head -n1)
    fi
    if [[ -z "$rocm_ver" ]]; then
        error_exit "ROCm not detected. AMD GPU support requires ROCm 6.2.0+."
    fi
    if version_lt "$rocm_ver" "6.2.0"; then
        error_exit "ROCm $rocm_ver is not supported. Requires 6.2.0 or newer."
    fi
    status "ROCm $rocm_ver detected."
}

###############################################################################
# Dependency management
###############################################################################
ensure_wget() {
    if ! command_exists wget; then
        status "Installing wget..."
        apt-get update -qq && apt-get install -y -qq wget
    fi
}

ensure_openssl() {
    if ! command_exists openssl; then
        status "Installing openssl..."
        { apt-get update -qq && apt-get install -y -qq openssl; } \
            || error_exit "Failed to install openssl, which is required to verify the release signature."
    fi
}

ensure_docker() {
    if command_exists docker; then
        status "Docker already installed: $(docker --version)"
        return
    fi
    status "Installing Docker..."
    wget -qO- https://get.docker.com | sh
    systemctl enable --now docker
}

###############################################################################
# Component installers
###############################################################################
# Installs a native-mode Go binary to INSTALL_DIR. $1 = binary name. The native
# bundle carries it at its root; a repo checkout falls back to a local build.
install_release_binary() {
    local name="$1" src=""

    for candidate in \
        "${SCRIPT_DIR}/${name}" \
        "${SCRIPT_DIR}/../dist/${name}"; do
        if [[ -f "$candidate" ]]; then
            src="$candidate"
            break
        fi
    done

    [[ -n "$src" ]] || error_exit "${name} is missing from the release bundle."

    status "Installing ${name} to ${INSTALL_DIR}/${name}"
    install -m 0755 "$src" "${INSTALL_DIR}/${name}"
}

install_vector_native() {
    local installed_ver=""
    if command_exists vector; then
        installed_ver=$(vector --version 2>/dev/null | awk '/^vector / {print $2}')
        if [[ "${CWA_UPGRADE:-}" != "1" ]] \
            || [[ "$installed_ver" == "$VECTOR_VERSION" ]] \
            || { [[ -n "$installed_ver" ]] && version_lt "$VECTOR_VERSION" "$installed_ver"; }; then
            echo "Vector ${installed_ver:-unknown} already present; leaving as-is."
            systemctl disable --now vector.service 2>/dev/null || true
            return
        fi
        status "Upgrading Vector ${installed_ver:-unknown} -> ${VECTOR_VERSION} via APT."
    else
        status "Installing Vector ${VECTOR_VERSION} via APT."
    fi

    bash -c "$(curl -L https://setup.vector.dev)" || error_exit "Failed to add Vector APT repository."
    apt-get install -y "vector=${VECTOR_VERSION}-1" || error_exit "Failed to install Vector ${VECTOR_VERSION}-1."
    systemctl disable --now vector.service 2>/dev/null || true
}

# DCGM subsystem

setup_nvidia_cuda_repo() {
    status "Setting up NVIDIA CUDA apt repository."

    local ubuntu_short="${UBUNTU_VERSION//./}"

    local keyring_url="https://developer.download.nvidia.com/compute/cuda/repos/ubuntu${ubuntu_short}/x86_64/cuda-keyring_1.1-1_all.deb"
    local keyring_deb="/tmp/cuda-keyring.deb"

    wget -q -O "$keyring_deb" "$keyring_url" || error_exit "Failed to download cuda-keyring from $keyring_url"
    dpkg -i "$keyring_deb" || error_exit "Failed to install cuda-keyring."
    rm -f "$keyring_deb"

    apt-get update || error_exit "Failed to update package lists after adding NVIDIA repo."
    status "NVIDIA CUDA apt repository configured."
}

# Detect for DCGM's proprietary profiling module.
dcgm_profiling_available() {
    local libs pkg_status

    libs=$(ldconfig -p 2>/dev/null || true)
    [[ "$libs" == *libdcgmmoduleprofiling* ]] && return 0

    compgen -G "/usr/lib/*/libdcgmmoduleprofiling.so*" >/dev/null && return 0

    pkg_status=$(dpkg-query -W -f='${Status}' datacenter-gpu-manager-4-proprietary 2>/dev/null || true)
    [[ "$pkg_status" == *"install ok installed"* ]]
}

# Purge old DCGM, detect CUDA version, install DCGM 4.x, start service.
dcgm_apt_install() {
    systemctl --now disable nvidia-dcgm 2>/dev/null || true

    # Purge first: apt only pulls the profiling module recommend for a package it considers new.
    local -a installed=()
    while IFS= read -r pkg; do
        [[ -n "$pkg" ]] && installed+=("$pkg")
    done < <(dpkg-query -W -f='${Package} ${Status}\n' 'datacenter-gpu-manager*' 2>/dev/null \
        | awk '$NF != "not-installed" { print $1 }')

    if (( ${#installed[@]} > 0 )); then
        status "Purging existing DCGM packages: ${installed[*]}"
        apt-get purge --yes "${installed[@]}" || true
    fi

    local cuda_version
    cuda_version=$(nvidia-smi 2>/dev/null | sed -E -n 's/.*CUDA Version: ([0-9]+)\..*/\1/p')
    [[ -z "$cuda_version" ]] && error_exit "Could not determine CUDA version from nvidia-smi."
    status "CUDA version: ${cuda_version}"

    setup_nvidia_cuda_repo
    apt-get install --yes --install-recommends "datacenter-gpu-manager-4-cuda${cuda_version}" \
        || error_exit "Failed to install datacenter-gpu-manager-4-cuda${cuda_version}."
    systemctl --now enable nvidia-dcgm || error_exit "Failed to start nvidia-dcgm service."
    DCGM_REINSTALLED="true"
}

dcgm_install() {
    if command_exists dcgmi; then
        status "DCGM already installed."
        # Check if upgrade needed.
        local major
        major=$(dcgmi --version 2>/dev/null | grep -i 'version:' | awk '{print $3}' | cut -d. -f1)
        major="${major:-0}"
        if (( major >= 4 )); then
            if dcgm_profiling_available; then
                status "DCGM version ${major}.x — no upgrade needed."
                return
            fi
            status "DCGM ${major}.x has no profiling module — reinstalling for DCGM_FI_PROF_* metrics."
        else
            status "DCGM version ${major}.x < 4.x — upgrading..."
        fi
    else
        status "Installing DCGM (Data Center GPU Manager)..."
    fi
    dcgm_apt_install

    if dcgm_profiling_available; then
        status "DCGM ready."
    else
        echo "WARNING: DCGM profiling module still not found; DCGM_FI_PROF_* metrics will be empty." >&2
    fi
}

dcgm_setup() {
    status "Setting up NVIDIA DCGM..."

    # In Docker mode, leave an exporter deployed by a previous install running untouched on a plain re-install.
    if [[ "$INSTALL_MODE" == "docker" && "${CWA_UPGRADE:-}" != "1" ]] \
        && service_exists "crusoe-dcgm-exporter.service"; then
        DCGM_EXPORTER_SKIP="true"
        status "crusoe-dcgm-exporter.service already present; leaving it running (run 'upgrade' to redeploy)."
    fi

    dcgm_install

    if [[ "$DCGM_EXPORTER_SKIP" == "true" && "$DCGM_REINSTALLED" == "true" ]]; then
        DCGM_EXPORTER_SKIP="false"
        status "DCGM was reinstalled; redeploying crusoe-dcgm-exporter.service to reconnect."
    fi

    copy_asset "vm/config/dcp-metrics-included.csv" "${CONFIG_DIR}/dcp-metrics-included.csv"

    if [[ "$INSTALL_MODE" == "docker" ]]; then
        [[ "$DCGM_EXPORTER_SKIP" == "true" ]] \
            || copy_asset "vm/docker/docker-compose-dcgm-exporter.yaml" "${CONFIG_DIR}/docker-compose-dcgm-exporter.yaml"
    else
        install_dcgm_exporter_native
    fi
}

install_dcgm_exporter_native() {
    if command_exists dcgm-exporter; then
        echo "dcgm-exporter is already installed."
        return
    fi

    status "Building dcgm-exporter from source."
    local BUILD_DIR
    BUILD_DIR=$(mktemp -d)

    # Ensure Git is installed
    if ! command_exists git; then
        { apt-get update && apt-get install -y git; } || error_exit "Failed to install git."
    fi

    # Ensure build tools are installed (make + gcc for CGo)
    if ! command_exists make || ! command_exists gcc; then
        { apt-get update && apt-get install -y build-essential; } || error_exit "Failed to install build-essential."
    fi

    # Ensure Go >= 1.26 is installed (Ubuntu apt packages are too old)
    local NEED_GO=false
    if ! command_exists go; then
        NEED_GO=true
    else
        local GO_VER
        GO_VER=$(go version | sed -E 's/.*go([0-9]+\.[0-9]+).*/\1/')
        if awk "BEGIN{exit !($GO_VER < 1.26)}"; then
            echo "Installed Go version ($GO_VER) is too old. Upgrading."
            NEED_GO=true
        fi
    fi
    if $NEED_GO; then
        status "Installing Go 1.26 from official tarball."
        local go_arch
        go_arch=$(dpkg --print-architecture)
        local GO_TAR="go1.26.0.linux-${go_arch}.tar.gz"
        wget -q -O "/tmp/$GO_TAR" "https://go.dev/dl/$GO_TAR" || error_exit "Failed to download Go."
        rm -rf /usr/local/go
        tar -C /usr/local -xzf "/tmp/$GO_TAR" || error_exit "Failed to extract Go."
        rm -f "/tmp/$GO_TAR"
        export PATH="/usr/local/go/bin:$PATH"
    fi

    git clone https://github.com/NVIDIA/dcgm-exporter.git "$BUILD_DIR" || error_exit "Failed to clone dcgm-exporter."
    make -C "$BUILD_DIR" binary || error_exit "Failed to build dcgm-exporter."
    make -C "$BUILD_DIR" install || error_exit "Failed to install dcgm-exporter."
    rm -rf "$BUILD_DIR"
    status "dcgm-exporter installed successfully."
}

###############################################################################
# AMD exporter setup
###############################################################################
amd_setup() {
    status "Setting up AMD GPU exporter..."
    mkdir -p "${CONFIG_DIR}/config"
    copy_asset "vm/config/amd_metrics_config.json" "${CONFIG_DIR}/config/config.json"
    copy_asset "vm/docker/docker-compose-amd-exporter.yaml" "${CONFIG_DIR}/docker-compose-amd-exporter.yaml"
}

###############################################################################
# CME (Crusoe Metrics Exporter) setup
###############################################################################
cme_setup() {
    status "Setting up Crusoe Metrics Exporter..."
    if [[ "$INSTALL_MODE" == "docker" ]]; then
        copy_asset "vm/docker/docker-compose-crusoe-metrics-exporter.yaml" \
            "${CONFIG_DIR}/docker-compose-crusoe-metrics-exporter.yaml"
    else
        install_metrics_exporter_native
    fi
}

install_metrics_exporter_native() {
    local arch
    arch=$(dpkg --print-architecture)
    local tarball="crusoe-metrics-exporter-${CME_VERSION}-linux-${arch}.tar.gz"
    local url="https://github.com/crusoecloud/crusoe-metrics-exporter/releases/download/v${CME_VERSION}/${tarball}"
    local sha_url="https://github.com/crusoecloud/crusoe-metrics-exporter/releases/download/v${CME_VERSION}/SHA256SUMS"
    local tmpdir
    tmpdir=$(mktemp -d)

    status "Downloading crusoe-metrics-exporter v${CME_VERSION} (${arch})."
    wget -q -O "${tmpdir}/${tarball}" "$url" || error_exit "Failed to download ${url}"
    wget -q -O "${tmpdir}/SHA256SUMS" "$sha_url" || error_exit "Failed to download ${sha_url}"

    status "Verifying checksum."
    grep "${tarball}" "${tmpdir}/SHA256SUMS" > "${tmpdir}/${tarball}.sha256"
    (cd "$tmpdir" && sha256sum -c "${tarball}.sha256") || error_exit "Checksum verification failed for ${tarball}"

    status "Installing crusoe-metrics-exporter binary and systemd unit."
    tar -xzf "${tmpdir}/${tarball}" -C "$tmpdir"
    local stage="${tmpdir}/crusoe-metrics-exporter-${CME_VERSION}-linux-${arch}"
    install -m 0755 "${stage}/crusoe-metrics-exporter" "${INSTALL_DIR}/crusoe-metrics-exporter"
    install -m 0644 "${stage}/crusoe-metrics-exporter.service" "$SYSTEMCTL_DIR/crusoe-metrics-exporter.service"

    rm -rf "$tmpdir"
}

###############################################################################
# Configuration
###############################################################################
write_token() {
    if [[ -z "$MONITORING_TOKEN" ]]; then
        error_exit "Monitoring token is required."
    fi
    if [[ ${#MONITORING_TOKEN} -ne 82 ]]; then
        error_exit "Token length is ${#MONITORING_TOKEN} (expected 82). Generate one with: crusoe monitoring tokens create"
    fi
    mkdir -p "$SECRETS_DIR"
    # Single-quote the token value to prevent $-interpolation by systemd/docker-compose.
    echo "CRUSOE_MONITORING_TOKEN='${MONITORING_TOKEN}'" > "${SECRETS_DIR}/.monitoring-token"
    chmod 600 "${SECRETS_DIR}/.monitoring-token"
}

handle_token() {
    # Priority: --token flag > CRUSOE_MONITORING_TOKEN env > existing file > interactive prompt.
    if [[ -n "$MONITORING_TOKEN" ]]; then
        status "Using token from --token flag."
    elif [[ -n "${CRUSOE_MONITORING_TOKEN:-}" ]]; then
        MONITORING_TOKEN="$CRUSOE_MONITORING_TOKEN"
        status "Using token from CRUSOE_MONITORING_TOKEN environment variable."
    elif [[ -s "${SECRETS_DIR}/.monitoring-token" ]]; then
        MONITORING_TOKEN=$(sed "s/^CRUSOE_MONITORING_TOKEN=//; s/^'//; s/'$//" "${SECRETS_DIR}/.monitoring-token")
        status "Using existing token from ${SECRETS_DIR}/.monitoring-token"
    else
        echo "Enter monitoring token:"
        read -rs MONITORING_TOKEN
        echo ""
    fi
    write_token
}

write_env_file() {
    local vm_id="$1"
    local cms_url="${INGRESS_URL:-$CMS_BASE_URL}"

    mkdir -p "$CONFIG_DIR"
    status "Writing env file to ${ENV_FILE}"

    # Telemetry install-type tag. Mirrors the proto enum sent to the coordinator:
    # native systemd → "systemd", Docker → "docker". K8s ("kubernetes") is set in the daemonset.
    local install_type="docker"
    [[ "$INSTALL_MODE" == "native" ]] && install_type="systemd"

    cat > "$ENV_FILE" <<EOF
VM_ID='${vm_id}'
TELEMETRY_INGRESS_ENDPOINT='${cms_url}/ingest'
LOGS_INGRESS_ENDPOINT='${cms_url}/logs/ingest'
AGENT_VERSION='${AGENT_VERSION}'
CWA_MANAGER_VERSION='${CWA_MANAGER_VERSION}'
REPORT_RUNNER_VERSION='${REPORT_RUNNER_VERSION}'
VECTOR_VERSION='${VECTOR_VERSION}'
INSTALL_TYPE='${install_type}'
EOF

    # NODE_NAME mirrors the K8s downward-API var so cwa-manager can parse the
    # region (first dot-segment of the domain) the same way in both modes.
    local detected_fqdn
    detected_fqdn=$(hostname -f 2>/dev/null || true)
    if [[ -n "$detected_fqdn" ]]; then
        echo "NODE_NAME='${detected_fqdn}'" >> "$ENV_FILE"
    fi

    # GPU-specific vars.
    if [[ "$GPU_TYPE" == "nvidia" ]]; then
        echo "DCGM_EXPORTER_PORT='${DCGM_EXPORTER_PORT}'" >> "$ENV_FILE"
        if [[ "$INSTALL_MODE" == "docker" ]]; then
            local image_ver="${DCGM_EXPORTER_VERSION_MAP[$UBUNTU_VERSION]:-}"
            [[ -n "$image_ver" ]] || error_exit "no dcgm-exporter image pinned for Ubuntu ${UBUNTU_VERSION}"
            echo "DCGM_EXPORTER_VERSION='${image_ver}'" >> "$ENV_FILE"
        fi
    elif [[ "$GPU_TYPE" == "amd" ]]; then
        {
            echo "AMD_EXPORTER_PORT='${AMD_EXPORTER_PORT}'"
            echo "AMD_EXPORTER_VERSION='${AMD_EXPORTER_VERSION}'"
        } >> "$ENV_FILE"
    fi

    # CME vars.
    echo "CRUSOE_METRICS_EXPORTER_PORT='${CME_PORT}'" >> "$ENV_FILE"
    # Derive OBJSTORE_ENDPOINT_FQDN from the VM's hostname domain.
    # Crusoe VMs have a domain like "us-east1-a.compute.internal"; the first
    # dot-separated segment is the region.
    local detected_domain
    detected_domain=$(hostname -d 2>/dev/null || true)
    if [[ -n "$detected_domain" ]]; then
        local region="${detected_domain%%.*}"
        echo "OBJSTORE_ENDPOINT_FQDN='object.${region}.crusoecloudcompute.com'" >> "$ENV_FILE"
        status "Derived OBJSTORE_ENDPOINT_FQDN from hostname (region: ${region})"
    fi
    if [[ "$INSTALL_MODE" == "docker" ]]; then
        echo "CME_VERSION='${CME_VERSION}'" >> "$ENV_FILE"
    fi

    chmod 640 "$ENV_FILE"
}

install_vector_compose() {
    copy_asset "vm/docker/docker-compose-vector.yaml" "${CONFIG_DIR}/docker-compose-vector.yaml"
}

###############################################################################
# Systemd units
###############################################################################

# Install a systemd unit file from the repo.
#   $1 = unit filename (e.g., cwa-manager.service)
#   $2 = ExecStart command (optional, replaces @@EXEC_START@@)
#   $3 = ExecStop command  (optional, replaces @@EXEC_STOP@@; line removed if empty)
install_unit() {
    local name="$1" exec_start="${2:-}" exec_stop="${3:-}"
    local dest="${SYSTEMCTL_DIR}/${name}"
    local d=$'\x01'

    copy_asset "vm/systemctl/${name}" "$dest"

    if [[ -n "$exec_start" ]]; then
        sed -i "s${d}@@EXEC_START@@${d}${exec_start}${d}" "$dest"
    fi
    if [[ -n "$exec_stop" ]]; then
        sed -i "s${d}@@EXEC_STOP@@${d}${exec_stop}${d}" "$dest"
    else
        sed -i '/@@EXEC_STOP@@/d' "$dest"
    fi
}

# Install a systemd unit whose ExecStart/ExecStop drive a Compose file.
#   $1 = unit filename; $2 = compose filename under CONFIG_DIR
install_compose_unit() {
    install_unit "$1" \
        "/usr/bin/docker compose -f ${CONFIG_DIR}/$2 up" \
        "/usr/bin/docker compose -f ${CONFIG_DIR}/$2 down"
}

install_systemd_units() {
    status "Installing systemd units..."

    # cwa-manager
    if [[ "$INSTALL_MODE" == "docker" ]]; then
        copy_asset "vm/docker/docker-compose-cwa-manager.yaml" \
            "${CONFIG_DIR}/docker-compose-cwa-manager.yaml"
        install_compose_unit "cwa-manager.service" "docker-compose-cwa-manager.yaml"
    else
        install_unit "cwa-manager.service" "${INSTALL_DIR}/cwa-manager"
    fi

    # Vector
    if [[ "$INSTALL_MODE" == "docker" ]]; then
        install_compose_unit "cwa-vector.service" "docker-compose-vector.yaml"
    else
        install_unit "cwa-vector.service" "/usr/bin/vector --config ${VECTOR_CONFIG} --watch-config"
    fi

    # DCGM exporter
    if [[ "$GPU_TYPE" == "nvidia" ]]; then
        if [[ "$INSTALL_MODE" != "docker" ]]; then
            install_unit "crusoe-dcgm-exporter.service" \
                "/usr/bin/dcgm-exporter -f ${CONFIG_DIR}/dcp-metrics-included.csv -r localhost:5555 -a :${DCGM_EXPORTER_PORT}"
        elif [[ "$DCGM_EXPORTER_SKIP" == "true" ]]; then
            status "Keeping existing crusoe-dcgm-exporter.service unit."
        else
            install_compose_unit "crusoe-dcgm-exporter.service" "docker-compose-dcgm-exporter.yaml"
        fi
    fi

    # Report runner: the bug-report collector. Native mode runs the binary.
    # Docker mode runs a sidecar container with the vendor bug-report tool.
    if [[ "$GPU_TYPE" != "none" ]]; then
        if [[ "$INSTALL_MODE" == "native" ]]; then
            install_unit "cwa-report-runner.service" "${INSTALL_DIR}/report-runner"
        else
            local runner_compose="docker-compose-cwa-report-runner.yaml"
            [[ "$GPU_TYPE" == "amd" ]] && runner_compose="docker-compose-cwa-report-runner-amd.yaml"
            copy_asset "vm/docker/${runner_compose}" "${CONFIG_DIR}/${runner_compose}"
            install_compose_unit "cwa-report-runner.service" "$runner_compose"
        fi
    fi

    # AMD exporter (Docker only, no placeholders)
    if [[ "$GPU_TYPE" == "amd" ]]; then
        install_unit "crusoe-amd-exporter.service"
    fi

    # CME Docker (native installs its own unit from tarball)
    if [[ "$INSTALL_MODE" == "docker" ]]; then
        install_unit "crusoe-metrics-exporter.service"
    fi
}

###############################################################################
# Container images & readiness
###############################################################################
# Pull images ($1 = label, $@ = compose files) so the units start instantly and
# the download stays outside wait_for_containers' deadline. Local images no-op.
pull_images() {
    local label="$1"; shift
    status "Pre-pulling ${label} images..."
    for cf in "$@"; do
        [[ -f "$cf" ]] || continue
        docker compose -f "$cf" pull || echo "WARNING: failed to pull $(basename "$cf")" >&2
    done
}

# Wait for the containers themselves to be active, under one shared deadline.
wait_for_containers() {
    status "Waiting for containers: $*"
    local deadline=$((SECONDS + 120)) c
    for c in "$@"; do
        until [[ "$(docker inspect -f '{{.State.Running}}' "$c" 2>/dev/null)" == "true" ]]; do
            (( SECONDS < deadline )) \
                || error_exit "container ${c} not running after 120s. Check: sudo docker ps -a; journalctl -u ${c}"
            sleep 2
        done
    done
}

###############################################################################
# Persistence (for upgrade)
###############################################################################
save_install_mode() {
    mkdir -p "$CONFIG_DIR"
    echo "$INSTALL_MODE" > "${CONFIG_DIR}/.install-mode"
}

save_install_args() {
    mkdir -p "$SECRETS_DIR"
    printf '%s\n' "$@" > "${SECRETS_DIR}/.install-args"
}

save_version() {
    echo "$AGENT_VERSION" > "${CONFIG_DIR}/VERSION"
}

# Save args for upgrade replay (exclude the command and --token value).
save_replay_args() {
    local -a replay=()
    local skip_next=false
    for arg in "${ORIGINAL_ARGS[@]:1}"; do
        if [[ "$skip_next" == "true" ]]; then
            skip_next=false
            continue
        fi
        if [[ "$arg" == "--token" ]]; then
            skip_next=true
            continue
        fi
        replay+=("$arg")
    done
    save_install_args "${replay[@]}"
}

###############################################################################
# v1 → v2 migration
###############################################################################
# Returns 0 if any v1-only artifact is on disk.
v1_installed() {
    [[ -f "${SYSTEMCTL_DIR}/crusoe-watch-agent.service" \
       || -f "${CONFIG_DIR}/vector.yaml" \
       || -f "${SYSTEMCTL_DIR}/crusoe-log-collector.service" \
       || -f "${SYSTEMCTL_DIR}/crusoe-amd-log-collector.service" \
       || -f "${SYSTEMCTL_DIR}/crusoe-nvidia-log-collector.service" ]]
}

# Tear down v1 artifacts that v2 won't replace in place.
migrate_from_v1() {
    v1_installed || return 0
    status "v1 install detected — cleaning up."

    for svc in crusoe-watch-agent crusoe-log-collector crusoe-amd-log-collector crusoe-nvidia-log-collector; do
        systemctl stop "${svc}.service" 2>/dev/null || true
        systemctl disable "${svc}.service" 2>/dev/null || true
        rm -f "${SYSTEMCTL_DIR}/${svc}.service"
    done

    if command_exists docker; then
        for cf in docker-compose-log-collector.yaml docker-compose-amd-log-collector.yaml; do
            [[ -f "${CONFIG_DIR}/${cf}" ]] || continue
            docker compose -f "${CONFIG_DIR}/${cf}" down --remove-orphans 2>/dev/null || true
            rm -f "${CONFIG_DIR}/${cf}"
        done
    fi

    rm -f "${CONFIG_DIR}/vector.yaml"
    rm -rf /opt/crusoe-log-collector /opt/crusoe-amd-log-collector

    # v1 token files may use $$-escaped values that v2 can't decode.
    rm -f "${SECRETS_DIR}/.monitoring-token"

    systemctl daemon-reload
}

###############################################################################
# Commands
###############################################################################
do_install() {
    require_root
    migrate_from_v1
    UBUNTU_VERSION=$(get_ubuntu_version)

    # Auto-detect GPU.
    GPU_TYPE=$(detect_gpu)
    status "Detected GPU type: ${GPU_TYPE}"

    validate_flags
    validate_os_support

    # Validate GPU-specific deps.
    case "$GPU_TYPE" in
        nvidia) validate_nvidia_deps ;;
        amd)    validate_amd_deps ;;
    esac

    # Install base dependencies.
    ensure_wget
    if [[ "$INSTALL_MODE" == "docker" ]]; then
        ensure_docker
    fi

    # Everything installed below this line comes out of the verified bundle.
    fetch_bundle

    local vm_id
    vm_id=$(read_vm_id)
    status "VM ID: ${vm_id}"

    handle_token
    mkdir -p "$CONFIG_DIR"

    # Docker mode runs cwa-manager as a container (see install_systemd_units).
    if [[ "$INSTALL_MODE" == "native" ]]; then
        install_release_binary cwa-manager
        # report-runner collects GPU bug reports; native mode is NVIDIA-only.
        [[ "$GPU_TYPE" == "nvidia" ]] && install_release_binary report-runner
    fi

    # Install Vector.
    if [[ "$INSTALL_MODE" == "docker" ]]; then
        install_vector_compose
    else
        install_vector_native
    fi

    # GPU-specific setup.
    case "$GPU_TYPE" in
        nvidia) dcgm_setup ;;
        amd)    amd_setup ;;
    esac

    cme_setup

    write_env_file "$vm_id"
    mkdir -p /var/lib/vector
    install_systemd_units

    save_install_mode
    save_version

    # Start services. cwa-manager owns Vector config generation, so it starts
    # first and must produce the config before Vector starts against it.
    status "Starting services..."
    systemctl daemon-reload

    if [[ "$INSTALL_MODE" == "docker" ]]; then
        pull_images "cwa-manager" "${CONFIG_DIR}/docker-compose-cwa-manager.yaml"
    fi

    systemctl enable cwa-manager.service
    systemctl restart cwa-manager.service

    # Gate on the container first, so the config wait below times the manager, not Docker.
    if [[ "$INSTALL_MODE" == "docker" ]]; then
        wait_for_containers cwa-manager
    fi

    status "Waiting for cwa-manager to write ${VECTOR_CONFIG}..."
    for _ in $(seq 1 60); do
        [[ -f "$VECTOR_CONFIG" ]] && break
        sleep 1
    done
    [[ -f "$VECTOR_CONFIG" ]] || error_exit "cwa-manager did not write ${VECTOR_CONFIG}. Check: journalctl -u cwa-manager"

    # Exporters before Vector, so its first scrapes find them listening.
    local services=()
    [[ "$GPU_TYPE" == "nvidia" && "$DCGM_EXPORTER_SKIP" != "true" ]] && services+=("crusoe-dcgm-exporter.service")
    [[ "$GPU_TYPE" != "none" ]]   && services+=("cwa-report-runner.service")
    [[ "$GPU_TYPE" == "amd" ]]    && services+=("crusoe-amd-exporter.service")
    services+=("crusoe-metrics-exporter.service" "cwa-vector.service")

    if [[ "$INSTALL_MODE" == "docker" ]]; then
        pull_images "exporter and Vector" "${CONFIG_DIR}"/docker-compose-*.yaml
    fi

    for svc in "${services[@]}"; do
        systemctl enable "$svc"
        systemctl restart "$svc"
    done

    # Every compose container_name matches its unit name, so strip the suffix.
    if [[ "$INSTALL_MODE" == "docker" ]]; then
        wait_for_containers "${services[@]%.service}"
    fi

    echo ""
    status "Install complete. Check status:"
    echo "  systemctl status cwa-manager"
    echo "  systemctl status cwa-vector"
    [[ "$GPU_TYPE" == "nvidia" ]] && echo "  systemctl status crusoe-dcgm-exporter"
    [[ "$GPU_TYPE" == "amd" ]]    && echo "  systemctl status crusoe-amd-exporter"
    echo "  systemctl status crusoe-metrics-exporter"
}

do_uninstall() {
    require_root

    # Sweep v1 leftovers first (no-op on a clean v2 install).
    migrate_from_v1

    status "Stopping and disabling services..."
    local all_services=(
        cwa-manager.service
        cwa-report-runner.service
        cwa-vector.service
        crusoe-dcgm-exporter.service
        crusoe-amd-exporter.service
        crusoe-metrics-exporter.service
    )
    for svc in "${all_services[@]}"; do
        systemctl stop "$svc" 2>/dev/null || true
        systemctl disable "$svc" 2>/dev/null || true
    done

    # Tear down Docker Compose services and clean up containers.
    if command_exists docker; then
        for cf in "${CONFIG_DIR}"/docker-compose-*.yaml; do
            [[ -f "$cf" ]] || continue
            status "Removing Docker containers: $(basename "$cf")"
            docker compose -f "$cf" down --remove-orphans 2>/dev/null || true
        done
    fi

    status "Removing files..."
    for svc in "${all_services[@]}"; do
        rm -f "${SYSTEMCTL_DIR}/${svc}"
    done
    rm -f "${INSTALL_DIR}/cwa-manager"
    rm -f "${INSTALL_DIR}/report-runner"
    rm -f "${VECTOR_CONFIG}"
    rm -rf "${CONFIG_DIR}"
    rm -rf /var/lib/vector

    # Remove native packages if installed.
    if dpkg -l vector &>/dev/null; then
        status "Removing Vector APT package..."
        apt-get remove -y vector 2>/dev/null || true
    fi
    if command -v dcgm-exporter &>/dev/null; then
        rm -f /usr/bin/dcgm-exporter || true
    fi
    if dpkg -l 'datacenter-gpu-manager-4-cuda*' 2>/dev/null | grep -q '^ii'; then
        status "Removing DCGM (datacenter-gpu-manager-4)."
        systemctl stop nvidia-dcgm || true
        systemctl disable nvidia-dcgm || true
        apt-get remove -y 'datacenter-gpu-manager-4-cuda*' || true
    fi
    rm -f "${INSTALL_DIR}/crusoe-metrics-exporter"

    systemctl daemon-reload

    status "Uninstall complete. Secrets at ${SECRETS_DIR} preserved."
}

do_upgrade() {
    require_root

    local installed_version=""
    if [[ -f "${CONFIG_DIR}/VERSION" ]]; then
        installed_version=$(cat "${CONFIG_DIR}/VERSION")
    fi

    if [[ -z "$installed_version" ]]; then
        error_exit "No installed version found. Run 'install' first."
    fi

    status "Installed version: ${installed_version}"

    # Fetch the latest published VM release version. The release pipeline
    # attaches a VERSION asset (containing the tag string) to every GitHub
    # Release, and /releases/latest/download/ redirects to whichever release
    # currently holds the "latest" pointer.
    local remote_version
    local version_url="${GITHUB_LATEST_RELEASE_URL}/VERSION"
    remote_version=$(wget -qO- "$version_url" 2>/dev/null | tr -d '[:space:]') || true

    if [[ -z "$remote_version" ]]; then
        error_exit "Could not fetch remote version from ${version_url}."
    fi

    status "Remote version: ${remote_version}"

    if ! version_lt "$installed_version" "$remote_version"; then
        status "Already up to date (${installed_version} >= ${remote_version}). Nothing to do."
        return
    fi

    status "Upgrading ${installed_version} → ${remote_version}..."

    # The new installer runs as root and is what verifies the bundle, so it is itself verified first.
    fetch_verified "$GITHUB_LATEST_RELEASE_URL" crusoe_watch_agent.sh
    chmod +x "${DOWNLOAD_DIR}/crusoe_watch_agent.sh"

    # Replay saved args.
    local -a saved_args=()
    if [[ -f "${SECRETS_DIR}/.install-args" ]]; then
        while IFS= read -r line; do
            [[ -n "$line" ]] && saved_args+=("$line")
        done < "${SECRETS_DIR}/.install-args"
    fi

    # The upgrade is performed by the new version's installer, not this one.
    CWA_UPGRADE=1 "${DOWNLOAD_DIR}/crusoe_watch_agent.sh" install "${saved_args[@]}"

    status "Upgrade complete."
}

do_refresh_token() {
    require_root

    if [[ -n "$MONITORING_TOKEN" ]]; then
        status "Using token from --token flag."
    else
        echo "Enter new monitoring token:"
        read -rs MONITORING_TOKEN
        echo ""
    fi

    write_token
    status "Token updated."
    echo "For the changes to take effect, restart the services:"
    echo "  sudo systemctl restart cwa-vector"
    echo "  sudo systemctl restart cwa-manager"
}

do_help() {
    cat <<'HELP'
Crusoe Watch Agent 2.0 — VM Installer

Usage:
  sudo ./crusoe_watch_agent.sh COMMAND [OPTIONS]

Commands:
  install         Install cwa-manager, Vector, and GPU exporters
  uninstall       Stop services and remove all files (preserves secrets)
  upgrade         Check for new version and upgrade in place
  refresh-token   Update monitoring token and restart services
  help            Show this help message

Install Options:
  --no-docker                Use native Vector binary (default: Docker)
  --token TOKEN              Monitoring token (prompted if omitted; use single quotes)
  --ingress-url URL          Override CMS base URL
  --dcgm-exporter-port PORT  DCGM exporter port (default: 9400)
  --amd-exporter-port PORT   AMD exporter port (default: 5000)

GPU type is auto-detected. NVIDIA and AMD GPUs are supported.
AMD GPUs require Docker mode (--no-docker is not supported with AMD).

Examples:
  sudo ./crusoe_watch_agent.sh install
  sudo ./crusoe_watch_agent.sh install --no-docker
  sudo ./crusoe_watch_agent.sh install --token "$(crusoe monitoring tokens create -f token)"
  sudo ./crusoe_watch_agent.sh upgrade
  sudo ./crusoe_watch_agent.sh refresh-token
  sudo ./crusoe_watch_agent.sh uninstall
HELP
}

###############################################################################
# Argument parsing
###############################################################################
ORIGINAL_ARGS=("$@")

COMMAND="${1:-help}"
shift || true

while [[ $# -gt 0 ]]; do
    case "$1" in
        --no-docker)
            INSTALL_MODE="native"
            shift
            ;;
        --token)
            MONITORING_TOKEN="${2:?Missing value for --token}"
            shift 2
            ;;
        --ingress-url)
            INGRESS_URL="${2:?Missing value for --ingress-url}"
            shift 2
            ;;
        --dcgm-exporter-port)
            DCGM_EXPORTER_PORT="${2:?Missing value for --dcgm-exporter-port}"
            shift 2
            ;;
        --amd-exporter-port)
            AMD_EXPORTER_PORT="${2:?Missing value for --amd-exporter-port}"
            shift 2
            ;;
        *)
            error_exit "Unknown option: $1. Run '$0 help' for usage."
            ;;
    esac
done

###############################################################################
# Dispatch
###############################################################################
case "$COMMAND" in
    install)
        do_install
        save_replay_args
        ;;
    uninstall)
        do_uninstall
        ;;
    upgrade)
        do_upgrade
        ;;
    refresh-token)
        do_refresh_token
        ;;
    help|--help|-h)
        do_help
        ;;
    *)
        echo "Unknown command: ${COMMAND}"
        echo ""
        do_help
        exit 1
        ;;
esac
