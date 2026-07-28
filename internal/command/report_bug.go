package command

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/bugreport"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/vector"
)

// ReportBugCommand triggers collection of a GPU vendor bug report and uploads the
// resulting archive to cwa-coordinator's /upload endpoint.
const ReportBugCommand = "report.bug"

// report.bug parameters.
const (
	// ParamEventID identifies the log-collection event.
	ParamEventID = "event_id"
)

// Upload retry policy.
const (
	maxUploadAttempts       = 3
	defaultUploadRetryDelay = 5 * time.Second
)

// Generator runs a vendor bug-report tool and returns the path to the report archive it produced.
// Health preflights the report-runner so Run can fail fast when it's down.
type Generator interface {
	Generate(ctx context.Context, gpu vector.GPUType, eventID string) (string, error)
	Health(ctx context.Context) (string, error)
}

// Uploader sends the report archive at path (tagged with eventID) to the coordinator.
// The production implementation POSTs it as multipart/form-data to the /upload endpoint.
type Uploader interface {
	Upload(ctx context.Context, path, eventID string) error
}

// ReportBug is the report.bug handler.
type ReportBug struct {
	generator  Generator
	uploader   Uploader
	detectGPU  func() vector.GPUType
	retryDelay time.Duration
}

// NewReportBug creates a report.bug handler.
func NewReportBug(generator Generator, uploader Uploader) *ReportBug {
	return &ReportBug{
		generator:  generator,
		uploader:   uploader,
		detectGPU:  vector.DetectGPU,
		retryDelay: defaultUploadRetryDelay,
	}
}

// Timeout returns the long-running class: bug report collection can take several minutes.
func (r *ReportBug) Timeout() time.Duration { return LongRunning }

// Run generates a GPU bug report and uploads it to the coordinator.
func (r *ReportBug) Run(ctx context.Context, params map[string]string) (string, error) {
	gpu := r.detectGPU()
	if gpu == vector.GPUNone {
		return "", bugreport.CodeNoGPU.Errorf("no supported GPU detected on this host")
	}

	// Preflight the report-runner and fail fast if it's unreachable or unhealthy,
	// rather than attempting a collection that cannot succeed.
	if _, err := r.generator.Health(ctx); err != nil {
		return "", bugreport.CodeScriptUnavailable.Errorf("report-runner unavailable: %w", err)
	}

	eventID := params[ParamEventID]

	path, err := r.generator.Generate(ctx, gpu, eventID)
	if err != nil {
		return "", fmt.Errorf("generating bug report: %w", err)
	}

	defer func() { _ = os.Remove(path) }()

	if err := r.uploadWithRetry(ctx, path, eventID); err != nil {
		return "", fmt.Errorf("uploading bug report: %w", err)
	}

	return "", nil
}

// uploadWithRetry uploads the archive, retrying transient failures up to maxUploadAttempts.
// A permanent failure (a definitive 4xx from the coordinator) is not retried. The
// archive is only discarded once uploading gives up.
func (r *ReportBug) uploadWithRetry(ctx context.Context, path, eventID string) error {
	var err error

	for attempt := 1; attempt <= maxUploadAttempts; attempt++ {
		if err = r.uploader.Upload(ctx, path, eventID); err == nil {
			return nil
		}

		// Retrying a rejected request (bad token, invalid file) only delays the
		// FAILED ack, so give up as soon as the coordinator says it's permanent.
		if errors.Is(err, bugreport.ErrUploadPermanent) {
			return bugreport.CodeUploadFailed.Errorf("permanent failure: %w", err)
		}

		if attempt == maxUploadAttempts {
			break
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("upload interrupted: %w", ctx.Err())
		case <-time.After(r.retryDelay):
		}
	}

	return bugreport.CodeUploadFailed.Errorf("after %d attempts: %w", maxUploadAttempts, err)
}
