package k8sexec

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/bugreport"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/vector"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/version"
)

// --- K8sGenerator (router) tests ---

// recordingGen records whether Generate ran and with which GPU. Implements reportGenerator.
type recordingGen struct {
	called bool
	gotGPU vector.GPUType
}

func (r *recordingGen) Generate(_ context.Context, gpu vector.GPUType, _ string) (string, error) {
	r.called = true
	r.gotGPU = gpu

	return "/path/report.gz", nil
}

// recordingRunner also records Health, so it satisfies runnerGenerator.
type recordingRunner struct {
	recordingGen
	healthCalled bool
}

func (r *recordingRunner) Health(_ context.Context) (string, error) {
	r.healthCalled = true

	return "runner-v1", nil
}

func testNode(name, instanceType string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{nodeLabelInstanceType: instanceType}},
	}
}

func newK8sGen(gpu vector.GPUType, instanceType string) (*K8sGenerator, *recordingGen, *recordingRunner) {
	exec := &recordingGen{}
	runner := &recordingRunner{}
	client := fake.NewSimpleClientset(testNode("test-node", instanceType))
	gen := NewK8sGenerator(client, exec, runner, "test-node", gpu)

	return gen, exec, runner
}

func TestK8sGenerator_RoutesAMDToRunner(t *testing.T) {
	gen, exec, runner := newK8sGen(vector.GPUAMD, "mi300x.1x")

	_, err := gen.Generate(context.Background(), vector.GPUAMD, "evt")
	require.NoError(t, err)
	assert.True(t, runner.called, "AMD should route to the host runner")
	assert.False(t, exec.called, "AMD should not exec into a driver pod")
}

func TestK8sGenerator_RoutesBundledNvidiaToRunner(t *testing.T) {
	for _, instanceType := range []string{"gb200-320gb.1x", "gb300-288gb-nvl-ib.4x"} {
		gen, exec, runner := newK8sGen(vector.GPUNvidia, instanceType)

		_, err := gen.Generate(context.Background(), vector.GPUNvidia, "evt")
		require.NoError(t, err)
		assert.Truef(t, runner.called, "%s has a bundled host driver -> runner", instanceType)
		assert.Falsef(t, exec.called, "%s should not exec into a driver pod", instanceType)
	}
}

func TestK8sGenerator_RoutesOperatorNvidiaToExec(t *testing.T) {
	gen, exec, runner := newK8sGen(vector.GPUNvidia, "a100-80gb.1x")

	_, err := gen.Generate(context.Background(), vector.GPUNvidia, "evt")
	require.NoError(t, err)
	assert.True(t, exec.called, "GPU Operator NVIDIA -> exec into driver pod")
	assert.False(t, runner.called)
}

func TestK8sGenerator_HealthOperatorPingsAPI(t *testing.T) {
	gen, _, runner := newK8sGen(vector.GPUNvidia, "a100-80gb.1x")

	ver, err := gen.Health(context.Background())
	require.NoError(t, err)
	assert.Equal(t, version.Version, ver)
	assert.False(t, runner.healthCalled, "operator node has no runner to poll")
}

func TestK8sGenerator_HealthBundledPollsRunner(t *testing.T) {
	gen, _, runner := newK8sGen(vector.GPUAMD, "mi300x.1x")

	ver, err := gen.Health(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "runner-v1", ver)
	assert.True(t, runner.healthCalled)
}

func TestK8sGenerator_HealthOperatorAPIError(t *testing.T) {
	exec := &recordingGen{}
	runner := &recordingRunner{}
	// No node object -> API Get fails -> Health surfaces it as unavailable.
	client := fake.NewSimpleClientset()
	gen := NewK8sGenerator(client, exec, runner, "missing-node", vector.GPUNvidia)

	_, err := gen.Health(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), string(bugreport.CodeScriptUnavailable))
}

func TestK8sBundledDriver(t *testing.T) {
	tests := []struct {
		gpu          vector.GPUType
		instanceType string
		want         bool
	}{
		{vector.GPUAMD, "", true},
		{vector.GPUAMD, "mi300x.1x", true},
		{vector.GPUNvidia, "gb200-320gb.1x", true},
		{vector.GPUNvidia, "GB200-320GB.1X", true}, // case-insensitive
		{vector.GPUNvidia, "gb300-288gb-nvl-ib.4x", true},
		{vector.GPUNvidia, "gb300-288gb-nvl.4x", true},
		{vector.GPUNvidia, "a100-80gb.1x", false},
		{vector.GPUNvidia, "", false},
		{vector.GPUNone, "gb200-320gb.1x", false},
	}

	for _, tt := range tests {
		assert.Equalf(t, tt.want, K8sBundledDriver(tt.gpu, tt.instanceType),
			"gpu=%v instanceType=%q", tt.gpu, tt.instanceType)
	}
}

// Ensure the router surfaces a node-read failure during routing (not just Health).
func TestK8sGenerator_RoutingNodeReadError(t *testing.T) {
	exec := &recordingGen{}
	runner := &recordingRunner{}
	client := fake.NewSimpleClientset() // no node
	gen := NewK8sGenerator(client, exec, runner, "missing-node", vector.GPUNvidia)

	_, err := gen.Generate(context.Background(), vector.GPUNvidia, "evt")
	require.Error(t, err)
	assert.False(t, exec.called)
	assert.False(t, runner.called)
}

