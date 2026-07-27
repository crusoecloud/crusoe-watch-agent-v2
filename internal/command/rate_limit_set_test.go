package command

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "gitlab.com/crusoeenergy/schemas/api/island/v2/observability"
)

// reqRateLimit returns the rate_limit_num on a sink, or nil if it has no cap.
func reqRateLimit(t *testing.T, sinks map[string]any, sink string) any {
	t.Helper()
	req := sinks[sink].(map[string]any)["request"].(map[string]any)

	return req["rate_limit_num"]
}

func TestRateLimitSet_VM(t *testing.T) {
	deps := testDeps(t)
	h := NewRateLimitSet(deps)

	require.NoError(t, h.Run(context.Background(), map[string]string{ParamRateLimitNum: "100"}))

	// With no sink named, every external forwarding sink is capped; the map persists.
	sinks := vmSinks(t, deps.VMConfigPath)
	req := sinks["crusoe_ingest"].(map[string]any)["request"].(map[string]any)
	assert.Equal(t, 100, req["rate_limit_num"])
	assert.Equal(t, 60, req["rate_limit_duration_secs"])
	assert.Equal(t, 100, reqRateLimit(t, sinks, "cms_gateway"))
	assert.Equal(t, map[string]int{"*": 100}, LoadRateLimits(deps.RateLimitStatePath))
}

func TestRateLimitSet_VM_SpecificSink(t *testing.T) {
	deps := testDeps(t)
	h := NewRateLimitSet(deps)

	require.NoError(t, h.Run(context.Background(), map[string]string{
		ParamRateLimitNum:  "100",
		ParamRateLimitSink: "crusoe_ingest",
	}))

	// Only the named sink is capped; every other sink is left unlimited.
	sinks := vmSinks(t, deps.VMConfigPath)
	assert.Equal(t, 100, reqRateLimit(t, sinks, "crusoe_ingest"))
	assert.Nil(t, reqRateLimit(t, sinks, "cms_gateway"))
	assert.Equal(t, map[string]int{"crusoe_ingest": 100}, LoadRateLimits(deps.RateLimitStatePath))
}

func TestRateLimitSet_VM_AccumulatesPerSink(t *testing.T) {
	deps := testDeps(t)
	h := NewRateLimitSet(deps)

	require.NoError(t, h.Run(context.Background(), map[string]string{
		ParamRateLimitNum:  "100",
		ParamRateLimitSink: "crusoe_ingest",
	}))
	require.NoError(t, h.Run(context.Background(), map[string]string{
		ParamRateLimitNum:  "50",
		ParamRateLimitSink: "cms_gateway",
	}))

	// Both caps coexist: the second command does not clobber the first.
	sinks := vmSinks(t, deps.VMConfigPath)
	assert.Equal(t, 100, reqRateLimit(t, sinks, "crusoe_ingest"))
	assert.Equal(t, 50, reqRateLimit(t, sinks, "cms_gateway"))
	assert.Equal(t, map[string]int{"crusoe_ingest": 100, "cms_gateway": 50},
		LoadRateLimits(deps.RateLimitStatePath))
}

func TestRateLimitSet_VM_DefaultPlusOverride(t *testing.T) {
	deps := testDeps(t)
	h := NewRateLimitSet(deps)

	// A fleet-wide default, then a tighter cap on one sink.
	require.NoError(t, h.Run(context.Background(), map[string]string{ParamRateLimitNum: "100"}))
	require.NoError(t, h.Run(context.Background(), map[string]string{
		ParamRateLimitNum:  "10",
		ParamRateLimitSink: "crusoe_ingest",
	}))

	sinks := vmSinks(t, deps.VMConfigPath)
	assert.Equal(t, 10, reqRateLimit(t, sinks, "crusoe_ingest"))
	assert.Equal(t, 100, reqRateLimit(t, sinks, "cms_gateway"))
}

func TestRateLimitSet_VM_ZeroRemovesTarget(t *testing.T) {
	deps := testDeps(t)

	// crusoe_ingest was capped earlier while cms_gateway kept the default.
	require.NoError(t, os.WriteFile(deps.RateLimitStatePath,
		[]byte(`{"*":100,"crusoe_ingest":10}`), 0o600))

	h := NewRateLimitSet(deps)
	require.NoError(t, h.Run(context.Background(), map[string]string{
		ParamRateLimitNum:  "0",
		ParamRateLimitSink: "crusoe_ingest",
	}))

	// The override is cleared, so crusoe_ingest falls back to the default; others keep it.
	sinks := vmSinks(t, deps.VMConfigPath)
	assert.Equal(t, 100, reqRateLimit(t, sinks, "crusoe_ingest"))
	assert.Equal(t, 100, reqRateLimit(t, sinks, "cms_gateway"))
	assert.Equal(t, map[string]int{"*": 100}, LoadRateLimits(deps.RateLimitStatePath))
}

