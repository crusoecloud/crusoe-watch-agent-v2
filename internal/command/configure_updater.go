package command

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/upgrade"
)

// ConfigureUpdaterCommand delivers a new configuration to cwa-updater without replacing its binary.
const ConfigureUpdaterCommand = "configure-updater"

// configure-updater parameters. The delivery carries the whole configuration,
// so an omitted parameter returns that setting to its default.
const (
	ParamDownloadURLBase       = upgrade.FieldDownloadURLBase
	ParamRollbackTimeoutMin    = upgrade.FieldRollbackTimeoutMin
	ParamDownloadBackoffMaxSec = upgrade.FieldDownloadBackoffMaxSec
)

// Configurer is the cwa-updater surface this handler needs. *upgrade.Client implements it.
type Configurer interface {
	Configure(ctx context.Context, cfg upgrade.RuntimeConfig) (upgrade.ConfigAck, error)
}

// ConfigureUpdater is the configure-updater handler. cwa-updater makes the config
// live before answering, so SUCCEEDED means the reload happened. The result
// payload names which fields took effect.
type ConfigureUpdater struct {
	client Configurer
}

// NewConfigureUpdater creates a configure-updater handler.
func NewConfigureUpdater(client Configurer) *ConfigureUpdater {
	return &ConfigureUpdater{client: client}
}

// Timeout returns the instant class (configure-updater is a 30s command).
func (c *ConfigureUpdater) Timeout() time.Duration { return Instant }

// Run delivers the configuration to cwa-updater and reports what it made live.
func (c *ConfigureUpdater) Run(ctx context.Context, params map[string]string) (string, error) {
	cfg, err := updaterConfigFromParams(params)
	if err != nil {
		return "", err
	}

	ack, err := c.client.Configure(ctx, cfg)
	if err != nil {
		return "", fmt.Errorf("configuring cwa-updater: %w", err)
	}

	return encode(ack, "the updater config acknowledgement")
}

// updaterConfigFromParams reads the string parameters into a config.
func updaterConfigFromParams(params map[string]string) (upgrade.RuntimeConfig, error) {
	rollback, err := intParam(params, ParamRollbackTimeoutMin)
	if err != nil {
		return upgrade.RuntimeConfig{}, err
	}

	backoff, err := intParam(params, ParamDownloadBackoffMaxSec)
	if err != nil {
		return upgrade.RuntimeConfig{}, err
	}

	return upgrade.RuntimeConfig{
		DownloadURLBase:       params[ParamDownloadURLBase],
		RollbackTimeoutMin:    rollback,
		DownloadBackoffMaxSec: backoff,
	}, nil
}

// intParam reads an integer parameter; an absent one is zero, meaning unset.
func intParam(params map[string]string, key string) (int, error) {
	raw, ok := params[key]
	if !ok || raw == "" {
		return 0, nil
	}

	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: %s must be an integer, got %q",
			upgrade.ErrInvalidConfig, key, raw)
	}

	return value, nil
}
