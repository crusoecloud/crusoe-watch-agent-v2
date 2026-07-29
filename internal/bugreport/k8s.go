package bugreport

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/vector"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/version"
)

// K8sGenerator routes a K8s bug-report collection by where the GPU driver lives:
//   - Bundled driver (AMD, or NVIDIA GB200): host report-runner over a unix socket (same as VM mode).
//   - GPU Operator NVIDIA (rest of the fleet): exec into the driver pod.
type K8sGenerator struct {
	exec     reportGenerator
	runner   runnerGenerator
	client   kubernetes.Interface
	nodeName string
	gpu      vector.GPUType
}

// runnerGenerator is the bundled-driver path: a report-runner reached over a socket.
type runnerGenerator interface {
	Generate(ctx context.Context, gpu vector.GPUType, eventID string) (string, error)
	Health(ctx context.Context) (string, error)
}

// NewK8sGenerator wires the operator-exec and bundled-runner paths for one node.
func NewK8sGenerator(
	client kubernetes.Interface, exec reportGenerator, runner runnerGenerator, nodeName string, gpu vector.GPUType,
) *K8sGenerator {
	return &K8sGenerator{
		exec:     exec,
		runner:   runner,
		client:   client,
		nodeName: nodeName,
		gpu:      gpu,
	}
}

// Generate routes to the bundled runner or the operator-exec path per the node's driver placement.
func (g *K8sGenerator) Generate(ctx context.Context, gpu vector.GPUType, eventID string) (string, error) {
	bundled, err := g.bundled(ctx, gpu)
	if err != nil {
		return "", err
	}

	if bundled {
		path, err := g.runner.Generate(ctx, gpu, eventID)
		if err != nil {
			return "", fmt.Errorf("bundled runner: %w", err)
		}

		return path, nil
	}

	path, err := g.exec.Generate(ctx, gpu, eventID)
	if err != nil {
		return "", fmt.Errorf("operator exec: %w", err)
	}

	return path, nil
}

// Health preflights the path this node would use. Bundled nodes poll the report-runner;
// operator nodes confirm the API server is reachable (exec needs it) and report the agent version.
func (g *K8sGenerator) Health(ctx context.Context) (string, error) {
	bundled, err := g.bundled(ctx, g.gpu)
	if err != nil {
		return "", err
	}

	if bundled {
		status, err := g.runner.Health(ctx)
		if err != nil {
			return "", fmt.Errorf("report-runner health: %w", err)
		}

		return status, nil
	}

	if _, err := g.client.CoreV1().Nodes().Get(ctx, g.nodeName, metav1.GetOptions{}); err != nil {
		return "", CodeScriptUnavailable.Errorf("kubernetes API unreachable: %w", err)
	}

	return version.Version, nil
}

// bundled reports whether this node collects via the host runner rather than a driver pod.
func (g *K8sGenerator) bundled(ctx context.Context, gpu vector.GPUType) (bool, error) {
	if gpu == vector.GPUAMD {
		return true, nil
	}

	instanceType, err := g.instanceType(ctx)
	if err != nil {
		return false, err
	}

	return K8sBundledDriver(gpu, instanceType), nil
}

// instanceType reads the node's instance-type label.
func (g *K8sGenerator) instanceType(ctx context.Context) (string, error) {
	node, err := g.client.CoreV1().Nodes().Get(ctx, g.nodeName, metav1.GetOptions{})
	if err != nil {
		return "", CodeInternal.Errorf("reading node %s: %w", g.nodeName, err)
	}

	return node.Labels[nodeLabelInstanceType], nil
}

// nodeLabelInstanceType is the GA Kubernetes instance-type label.
const nodeLabelInstanceType = "node.kubernetes.io/instance-type"

// K8sBundledDriver reports whether a node's GPU driver is bundled on the host (collect via
// the host runner) rather than deployed as a GPU Operator driver pod (collect via exec).
// AMD always bundles its driver; NVIDIA bundles only on GB200.
func K8sBundledDriver(gpu vector.GPUType, instanceType string) bool {
	if gpu == vector.GPUAMD {
		return true
	}

	return gpu == vector.GPUNvidia && strings.Contains(strings.ToLower(instanceType), "gb200")
}
