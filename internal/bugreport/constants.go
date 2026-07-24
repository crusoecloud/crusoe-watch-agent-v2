package bugreport

import "fmt"

// Deployment defaults shared by cwa-manager and the report-runner (native systemd service or docker sidecar container).
const (
	// EnvReportDir overrides DefaultReportDir.
	EnvReportDir = "BUG_REPORT_OUTPUT_DIR"
	// EnvSocketPath overrides DefaultSocketPath.
	EnvSocketPath = "REPORT_RUNNER_SOCKET"

	// DefaultReportDir is where the runner writes report archives and cwa-manager reads them:
	// a host directory natively, a bind mount shared by both containers in docker mode.
	DefaultReportDir = "/etc/crusoe/bug-reports"
	// DefaultSocketPath is the unix socket the runner listens on and cwa-manager dials.
	DefaultSocketPath = DefaultReportDir + "/report-runner.sock"
)

// Code classifies a bug-report failure (CWA-BR-NNNN), ported from v1. A Code is a label,
// not an error: use Errorf to build an error tagged with it. The tag rides in the
// command result reason, where the coordinator parses the "CWA-BR-<digits>" token back out.
type Code string

// Bug-report failure codes (from v1).
const (
	CodeScriptUnavailable  Code = "CWA-BR-5001" // bug-report tool unavailable, or report-runner unreachable
	CodeScriptTimedOut     Code = "CWA-BR-5002" // tool execution timed out
	CodeScriptFailed       Code = "CWA-BR-5003" // tool ran but exited non-zero
	CodeDriverPodNotFound  Code = "CWA-BR-5004" // NVIDIA driver pod not found (K8s)
	CodeExecError          Code = "CWA-BR-5005" // error executing the bug-report tool
	CodeNoOutput           Code = "CWA-BR-5006" // tool succeeded but produced no archive
	CodeDownloadFailed     Code = "CWA-BR-5007" // unexpected error downloading the report
	CodeCollectionTimedOut Code = "CWA-BR-5008" // report generation timed out
	CodeUploadFailed       Code = "CWA-BR-5009" // archive upload failed after retries
	CodeNoGPU              Code = "CWA-BR-5010" // no supported GPU on this host (v2 only)
	CodeInternal           Code = "CWA-BR-5099" // unexpected internal error
)

// Errorf returns an error tagged with the code: "CWA-BR-NNNN: <message>". A %w verb
// in format wraps an underlying error so callers can still match it with errors.Is.
func (c Code) Errorf(format string, args ...any) error {
	//nolint:err113 // this is the tagging helper; callers pass %w to wrap static errors
	return fmt.Errorf(string(c)+": "+format, args...)
}
