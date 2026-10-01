package upgrade

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fullConfig sets every field, so a round trip proves nothing is dropped.
func fullConfig() RuntimeConfig {
	return RuntimeConfig{
		DownloadURLBase:       "https://mirror.internal/releases",
		RollbackTimeoutMin:    20,
		DownloadBackoffMaxSec: 120,
	}
}

func TestRuntimeConfigAcceptsAnEmptyConfig(t *testing.T) {
	t.Parallel()

	// Every field is optional: an empty config means "leave the defaults alone".
	var cfg RuntimeConfig

	require.NoError(t, cfg.Validate())
	ack := NewConfigAck(cfg, ScriptExecutorFields())
	assert.Empty(t, ack.Applied)
	assert.Empty(t, ack.Unsupported)
	assert.Zero(t, cfg.RollbackTimeout())
}

func TestRuntimeConfigRejectsValuesThatWouldBreakAnUpgrade(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  RuntimeConfig
		want string
	}{
		{"unparseable url", RuntimeConfig{DownloadURLBase: "://nope"}, "download_url_base"},
		{"plaintext mirror", RuntimeConfig{DownloadURLBase: "http://mirror.internal"}, "must be https"},
		{"url with no host", RuntimeConfig{DownloadURLBase: "https:///releases"}, "no host"},
		{"window below the floor", RuntimeConfig{RollbackTimeoutMin: 4}, "rollback_timeout_min"},
		{"window above the ceiling", RuntimeConfig{RollbackTimeoutMin: 121}, "rollback_timeout_min"},
		{"negative backoff", RuntimeConfig{DownloadBackoffMaxSec: -1}, "download_backoff_max_sec"},
		{"backoff above the ceiling", RuntimeConfig{DownloadBackoffMaxSec: 3601}, "download_backoff_max_sec"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.cfg.Validate()
			require.ErrorIs(t, err, ErrInvalidConfig)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// Which fields count as applied is the executor's answer, not the config's.
func TestConfigAckSplitsFieldsByWhatTheExecutorReads(t *testing.T) {
	t.Parallel()

	cfg := fullConfig()
	require.NoError(t, cfg.Validate())
	assert.Equal(t, 20*time.Minute, cfg.RollbackTimeout())

	for _, tc := range []struct {
		name                 string
		honored              []string
		applied, unsupported []string
	}{
		{
			name:        "vm",
			honored:     ScriptExecutorFields(),
			applied:     []string{"download_url_base", "rollback_timeout_min", "download_backoff_max_sec"},
			unsupported: nil,
		},
		{
			name:    "kubernetes",
			honored: HelmExecutorFields(),
			applied: []string{"rollback_timeout_min", "download_backoff_max_sec"},
			// helm pulls the chart from ChartRepo, so a mirror URL changes nothing.
			unsupported: []string{"download_url_base"},
		},
		{
			name:        "nothing wired",
			honored:     nil,
			applied:     nil,
			unsupported: []string{"download_url_base", "rollback_timeout_min", "download_backoff_max_sec"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ack := NewConfigAck(cfg, tc.honored)
			assert.Equal(t, tc.applied, ack.Applied)
			assert.Equal(t, tc.unsupported, ack.Unsupported)
		})
	}
}

func TestFileConfigStoreReadsBackWhatItWrote(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "updater-config.json")
	store := NewFileConfigStore(path)

	require.NoError(t, store.Save(context.Background(), fullConfig()))

	loaded, err := store.Load(context.Background())
	require.NoError(t, err)
	// Including the inert fields: a later build must find what was sent.
	assert.Equal(t, fullConfig(), loaded)

	// It names what gets installed, so it must not be world-readable.
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestFileConfigStoreReadsAnAbsentFileAsDefaults(t *testing.T) {
	t.Parallel()

	store := NewFileConfigStore(filepath.Join(t.TempDir(), "absent.json"))

	cfg, err := store.Load(context.Background())
	require.NoError(t, err)
	assert.Equal(t, RuntimeConfig{}, cfg)
}

func TestFileConfigStoreRejectsAnUnparseableFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "updater-config.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	_, err := NewFileConfigStore(path).Load(context.Background())
	require.ErrorIs(t, err, ErrInvalidConfig)
}

func TestHolderAppliesAndPersists(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "updater-config.json")
	holder := NewHolder(NewFileConfigStore(path), ScriptExecutorFields())

	require.NoError(t, holder.Apply(context.Background(), fullConfig()))
	assert.Equal(t, fullConfig(), holder.Get())

	// A restart reads it back, so a delivery outlives the process.
	restarted := NewHolder(NewFileConfigStore(path), ScriptExecutorFields())
	require.NoError(t, restarted.Load(context.Background()))
	assert.Equal(t, fullConfig(), restarted.Get())
}

