package bugreport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/vector"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/version"
)

// This file defines the report-runner protocol and both ends: RunnerClient (cwa-manager)
// dials the unix socket, RunnerServer (the report-runner) serves it and drives a
// ToolGenerator. The report-runner is a separate process (a systemd service natively,
// a sidecar container in docker), so cwa-manager needs no GPU access or host privileges.

// collectPath is the HTTP route the manager calls to trigger a collection.
const collectPath = "/collect"

// healthPath is the HTTP route the manager polls for the runner's health and version.
const healthPath = "/health"

// socketPerm restricts the runner socket to its owner.
// The manager and the runner both run as root and share the mount.
const socketPerm = 0o600

// readHeaderTimeout bounds how long the runner waits for request headers.
const readHeaderTimeout = 10 * time.Second

// CollectRequest asks the runner to produce a bug report for one event.
type CollectRequest struct {
	EventID string         `json:"eventId"`
	GPU     vector.GPUType `json:"gpu"`
}

// CollectResponse returns the archive path the runner wrote, or an error message.
// The message already leads with a CWA-BR code, so the manager can relay it without re-tagging.
type CollectResponse struct {
	Path  string `json:"path,omitempty"`
	Error string `json:"error,omitempty"`
}

// HealthResponse is the /health body. A successful response is itself the liveness signal.
type HealthResponse struct {
	Version string `json:"version"`
}

// runnerError carries a failure reported by the runner back to the manager verbatim.
type runnerError struct{ msg string }

func (e *runnerError) Error() string { return e.msg }

// RunnerClient is cwa-manager's end of the protocol: it triggers a collection over the
// unix socket and returns the archive path the runner wrote.
type RunnerClient struct {
	socketPath string
	client     *http.Client
}

// NewRunnerClient builds a client that talks to the runner at socketPath.
func NewRunnerClient(socketPath string) *RunnerClient {
	return &RunnerClient{
		socketPath: socketPath,
		client: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var dialer net.Dialer

					return dialer.DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

// Generate asks the report-runner to produce a report and returns its path.
func (c *RunnerClient) Generate(ctx context.Context, gpu vector.GPUType, eventID string) (string, error) {
	body, err := json.Marshal(CollectRequest{EventID: eventID, GPU: gpu})
	if err != nil {
		return "", CodeInternal.Errorf("encoding collect request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://report-runner"+collectPath, bytes.NewReader(body))
	if err != nil {
		return "", CodeInternal.Errorf("building collect request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", CodeInternal.Errorf("reaching report-runner: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	var out CollectResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", CodeInternal.Errorf("decoding runner response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", &runnerError{msg: out.Error}
	}

	return out.Path, nil
}

// Health polls the report-runner's /health route and returns its version.
func (c *RunnerClient) Health(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://report-runner"+healthPath, nil)
	if err != nil {
		return "", CodeInternal.Errorf("building health request: %w", err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return "", CodeInternal.Errorf("reaching report-runner: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", CodeInternal.Errorf("report-runner health returned status %d", resp.StatusCode)
	}

	var out HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", CodeInternal.Errorf("decoding health response: %w", err)
	}

	return out.Version, nil
}

// reportGenerator produces a report archive; *ToolGenerator implements it.
type reportGenerator interface {
	Generate(ctx context.Context, gpu vector.GPUType, eventID string) (string, error)
}

// RunnerServer is the report-runner's end of the protocol: it owns the socket, decodes
// requests, and delegates collection to a ToolGenerator.
type RunnerServer struct {
	gen    reportGenerator
	logger *slog.Logger
}

// NewRunnerServer builds a server that delegates collection to gen.
func NewRunnerServer(gen reportGenerator, logger *slog.Logger) *RunnerServer {
	return &RunnerServer{gen: gen, logger: logger}
}

// Serve listens on the unix socket at socketPath until ctx is cancelled.
func (s *RunnerServer) Serve(ctx context.Context, socketPath string) error {
	// Ensure the socket dir exists before binding (otherwise created lazily on collection).
	if err := os.MkdirAll(filepath.Dir(socketPath), DirPerm); err != nil {
		return CodeInternal.Errorf("creating socket dir: %w", err)
	}

	// A leftover socket from a previous run would block Listen.
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return CodeInternal.Errorf("removing stale socket %s: %w", socketPath, err)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return CodeInternal.Errorf("listening on %s: %w", socketPath, err)
	}

	if err := os.Chmod(socketPath, socketPerm); err != nil {
		return CodeInternal.Errorf("securing socket %s: %w", socketPath, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc(collectPath, s.handleCollect)
	mux.HandleFunc(healthPath, s.handleHealth)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: readHeaderTimeout}

	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return CodeInternal.Errorf("serving report runner: %w", err)
	}

	return nil
}

func (s *RunnerServer) handleCollect(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	var req CollectRequest
	if err := json.NewDecoder(request.Body).Decode(&req); err != nil {
		s.respond(writer, http.StatusBadRequest, CollectResponse{Error: "invalid request: " + err.Error()})

		return
	}

	s.logger.Info("collecting bug report", "event_id", req.EventID, "gpu", req.GPU)

	path, err := s.gen.Generate(request.Context(), req.GPU, req.EventID)
	if err != nil {
		s.logger.Error("collection failed", "event_id", req.EventID, "err", err)
		s.respond(writer, http.StatusInternalServerError, CollectResponse{Error: err.Error()})

		return
	}

	s.logger.Info("bug report collected", "event_id", req.EventID, "path", path)
	s.respond(writer, http.StatusOK, CollectResponse{Path: path})
}

// handleHealth returns the runner's version. cwa-manager polls it for the heartbeat.
func (s *RunnerServer) handleHealth(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)

	if err := json.NewEncoder(writer).Encode(HealthResponse{Version: version.Version}); err != nil {
		s.logger.Error("writing health response", "err", err)
	}
}

func (s *RunnerServer) respond(writer http.ResponseWriter, status int, body CollectResponse) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)

	if err := json.NewEncoder(writer).Encode(body); err != nil {
		s.logger.Error("writing runner response", "err", err)
	}
}
