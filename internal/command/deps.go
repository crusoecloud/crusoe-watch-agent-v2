package command

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/vector"
	pb "gitlab.com/crusoeenergy/schemas/api/island/v2/observability"
)

// stateFilePerm is used for all persisted control-plane state files.
const stateFilePerm = 0o600

var (
	errUnsupportedInstallType = errors.New("unsupported install type")
	errWatcherUnavailable     = errors.New("k8s watcher unavailable")
)

// Reloader is the K8s watcher surface handlers use to apply control-plane
// overrides so they survive subsequent data-plane reconciles.
// *watcher.Watcher implements it.
type Reloader interface {
	SetIngestionEndpoints(logs, metrics string)
	SetIngestionBlocked(blocked bool)
	SetRateLimits(limits map[string]int)
}

// Deps wires command handlers to platform-specific machinery.
// One value is shared by every handler registered on the dispatcher.
type Deps struct {
	InstallType  pb.CwaInstallType
	VMCfg        vector.VMConfig // baseline VM config; control-plane state folded in per-apply
	VMConfigPath string          // where the VM Vector config is written
	Watcher      Reloader        // non-nil on K8s

	// Store records in-flight command executions for crash recovery.
	Store *ExecStore

	// Persisted control-plane.
	LogsStatePath      string // config.apply logs base URL (file content)
	MetricsStatePath   string // config.apply metrics base URL (file content)
	BlockedStatePath   string // ingestion.block marker (file presence)
	RateLimitStatePath string // rate_limit.set sink→cap map (JSON file content)
}

// apply routes a state change to the platform-specific machinery: on K8s the
// watcher folds it into every subsequent reconcile; on VMs the Vector config
// is regenerated with the full control-plane state and atomically rewritten
// for --watch-config to pick up.
func (d Deps) apply(
	applyK8s func(Reloader),
	vmLogs, vmMetrics string, vmBlocked bool, vmRateLimits map[string]int,
) error {
	switch d.InstallType {
	case pb.CwaInstallType_CWA_INSTALL_TYPE_KUBERNETES:
		if d.Watcher == nil {
			return errWatcherUnavailable
		}

		applyK8s(d.Watcher)

		return nil
	case pb.CwaInstallType_CWA_INSTALL_TYPE_DOCKER, pb.CwaInstallType_CWA_INSTALL_TYPE_SYSTEMD:
		vmCfg := d.VMCfg
		vmCfg.LogsEndpoint = vmLogs
		vmCfg.MetricsEndpoint = vmMetrics
		vmCfg.IngestionBlocked = vmBlocked
		vmCfg.RateLimits = vmRateLimits

		out, err := vector.GenerateVM(vmCfg)
		if err != nil {
			return fmt.Errorf("generating vector config: %w", err)
		}

		if err := vector.WriteConfigFile(d.VMConfigPath, out); err != nil {
			return fmt.Errorf("writing vector config: %w", err)
		}

		return nil
	case pb.CwaInstallType_CWA_INSTALL_TYPE_UNSPECIFIED:
	}

	return fmt.Errorf("%w: %s", errUnsupportedInstallType, d.InstallType)
}

// LoadEndpoint reads a persisted control-plane endpoint file.
func LoadEndpoint(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(data))
}

// LoadIngestionBlocked reports whether the persisted ingestion-blocked marker
// exists. The marker's presence — not its content — is the blocked state.
func LoadIngestionBlocked(path string) bool {
	if path == "" {
		return false
	}

	_, err := os.Stat(path)

	return err == nil
}

// LoadRateLimits reads the persisted rate_limit.set map (sink name → requests
// per minute; the vector.RateLimitAll key is the fleet-wide default).
func LoadRateLimits(path string) map[string]int {
	limits := map[string]int{}
	if path == "" {
		return limits
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return limits
	}

	if err := json.Unmarshal(data, &limits); err != nil {
		return map[string]int{}
	}

	for name, rate := range limits {
		if rate <= 0 {
			delete(limits, name)
		}
	}

	return limits
}
