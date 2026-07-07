package command

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	pb "gitlab.com/crusoeenergy/schemas/api/island/v2/observability"
)

type fakeReloader struct {
	logs    string
	metrics string
}

func (f *fakeReloader) SetIngestionEndpoints(logs, metrics string) {
	f.logs = logs
	f.metrics = metrics
}

func vmSinks(t *testing.T, path string) map[string]any {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var cfg map[string]any
	require.NoError(t, yaml.Unmarshal(data, &cfg))

	sinks, ok := cfg["sinks"].(map[string]any)
	require.True(t, ok, "config should have sinks")

	return sinks
}

// testDeps returns VM deps with config and state paths in a temp dir.
func testDeps(t *testing.T) ConfigApplyDeps {
	t.Helper()
	dir := t.TempDir()

	return ConfigApplyDeps{
		InstallType:      pb.CwaInstallType_CWA_INSTALL_TYPE_DOCKER,
		VMConfigPath:     filepath.Join(dir, "vector.yaml"),
		LogsStatePath:    filepath.Join(dir, ".logs-endpoint"),
		MetricsStatePath: filepath.Join(dir, ".metrics-endpoint"),
	}
}

func TestConfigApply_VM_BaseEndpoint(t *testing.T) {
	deps := testDeps(t)
	h := NewConfigApply(deps)

	err := h.Run(context.Background(), map[string]string{
		ParamIngestionEndpoint: "https://cms.example.com",
	})
	require.NoError(t, err)

	// Both sink endpoints are derived from the base, matching the installer's
	// cms_url derivation, and written as literals (replacing the ${...} placeholders).
	sinks := vmSinks(t, deps.VMConfigPath)
	assert.Equal(t, "https://cms.example.com/logs/ingest", sinks["crusoe_ingest"].(map[string]any)["uri"])
	assert.Equal(t, "https://cms.example.com/ingest", sinks["cms_gateway"].(map[string]any)["endpoint"])

	// Both endpoints are persisted for restart.
	assert.Equal(t, "https://cms.example.com", LoadEndpoint(deps.LogsStatePath))
	assert.Equal(t, "https://cms.example.com", LoadEndpoint(deps.MetricsStatePath))
}

func TestConfigApply_VM_SeparateEndpoints(t *testing.T) {
	deps := testDeps(t)
	h := NewConfigApply(deps)

	err := h.Run(context.Background(), map[string]string{
		ParamLogsEndpoint:    "https://logs.example.com",
		ParamMetricsEndpoint: "https://metrics.example.com",
	})
	require.NoError(t, err)

	sinks := vmSinks(t, deps.VMConfigPath)
	assert.Equal(t, "https://logs.example.com/logs/ingest", sinks["crusoe_ingest"].(map[string]any)["uri"])
	assert.Equal(t, "https://metrics.example.com/ingest", sinks["cms_gateway"].(map[string]any)["endpoint"])

	assert.Equal(t, "https://logs.example.com", LoadEndpoint(deps.LogsStatePath))
	assert.Equal(t, "https://metrics.example.com", LoadEndpoint(deps.MetricsStatePath))
}

func TestConfigApply_VM_SpecificWinsOverBase(t *testing.T) {
	deps := testDeps(t)
	h := NewConfigApply(deps)

	err := h.Run(context.Background(), map[string]string{
		ParamIngestionEndpoint: "https://cms.example.com",
		ParamLogsEndpoint:      "https://logs.example.com",
	})
	require.NoError(t, err)

	sinks := vmSinks(t, deps.VMConfigPath)
	assert.Equal(t, "https://logs.example.com/logs/ingest", sinks["crusoe_ingest"].(map[string]any)["uri"])
	assert.Equal(t, "https://cms.example.com/ingest", sinks["cms_gateway"].(map[string]any)["endpoint"])
}

func TestConfigApply_VM_PartialUpdateKeepsOtherEndpoint(t *testing.T) {
	deps := testDeps(t)

	// An earlier apply set the metrics endpoint.
	require.NoError(t, os.WriteFile(deps.MetricsStatePath, []byte("https://metrics.example.com\n"), 0o600))

	h := NewConfigApply(deps)

	// This apply only updates logs; metrics must keep its last-applied value.
	err := h.Run(context.Background(), map[string]string{
		ParamLogsEndpoint: "https://logs.example.com",
	})
	require.NoError(t, err)

	sinks := vmSinks(t, deps.VMConfigPath)
	assert.Equal(t, "https://logs.example.com/logs/ingest", sinks["crusoe_ingest"].(map[string]any)["uri"])
	assert.Equal(t, "https://metrics.example.com/ingest", sinks["cms_gateway"].(map[string]any)["endpoint"])
	assert.Equal(t, "https://metrics.example.com", LoadEndpoint(deps.MetricsStatePath))
}

func TestConfigApply_MissingEndpointFails(t *testing.T) {
	h := NewConfigApply(ConfigApplyDeps{
		InstallType: pb.CwaInstallType_CWA_INSTALL_TYPE_DOCKER,
	})

	err := h.Run(context.Background(), map[string]string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), ParamIngestionEndpoint)
}

func TestConfigApply_VM_WriteFailure(t *testing.T) {
	// A config path whose parent is a file (not a directory) can't be written.
	dir := t.TempDir()
	notADir := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(notADir, []byte("x"), 0o600))

	h := NewConfigApply(ConfigApplyDeps{
		InstallType:  pb.CwaInstallType_CWA_INSTALL_TYPE_DOCKER,
		VMConfigPath: filepath.Join(notADir, "vector.yaml"),
	})

	err := h.Run(context.Background(), map[string]string{
		ParamIngestionEndpoint: "https://cms.example.com",
	})
	assert.Error(t, err)
}

func TestConfigApply_K8s(t *testing.T) {
	rel := &fakeReloader{}

	h := NewConfigApply(ConfigApplyDeps{
		InstallType: pb.CwaInstallType_CWA_INSTALL_TYPE_KUBERNETES,
		Watcher:     rel,
	})

	err := h.Run(context.Background(), map[string]string{
		ParamIngestionEndpoint: "https://cms.example.com",
		ParamMetricsEndpoint:   "https://metrics.example.com",
	})
	require.NoError(t, err)
	assert.Equal(t, "https://cms.example.com", rel.logs)
	assert.Equal(t, "https://metrics.example.com", rel.metrics)
}

func TestConfigApply_K8s_NoWatcher(t *testing.T) {
	h := NewConfigApply(ConfigApplyDeps{
		InstallType: pb.CwaInstallType_CWA_INSTALL_TYPE_KUBERNETES,
	})

	err := h.Run(context.Background(), map[string]string{
		ParamIngestionEndpoint: "https://cms.example.com",
	})
	assert.Error(t, err)
}

func TestConfigApply_UnsupportedInstallType(t *testing.T) {
	h := NewConfigApply(ConfigApplyDeps{
		InstallType: pb.CwaInstallType_CWA_INSTALL_TYPE_UNSPECIFIED,
	})

	err := h.Run(context.Background(), map[string]string{
		ParamIngestionEndpoint: "https://cms.example.com",
	})
	assert.Error(t, err)
}

func TestConfigApply_Timeout(t *testing.T) {
	h := NewConfigApply(ConfigApplyDeps{})
	assert.Equal(t, Instant, h.Timeout())
}

func TestLoadEndpoint_MissingFile(t *testing.T) {
	assert.Empty(t, LoadEndpoint(filepath.Join(t.TempDir(), "absent")))
}
