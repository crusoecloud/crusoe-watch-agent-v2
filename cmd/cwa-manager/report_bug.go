package main

import (
	"log/slog"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/bugreport"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/command"
	pb "gitlab.com/crusoeenergy/schemas/api/island/v2/observability"
)

// buildGenerator constructs the platform-specific bug-report generator.
func buildGenerator(deps command.Deps, logger *slog.Logger) command.Generator {
	switch deps.InstallType {
	case pb.CwaInstallType_CWA_INSTALL_TYPE_SYSTEMD:
		return bugreport.NewRunnerClient(getEnvOrDefault(bugreport.EnvSocketPath, bugreport.DefaultSocketPath))
	case pb.CwaInstallType_CWA_INSTALL_TYPE_DOCKER:
		return bugreport.NewRunnerClient(getEnvOrDefault(bugreport.EnvSocketPath, bugreport.DefaultSocketPath))
	case pb.CwaInstallType_CWA_INSTALL_TYPE_KUBERNETES, pb.CwaInstallType_CWA_INSTALL_TYPE_UNSPECIFIED:
		// TODO: K8s bug report support.
	}

	logger.Info("report.bug not available for install type", "install_type", deps.InstallType.String())

	return nil
}
