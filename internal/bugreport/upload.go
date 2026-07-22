// Package bugreport implements GPU bug-report generation (via a report-runner
// process) and upload for the report.bug command. Generators run a vendor collector
// that writes a report archive to a shared directory; the manager then reads the
// archive and uploads it to cwa-coordinator's /upload endpoint.
package bugreport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
)

// contentType is the media type of the report archive part.
const contentType = "application/gzip"

// errUploadStatus is returned when the coordinator responds with a non-2xx status.
var errUploadStatus = errors.New("upload failed")

// ErrUploadPermanent wraps an upload failure that a retry cannot fix.
var ErrUploadPermanent = errors.New("permanent upload failure")

// permanentStatus reports whether a non-2xx status is a definitive rejection. 408
// (Request Timeout) and 429 (Too Many Requests), 5xx is transient (server-side) are transient.
func permanentStatus(code int) bool {
	if code == http.StatusRequestTimeout || code == http.StatusTooManyRequests {
		return false
	}

	return code >= http.StatusBadRequest && code < http.StatusInternalServerError
}

// HTTPUploader POSTs a report archive as multipart/form-data to cwa-coordinator's /upload endpoint.
type HTTPUploader struct {
	endpoint string
	token    string
	vmID     string
	nodeName string
	client   *http.Client
}

// NewHTTPUploader creates an HTTPUploader targeting endpoint, authenticating with
// token, and tagging uploads with vmID/nodeName. Request lifetime is bounded by the
// context passed to Upload, so the client itself sets no timeout.
func NewHTTPUploader(endpoint, token, vmID, nodeName string) *HTTPUploader {
	return &HTTPUploader{
		endpoint: endpoint,
		token:    token,
		vmID:     vmID,
		nodeName: nodeName,
		client:   &http.Client{},
	}
}

// Upload POSTs the archive at path to the coordinator as multipart/form-data, tagged with eventID.
func (u *HTTPUploader) Upload(ctx context.Context, path, eventID string) error {
	body, formType, err := u.buildForm(path, eventID)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.endpoint, body)
	if err != nil {
		return fmt.Errorf("building upload request: %w", err)
	}

	req.Header.Set("Content-Type", formType)
	if u.token != "" {
		req.Header.Set("Authorization", "Bearer "+u.token)
	}

	resp, err := u.client.Do(req)
	if err != nil {
		return fmt.Errorf("uploading report: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		if permanentStatus(resp.StatusCode) {
			return fmt.Errorf("%w: %s", ErrUploadPermanent, resp.Status)
		}

		return fmt.Errorf("%w: %s", errUploadStatus, resp.Status)
	}

	return nil
}

// buildForm assembles the multipart body and returns it with its Content-Type header value.
// Bug-report archives are small enough to hold in memory.
func (u *HTTPUploader) buildForm(path, eventID string) (io.Reader, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("opening report: %w", err)
	}

	defer func() { _ = file.Close() }()

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filepath.Base(path)))
	header.Set("Content-Type", contentType)

	part, err := writer.CreatePart(header)
	if err != nil {
		return nil, "", fmt.Errorf("creating file part: %w", err)
	}

	if _, err := io.Copy(part, file); err != nil {
		return nil, "", fmt.Errorf("reading report: %w", err)
	}

	for _, field := range []struct{ name, value string }{
		{"vm_id", u.vmID},
		{"event_id", eventID},
		{"node_name", u.nodeName},
		{"status", "success"},
	} {
		if err := writer.WriteField(field.name, field.value); err != nil {
			return nil, "", fmt.Errorf("writing %s field: %w", field.name, err)
		}
	}

	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("finalizing upload body: %w", err)
	}

	return &buf, writer.FormDataContentType(), nil
}
