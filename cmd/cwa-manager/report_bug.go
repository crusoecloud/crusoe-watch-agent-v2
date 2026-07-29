package main

import (
	"log/slog"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/bugreport"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/command"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/vector"
	pb "gitlab.com/crusoeenergy/schemas/api/island/v2/observability"
)

// buildGenerator constructs the platform-specific bug-report generator and any handler
// options it needs. It returns (nil, nil) when report.bug is unavailable on this install type.
func buildGenerator(
	deps command.Deps, k8sRT *k8sRuntime, logger *slog.Logger,
) (command.Generator, []command.ReportBugOption) {
	socket := getEnvOrDefault(bugreport.EnvSocketPath, bugreport.DefaultSocketPath)

	switch deps.InstallType {
	case pb.CwaInstallType_CWA_INSTALL_TYPE_SYSTEMD, pb.CwaInstallType_CWA_INSTALL_TYPE_DOCKER:
		return bugreport.NewRunnerClient(socket), nil
	case pb.CwaInstallType_CWA_INSTALL_TYPE_KUBERNETES:
		if k8sRT == nil {
			logger.Error("k8s runtime unavailable, report.bug disabled")

			return nil, nil
		}
		// On K8s, classify the GPU from the host sysfs cwa-manager mounts.
		opts := []command.ReportBugOption{
			command.WithGPUDetector(func() vector.GPUType { return vector.DetectGPUAt(hostSysModuleDir) }),
		}

		return k8sRT.buildK8sGenerator(), opts
	case pb.CwaInstallType_CWA_INSTALL_TYPE_UNSPECIFIED:
	}

	logger.Info("report.bug not available for install type", "install_type", deps.InstallType.String())

	return nil, nil
}
