#!/usr/bin/env bash
# Crusoe Watch Agent 2.0 installer for VM targets (systemd).
# Installs cwa-manager and Vector as peer systemd services.
#
# Usage:
#   sudo ./crusoe_watch_agent.sh install [--token TOKEN] [--gpu none|nvidia|amd] [--coordinator ADDR]
#   sudo ./crusoe_watch_agent.sh uninstall
set -euo pipefail

# --- Defaults ---
CMS_BASE_URL="https://cms-monitoring.crusoecloud.com"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/crusoe/cwa"
SECRETS_DIR="/etc/crusoe/secrets"
ENV_FILE="${CONFIG_DIR}/.env"
VECTOR_CONFIG="/etc/vector/vector.yaml"
INSTALL_MODE_FILE="${CONFIG_DIR}/.install-mode"

# --- Configurable via flags ---
GPU_TYPE=""
ENABLE_CME=false
COORDINATOR_ADDR=""
MONITORING_TOKEN=""

# --- Helpers ---
status() { echo "==> $1"; }
error_exit() { echo "ERROR: $1" >&2; exit 1; }

read_vm_id() {
    local vm_id=""
    for path in /sys/class/dmi/id/product_uuid; do
        if [[ -r "$path" ]]; then
            vm_id=$(tr -d '[:space:]' < "$path")
            break
        fi
    done
    if [[ -z "$vm_id" ]]; then
        vm_id=$(dmidecode -s system-uuid 2>/dev/null | tr -d '[:space:]') || true
    fi
    if [[ -z "$vm_id" ]]; then
        error_exit "Could not read VM UUID"
    fi
    echo "$vm_id"
}

detect_gpu() {
    if command -v nvidia-smi &>/dev/null && nvidia-smi &>/dev/null; then
        echo "nvidia"
    elif [[ -d /sys/module/amdgpu ]]; then
        echo "amd"
    else
        echo "none"
    fi
}

# --- Component installers ---

install_vector() {
    if command -v vector &>/dev/null; then
        status "Vector already installed: $(vector --version 2>&1 | head -1)"
        return
    fi
    status "Installing Vector..."
    bash -c "$(curl -fsSL https://setup.vector.dev)"
    apt-get install -y vector
    # Disable the default Vector service — we manage our own unit.
    systemctl disable --now vector.service 2>/dev/null || true
}

install_cwa_manager() {
    local src="${1:-./cwa-manager}"
    if [[ ! -f "$src" ]]; then
        error_exit "cwa-manager binary not found at $src. Build with: make cross"
    fi
    status "Installing cwa-manager to ${INSTALL_DIR}/cwa-manager"
    cp "$src" "${INSTALL_DIR}/cwa-manager"
    chmod 755 "${INSTALL_DIR}/cwa-manager"
}

# --- Configuration ---

write_env_file() {
    local vm_id="$1"
    local token="$2"

    mkdir -p "$CONFIG_DIR" "$SECRETS_DIR"

    status "Writing env file to ${ENV_FILE}"
    cat > "$ENV_FILE" <<EOF
VM_ID=${vm_id}
TELEMETRY_INGRESS_ENDPOINT=${CMS_BASE_URL}/ingest
LOGS_INGRESS_ENDPOINT=${CMS_BASE_URL}/logs/ingest
AGENT_VERSION=dev
EOF
    chmod 640 "$ENV_FILE"

    # Token in a separate file matching v1 convention.
    echo "CRUSOE_AUTH_TOKEN=${token}" > "${SECRETS_DIR}/.monitoring-token"
    chmod 600 "${SECRETS_DIR}/.monitoring-token"
}

write_vector_config() {
    local gpu="$1"

    local cme_flag=""
    if [[ "$ENABLE_CME" == "true" ]]; then
        cme_flag="--cme"
    fi

    status "Generating Vector config (gpu=${gpu}, cme=${ENABLE_CME})"
    mkdir -p "$(dirname "$VECTOR_CONFIG")"
    "${INSTALL_DIR}/cwa-manager" --dump-vector-config --gpu "$gpu" ${cme_flag} > "$VECTOR_CONFIG"
    chmod 640 "$VECTOR_CONFIG"
}

write_install_mode() {
    mkdir -p "$CONFIG_DIR"
    echo "native" > "$INSTALL_MODE_FILE"
}