// --- ExecGenerator (driver-pod exec) tests ---

// fakePodExecutor records exec calls and simulates the three exec steps by command name.
type fakePodExecutor struct {
	calls    [][]string
	genErr   error
	catBytes []byte
	catErr   error
}

func (f *fakePodExecutor) exec(_ context.Context, _, _, _ string, w io.Writer, command ...string) error {
	f.calls = append(f.calls, command)

	switch {
	case len(command) > 0 && command[0] == "/bin/sh": // runTool
		if f.genErr != nil {
			return f.genErr
		}

		_, _ = io.WriteString(w, execMarker+"\n")
	case len(command) > 0 && command[0] == "cat": // download
		if f.catErr != nil {
			return f.catErr
		}

		_, _ = w.Write(f.catBytes)
	case len(command) > 0 && command[0] == "rm": // cleanup
	}

	return nil
}

func driverPod(name, node, container string, labeled bool, phase corev1.PodPhase) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "nvidia-gpu-operator"},
		Spec:       corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: container}}},
		Status:     corev1.PodStatus{Phase: phase},
	}
	if labeled {
		pod.Labels = map[string]string{"app.kubernetes.io/component": "nvidia-driver"}
	}

	return pod
}

func newExecGen(t *testing.T, exec *fakePodExecutor, objs ...*corev1.Pod) *ExecGenerator {
	t.Helper()

	client := fake.NewSimpleClientset()
	for _, o := range objs {
		_, err := client.CoreV1().Pods(o.Namespace).Create(context.Background(), o, metav1.CreateOptions{})
		require.NoError(t, err)
	}

	return &ExecGenerator{
		client:          client,
		executor:        exec,
		outputDir:       t.TempDir(),
		nodeName:        "test-node",
		driverNamespace: "nvidia-gpu-operator",
		now:             func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
}

func TestExecGenerator_Generate(t *testing.T) {
	exec := &fakePodExecutor{catBytes: []byte("gzip-archive-bytes")}
	gen := newExecGen(t, exec, driverPod("nvidia-driver-abc", "test-node", "nvidia-driver-ctr", true, corev1.PodRunning))

	path, err := gen.Generate(context.Background(), vector.GPUNvidia, "evt-1")
	require.NoError(t, err)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "gzip-archive-bytes", string(data))

	// runTool, download (cat), and cleanup (rm) each ran.
	assert.Len(t, exec.calls, 3)
	assert.Equal(t, "cat", exec.calls[1][0])
	assert.Equal(t, "rm", exec.calls[2][0])
}

func TestExecGenerator_FindsPodByNamePrefixFallback(t *testing.T) {
	exec := &fakePodExecutor{catBytes: []byte("x")}
	// Unlabeled pod, matched only by the "nvidia-gpu-driver" name prefix.
	gen := newExecGen(t, exec, driverPod("nvidia-gpu-driver-daemonset-xyz", "test-node", "driver", false, corev1.PodRunning))

	_, err := gen.Generate(context.Background(), vector.GPUNvidia, "")
	require.NoError(t, err)
}

func TestExecGenerator_NoDriverPod(t *testing.T) {
	gen := newExecGen(t, &fakePodExecutor{})

	_, err := gen.Generate(context.Background(), vector.GPUNvidia, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), string(bugreport.CodeDriverPodNotFound))
}

func TestExecGenerator_SkipsNonRunningPod(t *testing.T) {
	gen := newExecGen(t, &fakePodExecutor{}, driverPod("nvidia-driver-abc", "test-node", "driver", true, corev1.PodPending))

	_, err := gen.Generate(context.Background(), vector.GPUNvidia, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), string(bugreport.CodeDriverPodNotFound))
}

func TestExecGenerator_ToolFailure(t *testing.T) {
	exec := &fakePodExecutor{genErr: errors.New("boom")}
	gen := newExecGen(t, exec, driverPod("nvidia-driver-abc", "test-node", "driver", true, corev1.PodRunning))

	_, err := gen.Generate(context.Background(), vector.GPUNvidia, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), string(bugreport.CodeExecError))
}

func TestExecGenerator_EmptyDownload(t *testing.T) {
	exec := &fakePodExecutor{catBytes: nil} // cat produces no bytes
	gen := newExecGen(t, exec, driverPod("nvidia-driver-abc", "test-node", "driver", true, corev1.PodRunning))

	_, err := gen.Generate(context.Background(), vector.GPUNvidia, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), string(bugreport.CodeNoOutput))
}

func TestDriverContainer(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
		{Name: "openvpn-client"}, {Name: "nvidia-driver-ctr"},
	}}}
	name, err := driverContainer(pod)
	require.NoError(t, err)
	assert.Equal(t, "nvidia-driver-ctr", name)

	// Falls back to the first container when none is named like a driver.
	pod = &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}}}
	name, err = driverContainer(pod)
	require.NoError(t, err)
	assert.Equal(t, "main", name)

	_, err = driverContainer(&corev1.Pod{})
	require.Error(t, err)
}

func TestShQuote(t *testing.T) {
	assert.Equal(t, "'/tmp/report.log'", shQuote("/tmp/report.log"))
	assert.Equal(t, `'a'\''b'`, shQuote("a'b"))
	assert.False(t, strings.Contains(shQuote("safe"), `\`))
}
