package command

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

const ConfigApplyCommand = "config.apply"

// config.apply parameters. All are base URLs; ingestion_endpoint targets both
// sink kinds, while logs_endpoint / metrics_endpoint override one kind each.
const (
	ParamIngestionEndpoint = "ingestion_endpoint"
	ParamLogsEndpoint      = "logs_endpoint"
	ParamMetricsEndpoint   = "metrics_endpoint"
)

var errMissingEndpoint = errors.New("missing required parameter: one of " +
	ParamIngestionEndpoint + ", " + ParamLogsEndpoint + ", " + ParamMetricsEndpoint)

// ConfigApply is the config.apply handler.
type ConfigApply struct {
	deps Deps
}

// NewConfigApply creates a config.apply handler.
func NewConfigApply(deps Deps) *ConfigApply {
	return &ConfigApply{deps: deps}
}

// Timeout returns the instant class (config.apply is a 30s command).
func (c *ConfigApply) Timeout() time.Duration { return Instant }

// Run applies new ingestion endpoints. It persists them first (so a restart
// converges toward them) then applies them to the running data plane.
func (c *ConfigApply) Run(_ context.Context, params map[string]string) (string, error) {
	if params[ParamIngestionEndpoint] == "" &&
		params[ParamLogsEndpoint] == "" && params[ParamMetricsEndpoint] == "" {

		return "", errMissingEndpoint
	}

	logs, metrics := c.resolveEndpoints(params)

	if err := persistEndpoint(c.deps.LogsStatePath, logs); err != nil {
		return "", err
	}

	if err := persistEndpoint(c.deps.MetricsStatePath, metrics); err != nil {
		return "", err
	}

	return "", c.deps.apply(
		func(w Reloader) { w.SetIngestionEndpoints(logs, metrics) },
		logs, metrics, LoadIngestionBlocked(c.deps.BlockedStatePath),
		LoadRateLimits(c.deps.RateLimitStatePath),
	)
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

	if err := os.WriteFile(path, []byte(endpoint+"\n"), stateFilePerm); err != nil {
		return fmt.Errorf("persisting endpoint: %w", err)
	}

	return nil
}
