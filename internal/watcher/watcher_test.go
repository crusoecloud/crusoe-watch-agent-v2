package watcher

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/vector"
	"gopkg.in/yaml.v3"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func testK8sCfg() vector.K8sConfig {
	return vector.K8sConfig{
		DCGM: vector.ExporterConfig{
			Enabled:        true,
			Port:           9400,
			Paths:          []string{"/metrics"},
			ScrapeInterval: 30,
		},
		AMD: vector.ExporterConfig{
			Enabled:        true,
			Port:           5000,
			Paths:          []string{"/metrics"},
			ScrapeInterval: 60,
		},
		KSM: vector.ExporterConfig{
			Enabled:        true,
			Port:           8080,
			Paths:          []string{"/metrics"},
			ScrapeInterval: 60,
		},
		Slurm: vector.ExporterConfig{
			Enabled:        false,
			Port:           6817,
			Paths:          []string{"/metrics/jobs"},
			ScrapeInterval: 60,
		},
		CME: vector.ExporterConfig{
			Enabled:        true,
			Port:           9500,
			Paths:          []string{"/metrics"},
			ScrapeInterval: 60,
		},
		CustomMetricsEnabled:       true,
		CustomMetricsDefaultPort:   9100,
		CustomMetricsDefaultPath:   "/metrics",
		CustomMetricsDefaultScrape: 30,
		LogsEnabled:                true,
		SinkEndpoint:               "https://cms-monitoring.crusoecloud.com",
	}
}

func testNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
			Labels: map[string]string{
				"crusoe.ai/instance.id":            "vm-abc-123",
				"crusoe.ai/nodepool.id":            "nodepool-1",
				"beta.kubernetes.io/instance-type": "gpu-a100",
				"crusoe.ai/pod.id":                 "pod-xyz",
				"crusoe.ai/project.id":             "project-1",
				"kubernetes.io/hostname":           "test-node",
			},
		},
	}
}

func TestReadNodeLabels(t *testing.T) {
	client := fake.NewSimpleClientset(testNode())

	labels, err := ReadNodeLabels(context.Background(), client, "test-node")
	require.NoError(t, err)
	assert.Equal(t, "vm-abc-123", labels.VMID)
	assert.Equal(t, "nodepool-1", labels.NodepoolID)
	assert.Equal(t, "gpu-a100", labels.InstanceType)
	assert.Equal(t, "pod-xyz", labels.PodID)
	assert.Equal(t, "project-1", labels.ProjectID)
	assert.Equal(t, "test-node", labels.Hostname)
}

func TestReadNodeLabels_NodeNotFound(t *testing.T) {
	client := fake.NewSimpleClientset()

	_, err := ReadNodeLabels(context.Background(), client, "nonexistent")
	assert.Error(t, err)
}

func TestReadNodeLabels_MissingLabels(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "bare-node",
			Labels: map[string]string{},
		},
	}
	client := fake.NewSimpleClientset(node)

	labels, err := ReadNodeLabels(context.Background(), client, "bare-node")
	require.NoError(t, err)
	assert.Equal(t, "", labels.VMID)
	assert.Equal(t, "", labels.NodepoolID)
}

func TestWatcher_WriteBaseConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vector.yaml")

	client := fake.NewSimpleClientset(testNode())
	cfg := Config{
		NodeName:   "test-node",
		ConfigPath: configPath,
		K8sCfg:     testK8sCfg(),
		Logger:     testLogger(),
	}

	w := New(cfg, client)

	// Simulate what Run does: read node labels, then write base config.
	nodeLabels, err := ReadNodeLabels(context.Background(), client, "test-node")
	require.NoError(t, err)
	w.cfg.K8sCfg.NodeLabels = nodeLabels

	err = w.writeBaseConfig()
	require.NoError(t, err)

	// Verify the file was written and is valid YAML.
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)

	var parsed map[string]any
	require.NoError(t, yaml.Unmarshal(data, &parsed))

	// Base config should have sources (host_metrics, internal_metrics, logs)
	// but no dynamic pod sources.
	sources := parsed["sources"].(map[string]any)
	assert.Contains(t, sources, "host_metrics")
	assert.Contains(t, sources, "internal_metrics")
	assert.NotContains(t, sources, "dcgm_exporter_scrape")
	assert.NotContains(t, sources, "amd_exporter_scrape")
}

