package command

import (
	"context"
	"fmt"
	"os"
	"time"
)

// ConfigGetCommand reports the agent's current Vector configuration back to the coordinator.
const ConfigGetCommand = "config.get"

// ConfigGet is the config.get handler.
type ConfigGet struct {
	deps Deps
}

// NewConfigGet creates a config.get handler.
func NewConfigGet(deps Deps) *ConfigGet {
	return &ConfigGet{deps: deps}
}

// Timeout returns the instant class (config.get is a 30s command).
func (c *ConfigGet) Timeout() time.Duration { return Instant }

// Run returns the full Vector config the agent is currently running, as its
// result payload. Both VM and K8s install types render their config to the same
// on-disk file (VECTOR_CONFIG_PATH).
func (c *ConfigGet) Run(_ context.Context, _ map[string]string) (string, error) {
	data, err := os.ReadFile(c.deps.VMConfigPath)
	if err != nil {
		return "", fmt.Errorf("reading vector config: %w", err)
	}

	return string(data), nil
}
