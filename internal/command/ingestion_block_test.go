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

func TestIngestionBlock_VM(t *testing.T) {
	deps := testDeps(t)
	h := NewIngestionBlock(deps, true)

	require.NoError(t, h.Run(context.Background(), nil))

	// Only the local internal-metrics exporter survives a block, and the
	// marker persists the state across restarts.
	sinks := vmSinks(t, deps.VMConfigPath)
	assert.Len(t, sinks, 1)
	assert.Contains(t, sinks, "internal_metrics_exporter")
	assert.True(t, LoadIngestionBlocked(deps.BlockedStatePath))
}

func TestIngestionUnblock_VM(t *testing.T) {
	deps := testDeps(t)

	// Start blocked, with endpoints persisted by an earlier config.apply.
	require.NoError(t, os.WriteFile(deps.BlockedStatePath, []byte("blocked\n"), 0o600))
	require.NoError(t, os.WriteFile(deps.LogsStatePath, []byte("https://logs.example.com\n"), 0o600))
	require.NoError(t, os.WriteFile(deps.MetricsStatePath, []byte("https://metrics.example.com\n"), 0o600))

	h := NewIngestionBlock(deps, false)

	require.NoError(t, h.Run(context.Background(), nil))

	// Sinks are restored, still carrying the persisted endpoint overrides,
	// and the blocked marker is gone.
	sinks := vmSinks(t, deps.VMConfigPath)
	assert.Equal(t, "https://logs.example.com/logs/ingest", sinks["crusoe_ingest"].(map[string]any)["uri"])
	assert.Equal(t, "https://metrics.example.com/ingest", sinks["cms_gateway"].(map[string]any)["endpoint"])
	assert.False(t, LoadIngestionBlocked(deps.BlockedStatePath))
}

func TestIngestionUnblock_VM_NeverBlocked(t *testing.T) {
	// Unblock with no marker present must succeed (already unblocked).
	deps := testDeps(t)
	h := NewIngestionBlock(deps, false)

	require.NoError(t, h.Run(context.Background(), nil))

	sinks := vmSinks(t, deps.VMConfigPath)
	assert.Contains(t, sinks, "crusoe_ingest")
}

func TestIngestionBlock_K8s(t *testing.T) {
	for _, blocked := range []bool{true, false} {
		rel := &fakeReloader{blocked: !blocked}

		h := NewIngestionBlock(Deps{
			InstallType:      pb.CwaInstallType_CWA_INSTALL_TYPE_KUBERNETES,
			Watcher:          rel,
			BlockedStatePath: filepath.Join(t.TempDir(), ".ingestion-blocked"),
		}, blocked)

		require.NoError(t, h.Run(context.Background(), nil))
		assert.True(t, rel.blockedSet)
		assert.Equal(t, blocked, rel.blocked)
	}
}

func TestIngestionBlock_K8s_NoWatcher(t *testing.T) {
	h := NewIngestionBlock(Deps{
		InstallType: pb.CwaInstallType_CWA_INSTALL_TYPE_KUBERNETES,
	}, true)

	assert.Error(t, h.Run(context.Background(), nil))
}

func TestIngestionBlock_UnsupportedInstallType(t *testing.T) {
	h := NewIngestionBlock(Deps{
		InstallType: pb.CwaInstallType_CWA_INSTALL_TYPE_UNSPECIFIED,
	}, true)

	assert.Error(t, h.Run(context.Background(), nil))
}

func TestIngestionBlock_Timeout(t *testing.T) {
	assert.Equal(t, Instant, NewIngestionBlock(Deps{}, true).Timeout())
}

func TestLoadIngestionBlocked_EmptyPath(t *testing.T) {
	assert.False(t, LoadIngestionBlocked(""))
}