func TestWatcher_Reconcile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vector.yaml")

	dcgmPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dcgm-exporter-abc",
			Namespace: "gpu-operator",
			Labels:    map[string]string{"app": "nvidia-dcgm-exporter"},
		},
		Spec: corev1.PodSpec{NodeName: "test-node"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "10.0.0.1",
		},
	}

	ksmPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kube-state-metrics-7f9b4-xyz",
			Namespace: "kube-system",
			Labels:    map[string]string{"app.kubernetes.io/name": "kube-state-metrics"},
		},
		Spec: corev1.PodSpec{NodeName: "test-node"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "10.0.0.2",
		},
	}

	client := fake.NewSimpleClientset(testNode(), dcgmPod, ksmPod)

	k8sCfg := testK8sCfg()
	k8sCfg.NodeLabels = vector.NodeLabels{
		VMID:         "vm-abc-123",
		NodepoolID:   "nodepool-1",
		InstanceType: "gpu-a100",
		Hostname:     "test-node",
	}

	cfg := Config{
		NodeName:   "test-node",
		ConfigPath: configPath,
		K8sCfg:     k8sCfg,
		Logger:     testLogger(),
	}

	w := New(cfg, client)

	// Manually populate informer store for unit testing (bypasses real informer).
	w.podInformer = newFakeInformer()
	w.cmInformer = newFakeInformer()
	w.podInformer.GetStore().Add(dcgmPod)
	w.podInformer.GetStore().Add(ksmPod)

	// Write base config first to initialize lastHash.
	require.NoError(t, w.writeBaseConfig())

	// Reconcile should detect pods and generate full config.
	w.reconcile()

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)

	var parsed map[string]any
	require.NoError(t, yaml.Unmarshal(data, &parsed))

	sources := parsed["sources"].(map[string]any)
	assert.Contains(t, sources, "dcgm_exporter_scrape")
	assert.Contains(t, sources, "kube_state_metrics_scrape")
	assert.Contains(t, sources, "host_metrics")

	sinks := parsed["sinks"].(map[string]any)
	assert.Contains(t, sinks, "kube_state_metrics_sink")
}

func TestWatcher_ReconcileSkipsUnchanged(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vector.yaml")

	client := fake.NewSimpleClientset(testNode())
	k8sCfg := testK8sCfg()
	k8sCfg.NodeLabels = vector.NodeLabels{VMID: "vm-1", Hostname: "test-node"}

	w := New(Config{
		NodeName:   "test-node",
		ConfigPath: configPath,
		K8sCfg:     k8sCfg,
		Logger:     testLogger(),
	}, client)

	w.podInformer = newFakeInformer()
	w.cmInformer = newFakeInformer()

	require.NoError(t, w.writeBaseConfig())

	// First reconcile writes the config.
	w.reconcile()
	info1, _ := os.Stat(configPath)

	// Wait briefly to ensure mtime would differ if file were rewritten.
	time.Sleep(50 * time.Millisecond)

	// Second reconcile with same state should skip write.
	w.reconcile()
	info2, _ := os.Stat(configPath)

	assert.Equal(t, info1.ModTime(), info2.ModTime(), "file should not be rewritten when config is unchanged")
}

func TestWatcher_SetIngestionBlocked(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vector.yaml")

	client := fake.NewSimpleClientset(testNode())
	k8sCfg := testK8sCfg()
	k8sCfg.NodeLabels = vector.NodeLabels{VMID: "vm-1", Hostname: "test-node"}

	w := New(Config{
		NodeName:   "test-node",
		ConfigPath: configPath,
		K8sCfg:     k8sCfg,
		Logger:     testLogger(),
	}, client)

	w.podInformer = newFakeInformer()
	w.cmInformer = newFakeInformer()

	readSinks := func() map[string]any {
		data, err := os.ReadFile(configPath)
		require.NoError(t, err)

		var parsed map[string]any
		require.NoError(t, yaml.Unmarshal(data, &parsed))

		return parsed["sinks"].(map[string]any)
	}

	// Block: the flag is stored on the watcher, so the reconcile it triggers
	// (and every later data-plane reconcile) writes a sink-stripped config.
	w.SetIngestionBlocked(true)
	w.reconcile()

	sinks := readSinks()
	assert.Len(t, sinks, 1)
	assert.Contains(t, sinks, "internal_metrics_exporter")

	// Unblock restores the sinks.
	w.SetIngestionBlocked(false)
	w.reconcile()

	sinks = readSinks()
	assert.Contains(t, sinks, "cms_gateway_node_metrics")
	assert.Contains(t, sinks, "crusoe_ingest")
}

