// Command vector-config-dump renders Vector config variants for CI validation
// and debugging. Unlike cwa-manager, which detects its config inputs at
// runtime, this dev tool takes them as flags (VM) or a fixed representative
// fixture (K8s) so any variant can be rendered on any machine.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/vector"
)

var (
	errUnknownGPUType  = errors.New("unknown GPU type (use none, nvidia, amd)")
	errUnknownPlatform = errors.New("unknown platform (use vm, k8s)")
)

// Representative K8s exporter ports and scrape intervals. Values mirror the
// production defaults exercised by internal/vector's tests.
const (
	dcgmPort   = 9400
	amdPort    = 5000
	ksmPort    = 8080
	cmePort    = 9500
	slurmPort  = 6817
	customPort = 9100

	fastScrapeSecs = 30
	slowScrapeSecs = 60
)

func main() {
	platform := flag.String("platform", "vm", "platform: vm, k8s")
	gpu := flag.String("gpu", "none", "VM GPU type: none, nvidia, amd")
	cme := flag.Bool("cme", false, "VM only: enable the Crusoe Metrics Exporter pipeline")
	flag.Parse()

	if err := run(*platform, *gpu, *cme); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(platform, gpu string, cme bool) error {
	var (
		out []byte
		err error
	)

	switch platform {
	case "vm":
		out, err = renderVM(gpu, cme)
	case "k8s":
		out, err = renderK8s()
	default:
		return fmt.Errorf("%w: %q", errUnknownPlatform, platform)
	}

	if err != nil {
		return err
	}

	if _, err := os.Stdout.Write(out); err != nil {
		return fmt.Errorf("writing vector config to stdout: %w", err)
	}

	return nil
}

func renderVM(gpu string, cme bool) ([]byte, error) {
	gpuType, err := parseGPUType(gpu)
	if err != nil {
		return nil, err
	}

	out, err := vector.GenerateVM(vector.VMConfig{GPUType: gpuType, EnableCME: cme})
	if err != nil {
		return nil, fmt.Errorf("generating vm vector config: %w", err)
	}

	return out, nil
}

// renderK8s renders a representative full-pipeline K8s config: one pod of each
// discovered type plus a custom-metrics pod, with every exporter enabled.
func renderK8s() ([]byte, error) {
	pods := []vector.ClassifiedPod{
		{Name: "dcgm-1", IP: "10.0.0.1", Type: vector.PodTypeDCGM},
		{Name: "amd-1", IP: "10.0.0.2", Type: vector.PodTypeAMD},
		{Name: "ksm-1", IP: "10.0.0.3", Type: vector.PodTypeKSM},
		{Name: "cme-1", IP: "10.0.0.5", Type: vector.PodTypeCME},
		{
			Name: "svc-x-1", IP: "10.2.0.1", Type: vector.PodTypeCustom,
			Port: customPort, Path: "/metrics", DeploymentName: "svc",
		},
	}

	metrics := []string{"/metrics"}
	slurmPaths := []string{"/metrics/nodes"}
	cfg := vector.K8sConfig{
		DCGM:  vector.ExporterConfig{Enabled: true, Port: dcgmPort, Paths: metrics, ScrapeInterval: fastScrapeSecs},
		AMD:   vector.ExporterConfig{Enabled: true, Port: amdPort, Paths: metrics, ScrapeInterval: slowScrapeSecs},
		KSM:   vector.ExporterConfig{Enabled: true, Port: ksmPort, Paths: metrics, ScrapeInterval: slowScrapeSecs},
		CME:   vector.ExporterConfig{Enabled: true, Port: cmePort, Paths: metrics, ScrapeInterval: slowScrapeSecs},
		Slurm: vector.ExporterConfig{Enabled: true, Port: slurmPort, Paths: slurmPaths, ScrapeInterval: slowScrapeSecs},

		CustomMetricsEnabled:       true,
		CustomMetricsDefaultPort:   customPort,
		CustomMetricsDefaultPath:   "/metrics",
		CustomMetricsDefaultScrape: fastScrapeSecs,
		LogsEnabled:                true,
		OperatorLogNamespaces:      []string{"nvidia-gpu-operator", "nvidia-network-operator"},

		SinkEndpoint: "https://cms-monitoring.example.com",
		NodeLabels: vector.NodeLabels{
			VMID:         "test-vm-id",
			NodepoolID:   "test-nodepool-id",
			InstanceType: "test-instance-type",
			Hostname:     "test-node",
		},
	}

	out, err := vector.GenerateK8s(pods, nil, cfg)
	if err != nil {
		return nil, fmt.Errorf("generating k8s vector config: %w", err)
	}

	return out, nil
}

func parseGPUType(gpu string) (vector.GPUType, error) {
	switch gpu {
	case "none":
		return vector.GPUNone, nil
	case "nvidia":
		return vector.GPUNvidia, nil
	case "amd":
		return vector.GPUAMD, nil
	default:
		return vector.GPUNone, fmt.Errorf("%w: %q", errUnknownGPUType, gpu)
	}
}
