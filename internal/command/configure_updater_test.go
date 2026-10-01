package command

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/upgrade"
)

// fakeConfigurer records what cwa-updater was handed and answers as it would.
type fakeConfigurer struct {
	got   upgrade.RuntimeConfig
	calls int
	err   error
}

func (f *fakeConfigurer) Configure(
	_ context.Context, cfg upgrade.RuntimeConfig,
) (upgrade.ConfigAck, error) {
	f.calls++
	f.got = cfg

	if f.err != nil {
		return upgrade.ConfigAck{}, f.err
	}

	return upgrade.NewConfigAck(cfg, upgrade.ScriptExecutorFields()), nil
}

func TestConfigureUpdaterIsInstant(t *testing.T) {
	t.Parallel()

	assert.Equal(t, Instant, NewConfigureUpdater(&fakeConfigurer{}).Timeout())
}

func TestConfigureUpdaterDeliversEveryParameter(t *testing.T) {
	t.Parallel()

	client := &fakeConfigurer{}

	result, err := NewConfigureUpdater(client).Run(context.Background(), map[string]string{
		ParamDownloadURLBase:       "https://mirror.internal/releases",
		ParamRollbackTimeoutMin:    "20",
		ParamDownloadBackoffMaxSec: "120",
	})
	require.NoError(t, err)

	assert.Equal(t, upgrade.RuntimeConfig{
		DownloadURLBase:       "https://mirror.internal/releases",
		RollbackTimeoutMin:    20,
		DownloadBackoffMaxSec: 120,
	}, client.got)

	// A stored-but-inert setting is never reported as a change that took effect.
	var ack upgrade.ConfigAck
	require.NoError(t, json.Unmarshal([]byte(result), &ack))
	assert.Equal(t, []string{"download_url_base", "rollback_timeout_min", "download_backoff_max_sec"},
		ack.Applied)
	assert.Empty(t, ack.Unsupported)
}

// The delivery carries the whole configuration, so an omitted parameter is a
// default rather than a value left alone.
func TestConfigureUpdaterTreatsOmittedParametersAsUnset(t *testing.T) {
	t.Parallel()

	client := &fakeConfigurer{}

	_, err := NewConfigureUpdater(client).Run(context.Background(), map[string]string{
		ParamRollbackTimeoutMin: "20",
	})
	require.NoError(t, err)

	assert.Equal(t, upgrade.RuntimeConfig{RollbackTimeoutMin: 20}, client.got)
	assert.Equal(t, 20*time.Minute, client.got.RollbackTimeout())
}

func TestConfigureUpdaterAcceptsNoParameters(t *testing.T) {
	t.Parallel()

	client := &fakeConfigurer{}

	// How the control plane restores every default.
	_, err := NewConfigureUpdater(client).Run(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, upgrade.RuntimeConfig{}, client.got)
}

func TestConfigureUpdaterRejectsANonNumericParameter(t *testing.T) {
	t.Parallel()

	client := &fakeConfigurer{}

	for _, key := range []string{ParamRollbackTimeoutMin, ParamDownloadBackoffMaxSec} {
		_, err := NewConfigureUpdater(client).Run(context.Background(), map[string]string{key: "soon"})

		require.ErrorIs(t, err, upgrade.ErrInvalidConfig)
		assert.Contains(t, err.Error(), key)
	}

	// Refused before delivery: cwa-updater is never handed a partial config.
	assert.Equal(t, 0, client.calls)
}

func TestConfigureUpdaterReportsAnUnreachableUpdater(t *testing.T) {
	t.Parallel()

	client := &fakeConfigurer{err: upgrade.ErrUpdaterUnavailable}

	_, err := NewConfigureUpdater(client).Run(context.Background(), map[string]string{
		ParamRollbackTimeoutMin: "20",
	})

	// FAILED rather than SUCCEEDED: nothing confirmed the reload.
	require.ErrorIs(t, err, upgrade.ErrUpdaterUnavailable)
}

func TestConfigureUpdaterReportsARefusedConfig(t *testing.T) {
	t.Parallel()

	client := &fakeConfigurer{err: upgrade.ErrInvalidRequest}

	_, err := NewConfigureUpdater(client).Run(context.Background(), map[string]string{
		ParamRollbackTimeoutMin: "20",
	})

	require.ErrorIs(t, err, upgrade.ErrInvalidRequest)
}
