package command

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "gitlab.com/crusoeenergy/schemas/api/island/v2/observability"
)

const execStoreDirPerm = 0o750

// ExecStatusInProgress is the status persisted for a mid-execution command.
const ExecStatusInProgress = "in_progress"

// ExecRecord is one command execution's persisted record.
type ExecRecord struct {
	ExecutionID string    `json:"executionId"`
	Command     string    `json:"command"`
	Status      string    `json:"status"`
	StartedAt   time.Time `json:"startedAt"`
}

// ExecStore is a crash-safe, on-disk record of in-flight command executions, one
// file per execution: the dispatcher writes a record before running a command
// and removes it on completion. A record therefore survives to the next startup
// only if a hard stop (OOM/SIGKILL/reboot) killed the command mid-flight, where
// RecoverInterrupted reports it to the control plane as INTERRUPTED.
type ExecStore struct {
	dir string
}

// NewExecStore returns a store persisting records under dir, created lazily on first write.
func NewExecStore(dir string) *ExecStore {
	return &ExecStore{dir: dir}
}

// path maps an execution ID to its record file. base64url keeps any ID a valid,
// collision-free filename; the authoritative ID also lives inside the record.
func (s *ExecStore) path(execID string) string {
	name := base64.RawURLEncoding.EncodeToString([]byte(execID)) + ".json"

	return filepath.Join(s.dir, name)
}

// Begin records a command as in-progress. Call before execution starts.
func (s *ExecStore) Begin(execID, command string) error {
	if err := os.MkdirAll(s.dir, execStoreDirPerm); err != nil {
		return fmt.Errorf("creating exec store dir: %w", err)
	}

	data, err := json.Marshal(ExecRecord{
		ExecutionID: execID,
		Command:     command,
		Status:      ExecStatusInProgress,
		StartedAt:   time.Now(),
	})
	if err != nil {
		return fmt.Errorf("marshaling exec record: %w", err)
	}

	return writeFileAtomic(s.path(execID), data)
}

// Complete removes a command's record. Call once it has a terminal result, and
// before that result reaches the heartbeat. A missing record is not an error.
func (s *ExecStore) Complete(execID string) error {
	if err := os.Remove(s.path(execID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing exec record: %w", err)
	}

	return nil
}

// List returns all surviving records. A missing directory yields none;
// unreadable or unparseable files are skipped so one bad file can't block the rest.
func (s *ExecStore) List() ([]ExecRecord, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("reading exec store dir: %w", err)
	}

	records := make([]ExecRecord, 0, len(entries))

	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}

		data, readErr := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if readErr != nil {
			continue
		}

		var rec ExecRecord
		if json.Unmarshal(data, &rec) != nil {
			continue
		}

		records = append(records, rec)
	}

	return records, nil
}

// RecoverInterrupted delivers an INTERRUPTED result into sink for every
// surviving record, then removes it. Call once at startup before the heartbeat
// loop starts so the results ride the first heartbeat. A nil store is a no-op.
func RecoverInterrupted(store *ExecStore, sink ResultSink, logger *slog.Logger) {
	if store == nil {
		return
	}

	records, err := store.List()
	if err != nil {
		logger.Warn("failed to scan command crash-recovery store", "error", err)

		return
	}

	for _, rec := range records {
		sink.DeliverResult(&pb.CwaCommandResult{
			ExecutionId: rec.ExecutionID,
			Command:     rec.Command,
			Status:      interruptedStatus,
			Reason:      "interrupted by agent restart (crash recovery)",
			CompletedAt: timestamppb.Now(),
		})

		logger.Info("recovered interrupted command",
			"execution_id", rec.ExecutionID, "command", rec.Command, "started_at", rec.StartedAt)

		if rmErr := store.Complete(rec.ExecutionID); rmErr != nil {
			logger.Warn("failed to clear recovered command record",
				"execution_id", rec.ExecutionID, "error", rmErr)
		}
	}
}

// writeFileAtomic durably writes data to path via temp file + rename, so neither
// a reader nor a crash ever sees a partial or missing-after-reboot file.
func writeFileAtomic(path string, data []byte) error {
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

	if err := tmp.Chmod(stateFilePerm); err != nil {
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

// fsyncDir flushes a directory entry change (the rename above) to disk so it
// survives a hard reboot or power loss.
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
