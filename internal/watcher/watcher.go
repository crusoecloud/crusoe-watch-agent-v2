package watcher

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/vector"
)

const (
	configMapName      = "crusoe-custom-metrics-config"
	configMapNamespace = "crusoe-monitoring"

	reconcileDebounce = 2 * time.Second

	envNodeName = "NODE_NAME"
)

var errCacheSyncFailed = errors.New("timed out waiting for informer cache sync")

// Node label keys used to populate vector.NodeLabels.
const (
	nodeLabelVMID         = "crusoe.ai/instance.id"
	nodeLabelNodepoolID   = "crusoe.ai/nodepool.id"
	nodeLabelInstanceType = "beta.kubernetes.io/instance-type"
	nodeLabelPodID        = "crusoe.ai/pod.id"
	nodeLabelProjectID    = "crusoe.ai/project.id"
	nodeLabelHostname     = "kubernetes.io/hostname"
)

// Config holds all parameters the watcher needs to run.
type Config struct {
	NodeName   string           // Kubernetes node name (from downward API or hostname)
	ConfigPath string           // Output path for Vector config, e.g. /etc/vector/vector.yaml
	K8sCfg     vector.K8sConfig // NodeLabels populated by watcher at startup
	Logger     *slog.Logger
}

// Watcher watches Kubernetes pod events and ConfigMap changes, regenerates
// the Vector config, and writes it to disk for Vector to reload.
type Watcher struct {
	cfg Config

	client      kubernetes.Interface
	podInformer cache.SharedIndexInformer
	cmInformer  cache.SharedIndexInformer

	reconcileCh chan struct{}

	mu       sync.Mutex
	lastHash [sha256.Size]byte

	logger *slog.Logger
}

// New creates a Watcher. The caller must provide a Kubernetes client.
func New(cfg Config, client kubernetes.Interface) *Watcher {
	return &Watcher{
		cfg:         cfg,
		client:      client,
		reconcileCh: make(chan struct{}, 1),
		logger:      cfg.Logger,
	}
}

// Run starts informers and blocks until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) error {
	w.logger.Info("watcher starting", "node", w.cfg.NodeName)

	// Preserve existing config on disk as baseline so Vector can start before we reach the K8s API.
	// If no config exists yet, write a fallback with static components.
	hasExisting := w.loadExistingConfigHash()
	if !hasExisting {
		if err := w.writeBaseConfig(); err != nil {
			return fmt.Errorf("writing fallback vector config: %w", err)
		}

		w.logger.Info("fallback vector config written", "path", w.cfg.ConfigPath)
	} else {
		w.logger.Info("using existing vector config as baseline", "path", w.cfg.ConfigPath)
	}

	// Read node labels to populate K8sConfig.
	nodeLabels, err := ReadNodeLabels(ctx, w.client, w.cfg.NodeName)
	if err != nil {
		return fmt.Errorf("reading node labels: %w", err)
	}

	w.cfg.K8sCfg.NodeLabels = nodeLabels
	w.logger.Info("node labels resolved",
		"vm_id", nodeLabels.VMID,
		"nodepool_id", nodeLabels.NodepoolID,
	)

	// Create and start informers.
	w.setupInformers()

	go w.podInformer.Run(ctx.Done())
	go w.cmInformer.Run(ctx.Done())

	synced := cache.WaitForCacheSync(ctx.Done(),
		w.podInformer.HasSynced,
		w.cmInformer.HasSynced,
	)
	if !synced {
		return errCacheSyncFailed
	}

	w.logger.Info("informer caches synced")

	// Bootstrap: generate full config from current cluster state.
	w.reconcile()

	// Block on event-driven reconcile loop.
	w.reconcileLoop(ctx)

	return nil
}

// ResolveNodeName returns the Kubernetes node name from the NODE_NAME env var
// (set via the downward API) or falls back to os.Hostname.
func ResolveNodeName() (string, error) {
	if name := os.Getenv(envNodeName); name != "" {
		return name, nil
	}

	name, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("hostname fallback: %w", err)
	}

	return name, nil
}

// ReadNodeLabels reads Crusoe-specific labels from the Kubernetes node object.
func ReadNodeLabels(ctx context.Context, client kubernetes.Interface, nodeName string) (vector.NodeLabels, error) {
	node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return vector.NodeLabels{}, fmt.Errorf("getting node %q: %w", nodeName, err)
	}

	labels := node.Labels

	return vector.NodeLabels{
		VMID:         labels[nodeLabelVMID],
		NodepoolID:   labels[nodeLabelNodepoolID],
		InstanceType: labels[nodeLabelInstanceType],
		PodID:        labels[nodeLabelPodID],
		ProjectID:    labels[nodeLabelProjectID],
		Hostname:     labels[nodeLabelHostname],
	}, nil
}

