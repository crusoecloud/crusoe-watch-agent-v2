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

func TestClassifyPod_TerminatingCustomKept(t *testing.T) {
	pod := runningPod("app-7fb96c846b-abc12", "10.0.0.1", nil,
		map[string]string{AnnotationScrape: "true"})
	now := metav1.NewTime(time.Now())
	pod.DeletionTimestamp = &now

	cp, ok := ClassifyPod(pod, testDefaultPort, testDefaultPath)
	require.True(t, ok, "draining custom metrics pods should still be scraped")
	assert.Equal(t, vector.PodTypeCustom, cp.Type)
}

func TestClassifyPod_Irrelevant(t *testing.T) {
	pod := runningPod("nginx-abc-123", "10.0.0.10",
		map[string]string{"app": "nginx"}, nil)

	_, ok := ClassifyPod(pod, testDefaultPort, testDefaultPath)
	assert.False(t, ok)
}

func componentPod(name, namespace string, labels, annotations map[string]string, ports ...int32) *corev1.Pod {
	pod := runningPod(name, "10.1.0.1", labels, annotations)
	pod.Namespace = namespace
	pod.Spec.NodeName = "node-1"
	container := corev1.Container{Name: "main"}
	for _, port := range ports {
		container.Ports = append(container.Ports, corev1.ContainerPort{ContainerPort: port})
	}
	pod.Spec.Containers = []corev1.Container{container}

	return pod
}

func promAnnotations(port string) map[string]string {
	return map[string]string{"prometheus.io/scrape": "true", "prometheus.io/port": port}
}

func TestClassifyPod_PlatformConnector(t *testing.T) {
	pod := componentPod("platform-connectors-4dzzd", "nvsentinel", nil, nil)

	cp, ok := ClassifyPod(pod, testDefaultPort, testDefaultPath)
	require.True(t, ok)
	assert.Equal(t, vector.PodTypeComponent, cp.Type)
	assert.Equal(t, "nvsentinel-platform-connector", cp.Job)
	assert.Equal(t, 2112, cp.Port)
	assert.Equal(t, 30, cp.ScrapeInterval)
	assert.Equal(t, map[string]string{"pod": "platform-connectors-4dzzd", "node": "node-1"}, cp.ScrapeLabels)
}

func TestClassifyPod_CoreDNS(t *testing.T) {
	pod := componentPod("coredns-5d78c9869d-abcde", "kube-system", map[string]string{"k8s-app": "kube-dns"}, nil)

	cp, ok := ClassifyPod(pod, testDefaultPort, testDefaultPath)
	require.True(t, ok)
	assert.Equal(t, "coredns", cp.Job)
	assert.Equal(t, 9153, cp.Port)
}

// kubernetes-pods components take the port from the annotation and copy the pod
// labels, with the target labels winning on a clash.
func TestClassifyPod_KubernetesPodsComponent(t *testing.T) {
	pod := componentPod("cilium-66v7b", "kube-system",
		map[string]string{"k8s-app": "cilium", "app.kubernetes.io/name": "cilium-agent", "pod": "clash"},
		promAnnotations("9962"), 9962)
	pod.Spec.Containers[0].Name = "cilium-agent"

	cp, ok := ClassifyPod(pod, testDefaultPort, testDefaultPath)
	require.True(t, ok)
	assert.Equal(t, vector.PodTypeComponent, cp.Type)
	assert.Equal(t, "kubernetes-pods", cp.Job)
	assert.Equal(t, 9962, cp.Port)
	assert.Equal(t, "/metrics", cp.Path)
	assert.Equal(t, 150, cp.ScrapeInterval)
	assert.Equal(t, map[string]string{
		"k8s_app":                "cilium",
		"app_kubernetes_io_name": "cilium-agent",
		"pod":                    "cilium-66v7b",
		"container":              "cilium-agent",
		"namespace":              "kube-system",
		"node":                   "node-1",
	}, cp.ScrapeLabels)
}

func TestClassifyPod_KubernetesPodsComponentMatches(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		labels    map[string]string
	}{
		{"cilium-operator", "kube-system", map[string]string{"io.cilium/app": "operator"}},
		{"cert-manager", "cert-manager", map[string]string{"app.kubernetes.io/instance": "cert-manager"}},
		{"cluster-autoscaler", "crusoe-system",
			map[string]string{"app.kubernetes.io/name": "crusoecloud-cluster-autoscaler"}},
		{"crusoe-mitigator", "crusoe-mitigator", map[string]string{"app": "crusoe-mitigator"}},
	}

	for _, tt := range tests {
		pod := componentPod(tt.name+"-abc", tt.namespace, tt.labels, promAnnotations("9402"), 9402)
		cp, ok := ClassifyPod(pod, testDefaultPort, testDefaultPath)
		require.True(t, ok, tt.name)
		assert.Equal(t, "kubernetes-pods", cp.Job, tt.name)
	}
}

// Only pods annotated for scraping on a declared container port are kept.
// Annotated pods that match no rule are not components.
func TestClassifyPod_KubernetesPodsComponentNeedsAnnotatedPort(t *testing.T) {
	labels := map[string]string{"k8s-app": "cilium"}

	for name, pod := range map[string]*corev1.Pod{
		"no annotations":    componentPod("cilium-a", "kube-system", labels, nil, 9962),
		"undeclared port":   componentPod("cilium-b", "kube-system", labels, promAnnotations("9962"), 4240),
		"non-numeric port":  componentPod("cilium-c", "kube-system", labels, promAnnotations("metrics"), 9962),
		"another namespace": componentPod("cilium-d", "default", labels, promAnnotations("9962"), 9962),
		"unlisted pod":      componentPod("node-local-dns-e", "kube-system", nil, promAnnotations("9253"), 9253),
	} {
		_, ok := ClassifyPod(pod, testDefaultPort, testDefaultPath)
		assert.False(t, ok, name)
	}
}

// Service-backed components name the job after the Service.
func TestClassifyPod_ServiceEndpointComponents(t *testing.T) {
	tests := []struct {
		labelName string
		namespace string
		port      int32
		job       string
	}{
		{"kubelet-csr-approver", "kube-system", 8080, "kubelet-csr-approver"},
		{"atero-models-validator", "cell-a", 9090, "atero-models-validator"},
		{"atero-models-monitor", "cell-b", 19427, "atero-models-monitor-metrics"},
	}

	for _, tt := range tests {
		pod := componentPod(tt.labelName+"-abc", tt.namespace,
			map[string]string{"app.kubernetes.io/name": tt.labelName}, nil, tt.port)
		cp, ok := ClassifyPod(pod, testDefaultPort, testDefaultPath)
		require.True(t, ok, tt.job)
		assert.Equal(t, tt.job, cp.Job)
		assert.Equal(t, int(tt.port), cp.Port, tt.job)
		assert.Equal(t, 300, cp.ScrapeInterval, tt.job)
		assert.Equal(t, tt.namespace, cp.ScrapeLabels["namespace"], tt.job)
		assert.Equal(t, "main", cp.ScrapeLabels["container"], tt.job)
		assert.Equal(t, tt.labelName, cp.ScrapeLabels["app_kubernetes_io_name"], tt.job)
	}
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
