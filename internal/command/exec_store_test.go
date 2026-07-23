package command

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "gitlab.com/crusoeenergy/schemas/api/island/v2/observability"
)

func TestExecStore_BeginListComplete(t *testing.T) {
	s := NewExecStore(filepath.Join(t.TempDir(), ".commands"))

	require.NoError(t, s.Begin("exec-1", "report.bug"))
	require.NoError(t, s.Begin("exec-2", "config.apply"))

	records, err := s.List()
	require.NoError(t, err)
	require.Len(t, records, 2)

	byID := map[string]ExecRecord{}
	for _, r := range records {
		byID[r.ExecutionID] = r
	}
	assert.Equal(t, "report.bug", byID["exec-1"].Command)
	assert.Equal(t, ExecStatusInProgress, byID["exec-1"].Status)
	assert.False(t, byID["exec-1"].StartedAt.IsZero(), "started_at must be recorded")

	// Completing one leaves the other.
	require.NoError(t, s.Complete("exec-1"))

	records, err = s.List()
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, "exec-2", records[0].ExecutionID)
}

func TestExecStore_ListMissingDirIsEmpty(t *testing.T) {
	s := NewExecStore(filepath.Join(t.TempDir(), "never-created"))

	records, err := s.List()
	require.NoError(t, err)
	assert.Empty(t, records)
}

func TestExecStore_CompleteMissingIsNoError(t *testing.T) {
	s := NewExecStore(filepath.Join(t.TempDir(), ".commands"))
	assert.NoError(t, s.Complete("never-began"))
}

func TestExecStore_ArbitraryExecIDFilename(t *testing.T) {
	s := NewExecStore(filepath.Join(t.TempDir(), ".commands"))

	// An execution ID with path-hostile characters must still round-trip.
	const id = "a/b c:d\\e"
	require.NoError(t, s.Begin(id, "report.bug"))

	records, err := s.List()
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, id, records[0].ExecutionID)

	require.NoError(t, s.Complete(id))
	records, err = s.List()
	require.NoError(t, err)
	assert.Empty(t, records)
}

func TestRecoverInterrupted_DeliversAndClears(t *testing.T) {
	s := NewExecStore(filepath.Join(t.TempDir(), ".commands"))
	require.NoError(t, s.Begin("exec-1", "report.bug"))
	require.NoError(t, s.Begin("exec-2", "config.apply"))

	sink := newFakeSink()
	RecoverInterrupted(s, sink, discardLogger())

	r1 := sink.get("exec-1")
	require.NotNil(t, r1)
	assert.Equal(t, pb.CwaCommandResultStatus_CWA_COMMAND_RESULT_STATUS_INTERRUPTED, r1.GetStatus())
	assert.Equal(t, "report.bug", r1.GetCommand())
	assert.NotNil(t, r1.GetCompletedAt())

	require.NotNil(t, sink.get("exec-2"))

	// Recovered records are cleared so they are not re-reported on the next start.
	records, err := s.List()
	require.NoError(t, err)
	assert.Empty(t, records)
}

func TestRecoverInterrupted_NilStoreIsNoOp(t *testing.T) {
	sink := newFakeSink()
	assert.NotPanics(t, func() { RecoverInterrupted(nil, sink, discardLogger()) })
}

func TestRecoverInterrupted_ListErrorDeliversNothing(t *testing.T) {
	// A file where the store dir is expected makes List fail with a non-NotExist
	// error; recovery must log and return without delivering or panicking.
	dir := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(dir, []byte("x"), 0o600))

	s := NewExecStore(dir)
	sink := newFakeSink()

	assert.NotPanics(t, func() { RecoverInterrupted(s, sink, discardLogger()) })
	assert.Nil(t, sink.get("anything"))
}

// Begin creates the store dir lazily on first write, with the expected perms.
func TestExecStore_BeginCreatesDirLazily(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".commands")
	s := NewExecStore(dir)

	_, statErr := os.Stat(dir)
	require.True(t, os.IsNotExist(statErr), "dir must not exist before first Begin")

	require.NoError(t, s.Begin("exec-1", "report.bug"))

	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Equal(t, os.FileMode(execStoreDirPerm), info.Mode().Perm())
}

// A second Begin for the same execution ID overwrites the prior record rather
// than creating a duplicate.
func TestExecStore_BeginOverwritesSameID(t *testing.T) {
	s := NewExecStore(filepath.Join(t.TempDir(), ".commands"))

	require.NoError(t, s.Begin("exec-1", "report.bug"))
	require.NoError(t, s.Begin("exec-1", "config.apply"))

	records, err := s.List()
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, "config.apply", records[0].Command)
}

// The record file is written with the restrictive state-file perm, and no temp
// file is left behind by the atomic write.
func TestExecStore_RecordPermsNoTempLeftover(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".commands")
	s := NewExecStore(dir)

	require.NoError(t, s.Begin("exec-1", "report.bug"))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "atomic write must leave no temp file behind")

	info, err := entries[0].Info()
	require.NoError(t, err)
	assert.Equal(t, filepath.Ext(entries[0].Name()), ".json")
	assert.Equal(t, os.FileMode(stateFilePerm), info.Mode().Perm())
}

// List ignores subdirectories and non-.json files, returning only real records.
func TestExecStore_ListIgnoresNonRecords(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".commands")
	s := NewExecStore(dir)
	require.NoError(t, s.Begin("exec-1", "report.bug"))

	require.NoError(t, os.Mkdir(filepath.Join(dir, "subdir"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignore me"), 0o600))

	records, err := s.List()
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, "exec-1", records[0].ExecutionID)
}

// A single corrupt record file must not block the surviving valid records.
func TestExecStore_ListSkipsCorruptFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".commands")
	s := NewExecStore(dir)
	require.NoError(t, s.Begin("exec-1", "report.bug"))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600))

	records, err := s.List()
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, "exec-1", records[0].ExecutionID)
}

// Records persist on disk so a fresh store over the same dir sees them — the
// crash-recovery contract across an agent restart.
func TestExecStore_RecordsPersistAcrossInstances(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".commands")
	require.NoError(t, NewExecStore(dir).Begin("exec-1", "report.bug"))

	records, err := NewExecStore(dir).List()
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, "report.bug", records[0].Command)
	assert.Equal(t, ExecStatusInProgress, records[0].Status)
}
