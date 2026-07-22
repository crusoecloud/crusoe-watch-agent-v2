// Command report-runner runs GPU bug-report collection, triggered by cwa-manager over
// a unix socket (see internal/bugreport RunnerServer). It drives a ToolGenerator.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/bugreport"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	reportDir := getenv(bugreport.EnvReportDir, bugreport.DefaultReportDir)
	socketPath := getenv(bugreport.EnvSocketPath, bugreport.DefaultSocketPath)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	server := bugreport.NewRunnerServer(bugreport.NewToolGenerator(reportDir), logger)
	logger.Info("report runner listening", "socket", socketPath, "report_dir", reportDir)

	if err := server.Serve(ctx, socketPath); err != nil {
		logger.Error("report runner stopped", "err", err)
		os.Exit(1)
	}
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}

	return fallback
}
