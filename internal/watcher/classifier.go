// Package watcher implements Kubernetes pod and ConfigMap watching for
// dynamic Vector configuration management in cwa-manager.
package watcher

import (
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/vector"
)

// Kubernetes label keys used for pod classification.
const (
	labelApp        = "app"
	labelAppK8sName = "app.kubernetes.io/name"
)

// Label values that identify known exporter pod types.
const (
	dcgmLabelValue  = "nvidia-dcgm-exporter"
	ksmLabelValue   = "kube-state-metrics"
	slurmLabelValue = "slurmctld"
	cmeLabelValue   = "crusoe-metrics-exporter"
	amdLabelValue   = "metrics-exporter"
	amdNamespace    = "kube-amd-gpu"
)

// Crusoe-specific annotations for custom metrics opt-in.
const (
	AnnotationScrape = "crusoe.ai/scrape"
	AnnotationPort   = "crusoe.ai/port"
	AnnotationPath   = "crusoe.ai/path"
	AnnotationAppID  = "crusoe.ai/app_id"
)

// isPodActive returns true if the pod is running, has an IP, and is not being deleted.
func isPodActive(pod *corev1.Pod) bool {
	return pod.Status.PodIP != "" && pod.Status.Phase == corev1.PodRunning && pod.DeletionTimestamp == nil
}

// ClassifyPod determines whether a K8s pod is a relevant exporter and returns
// a ClassifiedPod. Returns false if the pod is not relevant.
func ClassifyPod(pod *corev1.Pod, defaultPort int, defaultPath string) (vector.ClassifiedPod, bool) {
	if !isPodActive(pod) {
		return vector.ClassifiedPod{}, false
	}

	labels := pod.Labels
	annotations := pod.Annotations

	// Custom metrics (annotation-based, checked first).
	if annotations[AnnotationScrape] == "true" {
		return classifyCustomPod(pod, annotations, defaultPort, defaultPath), true
	}

	// DCGM exporter (legacy label).
	if labels[labelApp] == dcgmLabelValue {
		return vector.ClassifiedPod{Name: pod.Name, IP: pod.Status.PodIP, Type: vector.PodTypeDCGM}, true
	}

	// KSM, Slurm, CME (standard app.kubernetes.io/name label).
	switch labels[labelAppK8sName] {
	case ksmLabelValue:
		return vector.ClassifiedPod{Name: pod.Name, IP: pod.Status.PodIP, Type: vector.PodTypeKSM}, true
	case slurmLabelValue:
		return vector.ClassifiedPod{Name: pod.Name, IP: pod.Status.PodIP, Type: vector.PodTypeSlurm}, true
	case cmeLabelValue:
		return vector.ClassifiedPod{Name: pod.Name, IP: pod.Status.PodIP, Type: vector.PodTypeCME}, true
	}

	// AMD exporter (namespace + label).
	if pod.Namespace == amdNamespace && labels[labelAppK8sName] == amdLabelValue {
		return vector.ClassifiedPod{Name: pod.Name, IP: pod.Status.PodIP, Type: vector.PodTypeAMD}, true
	}

	return vector.ClassifiedPod{}, false
}

func classifyCustomPod(
	pod *corev1.Pod, annotations map[string]string, defaultPort int, defaultPath string,
) vector.ClassifiedPod {
	port := defaultPort
	if raw, ok := annotations[AnnotationPort]; ok {
		if parsed, err := strconv.Atoi(raw); err == nil {
			port = parsed
		}
	}

	path := defaultPath
	if raw, ok := annotations[AnnotationPath]; ok && raw != "" {
		path = raw
	}

	return vector.ClassifiedPod{
		Name:           pod.Name,
		IP:             pod.Status.PodIP,
		Type:           vector.PodTypeCustom,
		Port:           port,
		Path:           path,
		AppID:          annotations[AnnotationAppID],
		DeploymentName: ExtractDeploymentName(pod.Name),
	}
}

// ExtractDeploymentName strips the ReplicaSet hash and pod hash suffixes from
// a pod name to recover the deployment name (e.g. "my-app-7fb96c846b-abc12" -> "my-app").
func ExtractDeploymentName(podName string) string {
	lastDash := strings.LastIndex(podName, "-")
	if lastDash <= 0 {
		return ""
	}

	prefix := podName[:lastDash]

	secondLastDash := strings.LastIndex(prefix, "-")
	if secondLastDash <= 0 {
		return ""
	}

	return podName[:secondLastDash]
}
