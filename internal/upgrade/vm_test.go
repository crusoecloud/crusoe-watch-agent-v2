package upgrade

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// statePath returns a path inside a fresh temp dir, plus the dir itself.
func statePath(t *testing.T) string {
	t.Helper()

	return filepath.Join(t.TempDir(), "upgrade-request.json")
}

func TestSingleAgentCounter(t *testing.T) {
	ready, err := SingleAgentCounter{}.ReadyCount(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, ready)
}

func TestFileStoreLoadNoFile(t *testing.T) {
	_, err := NewFileStore(statePath(t)).Load(context.Background())
	assert.ErrorIs(t, err, ErrNoState)
}

// A zero-length file is indistinguishable from nothing recorded, and reporting
// it as corrupt would need a DELETE /status to clear a store that is empty.
func TestFileStoreLoadEmptyFile(t *testing.T) {
	path := statePath(t)
	require.NoError(t, os.WriteFile(path, nil, stateFileMode))

	_, err := NewFileStore(path).Load(context.Background())
	assert.ErrorIs(t, err, ErrNoState)
}

func TestFileStoreLoadCorrupt(t *testing.T) {
	path := statePath(t)
	require.NoError(t, os.WriteFile(path, []byte("{not json"), stateFileMode))

	_, err := NewFileStore(path).Load(context.Background())
	assert.ErrorIs(t, err, ErrCorruptState)
}

// A record this build wrote must come back identical, or a restart mid-upgrade
// would resume against different state than it persisted.
func TestFileStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(statePath(t))

	saved := &State{
		Phase:           PhaseInProgress,
		TargetVersion:   "v2.1",
		RollbackVersion: "v2.0",
		ArtifactURL:     "https://example.invalid/cwa-native-amd64.tar.gz",
		Checksum:        "sha256:abc123",
		AgentExecutions: map[string]string{"agent-1": "cmd-abc"},
		ExpectedCount:   1,
		CollectDeadline: time.Now().UTC().Add(time.Minute).Truncate(time.Second),
		RequestedAt:     time.Now().UTC().Truncate(time.Second),
		Result: &Result{
			Status:      ResultRolledBack,
			FromVersion: "v2.0",
			ToVersion:   "v2.1",
			Reason:      "health check never passed",
			CompletedAt: time.Now().UTC().Truncate(time.Second),
		},
	}

	require.NoError(t, store.Save(ctx, saved))

	loaded, err := store.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, saved, loaded)
}

// The store owns its directory, rather than relying on the installer to have
// created it before the first handoff arrives.
func TestFileStoreSaveCreatesDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crusoe", "upgrade-request.json")
	store := NewFileStore(path)

	require.NoError(t, store.Save(context.Background(), &State{Phase: PhasePending}))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(stateFileMode), info.Mode().Perm())
}

// Save renames over the target, so nothing may be left beside it holding a copy
// of the record.
func TestFileStoreSaveLeavesNoStagingFile(t *testing.T) {
	path := statePath(t)
	require.NoError(t, NewFileStore(path).Save(context.Background(), &State{Phase: PhasePending}))

	staged, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".tmp-*"))
	require.NoError(t, err)
	assert.Empty(t, staged)
}

func TestFileStoreSaveOverwrites(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(statePath(t))

	require.NoError(t, store.Save(ctx, &State{Phase: PhasePending, TargetVersion: "v2.1"}))
	require.NoError(t, store.Save(ctx, &State{Phase: PhaseComplete, TargetVersion: "v2.2"}))

	loaded, err := store.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, PhaseComplete, loaded.Phase)
	assert.Equal(t, "v2.2", loaded.TargetVersion)
}

// Service reads AgentExecutions without a nil check, so a record written before
// any handoff was collected must still load with a usable map.
func TestFileStoreLoadFillsAgentExecutions(t *testing.T) {
	path := statePath(t)
	data, err := json.Marshal(&State{Phase: PhasePending})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, stateFileMode))

	loaded, err := NewFileStore(path).Load(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, loaded.AgentExecutions)
}

func TestFileStoreClear(t *testing.T) {
	ctx := context.Background()
	path := statePath(t)
	store := NewFileStore(path)

	require.NoError(t, store.Save(ctx, &State{Phase: PhaseComplete}))
	require.NoError(t, store.Clear(ctx))

	_, err := store.Load(ctx)
	assert.ErrorIs(t, err, ErrNoState)
}

// ClearStatus can be called again for a result already acknowledged.
func TestFileStoreClearMissing(t *testing.T) {
	assert.NoError(t, NewFileStore(statePath(t)).Clear(context.Background()))
}

// The whole recovery path has to work over the file store, not just the
// ConfigMap one: a terminal result must survive a restart and stay readable
// until it is acknowledged.
func TestFileStoreRecoversTerminalResult(t *testing.T) {
	ctx := context.Background()
	path := statePath(t)

	require.NoError(t, NewFileStore(path).Save(ctx, &State{
		Phase:           PhaseRolledBack,
		TargetVersion:   "v2.1",
		RollbackVersion: "v2.0",
		AgentExecutions: map[string]string{"agent-1": "cmd-abc"},
		Result:          &Result{Status: ResultRolledBack, FromVersion: "v2.0", ToVersion: "v2.1"},
	}))

	svc := New(Config{
		Store:   NewFileStore(path),
		Counter: SingleAgentCounter{},
		Logger:  discardLogger(),
	})
	require.NoError(t, svc.Recover(ctx))

	assert.Equal(t, StatusRolledBack, svc.HealthStatus())

	view, err := svc.Status()
	require.NoError(t, err)
	assert.Equal(t, "cmd-abc", view.AgentExecutions["agent-1"])

	require.NoError(t, svc.ClearStatus(ctx))
	assert.Equal(t, StatusIdle, svc.HealthStatus())
}
