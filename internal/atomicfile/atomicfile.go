// Package atomicfile writes files in a way that survives a crash mid-write.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// Write durably writes data to path via temp file + rename, so neither a reader
// nor a crash sees a partial file. The parent directory must already exist.
func Write(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}

	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("writing temp file: %w", err)
	}

	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("chmod temp file: %w", err)
	}

	// fsync the contents before the rename so the renamed file is never empty.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("syncing temp file: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming temp file: %w", err)
	}

	return fsyncDir(dir)
}

// fsyncDir flushes the rename above to disk, so it survives a power loss.
func fsyncDir(dir string) error {
	dirFile, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("opening dir for fsync: %w", err)
	}
	defer func() { _ = dirFile.Close() }()

	if err := dirFile.Sync(); err != nil {
		return fmt.Errorf("syncing dir: %w", err)
	}

	return nil
}
