package bugreport

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/vector"
)

type fakeRunner struct {
	name       string
	args       []string
	err        error
	onRun      func()
	captureOut string // stdout the fake writes when capture() is called
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) error {
	f.name = name
	f.args = args

	if f.onRun != nil {
		f.onRun()
	}

	return f.err
}

func (f *fakeRunner) capture(_ context.Context, w io.Writer, name string, args ...string) error {
	f.name = name
	f.args = args

	if f.onRun != nil {
		f.onRun()
	}

	if f.err == nil {
		_, _ = io.WriteString(w, f.captureOut)
	}

	return f.err
}

var fixedTime = time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC)

// withFake wires a generator to a fake runner and the fixed clock.
func withFake(g *ToolGenerator, r runner) *ToolGenerator {
	g.runner = r
	g.now = func() time.Time { return fixedTime }

	return g
}

func TestToolGenerate_Success(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRunner{}
	g := withFake(NewToolGenerator(dir), r)

	// The tool writes <output-file>.gz; the manager reads that back.
	base := filepath.Join(dir, "bug-report-evt-1-20260709-123000.log")
	r.onRun = func() { require.NoError(t, os.WriteFile(base+".gz", []byte("x"), 0o600)) }

	path, err := g.Generate(context.Background(), vector.GPUNvidia, "evt-1")
	require.NoError(t, err)
	assert.Equal(t, base+".gz", path)

	assert.Equal(t, nvidiaBugReportScript, r.name)
	assert.Equal(t, []string{"--output-file", base}, r.args)
}

func TestToolGenerate_ScriptUnavailable(t *testing.T) {
	// A missing absolute-path binary surfaces as fs.ErrNotExist (not exec.ErrNotFound),
	// which must still be classified as "script unavailable".
	r := &fakeRunner{err: fmt.Errorf("run: %w", fs.ErrNotExist)}
	g := withFake(NewToolGenerator(t.TempDir()), r)

	_, err := g.Generate(context.Background(), vector.GPUNvidia, "evt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), string(CodeScriptUnavailable))
}

func TestToolGenerate_ScriptFailed(t *testing.T) {
	// A generic non-zero exit (not a missing binary) classifies as "script failed",
	// distinct from the "unavailable" case tested above.
	r := &fakeRunner{err: errors.New("exit status 1")}
	g := withFake(NewToolGenerator(t.TempDir()), r)

	_, err := g.Generate(context.Background(), vector.GPUNvidia, "evt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), string(CodeScriptFailed))
}

func TestToolGenerate_AMDCaptureFailsCleansUp(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRunner{err: errors.New("rocm boom")}
	g := withFake(NewToolGenerator(dir), r)

	_, err := g.Generate(context.Background(), vector.GPUAMD, "evt-amd")
	require.Error(t, err)
	assert.Contains(t, err.Error(), string(CodeScriptFailed))

	// A failed AMD run removes the partial archive it opened, leaving nothing behind.
	report := filepath.Join(dir, "bug-report-evt-amd-20260709-123000.log.gz")
	assert.NoFileExists(t, report)
}

func TestToolGenerate_AMD(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRunner{captureOut: "rocm techsupport diagnostics"}
	g := withFake(NewToolGenerator(dir), r)

	path, err := g.Generate(context.Background(), vector.GPUAMD, "evt-amd")
	require.NoError(t, err)

	// AMD runs rocm_techsupport.sh and gzips its stdout into the archive.
	assert.Equal(t, rocmTechSupportScript, r.name)
	assert.Equal(t, filepath.Join(dir, "bug-report-evt-amd-20260709-123000.log.gz"), path)

	// The archive is valid gzip holding the tool's stdout.
	file, err := os.Open(path)
	require.NoError(t, err)
	defer file.Close()

	gz, err := gzip.NewReader(file)
	require.NoError(t, err)

	content, err := io.ReadAll(gz)
	require.NoError(t, err)
	assert.Equal(t, "rocm techsupport diagnostics", string(content))
}

func TestToolGenerate_NoGPUUnsupported(t *testing.T) {
	g := withFake(NewToolGenerator(t.TempDir()), &fakeRunner{})

	_, err := g.Generate(context.Background(), vector.GPUNone, "evt")
	require.ErrorIs(t, err, errUnsupportedGPU)
}

func TestReportBase(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC)

	assert.Equal(t, "bug-report-evt-1-20260709-123000", ReportBase("evt-1", now))
	assert.Equal(t, "bug-report-20260709-123000", ReportBase("", now))
	// Unsafe characters in the event ID are neutralised.
	assert.Equal(t, "bug-report-a_b_c-20260709-123000", ReportBase("a/b c", now))
}

func TestToolGenerate_NoReportProduced(t *testing.T) {
	g := withFake(NewToolGenerator(t.TempDir()), &fakeRunner{})

	_, err := g.Generate(context.Background(), vector.GPUNvidia, "evt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no report")
}

// writeValidReport creates a valid report archive in dir and returns its name.
func writeValidReport(t *testing.T, dir string) string {
	t.Helper()

	name := ReportBase("evt-1", time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC)) + ".log.gz"
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("archive"), ReportPerm))

	return name
}

func TestResolveReportFile_AcceptsArchiveInReportDir(t *testing.T) {
	dir := t.TempDir()
	name := writeValidReport(t, dir)

	got, err := ResolveReportFile(dir, filepath.Join(dir, name))
	require.NoError(t, err)

	want, err := filepath.EvalSymlinks(filepath.Join(dir, name))
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// A correctly named report claimed to live elsewhere is looked for in dir.
func TestResolveReportFile_IgnoresReportedDirectory(t *testing.T) {
	dir := t.TempDir()
	name := writeValidReport(t, dir)

	got, err := ResolveReportFile(dir, filepath.Join("/some/other/place", name))
	require.NoError(t, err)

	// ResolveReportFile returns the symlink-evaluated path.
	wantDir, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	assert.Equal(t, wantDir, filepath.Dir(got))
}

func TestResolveReportFile_RejectsUnexpectedNames(t *testing.T) {
	dir := t.TempDir()

	for _, reported := range []string{
		"/etc/shadow",
		"/var/run/secrets/kubernetes.io/serviceaccount/token",
		"../../etc/shadow",
		"report.log.gz",                     // not a ReportBase name
		"bug-report-20260709-123000.tar.gz", // wrong extension
		"bug-report-evt-1-20260709.log.gz",  // no time component
		"",
	} {
		_, err := ResolveReportFile(dir, reported)
		require.Error(t, err, "reported=%q must be rejected", reported)
		assert.ErrorIs(t, err, ErrBadReportPath, "reported=%q", reported)
	}
}

// A planted symlink must not redirect the upload, nor the delete that follows.
func TestResolveReportFile_RejectsSymlinkOutOfDir(t *testing.T) {
	dir := t.TempDir()

	outside := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), ReportPerm))

	name := ReportBase("evt-1", time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC)) + ".log.gz"
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, name)))

	_, err := ResolveReportFile(dir, name)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBadReportPath)
}

// A well-named archive that is not there fails rather than uploading blind.
func TestResolveReportFile_RejectsMissingFile(t *testing.T) {
	dir := t.TempDir()

	name := ReportBase("evt-1", time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC)) + ".log.gz"

	_, err := ResolveReportFile(dir, name)
	require.Error(t, err)
	assert.Contains(t, err.Error(), string(CodeNoOutput))
}
