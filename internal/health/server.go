package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/version"
)

// Server routes and defaults.
const (
	// DefaultPort is cwa-manager's own health port, polled by cwa-updater per pod
	// after a rolling update and by the platform's liveness probe.
	DefaultPort = "8787"
	// PortEnv overrides DefaultPort.
	PortEnv = "CWA_HEALTH_PORT"

	// HealthPath is the route both platform probes and cwa-updater read.
	HealthPath = "/health"

	serverReadHeaderTimeout = 10 * time.Second
	serverShutdownTimeout   = 5 * time.Second
)

// Agent status values served by the endpoints.
const (
	// StatusHealthy means cwa-manager has reached the control plane at least once.
	StatusHealthy = "healthy"
	// StatusStarting means it is up but has not heartbeated yet.
	StatusStarting = "starting"
)

// Reporter is the agent state the endpoints serve. heartbeat.Loop implements it.
type Reporter interface {
	// LastHeartbeat is when the most recent heartbeat was sent, zero if none has been.
	LastHeartbeat() time.Time
}

// serverResponse is the /health body.
//
//nolint:tagliatelle // snake_case matches the cwa-updater health contract
type serverResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	// LastHeartbeat is absent until one has been delivered. It is the signal
	// cwa-updater's post-upgrade poll reads to tell an agent that is merely
	// listening from one that is actually reaching the control plane.
	LastHeartbeat *time.Time `json:"last_heartbeat,omitempty"`
}

// Server serves cwa-manager's own health, and answers 200 whenever the process is serving.
type Server struct {
	reporter Reporter
	logger   *slog.Logger
	addr     string
}

// NewServer returns a Server listening on addr.
func NewServer(addr string, reporter Reporter, logger *slog.Logger) *Server {
	return &Server{reporter: reporter, logger: logger, addr: addr}
}

// Run serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc(HealthPath, s.handleHealth)

	httpSrv := &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: serverReadHeaderTimeout,
	}

	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), serverShutdownTimeout)
		defer cancel()

		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			s.logger.Error("shutting down health server", "error", err)
		}
	}()

	s.logger.Info("health server listening", "addr", s.addr)

	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving health endpoints: %w", err)
	}

	return nil
}

// handleHealth reports that the process is serving, and whether it has reached
// the control plane. Always 200; the distinction lives in the body.
func (s *Server) handleHealth(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	last := s.reporter.LastHeartbeat()

	body := serverResponse{Status: StatusStarting, Version: version.Version}
	if !last.IsZero() {
		body.Status = StatusHealthy
		body.LastHeartbeat = &last
	}

	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)

	if err := json.NewEncoder(writer).Encode(body); err != nil {
		s.logger.Error("writing health response", "error", err)
	}
}
