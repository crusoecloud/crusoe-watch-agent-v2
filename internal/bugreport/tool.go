package bugreport

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/vector"
)

const (
	// DirPerm is the permission for the report output directory.
	DirPerm = 0o750
	// ReportPerm is the permission for a generated report archive.
	ReportPerm = 0o640
	// nvidiaBugReportScript is the NVIDIA vendor tool (from nvidia-utils). It writes its own <output-file>.gz.
	nvidiaBugReportScript = "/usr/bin/nvidia-bug-report.sh"
	// rocmTechSupportScript is AMD's rocm_techsupport.sh. It writes the report to stdout.
	rocmTechSupportScript = "/usr/bin/rocm_techsupport.sh"
)

// runner executes an external command to completion.
type runner interface {
	// run executes name and discards stdout, for tools that write own output file (nvidia-bug-report.sh).
	run(ctx context.Context, name string, args ...string) error
	// capture executes name and streams stdout+stderr to w, for tools that write to stdout (rocm_techsupport.sh).
	capture(ctx context.Context, w io.Writer, name string, args ...string) error
}

type execRunner struct{}

func (execRunner) run(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, out)
	}

	return nil
}

func (execRunner) capture(ctx context.Context, w io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = w
	cmd.Stderr = w

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}

	return nil
}

// ToolGenerator is the collection engine the report-runner drives: it runs the GPU
// vendor tool in-process, picking NVIDIA or AMD per the detected GPU.
type ToolGenerator struct {
	outputDir string
	runner    runner
	now       func() time.Time
}

// NewToolGenerator creates a ToolGenerator writing reports under outputDir.
func NewToolGenerator(outputDir string) *ToolGenerator {
	return &ToolGenerator{
		outputDir: outputDir,
		runner:    execRunner{},
		now:       time.Now,
	}
}

// Generate runs the vendor bug-report tool for gpu and returns the path to the archive.
func (g *ToolGenerator) Generate(ctx context.Context, gpu vector.GPUType, eventID string) (string, error) {
	if err := os.MkdirAll(g.outputDir, DirPerm); err != nil {
		return "", CodeInternal.Errorf("creating output dir: %w", err)
	}

	base := filepath.Join(g.outputDir, ReportBase(eventID, g.now()))

	switch gpu {
	case vector.GPUNvidia:
		return g.nvidia(ctx, base)
	case vector.GPUAMD:
		return g.amd(ctx, base)
	case vector.GPUNone:
		return "", CodeNoGPU.Errorf("no supported GPU for bug report: %w", errUnsupportedGPU)
	default:
		return "", CodeNoGPU.Errorf("unknown GPU type for bug report: %w", errUnsupportedGPU)
	}
}

// nvidia runs nvidia-bug-report.sh, which appends .gz to the --output-file path.
func (g *ToolGenerator) nvidia(ctx context.Context, base string) (string, error) {
	out := base + ".log"
	if err := g.runner.run(ctx, nvidiaBugReportScript, "--output-file", out); err != nil {
		return "", classifyToolErr(err, nvidiaBugReportScript)
	}

	report := out + ".gz"
	if _, err := os.Stat(report); err != nil {
		return "", CodeNoOutput.Errorf("collector produced no report at %s: %w", report, err)
	}

	return report, nil
}

// amd runs rocm_techsupport.sh, which writes to stdout. The output is captured and gzipped into the archive.
func (g *ToolGenerator) amd(ctx context.Context, base string) (string, error) {
	report := base + ".log.gz"

	file, err := os.OpenFile(report, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, ReportPerm)
	if err != nil {
		return "", CodeInternal.Errorf("creating report file: %w", err)
	}

	defer func() { _ = file.Close() }()

	gzipWriter := gzip.NewWriter(file)
	if err := g.runner.capture(ctx, gzipWriter, rocmTechSupportScript); err != nil {
		_ = gzipWriter.Close()
		_ = os.Remove(report)

		return "", classifyToolErr(err, rocmTechSupportScript)
	}

	if err := gzipWriter.Close(); err != nil {
		return "", CodeInternal.Errorf("finalizing report: %w", err)
	}

	return report, nil
}

// classifyToolErr maps a vendor-tool failure to a CWA-BR code: a missing binary is "unavailable", else is "failed".
func classifyToolErr(err error, tool string) error {
	code := CodeScriptFailed
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		code = CodeScriptUnavailable
	}

	return code.Errorf("%s failed: %w", tool, err)
}

var errUnsupportedGPU = errors.New("unsupported GPU type for bug report")

// unsafeChars matches anything not allowed in a report filename.
var unsafeChars = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

// ReportBase builds the report filename stem (no extension) for a collection.
// The event ID (when present) and timestamp make it unique and traceable.
func ReportBase(eventID string, now time.Time) string {
	stamp := now.UTC().Format("20060102-150405")
	if eventID == "" {
		return "bug-report-" + stamp
	}

	return "bug-report-" + sanitize(eventID) + "-" + stamp
}

// sanitize replaces characters that are unsafe in a filename.
func sanitize(s string) string {
	return unsafeChars.ReplaceAllString(s, "_")
}
