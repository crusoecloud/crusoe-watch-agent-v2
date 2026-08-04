package k8sexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/bugreport"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/vector"
)

// This file implements the GPU-operator collection path: on most K8s NVIDIA nodes the
// driver userspace (nvidia-bug-report.sh) lives only inside the operator's driver pod,
// so cwa-manager execs the tool there and streams the archive back — mirroring v1.

const (
	// driverPodLabelSelector is set by the official NVIDIA GPU Operator on its driver pods.
	driverPodLabelSelector = "app.kubernetes.io/component=nvidia-driver"
	// driverPodNamePrefix is the fallback match for custom/legacy driver deployments.
	driverPodNamePrefix = "nvidia-gpu-driver"
	// remoteReportDir is where the tool writes inside the driver pod before streaming back.
	remoteReportDir = "/tmp"
	// driverPodBugReportScript is invoked by name (PATH-resolved) inside the driver pod.
	driverPodBugReportScript = "nvidia-bug-report.sh"
	// execMarker confirms nvidia-bug-report.sh ran; the tool otherwise prints nothing to stdout.
	execMarker = "CWA_BUG_REPORT_OK"
)

// errNoDriverContainer means the driver pod exposed no usable container.
var errNoDriverContainer = errors.New("no driver container in pod")

// podExecutor runs command in a pod container, streaming stdout to dst.
type podExecutor interface {
	exec(ctx context.Context, namespace, pod, container string, dst io.Writer, command ...string) error
}

// ExecGenerator collects an NVIDIA bug report by exec'ing into the GPU Operator driver pod.
type ExecGenerator struct {
	client          kubernetes.Interface
	executor        podExecutor
	outputDir       string
	nodeName        string
	driverNamespace string
	now             func() time.Time
}

// NewExecGenerator builds an ExecGenerator for the given node and driver namespace.
func NewExecGenerator(
	client kubernetes.Interface, restCfg *rest.Config, outputDir, nodeName, driverNamespace string,
) *ExecGenerator {
	return &ExecGenerator{
		client:          client,
		executor:        &spdyExecutor{restCfg: restCfg},
		outputDir:       outputDir,
		nodeName:        nodeName,
		driverNamespace: driverNamespace,
		now:             time.Now,
	}
}

// Generate execs nvidia-bug-report.sh in the driver pod and returns the local archive path.
func (g *ExecGenerator) Generate(ctx context.Context, _ vector.GPUType, eventID string) (string, error) {
	pod, err := g.findDriverPod(ctx)
	if err != nil {
		return "", err
	}

	container, err := driverContainer(pod)
	if err != nil {
		return "", bugreport.CodeExecError.Errorf("driver pod %s: %w", pod.Name, err)
	}

	if err := os.MkdirAll(g.outputDir, bugreport.DirPerm); err != nil {
		return "", bugreport.CodeInternal.Errorf("creating output dir: %w", err)
	}

	// nvidia-bug-report.sh appends .gz to --output-file.
	remoteBase := filepath.Join(remoteReportDir, bugreport.ReportBase(g.nodeName+"-"+eventID, g.now())+".log")
	remoteArchive := remoteBase + ".gz"

	defer g.cleanupRemote(ctx, pod.Name, container, remoteArchive)

	if err := g.runTool(ctx, pod.Name, container, remoteBase); err != nil {
		return "", err
	}

	return g.download(ctx, pod.Name, container, remoteArchive, filepath.Base(remoteArchive))
}

// runTool executes nvidia-bug-report.sh in the pod, mapping failures to CWA-BR codes.
func (g *ExecGenerator) runTool(ctx context.Context, pod, container, remoteBase string) error {
	// nvidia-bug-report.sh writes its own <base>.gz, so stdout is discarded here.
	cmd := []string{
		"/bin/sh", "-c",
		driverPodBugReportScript + " --output-file " + shQuote(remoteBase) + " && echo " + execMarker,
	}

	var out bytes.Buffer
	if err := g.executor.exec(ctx, g.driverNamespace, pod, container, &out, cmd...); err != nil {
		return classifyExecErr(err, out.String())
	}

	if !strings.Contains(out.String(), execMarker) {
		return bugreport.CodeNoOutput.Errorf("nvidia-bug-report.sh produced no report in pod %s: %q", pod, out.String())
	}

	return nil
}

// download streams the remote archive to a local file and returns its path.
func (g *ExecGenerator) download(ctx context.Context, pod, container, remotePath, name string) (string, error) {
	local := filepath.Join(g.outputDir, name)

	file, err := os.OpenFile(local, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, bugreport.ReportPerm)
	if err != nil {
		return "", bugreport.CodeInternal.Errorf("creating report file: %w", err)
	}

	defer func() { _ = file.Close() }()

	if err := g.executor.exec(ctx, g.driverNamespace, pod, container, file, "cat", remotePath); err != nil {
		_ = os.Remove(local)

		return "", bugreport.CodeDownloadFailed.Errorf("downloading report from pod %s: %w", pod, err)
	}

	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		_ = os.Remove(local)

		return "", bugreport.CodeNoOutput.Errorf("empty report downloaded from pod %s", pod)
	}

	return local, nil
}

