package atomicfile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// staging returns the staging files left in path's directory.
func staging(t *testing.T, path string) []string {
	t.Helper()

	left, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".tmp-*"))
	require.NoError(t, err)

	return left
}

func TestWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record.json")

	require.NoError(t, Write(path, []byte(`{"phase":"pending"}`), 0o600))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, `{"phase":"pending"}`, string(got))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	assert.Empty(t, staging(t, path))
}

// The mode is the caller's, not a constant baked into the write.
func TestWriteHonoursPerm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record.json")
	require.NoError(t, Write(path, []byte("x"), 0o640))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())
}

// The rename replaces the record rather than appending to or truncating it.
func TestWriteReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record.json")

	require.NoError(t, Write(path, []byte("a much longer first record"), 0o600))
	require.NoError(t, Write(path, []byte("short"), 0o600))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "short", string(got))
	assert.Empty(t, staging(t, path))
}

// A missing parent directory is an error, not a silently skipped write.
func TestWriteNeedsItsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "record.json")

	require.Error(t, Write(path, []byte("x"), 0o600))

	_, err := os.Stat(path)
	assert.ErrorIs(t, err, os.ErrNotExist)
}