// ---------------------------------------------------------------------------
// Informer setup
// ---------------------------------------------------------------------------

func (w *Watcher) setupInformers() {
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(_ any) { w.triggerReconcile() },
		UpdateFunc: func(_, _ any) { w.triggerReconcile() },
		DeleteFunc: func(_ any) { w.triggerReconcile() },
	}

	// Pod informer: all namespaces, filtered to pods on this node.
	podLW := cache.NewListWatchFromClient(
		w.client.CoreV1().RESTClient(),
		"pods",
		corev1.NamespaceAll,
		fields.OneTermEqualSelector("spec.nodeName", w.cfg.NodeName),
	)
	w.podInformer = cache.NewSharedIndexInformer(podLW, &corev1.Pod{}, 0, cache.Indexers{})

	if _, err := w.podInformer.AddEventHandler(handler); err != nil {
		w.logger.Error("failed to add pod event handler", "error", err)
	}

	// ConfigMap informer: watches crusoe-custom-metrics-config in crusoe-monitoring.
	cmLW := cache.NewListWatchFromClient(
		w.client.CoreV1().RESTClient(),
		"configmaps",
		configMapNamespace,
		fields.OneTermEqualSelector("metadata.name", configMapName),
	)
	w.cmInformer = cache.NewSharedIndexInformer(cmLW, &corev1.ConfigMap{}, 0, cache.Indexers{})

	if _, err := w.cmInformer.AddEventHandler(handler); err != nil {
		w.logger.Error("failed to add configmap event handler", "error", err)
	}
}

func (w *Watcher) triggerReconcile() {
	select {
	case w.reconcileCh <- struct{}{}:
	default:
		// Reconcile already pending.
	}
}

// ---------------------------------------------------------------------------
// Reconciliation
// ---------------------------------------------------------------------------

func (w *Watcher) reconcileLoop(ctx context.Context) {
	var debounce <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.reconcileCh:
			debounce = time.After(reconcileDebounce)
		case <-debounce:
			debounce = nil
			w.reconcile()
		}
	}
}

func (w *Watcher) reconcile() {
	pods := w.classifyPods()
	cmData := w.readConfigMapData()

	out, err := vector.GenerateK8s(pods, cmData, w.cfg.K8sCfg)
	if err != nil {
		w.logger.Error("generating vector config failed", "error", err)

		return
	}

	// Skip write if config is unchanged.
	hash := sha256.Sum256(out)

	w.mu.Lock()
	unchanged := hash == w.lastHash
	if !unchanged {
		w.lastHash = hash
	}
	w.mu.Unlock()

	if unchanged {
		return
	}

	if err := writeConfig(w.cfg.ConfigPath, out); err != nil {
		w.logger.Error("writing vector config failed", "error", err)

		return
	}

	w.logger.Info("vector config updated", "pods", len(pods), "path", w.cfg.ConfigPath)
}

func (w *Watcher) classifyPods() []vector.ClassifiedPod {
	var classified []vector.ClassifiedPod

	defaultPort := w.cfg.K8sCfg.CustomMetricsDefaultPort
	defaultPath := w.cfg.K8sCfg.CustomMetricsDefaultPath

	for _, obj := range w.podInformer.GetStore().List() {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			continue
		}

		if cp, relevant := ClassifyPod(pod, defaultPort, defaultPath); relevant {
			classified = append(classified, cp)
		}
	}

	return classified
}

func (w *Watcher) readConfigMapData() map[string]string {
	for _, obj := range w.cmInformer.GetStore().List() {
		cm, ok := obj.(*corev1.ConfigMap)
		if !ok {
			continue
		}

		return cm.Data
	}

	return nil
}

func (w *Watcher) writeBaseConfig() error {
	out, err := vector.GenerateK8s(nil, nil, w.cfg.K8sCfg)
	if err != nil {
		return fmt.Errorf("generating base config: %w", err)
	}

	w.mu.Lock()
	w.lastHash = sha256.Sum256(out)
	w.mu.Unlock()

	return writeConfig(w.cfg.ConfigPath, out)
}

// writeConfig atomically writes data to path using a temp file + rename.
func writeConfig(path string, data []byte) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, ".vector-config-*.yaml")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}

	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)

		return fmt.Errorf("writing temp file: %w", err)
	}

	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)

		return fmt.Errorf("closing temp file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)

		return fmt.Errorf("renaming config: %w", err)
	}

	return nil
}

// loadExistingConfigHash checks whether a config file already exists on disk.
func (w *Watcher) loadExistingConfigHash() bool {
	data, err := os.ReadFile(w.cfg.ConfigPath)
	if err != nil {
		return false
	}

	w.mu.Lock()
	w.lastHash = sha256.Sum256(data)
	w.mu.Unlock()

	return true
}
