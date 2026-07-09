// Command vector-config-dump renders VM Vector config variants for CI
// validation and debugging. Unlike cwa-manager, which detects its config
// inputs at runtime, this dev tool takes them as flags so any variant can be
// rendered on any machine.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/vector"
)

var errUnknownGPUType = errors.New("unknown GPU type (use none, nvidia, amd)")

func main() {
	gpu := flag.String("gpu", "none", "GPU type: none, nvidia, amd")
	cme := flag.Bool("cme", false, "enable the Crusoe Metrics Exporter pipeline")
	flag.Parse()

	if err := run(*gpu, *cme); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(gpu string, cme bool) error {
	gpuType, err := parseGPUType(gpu)
	if err != nil {
		return err
	}

	out, err := vector.GenerateVM(vector.VMConfig{GPUType: gpuType, EnableCME: cme})
	if err != nil {
		return fmt.Errorf("generating vector config: %w", err)
	}

	if _, err := os.Stdout.Write(out); err != nil {
		return fmt.Errorf("writing vector config to stdout: %w", err)
	}

	return nil
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
