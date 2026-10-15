package watcher

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/vector"
)

// cpNamespace builds a namespace with the tenant labels buildUIDMap reads (empty value omits one).
func cpNamespace(name, cluster, project, org string) *corev1.Namespace {
	labels := map[string]string{}
	if cluster != "" {
		labels[cpLabelClusterID] = cluster
	}
	if project != "" {
		labels[cpLabelProjectID] = project
	}
	if org != "" {
		labels[cpLabelOrgID] = org
	}

	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

// cpPod builds a CP pod (carrying the control-plane service-class label) on test-node.
func cpPod(name, namespace, uid string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID(uid),
			Labels:    map[string]string{"policy.crusoe.ai/public-service-class": "cmkv2-control-plane"},
		},
		Spec: corev1.PodSpec{NodeName: "test-node"},
	}
}

// TestBuildUIDMap_AttributesAndSkipsMissingLabels: fully-labeled namespace -> row; missing label -> skipped.
func TestBuildUIDMap_AttributesAndSkipsMissingLabels(t *testing.T) {
	client := fake.NewSimpleClientset(
		cpNamespace("ns-good", "cluster-1", "project-1", "org-1"),
		cpNamespace("ns-missing", "cluster-2", "", "org-2"), // no project -> unattributable
		cpPod("cp-a", "ns-good", "uid-aaa"),
		cpPod("cp-b", "ns-missing", "uid-bbb"),
	)

	csv, hash, rows, err := buildUIDMap(context.Background(), client, "")
	require.NoError(t, err)

	assert.Equal(t, 1, rows)
	assert.True(t, strings.HasPrefix(csv, uidMapCSVHeader))
	assert.Contains(t, csv, "uid-aaa,cluster-1,project-1,org-1")
	assert.NotContains(t, csv, "uid-bbb")
	assert.NotEqual(t, [sha256.Size]byte{}, hash)
}

// TestBuildUIDMap_StableHashRegardlessOfInputOrder: rows are sorted, so discovery order -> same hash.
func TestBuildUIDMap_StableHashRegardlessOfInputOrder(t *testing.T) {
	nsA := cpNamespace("ns-a", "c-a", "p-a", "o-a")
	nsB := cpNamespace("ns-b", "c-b", "p-b", "o-b")

	clientOrderA := fake.NewSimpleClientset(nsA, nsB, cpPod("a", "ns-a", "uid-1"), cpPod("b", "ns-b", "uid-2"))
	clientOrderB := fake.NewSimpleClientset(nsB, nsA, cpPod("b", "ns-b", "uid-2"), cpPod("a", "ns-a", "uid-1"))

	_, hashA, rowsA, errA := buildUIDMap(context.Background(), clientOrderA, "")
	require.NoError(t, errA)
	_, hashB, rowsB, errB := buildUIDMap(context.Background(), clientOrderB, "")
	require.NoError(t, errB)

	assert.Equal(t, 2, rowsA)
	assert.Equal(t, 2, rowsB)
	assert.Equal(t, hashA, hashB)
}

// TestBuildUIDMap_EmptyWhenNoCPPods: no CP pods yields a header-only map, not an error.
func TestBuildUIDMap_EmptyWhenNoCPPods(t *testing.T) {
	csv, _, rows, err := buildUIDMap(context.Background(), fake.NewSimpleClientset(), "")
	require.NoError(t, err)

	assert.Equal(t, 0, rows)
	assert.Equal(t, uidMapCSVHeader, csv)
}

// TestReconcileUIDMap_WritesAndDefersHashCommit: writes the CSV but defers the lastMapHash commit to the caller.
func TestReconcileUIDMap_WritesAndDefersHashCommit(t *testing.T) {
	dir := t.TempDir()
	client := fake.NewSimpleClientset(
		cpNamespace("ns-good", "cluster-1", "project-1", "org-1"),
		cpPod("cp-a", "ns-good", "uid-aaa"),
	)
	w := New(Config{
		NodeName:   "", // empty -> no field selector; fake doesn't honor it anyway
		ConfigPath: filepath.Join(dir, "vector.yaml"),
		K8sCfg:     vector.K8sConfig{CMKv2CPEnabled: true},
		Logger:     testLogger(),
	}, client)

	changed, newHash := w.reconcileUIDMap(context.Background())
	require.True(t, changed)
	assert.NotEqual(t, [sha256.Size]byte{}, newHash)

	// The fix: lastMapHash is NOT advanced by reconcileUIDMap — the caller commits it post-write.
	assert.Equal(t, [sha256.Size]byte{}, w.lastMapHash, "reconcileUIDMap must not advance lastMapHash")

	data, err := os.ReadFile(filepath.Join(dir, "uid_map.csv"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "uid-aaa,cluster-1,project-1,org-1")

	// After the caller commits the hash, an unchanged map reports changed=false (no needless reload).
	w.lastMapHash = newHash
	changed2, _ := w.reconcileUIDMap(context.Background())
	assert.False(t, changed2)
}
