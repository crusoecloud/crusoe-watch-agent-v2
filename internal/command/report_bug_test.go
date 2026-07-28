package command

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/bugreport"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/vector"
)

type fakeGenerator struct {
	path      string
	err       error
	healthErr error
	gpu       vector.GPUType
	called    bool
}

func (f *fakeGenerator) Generate(_ context.Context, gpu vector.GPUType, _ string) (string, error) {
	f.called = true
	f.gpu = gpu

	return f.path, f.err
}

func (f *fakeGenerator) Health(context.Context) (string, error) {
	return "v-test", f.healthErr
}

type fakeUploader struct {
	path      string
	eventID   string
	err       error
	failFirst int // fail this many calls before succeeding
	calls     int
	called    bool
}

func (f *fakeUploader) Upload(_ context.Context, path, eventID string) error {
	f.called = true
	f.calls++
	f.path = path
	f.eventID = eventID

	if f.calls <= f.failFirst {
		return errors.New("transient upload failure")
	}

	return f.err
}

// reportBugFor builds a handler whose GPU detection is pinned to gpu, with retry
// backoff disabled so tests don't sleep.
func reportBugFor(gen Generator, up Uploader, gpu vector.GPUType) *ReportBug {
	h := NewReportBug(gen, up)
	h.detectGPU = func() vector.GPUType { return gpu }
	h.retryDelay = 0

	return h
}

func TestReportBug_Success(t *testing.T) {
	// A report the generator "produced": a real file so we can assert cleanup.
	report := filepath.Join(t.TempDir(), "report.tar.gz")
	require.NoError(t, os.WriteFile(report, []byte("report"), 0o600))

	gen := &fakeGenerator{path: report}
	up := &fakeUploader{}
	h := reportBugFor(gen, up, vector.GPUNvidia)

	require.NoError(t, runErr(t, h, map[string]string{ParamEventID: "evt-1"}))

	// The detected GPU reaches the generator, the produced report is uploaded with
	// its event ID, and it is removed once uploaded.
	assert.Equal(t, vector.GPUNvidia, gen.gpu)
	assert.Equal(t, report, up.path)
	assert.Equal(t, "evt-1", up.eventID)
	assert.NoFileExists(t, report)
}

func TestReportBug_PassesAMDGPU(t *testing.T) {
	gen := &fakeGenerator{path: filepath.Join(t.TempDir(), "absent.tar.gz")}
	h := reportBugFor(gen, &fakeUploader{}, vector.GPUAMD)

	require.NoError(t, runErr(t, h, map[string]string{ParamEventID: "evt-1"}))
	assert.Equal(t, vector.GPUAMD, gen.gpu)
}

func TestReportBug_NoGPU(t *testing.T) {
	gen := &fakeGenerator{}
	up := &fakeUploader{}
	h := reportBugFor(gen, up, vector.GPUNone)

	err := runErr(t, h, map[string]string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CWA-BR-5010") // no-GPU code carried in the reason
	assert.False(t, gen.called)
	assert.False(t, up.called)
}

func TestReportBug_UnreachableRunnerSkipsCollection(t *testing.T) {
	gen := &fakeGenerator{healthErr: errors.New("dial failed")}
	up := &fakeUploader{}
	h := reportBugFor(gen, up, vector.GPUNvidia)

	err := runErr(t, h, map[string]string{ParamEventID: "evt-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CWA-BR-5001") // script-unavailable code carried in the reason
	assert.False(t, gen.called, "collection must not run when the preflight fails")
	assert.False(t, up.called)
}

func TestReportBug_GenerateFailsSkipsUpload(t *testing.T) {
	gen := &fakeGenerator{err: errors.New("boom")}
	up := &fakeUploader{}
	h := reportBugFor(gen, up, vector.GPUNvidia)

	require.Error(t, runErr(t, h, map[string]string{}))
	assert.False(t, up.called)
}

func TestReportBug_UploadFails(t *testing.T) {
	report := filepath.Join(t.TempDir(), "report.tar.gz")
	require.NoError(t, os.WriteFile(report, []byte("report"), 0o600))

	gen := &fakeGenerator{path: report}
	up := &fakeUploader{err: errors.New("503")}
	h := reportBugFor(gen, up, vector.GPUNvidia)

	require.Error(t, runErr(t, h, map[string]string{}))

	// Every attempt was made before giving up, and the report is still cleaned up.
	assert.Equal(t, maxUploadAttempts, up.calls)
	assert.NoFileExists(t, report)
}

func TestReportBug_PermanentUploadFailureNotRetried(t *testing.T) {
	report := filepath.Join(t.TempDir(), "report.tar.gz")
	require.NoError(t, os.WriteFile(report, []byte("report"), 0o600))

	gen := &fakeGenerator{path: report}
	up := &fakeUploader{err: fmt.Errorf("coordinator rejected: %w", bugreport.ErrUploadPermanent)}
	h := reportBugFor(gen, up, vector.GPUNvidia)

	require.Error(t, runErr(t, h, map[string]string{}))

	// A permanent rejection fails fast (one attempt) and still cleans up the report.
	assert.Equal(t, 1, up.calls)
	assert.NoFileExists(t, report)
}

func TestReportBug_UploadRetriesThenSucceeds(t *testing.T) {
	report := filepath.Join(t.TempDir(), "report.tar.gz")
	require.NoError(t, os.WriteFile(report, []byte("report"), 0o600))

	gen := &fakeGenerator{path: report}
	up := &fakeUploader{failFirst: maxUploadAttempts - 1} // fail then succeed on the last try
	h := reportBugFor(gen, up, vector.GPUNvidia)

	require.NoError(t, runErr(t, h, map[string]string{}))

	// A transient failure didn't waste the collection — it retried and uploaded.
	assert.Equal(t, maxUploadAttempts, up.calls)
	assert.NoFileExists(t, report)
}

func TestReportBug_Timeout(t *testing.T) {
	assert.Equal(t, LongRunning, NewReportBug(nil, nil).Timeout())
}
