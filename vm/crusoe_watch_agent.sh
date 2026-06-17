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
VECTOR_VERSION="@@VECTOR_VERSION@@"
CME_VERSION="@@CRUSOE_METRICS_EXPORTER_VERSION@@"
AMD_EXPORTER_VERSION="@@AMD_EXPORTER_VERSION@@"
for v in AGENT_VERSION VECTOR_VERSION CME_VERSION AMD_EXPORTER_VERSION; do
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
VECTOR_CONFIG="/etc/vector/vector.yaml"
SYSTEMCTL_DIR="/etc/systemd/system"

DCGM_EXPORTER_PORT=9400
AMD_EXPORTER_PORT=5000
CME_PORT=9500

CME_BIN="/usr/local/bin/crusoe-metrics-exporter"

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
ENABLE_CME=false
MONITORING_TOKEN=""
INGRESS_URL=""

###############################################################################
# Helpers
###############################################################################
status()     { echo "==> $1"; }
error_exit() { echo "ERROR: $1" >&2; exit 1; }

command_exists() { command -v "$1" &>/dev/null; }

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

# Download a file from the repo, or copy from local checkout.
# When running from the repo directory (e.g., during development/testing),
# local files are used automatically without needing GitHub access.
download_file() {
    local remote_path="$1"
    local dest="$2"

    local local_path="${SCRIPT_DIR}/${remote_path#vm/}"
    if [[ -f "$local_path" ]]; then
        status "Copying local file: ${local_path}"
        cp "$local_path" "$dest"
    else
        local url="${GITHUB_RELEASE_URL}/${remote_path##*/}"
        wget -q -O "$dest" "$url" || error_exit "Failed to download ${url}"
    fi
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
            local major minor
            major=$(echo "$UBUNTU_VERSION" | cut -d. -f1)
            minor=$(echo "$UBUNTU_VERSION" | cut -d. -f2)
            if (( major < 22 || (major == 22 && minor < 4) )); then
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
    local major minor
    major=$(echo "$rocm_ver" | cut -d. -f1)
    minor=$(echo "$rocm_ver" | cut -d. -f2)
    if (( major < 6 || (major == 6 && minor < 2) )); then
        error_exit "ROCm $rocm_ver is too old. Requires 6.2.0+."
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
install_cwa_manager() {
    local src=""

    # Dev fallback: pick up a locally-built binary if present.
    for candidate in \
        "${SCRIPT_DIR}/cwa-manager" \
        "${SCRIPT_DIR}/../dist/cwa-manager"; do
        if [[ -f "$candidate" ]]; then
            src="$candidate"
            break
        fi
    done

    if [[ -z "$src" ]]; then
        local url="${GITHUB_RELEASE_URL}/cwa-manager-linux-amd64"
        status "Downloading cwa-manager from ${url}..."
        local tmp
        tmp=$(mktemp)
        if wget -q -O "$tmp" "$url"; then
            src="$tmp"
        else
            rm -f "$tmp"
            error_exit "Failed to download cwa-manager. Place binary next to the script or check network."
        fi
    fi

    status "Installing cwa-manager to ${INSTALL_DIR}/cwa-manager"
    install -m 0755 "$src" "${INSTALL_DIR}/cwa-manager"
    if [[ "$src" == /tmp/* ]]; then rm -f "$src"; fi
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

# Purge old DCGM, detect CUDA version, install DCGM 4.x, start service.
dcgm_apt_install() {
    # Purge any existing DCGM packages.
    systemctl --now disable nvidia-dcgm 2>/dev/null || true
    dpkg --list datacenter-gpu-manager &>/dev/null && apt-get purge --yes datacenter-gpu-manager 2>/dev/null || true
    dpkg --list datacenter-gpu-manager-config &>/dev/null && apt-get purge --yes datacenter-gpu-manager-config 2>/dev/null || true

    local cuda_version
    cuda_version=$(nvidia-smi 2>/dev/null | sed -E -n 's/.*CUDA Version: ([0-9]+)\..*/\1/p')
    [[ -z "$cuda_version" ]] && error_exit "Could not determine CUDA version from nvidia-smi."
    status "CUDA version: ${cuda_version}"

    setup_nvidia_cuda_repo
    apt-get install --yes --install-recommends "datacenter-gpu-manager-4-cuda${cuda_version}" \
        || error_exit "Failed to install datacenter-gpu-manager-4-cuda${cuda_version}."
    systemctl --now enable nvidia-dcgm || error_exit "Failed to start nvidia-dcgm service."
}

dcgm_install() {
    if command_exists dcgmi; then
        status "DCGM already installed."
        # Check if upgrade needed.
        local major
        major=$(dcgmi --version 2>/dev/null | grep -i 'version:' | awk '{print $3}' | cut -d. -f1)
        major="${major:-0}"
        if (( major >= 4 )); then
            status "DCGM version ${major}.x — no upgrade needed."
            return
        fi
        status "DCGM version ${major}.x < 4.x — upgrading..."
    else
        status "Installing DCGM (Data Center GPU Manager)..."
    fi
    dcgm_apt_install
    status "DCGM ready."
}

dcgm_setup() {
    status "Setting up NVIDIA DCGM..."
    dcgm_install
    download_file "vm/config/dcp-metrics-included.csv" "${CONFIG_DIR}/dcp-metrics-included.csv"

    if [[ "$INSTALL_MODE" == "docker" ]]; then
        download_file "vm/docker/docker-compose-dcgm-exporter.yaml" "${CONFIG_DIR}/docker-compose-dcgm-exporter.yaml"
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
        local GO_TAR="go1.26.0.linux-amd64.tar.gz"
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
    download_file "vm/config/amd_metrics_config.json" "${CONFIG_DIR}/config/config.json"
    download_file "vm/docker/docker-compose-amd-exporter.yaml" "${CONFIG_DIR}/docker-compose-amd-exporter.yaml"
}

###############################################################################
# CME (Crusoe Metrics Exporter) setup
###############################################################################
cme_setup() {
    status "Setting up Crusoe Metrics Exporter..."
    if [[ "$INSTALL_MODE" == "docker" ]]; then
        download_file "vm/docker/docker-compose-crusoe-metrics-exporter.yaml" \
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
    install -m 0755 "${stage}/crusoe-metrics-exporter" "$CME_BIN"
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
    echo "CRUSOE_AUTH_TOKEN='${MONITORING_TOKEN}'" > "${SECRETS_DIR}/.monitoring-token"
    chmod 600 "${SECRETS_DIR}/.monitoring-token"
}

handle_token() {
    # Priority: --token flag > CRUSOE_AUTH_TOKEN env > existing file > interactive prompt.
    if [[ -n "$MONITORING_TOKEN" ]]; then
        status "Using token from --token flag."
    elif [[ -n "${CRUSOE_AUTH_TOKEN:-}" ]]; then
        MONITORING_TOKEN="$CRUSOE_AUTH_TOKEN"
        status "Using token from CRUSOE_AUTH_TOKEN environment variable."
    elif [[ -s "${SECRETS_DIR}/.monitoring-token" ]]; then
        MONITORING_TOKEN=$(sed "s/^CRUSOE_AUTH_TOKEN=//; s/^'//; s/'$//" "${SECRETS_DIR}/.monitoring-token")
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

    cat > "$ENV_FILE" <<EOF
VM_ID='${vm_id}'
TELEMETRY_INGRESS_ENDPOINT='${cms_url}/ingest'
LOGS_INGRESS_ENDPOINT='${cms_url}/logs/ingest'
AGENT_VERSION='${AGENT_VERSION}'
EOF

    # Docker image pins (read by docker-compose at `up` time).
    if [[ "$INSTALL_MODE" == "docker" ]]; then
        echo "VECTOR_VERSION='${VECTOR_VERSION}-debian'" >> "$ENV_FILE"
    fi

    # GPU-specific vars.
    if [[ "$GPU_TYPE" == "nvidia" ]]; then
        echo "DCGM_EXPORTER_PORT='${DCGM_EXPORTER_PORT}'" >> "$ENV_FILE"
        if [[ "$INSTALL_MODE" == "docker" ]]; then
            local image_ver="${DCGM_EXPORTER_VERSION_MAP[$UBUNTU_VERSION]:-4.3.1-4.4.0-ubi9}"
            echo "DCGM_EXPORTER_VERSION='${image_ver}'" >> "$ENV_FILE"
        fi
    elif [[ "$GPU_TYPE" == "amd" ]]; then
        {
            echo "GPU_TYPE='amd'"
            echo "AMD_EXPORTER_PORT='${AMD_EXPORTER_PORT}'"
            echo "AMD_EXPORTER_VERSION='${AMD_EXPORTER_VERSION}'"
        } >> "$ENV_FILE"
    fi

    # CME vars.
    if [[ "$ENABLE_CME" == "true" ]]; then
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
    fi

    chmod 640 "$ENV_FILE"
}

write_vector_config() {
    local cme_flag=""
    [[ "$ENABLE_CME" == "true" ]] && cme_flag="--cme"

    status "Generating Vector config (gpu=${GPU_TYPE}, cme=${ENABLE_CME})..."
    mkdir -p "$(dirname "$VECTOR_CONFIG")"
    local tmp="${VECTOR_CONFIG}.tmp.$$"  # Write atomically
    "${INSTALL_DIR}/cwa-manager" --dump-vector-config --gpu "$GPU_TYPE" ${cme_flag} > "$tmp"
    chmod 640 "$tmp"
    mv -f "$tmp" "$VECTOR_CONFIG"
}

install_vector_compose() {
    download_file "vm/docker/docker-compose-vector.yaml" "${CONFIG_DIR}/docker-compose-vector.yaml"
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

    download_file "vm/systemctl/${name}" "$dest"

    if [[ -n "$exec_start" ]]; then
        sed -i "s${d}@@EXEC_START@@${d}${exec_start}${d}" "$dest"
    fi
    if [[ -n "$exec_stop" ]]; then
        sed -i "s${d}@@EXEC_STOP@@${d}${exec_stop}${d}" "$dest"
    else
        sed -i '/@@EXEC_STOP@@/d' "$dest"
    fi
}

install_systemd_units() {
    status "Installing systemd units..."

    # cwa-manager
    if [[ "$INSTALL_MODE" == "docker" ]]; then
        download_file "vm/docker/docker-compose-cwa-manager.yaml" \
            "${CONFIG_DIR}/docker-compose-cwa-manager.yaml"
        install_unit "cwa-manager.service" \
            "/usr/bin/docker compose -f ${CONFIG_DIR}/docker-compose-cwa-manager.yaml up" \
            "/usr/bin/docker compose -f ${CONFIG_DIR}/docker-compose-cwa-manager.yaml down"
    else
        install_unit "cwa-manager.service" \
            "${INSTALL_DIR}/cwa-manager"
    fi

    # Vector
    if [[ "$INSTALL_MODE" == "docker" ]]; then
        install_unit "crusoe-watch-agent.service" \
            "/usr/bin/docker compose -f ${CONFIG_DIR}/docker-compose-vector.yaml up" \
            "/usr/bin/docker compose -f ${CONFIG_DIR}/docker-compose-vector.yaml down"
    else
        install_unit "crusoe-watch-agent.service" \
            "/usr/bin/vector --config ${VECTOR_CONFIG} --watch-config"
    fi

    # DCGM exporter
    if [[ "$GPU_TYPE" == "nvidia" ]]; then
        if [[ "$INSTALL_MODE" == "docker" ]]; then
            install_unit "crusoe-dcgm-exporter.service" \
                "/usr/bin/docker compose -f ${CONFIG_DIR}/docker-compose-dcgm-exporter.yaml up" \
                "/usr/bin/docker compose -f ${CONFIG_DIR}/docker-compose-dcgm-exporter.yaml down"
        else
            install_unit "crusoe-dcgm-exporter.service" \
                "/usr/bin/dcgm-exporter -f ${CONFIG_DIR}/dcp-metrics-included.csv -r localhost:5555 -a :${DCGM_EXPORTER_PORT}"
        fi
    fi

    # AMD exporter (Docker only, no placeholders)
    if [[ "$GPU_TYPE" == "amd" ]]; then
        install_unit "crusoe-amd-exporter.service"
    fi

    # CME Docker (native installs its own unit from tarball)
    if [[ "$ENABLE_CME" == "true" && "$INSTALL_MODE" == "docker" ]]; then
        install_unit "crusoe-metrics-exporter.service"
    fi
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
    [[ -f "${CONFIG_DIR}/vector.yaml" \
       || -f "${SYSTEMCTL_DIR}/crusoe-log-collector.service" \
       || -f "${SYSTEMCTL_DIR}/crusoe-amd-log-collector.service" \
       || -f "${SYSTEMCTL_DIR}/crusoe-nvidia-log-collector.service" ]]
}

# Tear down v1 artifacts that v2 doesn't ship.
migrate_from_v1() {
    v1_installed || return 0
    status "v1 install detected — cleaning up."

    for svc in crusoe-log-collector crusoe-amd-log-collector crusoe-nvidia-log-collector; do
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

    local vm_id
    vm_id=$(read_vm_id)
    status "VM ID: ${vm_id}"

    handle_token
    mkdir -p "$CONFIG_DIR"
    install_cwa_manager

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

    # Optional CME.
    if [[ "$ENABLE_CME" == "true" ]]; then
        cme_setup
    fi

    write_env_file "$vm_id"
    write_vector_config
    mkdir -p /var/lib/vector
    install_systemd_units

    save_install_mode
    save_version

    # Start services.
    status "Starting services..."
    systemctl daemon-reload

    local services=("cwa-manager.service" "crusoe-watch-agent.service")
    [[ "$GPU_TYPE" == "nvidia" ]] && services+=("crusoe-dcgm-exporter.service")
    [[ "$GPU_TYPE" == "amd" ]]    && services+=("crusoe-amd-exporter.service")
    [[ "$ENABLE_CME" == "true" ]] && services+=("crusoe-metrics-exporter.service")

    for svc in "${services[@]}"; do
        systemctl enable "$svc"
        systemctl restart "$svc"
    done

    echo ""
    status "Install complete. Check status:"
    echo "  systemctl status cwa-manager"
    echo "  systemctl status crusoe-watch-agent"
    [[ "$GPU_TYPE" == "nvidia" ]] && echo "  systemctl status crusoe-dcgm-exporter"
    [[ "$GPU_TYPE" == "amd" ]]    && echo "  systemctl status crusoe-amd-exporter"
    [[ "$ENABLE_CME" == "true" ]] && echo "  systemctl status crusoe-metrics-exporter"
}

do_uninstall() {
    require_root

    # Sweep v1 leftovers first (no-op on a clean v2 install).
    migrate_from_v1

    status "Stopping and disabling services..."
    local all_services=(
        cwa-manager.service
        crusoe-watch-agent.service
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
        local compose_files=(
            docker-compose-cwa-manager.yaml
            docker-compose-vector.yaml
            docker-compose-dcgm-exporter.yaml
            docker-compose-amd-exporter.yaml
            docker-compose-crusoe-metrics-exporter.yaml
        )
        for cf in "${compose_files[@]}"; do
            if [[ -f "${CONFIG_DIR}/${cf}" ]]; then
                status "Removing Docker containers: ${cf}"
                docker compose -f "${CONFIG_DIR}/${cf}" down --remove-orphans 2>/dev/null || true
            fi
        done
    fi

    status "Removing files..."
    for svc in "${all_services[@]}"; do
        rm -f "${SYSTEMCTL_DIR}/${svc}"
    done
    rm -f "${INSTALL_DIR}/cwa-manager"
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
    rm -f "$CME_BIN"

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

    # Download latest install script.
    local script_url="${GITHUB_LATEST_RELEASE_URL}/crusoe_watch_agent.sh"
    local tmp_script
    tmp_script=$(mktemp)
    wget -q -O "$tmp_script" "$script_url" || error_exit "Failed to download latest installer."
    chmod +x "$tmp_script"

    # Replay saved args.
    local -a saved_args=()
    if [[ -f "${SECRETS_DIR}/.install-args" ]]; then
        while IFS= read -r line; do
            [[ -n "$line" ]] && saved_args+=("$line")
        done < "${SECRETS_DIR}/.install-args"
    fi

    CWA_UPGRADE=1 "$tmp_script" install "${saved_args[@]}"
    rm -f "$tmp_script"

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
    echo "  sudo systemctl restart crusoe-watch-agent"
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
  --cme                      Enable Crusoe Metrics Exporter
  --ingress-url URL          Override CMS base URL
  --dcgm-exporter-port PORT  DCGM exporter port (default: 9400)
  --amd-exporter-port PORT   AMD exporter port (default: 5000)

GPU type is auto-detected. NVIDIA and AMD GPUs are supported.
AMD GPUs require Docker mode (--no-docker is not supported with AMD).

Examples:
  sudo ./crusoe_watch_agent.sh install
  sudo ./crusoe_watch_agent.sh install --no-docker
  sudo ./crusoe_watch_agent.sh install --token "$(crusoe monitoring tokens create -f token)"
  sudo ./crusoe_watch_agent.sh install --cme
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
        --cme)
            ENABLE_CME=true
            shift
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