func TestRateLimitSet_VM_ZeroRemovesDefault(t *testing.T) {
	deps := testDeps(t)

	require.NoError(t, os.WriteFile(deps.RateLimitStatePath, []byte(`{"*":100}`), 0o600))

	h := NewRateLimitSet(deps)
	require.NoError(t, h.Run(context.Background(), map[string]string{ParamRateLimitNum: "0"}))

	// Zero with no sink clears the default: no sink keeps a cap.
	sinks := vmSinks(t, deps.VMConfigPath)
	assert.Nil(t, reqRateLimit(t, sinks, "crusoe_ingest"))
	assert.Nil(t, reqRateLimit(t, sinks, "cms_gateway"))
	assert.Empty(t, LoadRateLimits(deps.RateLimitStatePath))
}

func TestRateLimitSet_VM_PreservesEndpoints(t *testing.T) {
	deps := testDeps(t)

	// Endpoints were set by an earlier config.apply.
	require.NoError(t, os.WriteFile(deps.LogsStatePath, []byte("https://logs.example.com\n"), 0o600))
	require.NoError(t, os.WriteFile(deps.MetricsStatePath, []byte("https://metrics.example.com\n"), 0o600))

	h := NewRateLimitSet(deps)
	require.NoError(t, h.Run(context.Background(), map[string]string{ParamRateLimitNum: "50"}))

	// The rewritten config keeps the persisted endpoint overrides and adds the cap.
	sinks := vmSinks(t, deps.VMConfigPath)
	assert.Equal(t, "https://logs.example.com/logs/ingest", sinks["crusoe_ingest"].(map[string]any)["uri"])
	assert.Equal(t, "https://metrics.example.com/ingest", sinks["cms_gateway"].(map[string]any)["endpoint"])
	assert.Equal(t, 50, reqRateLimit(t, sinks, "cms_gateway"))
}

func TestRateLimitSet_InvalidParam(t *testing.T) {
	deps := testDeps(t)
	h := NewRateLimitSet(deps)

	for _, val := range []map[string]string{
		{},
		{ParamRateLimitNum: ""},
		{ParamRateLimitNum: "abc"},
		{ParamRateLimitNum: "-5"},
	} {
		err := h.Run(context.Background(), val)
		require.Error(t, err)
		assert.Contains(t, err.Error(), ParamRateLimitNum)
	}
}

func TestRateLimitSet_K8s(t *testing.T) {
	rel := &fakeReloader{}

	h := NewRateLimitSet(Deps{
		InstallType:        pb.CwaInstallType_CWA_INSTALL_TYPE_KUBERNETES,
		Watcher:            rel,
		RateLimitStatePath: filepath.Join(t.TempDir(), ".rate-limit.json"),
	})

	require.NoError(t, h.Run(context.Background(), map[string]string{
		ParamRateLimitNum:  "25",
		ParamRateLimitSink: "cms_gateway_node_metrics",
	}))
	assert.True(t, rel.rateLimitsSet)
	assert.Equal(t, map[string]int{"cms_gateway_node_metrics": 25}, rel.rateLimits)
}

func TestRateLimitSet_K8s_NoWatcher(t *testing.T) {
	h := NewRateLimitSet(Deps{
		InstallType: pb.CwaInstallType_CWA_INSTALL_TYPE_KUBERNETES,
	})

	assert.Error(t, h.Run(context.Background(), map[string]string{ParamRateLimitNum: "25"}))
}

func TestRateLimitSet_UnsupportedInstallType(t *testing.T) {
	h := NewRateLimitSet(Deps{
		InstallType: pb.CwaInstallType_CWA_INSTALL_TYPE_UNSPECIFIED,
	})

	assert.Error(t, h.Run(context.Background(), map[string]string{ParamRateLimitNum: "25"}))
}

func TestRateLimitSet_Timeout(t *testing.T) {
	assert.Equal(t, Instant, NewRateLimitSet(Deps{}).Timeout())
}

func TestLoadRateLimits(t *testing.T) {
	assert.Empty(t, LoadRateLimits(""))
	assert.Empty(t, LoadRateLimits(filepath.Join(t.TempDir(), "absent")))

	dir := t.TempDir()
	path := filepath.Join(dir, ".rate-limit.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"*":75,"crusoe_ingest":10}`), 0o600))
	assert.Equal(t, map[string]int{"*": 75, "crusoe_ingest": 10}, LoadRateLimits(path))

	// Non-positive entries are dropped so callers can treat every value as active.
	require.NoError(t, os.WriteFile(path, []byte(`{"*":75,"stale":0}`), 0o600))
	assert.Equal(t, map[string]int{"*": 75}, LoadRateLimits(path))

	// Malformed content yields an empty map, not an error.
	require.NoError(t, os.WriteFile(path, []byte("garbage"), 0o600))
	assert.Empty(t, LoadRateLimits(path))
}