func TestHolderKeepsTheRunningConfigWhenAnApplyIsRejected(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "updater-config.json")
	holder := NewHolder(NewFileConfigStore(path), ScriptExecutorFields())
	require.NoError(t, holder.Apply(context.Background(), RuntimeConfig{RollbackTimeoutMin: 20}))

	err := holder.Apply(context.Background(), RuntimeConfig{RollbackTimeoutMin: 999})
	require.ErrorIs(t, err, ErrInvalidConfig)

	// Rejected before persisting, so neither the live config nor the file moved.
	assert.Equal(t, 20, holder.Get().RollbackTimeoutMin)

	reloaded := NewHolder(NewFileConfigStore(path), ScriptExecutorFields())
	require.NoError(t, reloaded.Load(context.Background()))
	assert.Equal(t, 20, reloaded.Get().RollbackTimeoutMin)
}

func TestHolderKeepsTheRunningConfigWhenAHandEditedFileIsInvalid(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "updater-config.json")
	holder := NewHolder(NewFileConfigStore(path), ScriptExecutorFields())
	require.NoError(t, holder.Apply(context.Background(), RuntimeConfig{RollbackTimeoutMin: 20}))

	// What Load finds after a bad hand-edit.
	require.NoError(t, os.WriteFile(path, []byte(`{"rollback_timeout_min":999}`), 0o600))

	require.ErrorIs(t, holder.Load(context.Background()), ErrInvalidConfig)
	assert.Equal(t, 20, holder.Get().RollbackTimeoutMin)
}

func TestNilHolderReadsAsDefaults(t *testing.T) {
	t.Parallel()

	// Executors built without configure-updater wiring hold a nil Holder.
	var holder *Holder

	assert.Equal(t, RuntimeConfig{}, holder.Get())
	assert.Zero(t, holder.Get().RollbackTimeout())
}

func TestNewConfigAckDescribesWhatWentLive(t *testing.T) {
	t.Parallel()

	ack := NewConfigAck(fullConfig(), ScriptExecutorFields())

	assert.Equal(t, fullConfig(), ack.Config)
	assert.Equal(t, []string{"download_url_base", "rollback_timeout_min", "download_backoff_max_sec"},
		ack.Applied)
	assert.Empty(t, ack.Unsupported)
}

// A Kubernetes ack must not claim download_url_base applied.
func TestHolderAcksAgainstTheFieldsItHonours(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "updater-config.json")
	holder := NewHolder(NewFileConfigStore(path), HelmExecutorFields())
	require.NoError(t, holder.Apply(context.Background(), fullConfig()))

	ack := holder.Ack()

	assert.Equal(t, fullConfig(), ack.Config)
	assert.Equal(t, []string{"rollback_timeout_min", "download_backoff_max_sec"}, ack.Applied)
	assert.Contains(t, ack.Unsupported, "download_url_base")
}

// errConfigStore fails every write, standing in for a read-only or full disk.
type errConfigStore struct{ err error }

func (s errConfigStore) Load(context.Context) (RuntimeConfig, error) {
	return RuntimeConfig{}, s.err
}
func (s errConfigStore) Save(context.Context, RuntimeConfig) error { return s.err }

func TestHolderDoesNotGoLiveWhenThePersistFails(t *testing.T) {
	t.Parallel()

	failed := errors.New("read-only file system")
	holder := NewHolder(errConfigStore{err: failed}, ScriptExecutorFields())

	// Going live on an unpersisted config would silently revert on restart.
	require.ErrorIs(t, holder.Apply(context.Background(), fullConfig()), failed)
	assert.Equal(t, RuntimeConfig{}, holder.Get())
}