func TestWatcher_SetRateLimits(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vector.yaml")

	client := fake.NewSimpleClientset(testNode())
	k8sCfg := testK8sCfg()
	k8sCfg.NodeLabels = vector.NodeLabels{VMID: "vm-1", Hostname: "test-node"}

	w := New(Config{
		NodeName:   "test-node",
		ConfigPath: configPath,
		K8sCfg:     k8sCfg,
		Logger:     testLogger(),
	}, client)

	w.podInformer = newFakeInformer()
	w.cmInformer = newFakeInformer()

	readSinks := func() map[string]any {
		data, err := os.ReadFile(configPath)
		require.NoError(t, err)

		var parsed map[string]any
		require.NoError(t, yaml.Unmarshal(data, &parsed))

		return parsed["sinks"].(map[string]any)
	}

	// Set: the caps are stored on the watcher, so the reconcile it triggers (and
	// every later data-plane reconcile) writes a rate-limited config.
	w.SetRateLimits(map[string]int{vector.RateLimitAll: 100})
	w.reconcile()

	req := readSinks()["cms_gateway_node_metrics"].(map[string]any)["request"].(map[string]any)
	assert.Equal(t, 100, req["rate_limit_num"])

	// An empty map clears the caps.
	w.SetRateLimits(nil)
	w.reconcile()

	req = readSinks()["cms_gateway_node_metrics"].(map[string]any)["request"].(map[string]any)
	assert.NotContains(t, req, "rate_limit_num")
}

func TestWatcher_ReconcileWithConfigMap(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vector.yaml")

	customPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-svc-abc-123",
			Namespace: "crusoe-monitoring",
			Annotations: map[string]string{
				"crusoe.ai/scrape": "true",
			},
		},
		Spec: corev1.PodSpec{NodeName: "test-node"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "10.0.0.5",
		},
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName,
			Namespace: configMapNamespace,
		},
		Data: map[string]string{
			"custom-metrics-config.yaml": "my-svc:\n  scrape_interval_secs: 15\n",
		},
	}

	client := fake.NewSimpleClientset(testNode())
	k8sCfg := testK8sCfg()
	k8sCfg.NodeLabels = vector.NodeLabels{
		VMID:         "vm-abc-123",
		NodepoolID:   "nodepool-1",
		InstanceType: "gpu-a100",
		Hostname:     "test-node",
	}

	w := New(Config{
		NodeName:   "test-node",
		ConfigPath: configPath,
		K8sCfg:     k8sCfg,
		Logger:     testLogger(),
	}, client)

	w.podInformer = newFakeInformer()
	w.cmInformer = newFakeInformer()
	w.podInformer.GetStore().Add(customPod)
	w.cmInformer.GetStore().Add(cm)

	require.NoError(t, w.writeBaseConfig())
	w.reconcile()

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)

	var parsed map[string]any
	require.NoError(t, yaml.Unmarshal(data, &parsed))

	sources := parsed["sources"].(map[string]any)
	assert.Contains(t, sources, "my_svc_abc_123_scrape")

	transforms := parsed["transforms"].(map[string]any)
	assert.Contains(t, transforms, "my_svc_abc_123_transform")
}

func TestWatcher_LoadExistingConfigHash(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vector.yaml")

	client := fake.NewSimpleClientset(testNode())
	w := New(Config{
		NodeName:   "test-node",
		ConfigPath: configPath,
		K8sCfg:     testK8sCfg(),
		Logger:     testLogger(),
	}, client)

	t.Run("no existing file", func(t *testing.T) {
		assert.False(t, w.loadExistingConfigHash())
	})

	t.Run("existing file preserved", func(t *testing.T) {
		existing := []byte("data_dir: /vector-data-dir\n")
		require.NoError(t, os.WriteFile(configPath, existing, 0o644))

		assert.True(t, w.loadExistingConfigHash())

		// Reconcile with no pods should detect the config changed
		// (base config differs from the hand-written file above).
		nodeLabels, err := ReadNodeLabels(context.Background(), client, "test-node")
		require.NoError(t, err)
		w.cfg.K8sCfg.NodeLabels = nodeLabels

		w.podInformer = newFakeInformer()
		w.cmInformer = newFakeInformer()
		w.reconcile()

		// Config should have been overwritten with the generated version.
		data, err := os.ReadFile(configPath)
		require.NoError(t, err)
		assert.NotEqual(t, existing, data, "reconcile should overwrite stale baseline")

		var parsed map[string]any
		require.NoError(t, yaml.Unmarshal(data, &parsed))
		assert.Contains(t, parsed, "sources")
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// fakeInformer implements cache.SharedIndexInformer with a simple in-memory store
// for unit testing without starting real informer goroutines.
type fakeInformer struct {
	cache.SharedIndexInformer
	store cache.Store
}

func newFakeInformer() *fakeInformer {
	return &fakeInformer{
		store: cache.NewStore(cache.MetaNamespaceKeyFunc),
	}
}

func (f *fakeInformer) GetStore() cache.Store {
	return f.store
}