// cleanupRemote best-effort removes the temp archive from the driver pod.
func (g *ExecGenerator) cleanupRemote(ctx context.Context, pod, container, remotePath string) {
	//nolint:errcheck // best-effort cleanup of a temp file; nothing actionable on failure
	g.executor.exec(ctx, g.driverNamespace, pod, container, io.Discard, "rm", "-f", remotePath)
}

// findDriverPod locates the running NVIDIA driver pod on this node: first by the GPU
// Operator label, then by name prefix for custom deployments.
func (g *ExecGenerator) findDriverPod(ctx context.Context) (*corev1.Pod, error) {
	nodeField := fields.OneTermEqualSelector("spec.nodeName", g.nodeName).String()

	byLabel, err := g.client.CoreV1().Pods(g.driverNamespace).List(ctx, metav1.ListOptions{
		FieldSelector: nodeField,
		LabelSelector: driverPodLabelSelector,
	})
	if err != nil {
		return nil, bugreport.CodeDriverPodNotFound.Errorf("listing driver pods: %w", err)
	}

	if pod := runningPod(byLabel.Items, ""); pod != nil {
		return pod, nil
	}

	byNode, err := g.client.CoreV1().Pods(g.driverNamespace).List(ctx, metav1.ListOptions{FieldSelector: nodeField})
	if err != nil {
		return nil, bugreport.CodeDriverPodNotFound.Errorf("listing driver pods: %w", err)
	}

	if pod := runningPod(byNode.Items, driverPodNamePrefix); pod != nil {
		return pod, nil
	}

	return nil, bugreport.CodeDriverPodNotFound.Errorf(
		"no running NVIDIA driver pod on node %s in namespace %s", g.nodeName, g.driverNamespace)
}

// runningPod returns the first Running pod matching prefix.
func runningPod(pods []corev1.Pod, prefix string) *corev1.Pod {
	for i := range pods {
		pod := &pods[i]
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}

		if prefix == "" || strings.HasPrefix(pod.Name, prefix) {
			return pod
		}
	}

	return nil
}

// driverContainer picks the container to exec into: one named like a driver, else the first.
func driverContainer(pod *corev1.Pod) (string, error) {
	for _, c := range pod.Spec.Containers {
		if strings.Contains(strings.ToLower(c.Name), "driver") {
			return c.Name, nil
		}
	}

	if len(pod.Spec.Containers) > 0 {
		return pod.Spec.Containers[0].Name, nil
	}

	return "", errNoDriverContainer
}

// shQuote wraps s in single quotes for safe embedding in an /bin/sh -c command.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// classifyExecErr maps a remote-exec failure to a CWA-BR code.
// A non-zero exit is a tool failure, anything else is a transport/exec error.
func classifyExecErr(err error, stderr string) error {
	var codeExit utilexec.CodeExitError
	if errors.As(err, &codeExit) {
		return bugreport.CodeScriptFailed.Errorf("nvidia-bug-report.sh exited %d: %w: %s", codeExit.Code, err, stderr)
	}

	return bugreport.CodeExecError.Errorf("exec nvidia-bug-report.sh: %w: %s", err, stderr)
}

// spdyExecutor is the production podExecutor over an SPDY stream to the API server.
type spdyExecutor struct {
	restCfg *rest.Config
}

func (s *spdyExecutor) exec(
	ctx context.Context, namespace, pod, container string, dst io.Writer, command ...string,
) error {
	client, err := kubernetes.NewForConfig(s.restCfg)
	if err != nil {
		return fmt.Errorf("building kubernetes client: %w", err)
	}

	req := client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(s.restCfg, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("creating SPDY executor: %w", err)
	}

	var stderr bytes.Buffer

	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: dst, Stderr: &stderr})
	if err != nil {
		// Surface remote stderr alongside the transport error for diagnostics.
		if stderr.Len() > 0 {
			return &execStreamError{err: err, stderr: stderr.String()}
		}

		return fmt.Errorf("streaming exec: %w", err)
	}

	return nil
}

// execStreamError wraps a stream failure with the captured remote stderr while
// preserving errors.As matching against the underlying (e.g. CodeExitError).
type execStreamError struct {
	err    error
	stderr string
}

func (e *execStreamError) Error() string { return e.err.Error() + ": " + e.stderr }
func (e *execStreamError) Unwrap() error { return e.err }
