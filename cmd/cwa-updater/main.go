// Package main is the entry point for cwa-updater.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/version"
)

const (
	defaultPort = "8786"
	portEnv     = "CWA_UPDATER_PORT"

	healthPath  = "/health"
	statusPath  = "/status"
	upgradePath = "/upgrade"

	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 5 * time.Second

	// statusIdle is the only status this build reports: no upgrade running and
	// no unacknowledged terminal result.
	statusIdle = "idle"
)

// healthResponse is the /health body. /health answers 200 in every upgrade
// state; the status field carries the state.
type healthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

// errorResponse carries a refusal reason.
type errorResponse struct {
	Error string `json:"error"`
}

// server holds the handler dependencies.
type server struct {
	logger *slog.Logger
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	showVersion := flag.Bool("version", false, "print the cwa-updater version and exit")
	flag.Parse()

	if *showVersion {
		if _, err := os.Stdout.WriteString(version.Version + "\n"); err != nil {
			return fmt.Errorf("writing version: %w", err)
		}

		return nil
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	port := os.Getenv(portEnv)
	if port == "" {
		port = defaultPort
	}

	srv := &server{logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc(healthPath, srv.handleHealth)
	mux.HandleFunc(statusPath, srv.handleStatus)
	mux.HandleFunc(upgradePath, srv.handleUpgrade)

	httpSrv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()

		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutting down", "error", err)
		}
	}()

	logger.Info("cwa-updater listening", "addr", httpSrv.Addr, "version", version.Version)

	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving cwa-updater: %w", err)
	}

	return nil
}

// handleHealth reports liveness and the running version. Always 200.
func (s *server) handleHealth(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	s.respond(writer, http.StatusOK, healthResponse{Status: statusIdle, Version: version.Version})
}

// handleStatus serves, and clears, the persisted terminal upgrade result. This
// build never records one, so the read and the clear are both no-ops.
func (s *server) handleStatus(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet, http.MethodDelete:
		writer.WriteHeader(http.StatusNoContent)
	default:
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleUpgrade refuses the handoff. Returning 200 here would tell cwa-manager
// the upgrade was accepted and durably queued, and it would exit.
func (s *server) handleUpgrade(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	s.logger.Warn("refusing upgrade handoff: execution is not implemented in this build")
	s.respond(writer, http.StatusNotImplemented, errorResponse{
		Error: "upgrade execution is not implemented in this build",
	})
}

func (s *server) respond(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)

	if err := json.NewEncoder(writer).Encode(body); err != nil {
		s.logger.Error("writing response", "error", err)
	}
}
