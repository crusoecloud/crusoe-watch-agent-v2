package command

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/vector"
	pb "gitlab.com/crusoeenergy/schemas/api/island/v2/observability"
)

const ConfigApplyCommand = "config.apply"

// config.apply parameters. All are base URLs; ingestion_endpoint targets both
// sink kinds, while logs_endpoint / metrics_endpoint override one kind each.
const (
	ParamIngestionEndpoint = "ingestion_endpoint"
	ParamLogsEndpoint      = "logs_endpoint"
	ParamMetricsEndpoint   = "metrics_endpoint"
)

const endpointFilePerm = 0o600

var (
	errMissingEndpoint = errors.New("missing required parameter: one of " +
		ParamIngestionEndpoint + ", " + ParamLogsEndpoint + ", " + ParamMetricsEndpoint)
	errUnsupportedInstallType = errors.New("unsupported install type")
	errWatcherUnavailable     = errors.New("k8s watcher unavailable")
)

// LoadEndpoint reads a persisted control-plane endpoint file.
func LoadEndpoint(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(data))
}

// ConfigReloader applies the endpoint overrides on the K8s watcher so they
// survive subsequent data-plane reconciles. *watcher.Watcher implements it.
type ConfigReloader interface {
	SetIngestionEndpoints(logs, metrics string)
}

// ConfigApplyDeps wires the config.apply handler to platform-specific machinery.
type ConfigApplyDeps struct {
	InstallType  pb.CwaInstallType
	VMCfg        vector.VMConfig // baseline VM config; the endpoints are folded in per-apply
	VMConfigPath string          // where the VM Vector config is written
	Watcher      ConfigReloader  // non-nil on K8s

	LogsStatePath    string
	MetricsStatePath string
}

// ConfigApply is the config.apply handler.
type ConfigApply struct {
	deps ConfigApplyDeps
}

// NewConfigApply creates a config.apply handler.
func NewConfigApply(deps ConfigApplyDeps) *ConfigApply {
	return &ConfigApply{deps: deps}
}

// Timeout returns the instant class (config.apply is a 30s command).
func (c *ConfigApply) Timeout() time.Duration { return Instant }

// Run applies new ingestion endpoints. It persists them first (so a restart
// converges toward them) then applies them to the running data plane.
func (c *ConfigApply) Run(_ context.Context, params map[string]string) error {
	if params[ParamIngestionEndpoint] == "" &&
		params[ParamLogsEndpoint] == "" && params[ParamMetricsEndpoint] == "" {

		return errMissingEndpoint
	}

	logs, metrics := c.resolveEndpoints(params)

	if err := persistEndpoint(c.deps.LogsStatePath, logs); err != nil {
		return err
	}

	if err := persistEndpoint(c.deps.MetricsStatePath, metrics); err != nil {
		return err
	}

	switch c.deps.InstallType {
	case pb.CwaInstallType_CWA_INSTALL_TYPE_KUBERNETES:
		return c.applyK8s(logs, metrics)
	case pb.CwaInstallType_CWA_INSTALL_TYPE_DOCKER, pb.CwaInstallType_CWA_INSTALL_TYPE_SYSTEMD:
		return c.applyVM(logs, metrics)
	case pb.CwaInstallType_CWA_INSTALL_TYPE_UNSPECIFIED:
	}

	return fmt.Errorf("%w: %s", errUnsupportedInstallType, c.deps.InstallType)
}

// resolveEndpoints computes the effective logs and metrics base URLs.
func (c *ConfigApply) resolveEndpoints(params map[string]string) (string, string) {
	logs := params[ParamLogsEndpoint]
	metrics := params[ParamMetricsEndpoint]

	if base := params[ParamIngestionEndpoint]; base != "" {
		if logs == "" {
			logs = base
		}

		if metrics == "" {
			metrics = base
		}
	}

	if logs == "" {
		logs = LoadEndpoint(c.deps.LogsStatePath)
	}

	if metrics == "" {
		metrics = LoadEndpoint(c.deps.MetricsStatePath)
	}

	return logs, metrics
}

func persistEndpoint(path, endpoint string) error {
	if path == "" || endpoint == "" {
		return nil
	}

	if err := os.WriteFile(path, []byte(endpoint+"\n"), endpointFilePerm); err != nil {
		return fmt.Errorf("persisting endpoint: %w", err)
	}

	return nil
}

// applyK8s hands the endpoints to the watcher, which merges them into every
// subsequent reconcile alongside the data-plane-derived config.
func (c *ConfigApply) applyK8s(logs, metrics string) error {
	if c.deps.Watcher == nil {
		return errWatcherUnavailable
	}

	c.deps.Watcher.SetIngestionEndpoints(logs, metrics)

	return nil
}

// applyVM regenerates the VM Vector config with the new endpoints and atomically
// rewrites it. Vector's --watch-config picks it up.
func (c *ConfigApply) applyVM(logs, metrics string) error {
	vmCfg := c.deps.VMCfg
	vmCfg.LogsEndpoint = logs
	vmCfg.MetricsEndpoint = metrics

	out, err := vector.GenerateVM(vmCfg)
	if err != nil {
		return fmt.Errorf("generating vector config: %w", err)
	}

	if err := vector.WriteConfigFile(c.deps.VMConfigPath, out); err != nil {
		return fmt.Errorf("writing vector config: %w", err)
	}

	return nil
}
