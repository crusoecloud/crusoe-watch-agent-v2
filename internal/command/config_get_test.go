package command

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigGet_ReturnsRenderedConfig(t *testing.T) {
	deps := testDeps(t)
	want := "sources:\n  in: {}\nsinks:\n  out: {}\n"
	require.NoError(t, os.WriteFile(deps.VMConfigPath, []byte(want), 0o600))

	got, err := NewConfigGet(deps).Run(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestConfigGet_ReflectsApply(t *testing.T) {
	deps := testDeps(t)

	// A real config.apply renders the Vector config to VMConfigPath; config.get
	// must return that rendered config with the applied endpoint folded in.
	require.NoError(t, runErr(t, NewConfigApply(deps), map[string]string{
		ParamIngestionEndpoint: "https://cms.example.com",
	}))

	got, err := NewConfigGet(deps).Run(context.Background(), nil)
	require.NoError(t, err)
	assert.Contains(t, got, "cms.example.com", "returned config must be the live rendered pipeline")
}

func TestConfigGet_MissingConfigFails(t *testing.T) {
	deps := testDeps(t) // VMConfigPath points at a temp file that was never written

	_, err := NewConfigGet(deps).Run(context.Background(), nil)
	require.Error(t, err)
}

func TestConfigGet_Timeout(t *testing.T) {
	assert.Equal(t, Instant, NewConfigGet(Deps{}).Timeout())
}
