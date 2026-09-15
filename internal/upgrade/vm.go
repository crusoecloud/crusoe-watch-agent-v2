package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/atomicfile"
)

// The record decides what gets installed on the host, so only root may read or write it.
const (
	stateFileMode = 0o600
	stateDirMode  = 0o700
)

// SingleAgentCounter reports one ready agent, which is the VM case: exactly one
// cwa-manager runs per host, so the collection window closes on its handoff.
type SingleAgentCounter struct{}

// ReadyCount always reports one.
func (SingleAgentCounter) ReadyCount(context.Context) (int, error) {
	return 1, nil
}

// FileStore persists upgrade state in a local file, the VM stand-in for the
// handoff ConfigMap. cwa-updater is the only writer on the host.
type FileStore struct {
	path string
}

// NewFileStore returns a store backed by the file at path.
func NewFileStore(path string) *FileStore {
	return &FileStore{path: path}
}

// Load reads the persisted state, returning ErrNoState when none is recorded.
func (s *FileStore) Load(context.Context) (*State, error) {
	data, err := os.ReadFile(s.path)

	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, ErrNoState
	case err != nil:
		return nil, fmt.Errorf("reading upgrade state %s: %w", s.path, err)
	case len(data) == 0:
		return nil, ErrNoState
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("%w: parsing %s: %w", ErrCorruptState, s.path, err)
	}

	if state.AgentExecutions == nil {
		state.AgentExecutions = make(map[string]string)
	}

	return &state, nil
}

// Save durably records state, so a restart never reads a phase the two-phase
// write had not yet committed.
func (s *FileStore) Save(_ context.Context, state *State) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshaling upgrade state: %w", err)
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, stateDirMode); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	if err := atomicfile.Write(s.path, data, stateFileMode); err != nil {
		return fmt.Errorf("writing upgrade state %s: %w", s.path, err)
	}

	return nil
}

// Clear drops the persisted state, returning cwa-updater to idle. A record that
// is already gone is success: ClearStatus may be called more than once for the same result.
func (s *FileStore) Clear(context.Context) error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clearing upgrade state %s: %w", s.path, err)
	}

	return nil
}
