package watcher

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/vector"
)

const (
	testDefaultPort = 9100
	testDefaultPath = "/metrics"
)

func runningPod(name, ip string, labels, annotations map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      labels,
			Annotations: annotations,
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: ip,
		},
	}
}

func TestClassifyPod_DCGM(t *testing.T) {
	pod := runningPod("dcgm-exporter-abc12", "10.0.0.1",
		map[string]string{"app": "nvidia-dcgm-exporter"}, nil)

	cp, ok := ClassifyPod(pod, testDefaultPort, testDefaultPath)
	require.True(t, ok)
	assert.Equal(t, vector.PodTypeDCGM, cp.Type)
	assert.Equal(t, "10.0.0.1", cp.IP)
	assert.Equal(t, "dcgm-exporter-abc12", cp.Name)
}

func TestClassifyPod_KSM(t *testing.T) {
	pod := runningPod("kube-state-metrics-7f9b4-xyz", "10.0.0.2",
		map[string]string{"app.kubernetes.io/name": "kube-state-metrics"}, nil)

	cp, ok := ClassifyPod(pod, testDefaultPort, testDefaultPath)
	require.True(t, ok)
	assert.Equal(t, vector.PodTypeKSM, cp.Type)
}

func TestClassifyPod_Slurm(t *testing.T) {
	pod := runningPod("slurmctld-0", "10.0.0.3",
		map[string]string{"app.kubernetes.io/name": "slurmctld"}, nil)

	cp, ok := ClassifyPod(pod, testDefaultPort, testDefaultPath)
	require.True(t, ok)
	assert.Equal(t, vector.PodTypeSlurm, cp.Type)
}

func TestClassifyPod_CME(t *testing.T) {
	pod := runningPod("crusoe-metrics-exporter-abc", "10.0.0.4",
		map[string]string{"app.kubernetes.io/name": "crusoe-metrics-exporter"}, nil)

	cp, ok := ClassifyPod(pod, testDefaultPort, testDefaultPath)
	require.True(t, ok)
	assert.Equal(t, vector.PodTypeCME, cp.Type)
}

func TestClassifyPod_AMD(t *testing.T) {
	pod := runningPod("metrics-exporter-abc", "10.0.0.5",
		map[string]string{"app.kubernetes.io/name": "metrics-exporter"}, nil)
	pod.Namespace = "kube-amd-gpu"

	cp, ok := ClassifyPod(pod, testDefaultPort, testDefaultPath)
	require.True(t, ok)
	assert.Equal(t, vector.PodTypeAMD, cp.Type)
}

func TestClassifyPod_AMDWrongNamespace(t *testing.T) {
	pod := runningPod("metrics-exporter-abc", "10.0.0.5",
		map[string]string{"app.kubernetes.io/name": "metrics-exporter"}, nil)
	pod.Namespace = "default"

	_, ok := ClassifyPod(pod, testDefaultPort, testDefaultPath)
	assert.False(t, ok)
}

func TestClassifyPod_Custom(t *testing.T) {
	pod := runningPod("my-app-7fb96c846b-abc12", "10.0.0.6", nil,
		map[string]string{
			"crusoe.ai/scrape": "true",
			"crusoe.ai/port":   "9200",
			"crusoe.ai/path":   "/custom-metrics",
			"crusoe.ai/app_id": "app-xyz",
		})

	cp, ok := ClassifyPod(pod, testDefaultPort, testDefaultPath)
	require.True(t, ok)
	assert.Equal(t, vector.PodTypeCustom, cp.Type)
	assert.Equal(t, 9200, cp.Port)
	assert.Equal(t, "/custom-metrics", cp.Path)
	assert.Equal(t, "app-xyz", cp.AppID)
	assert.Equal(t, "my-app", cp.DeploymentName)
}

func TestClassifyPod_CustomDefaults(t *testing.T) {
	pod := runningPod("svc-a-replicaset-pod", "10.0.0.7", nil,
		map[string]string{"crusoe.ai/scrape": "true"})

	cp, ok := ClassifyPod(pod, testDefaultPort, testDefaultPath)
	require.True(t, ok)
	assert.Equal(t, vector.PodTypeCustom, cp.Type)
	assert.Equal(t, testDefaultPort, cp.Port)
	assert.Equal(t, testDefaultPath, cp.Path)
	assert.Equal(t, "", cp.AppID)
}

func TestClassifyPod_CustomInvalidPort(t *testing.T) {
	pod := runningPod("app-abc-123", "10.0.0.8", nil,
		map[string]string{
			"crusoe.ai/scrape": "true",
			"crusoe.ai/port":   "not-a-number",
		})

	cp, ok := ClassifyPod(pod, testDefaultPort, testDefaultPath)
	require.True(t, ok)
	assert.Equal(t, testDefaultPort, cp.Port, "should fall back to default on invalid port")
}

func TestClassifyPod_CustomTakesPrecedence(t *testing.T) {
	// Pod has both the custom annotation and a known label; custom wins.
	pod := runningPod("dcgm-exporter-abc", "10.0.0.9",
		map[string]string{"app": "nvidia-dcgm-exporter"},
		map[string]string{"crusoe.ai/scrape": "true"})

	cp, ok := ClassifyPod(pod, testDefaultPort, testDefaultPath)
	require.True(t, ok)
	assert.Equal(t, vector.PodTypeCustom, cp.Type)
}

func TestClassifyPod_NotRunning(t *testing.T) {
	pod := runningPod("dcgm-exporter-abc", "10.0.0.1",
		map[string]string{"app": "nvidia-dcgm-exporter"}, nil)
	pod.Status.Phase = corev1.PodPending

	_, ok := ClassifyPod(pod, testDefaultPort, testDefaultPath)
	assert.False(t, ok)
}

func TestClassifyPod_NoIP(t *testing.T) {
	pod := runningPod("dcgm-exporter-abc", "",
		map[string]string{"app": "nvidia-dcgm-exporter"}, nil)

	_, ok := ClassifyPod(pod, testDefaultPort, testDefaultPath)
	assert.False(t, ok)
}

func TestClassifyPod_Terminating(t *testing.T) {
	pod := runningPod("dcgm-exporter-abc", "10.0.0.1",
		map[string]string{"app": "nvidia-dcgm-exporter"}, nil)
	now := metav1.NewTime(time.Now())
	pod.DeletionTimestamp = &now

	_, ok := ClassifyPod(pod, testDefaultPort, testDefaultPath)
	assert.False(t, ok, "terminating pods should be excluded")
}

func TestClassifyPod_Irrelevant(t *testing.T) {
	pod := runningPod("nginx-abc-123", "10.0.0.10",
		map[string]string{"app": "nginx"}, nil)

	_, ok := ClassifyPod(pod, testDefaultPort, testDefaultPath)
	assert.False(t, ok)
}

func TestExtractDeploymentName(t *testing.T) {
	tests := []struct {
		podName  string
		expected string
	}{
		{"my-app-7fb96c846b-abc12", "my-app"},
		{"nginx-deployment-replicaset-pod", "nginx-deployment"},
		{"simple-ab", ""},                       // <=2 segments, no deployment name
		{"solo", ""},                            // single segment
		{"a-b-c-d-e", "a-b-c"},                 // multiple dashes
		{"svc-a-replicaset-pod", "svc-a"},       // service name with dash
	}

	for _, tt := range tests {
		assert.Equal(t, tt.expected, ExtractDeploymentName(tt.podName), "podName=%s", tt.podName)
	}
}
