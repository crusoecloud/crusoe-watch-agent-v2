package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/vector"
)

// RateLimitSetCommand adjusts the Vector forwarding rate limit.
const RateLimitSetCommand = "rate_limit.set"

// ParamRateLimitNum is the required rate_limit.set parameter: the maximum number
// of forwarding requests the targeted sink(s) may issue per minute.
// Zero removes the cap for the target (Vector default: unlimited).
const ParamRateLimitNum = "rate_limit_num"

// ParamRateLimitSink is the optional rate_limit.set parameter: the name of the
// single Vector sink to cap. When omitted the cap becomes the default applied to
// every sink without its own explicit entry.
const ParamRateLimitSink = "rate_limit_sink"

var errInvalidRateLimit = errors.New("missing or invalid required parameter: " +
	ParamRateLimitNum + " (must be a non-negative integer)")

// RateLimitSet handles rate_limit.set.
type RateLimitSet struct {
	deps Deps
}

// NewRateLimitSet creates a rate_limit.set handler.
func NewRateLimitSet(deps Deps) *RateLimitSet {
	return &RateLimitSet{deps: deps}
}

// Timeout returns the instant class (rate_limit.set is a 30s command).
func (r *RateLimitSet) Timeout() time.Duration { return Instant }

// Run parses the requested rate and target sink, merges it into the persisted
// sink→cap map, then applies the merged map to the running data plane. A rate of
// zero removes the target's entry; an absent sink targets the fleet-wide default.
func (r *RateLimitSet) Run(_ context.Context, params map[string]string) (string, error) {
	rate, err := strconv.Atoi(params[ParamRateLimitNum])
	if err != nil || rate < 0 {
		return "", errInvalidRateLimit
	}

	target := params[ParamRateLimitSink]
	if target == "" {
		target = vector.RateLimitAll
	}

	limits := LoadRateLimits(r.deps.RateLimitStatePath)
	if rate == 0 {
		delete(limits, target)
	} else {
		limits[target] = rate
	}

	if err := persistRateLimits(r.deps.RateLimitStatePath, limits); err != nil {
		return "", err
	}

	return "", r.deps.apply(
		func(w Reloader) { w.SetRateLimits(limits) },
		LoadEndpoint(r.deps.LogsStatePath), LoadEndpoint(r.deps.MetricsStatePath),
		LoadIngestionBlocked(r.deps.BlockedStatePath), limits,
	)
}

// persistRateLimits records the sink→cap map as JSON so it survives restarts.
func persistRateLimits(path string, limits map[string]int) error {
	if path == "" {
		return nil
	}

	data, err := json.Marshal(limits)
	if err != nil {
		return fmt.Errorf("encoding rate limits: %w", err)
	}

	if err := os.WriteFile(path, append(data, '\n'), stateFilePerm); err != nil {
		return fmt.Errorf("persisting rate limits: %w", err)
	}

	return nil
}
