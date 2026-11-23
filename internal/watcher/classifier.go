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

// Prometheus scrape annotations, read for annotation-configured components.
const (
	annotationPromScrape = "prometheus.io/scrape"
	annotationPromPort   = "prometheus.io/port"
	annotationPromPath   = "prometheus.io/path"
)

// isPodRunning returns true if the pod is running and has an IP.
func isPodRunning(pod *corev1.Pod) bool {
	return pod.Status.PodIP != "" && pod.Status.Phase == corev1.PodRunning
}

// ClassifyPod determines whether a K8s pod is a relevant exporter and returns
// a ClassifiedPod. Returns false if the pod is not relevant.
func ClassifyPod(pod *corev1.Pod, defaultPort int, defaultPath string) (vector.ClassifiedPod, bool) {
	if !isPodRunning(pod) {
		return vector.ClassifiedPod{}, false
	}

	labels := pod.Labels
	annotations := pod.Annotations

	// Custom metrics (annotation-based, checked first). Scraped while terminating.
	if annotations[AnnotationScrape] == "true" {
		return classifyCustomPod(pod, annotations, defaultPort, defaultPath), true
	}

	if pod.DeletionTimestamp != nil {
		return vector.ClassifiedPod{}, false
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

	return classifyComponent(pod)
}

// classifyComponent classifies pod by the first component rule it matches.
func classifyComponent(pod *corev1.Pod) (vector.ClassifiedPod, bool) {
	for _, rule := range componentRules {
		if rule.matches(pod) {
			return classifyComponentPod(pod, rule)
		}
	}

	return vector.ClassifiedPod{}, false
}

// componentRule matches a Crusoe-managed cluster component and carries its scrape
// job name, interval and port.
type componentRule struct {
	job            string
	scrapeInterval int
	matches        func(pod *corev1.Pod) bool
	// port is the fixed scrape port. Zero means the port and path come from the
	// prometheus.io annotations.
	port int
	// labelmap copies the pod labels onto the series and adds container and
	// namespace, as Prometheus labelmap does for pod and endpoint discovery.
	labelmap bool
}

// Scrape intervals for the pod and Service-backed components.
const (
	podsScrapeInterval             = 150
	serviceEndpointsScrapeInterval = 300
)

const namespaceKubeSystem = "kube-system"

// componentRules lists the Crusoe-managed components scraped into the cluster pipeline.
//
//nolint:gochecknoglobals,mnd // a fixed table of known components and their ports
var componentRules = []componentRule{
	{
		job:            "nvsentinel-platform-connector",
		scrapeInterval: 30,
		port:           2112,
		matches: func(pod *corev1.Pod) bool {
			return pod.Namespace == "nvsentinel" && strings.HasPrefix(pod.Name, "platform-connectors-")
		},
	},
	{
		job:            "coredns",
		scrapeInterval: 60,
		port:           9153,
		matches: func(pod *corev1.Pod) bool {
			return pod.Namespace == namespaceKubeSystem &&
				(pod.Labels["k8s-app"] == "kube-dns" || pod.Labels[labelAppK8sName] == "coredns")
		},
	},
	// Pods configured by prometheus.io annotations.
	{
		job:            "kubernetes-pods",
		scrapeInterval: podsScrapeInterval,
		labelmap:       true,
		matches: func(pod *corev1.Pod) bool {
			labels := pod.Labels
			cilium := pod.Namespace == namespaceKubeSystem &&
				(labels["k8s-app"] == "cilium" || labels["io.cilium/app"] == "operator")
			autoscaler := pod.Namespace == "crusoe-system" &&
				labels[labelAppK8sName] == "crusoecloud-cluster-autoscaler"
			certManager := labels["app.kubernetes.io/instance"] == "cert-manager"
			mitigator := labels["app"] == "crusoe-mitigator"

			return cilium || autoscaler || certManager || mitigator
		},
	},
	// Components behind a Service; the job is named after the Service.
	{
		job:            "kubelet-csr-approver",
		scrapeInterval: serviceEndpointsScrapeInterval,
		port:           8080,
		labelmap:       true,
		matches: func(pod *corev1.Pod) bool {
			return pod.Namespace == namespaceKubeSystem && pod.Labels[labelAppK8sName] == "kubelet-csr-approver"
		},
	},
	{
		job:            "atero-models-validator",
		scrapeInterval: serviceEndpointsScrapeInterval,
		port:           9090,
		labelmap:       true,
		matches: func(pod *corev1.Pod) bool {
			return pod.Labels[labelAppK8sName] == "atero-models-validator"
		},
	},
	{
		job:            "atero-models-monitor-metrics",
		scrapeInterval: serviceEndpointsScrapeInterval,
		port:           19427,
		labelmap:       true,
		matches: func(pod *corev1.Pod) bool {
			return pod.Labels[labelAppK8sName] == "atero-models-monitor"
		},
	},
}

func classifyComponentPod(pod *corev1.Pod, rule componentRule) (vector.ClassifiedPod, bool) {
	port, path := rule.port, "/metrics"
	if port == 0 {
		annotations := pod.Annotations
		if annotations[annotationPromScrape] != "true" {
			return vector.ClassifiedPod{}, false
		}
		parsed, err := strconv.Atoi(annotations[annotationPromPort])
		if err != nil {
			return vector.ClassifiedPod{}, false
		}
		port = parsed
		if annotated := annotations[annotationPromPath]; annotated != "" {
			path = annotated
		}
	}

	scrapeLabels := map[string]string{}
	if rule.labelmap {
		// Only a declared container port is scraped.
		container := containerWithPort(pod, port)
		if container == "" {
			return vector.ClassifiedPod{}, false
		}
		// labelmap first, so the target labels below win on a clash.
		for key, val := range pod.Labels {
			scrapeLabels[vector.SanitizeName(key)] = val
		}
		scrapeLabels["container"] = container
		scrapeLabels["namespace"] = pod.Namespace
	}
	scrapeLabels["pod"] = pod.Name
	scrapeLabels["node"] = pod.Spec.NodeName

	return vector.ClassifiedPod{
		Name:           pod.Name,
		IP:             pod.Status.PodIP,
		Type:           vector.PodTypeComponent,
		Port:           port,
		Path:           path,
		Job:            rule.job,
		ScrapeInterval: rule.scrapeInterval,
		ScrapeLabels:   scrapeLabels,
	}, true
}

// containerWithPort returns the first container that declares port, or "" if none does.
func containerWithPort(pod *corev1.Pod, port int) string {
	for _, container := range pod.Spec.Containers {
		for _, declared := range container.Ports {
			if int(declared.ContainerPort) == port {
				return container.Name
			}
		}
	}

	return ""
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