write_systemd_units() {
    local coordinator_flag=""
    if [[ -n "$COORDINATOR_ADDR" ]]; then
        coordinator_flag="--coordinator ${COORDINATOR_ADDR}"
    fi

    status "Writing systemd units"

    cat > /etc/systemd/system/cwa-manager.service <<EOF
[Unit]
Description=CWA Manager - Crusoe Watch Agent management process
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=${ENV_FILE}
EnvironmentFile=${SECRETS_DIR}/.monitoring-token
ExecStart=${INSTALL_DIR}/cwa-manager ${coordinator_flag}
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

    cat > /etc/systemd/system/crusoe-vector.service <<EOF
[Unit]
Description=Vector - Crusoe Watch Agent data pipeline
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=${ENV_FILE}
EnvironmentFile=${SECRETS_DIR}/.monitoring-token
ExecStart=/usr/bin/vector --config ${VECTOR_CONFIG} --watch-config
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
}

# --- Commands ---

do_install() {
    [[ $EUID -ne 0 ]] && error_exit "Must run as root"

    local vm_id
    vm_id=$(read_vm_id)
    status "VM ID: ${vm_id}"

    # Auto-detect GPU if not explicitly set.
    if [[ -z "$GPU_TYPE" ]]; then
        GPU_TYPE=$(detect_gpu)
        status "Detected GPU type: ${GPU_TYPE}"
    fi

    # Prompt for token if not provided.
    if [[ -z "$MONITORING_TOKEN" ]]; then
        if [[ -s "${SECRETS_DIR}/.monitoring-token" ]]; then
            status "Using existing monitoring token from ${SECRETS_DIR}/.monitoring-token"
            MONITORING_TOKEN=$(sed 's/^CRUSOE_AUTH_TOKEN=//' "${SECRETS_DIR}/.monitoring-token")
        else
            echo "Enter monitoring token (or press enter for test placeholder):"
            read -rs MONITORING_TOKEN
            if [[ -z "$MONITORING_TOKEN" ]]; then
                MONITORING_TOKEN="test-token-placeholder"
                status "Using test placeholder token"
            fi
        fi
    fi

    install_vector
    install_cwa_manager
    write_env_file "$vm_id" "$MONITORING_TOKEN"
    write_install_mode
    write_vector_config "$GPU_TYPE"
    write_systemd_units

    status "Starting services"
    systemctl enable --now crusoe-vector.service
    systemctl enable --now cwa-manager.service

    echo ""
    status "Install complete. Check status:"
    echo "  systemctl status cwa-manager"
    echo "  systemctl status crusoe-vector"
    echo "  curl http://127.0.0.1:8686/health"
    echo "  curl -s http://127.0.0.1:9598/metrics | head -20"
}

do_uninstall() {
    [[ $EUID -ne 0 ]] && error_exit "Must run as root"

    status "Stopping and disabling services"
    systemctl stop cwa-manager.service crusoe-vector.service 2>/dev/null || true
    systemctl disable cwa-manager.service crusoe-vector.service 2>/dev/null || true

    status "Removing files"
    rm -f /etc/systemd/system/cwa-manager.service
    rm -f /etc/systemd/system/crusoe-vector.service
    rm -f "${INSTALL_DIR}/cwa-manager"
    rm -f "${VECTOR_CONFIG}"
    rm -rf "${CONFIG_DIR}"
    systemctl daemon-reload

    status "Uninstall complete. Secrets at ${SECRETS_DIR} preserved."
}

# --- Argument parsing ---

parse_args() {
    COMMAND="${1:-}"
    shift || true

    while [[ $# -gt 0 ]]; do
        case "$1" in
            --token)
                MONITORING_TOKEN="${2:?Missing value for --token}"
                shift 2
                ;;
            --gpu)
                GPU_TYPE="${2:?Missing value for --gpu}"
                shift 2
                ;;
            --cme)
                ENABLE_CME=true
                shift
                ;;
            --coordinator)
                COORDINATOR_ADDR="${2:?Missing value for --coordinator}"
                shift 2
                ;;
            *)
                error_exit "Unknown option: $1"
                ;;
        esac
    done
}

parse_args "$@"

case "$COMMAND" in
    install)
        do_install
        ;;
    uninstall)
        do_uninstall
        ;;
    *)
        echo "Crusoe Watch Agent 2.0 Installer"
        echo ""
        echo "Usage: $0 {install|uninstall} [OPTIONS]"
        echo ""
        echo "Commands:"
        echo "  install     Install cwa-manager and Vector"
        echo "  uninstall   Stop services and remove files"
        echo ""
        echo "Options:"
        echo "  --token TOKEN          Monitoring token (prompted if omitted)"
        echo "  --gpu none|nvidia|amd  GPU type (auto-detected if omitted)"
        echo "  --cme                  Enable Crusoe Metrics Exporter"
        echo "  --coordinator ADDR     cwa-coordinator gRPC address"
        exit 1
        ;;
esac
