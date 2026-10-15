package watcher

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/atomicfile"
	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/vector"
)

// CMKv2 control-plane tenant map (uid -> cluster/project/org), built by cwa-manager and reloaded by Vector.
const (
	// Namespace label keys carrying tenant identity on CMKv2 CP namespaces.
	cpLabelClusterID = "crusoe.ai/cluster-id"
	cpLabelProjectID = "crusoe.ai/project-id"
	cpLabelOrgID     = "crusoe.ai/org-id"

	uidMapFileName   = "uid_map.csv"
	uidMapFileMode   = 0o600
	uidMapCSVHeader  = "uid,cluster_id,project_id,org_id\n"
	uidMapListMaxDur = 15 * time.Second
)

// uidMapPath is uid_map.csv in the same shared dir as the Vector config.
func (w *Watcher) uidMapPath() string {
	return filepath.Join(filepath.Dir(w.cfg.ConfigPath), uidMapFileName)
}

// reconcileUIDMap rewrites uid_map.csv on change, returning (changed, newHash) for the caller to commit post-write.
func (w *Watcher) reconcileUIDMap(ctx context.Context) (bool, [sha256.Size]byte) {
	csv, hash, rows, err := buildUIDMap(ctx, w.client, w.cfg.NodeName)
	if err != nil {
		w.logger.Error("building cmkv2 uid_map failed (keeping last-good)", "error", err)

		return false, hash
	}

	w.mu.Lock()
	unchanged := hash == w.lastMapHash
	w.mu.Unlock()

	if unchanged {
		return false, hash
	}

	if writeErr := atomicfile.Write(w.uidMapPath(), []byte(csv), uidMapFileMode); writeErr != nil {
		w.logger.Error("writing cmkv2 uid_map failed (keeping last-good)", "error", writeErr)

		return false, hash
	}

	w.logger.Info("cmkv2 uid_map updated", "rows", rows, "path", w.uidMapPath())

	return true, hash
}

// seedUIDMap writes an initial uid_map.csv (header-only if the map can't be built yet) before Vector starts.
func (w *Watcher) seedUIDMap(ctx context.Context) {
	csv, hash, rows, err := buildUIDMap(ctx, w.client, w.cfg.NodeName)
	if err != nil {
		w.logger.Error("seeding cmkv2 uid_map: build failed, writing empty map", "error", err)

		csv = uidMapCSVHeader
		hash = sha256.Sum256([]byte(csv))
		rows = 0
	}

	if writeErr := atomicfile.Write(w.uidMapPath(), []byte(csv), uidMapFileMode); writeErr != nil {
		w.logger.Error("seeding cmkv2 uid_map failed", "error", writeErr)

		return
	}

	w.mu.Lock()
	w.lastMapHash = hash
	w.mu.Unlock()

	w.logger.Info("cmkv2 uid_map seeded", "rows", rows, "path", w.uidMapPath())
}

// buildUIDMap lists CP pods on nodeName and returns the uid_map CSV, its hash, and row count.
func buildUIDMap(
	ctx context.Context,
	client kubernetes.Interface,
	nodeName string,
) (string, [sha256.Size]byte, int, error) {
	var empty [sha256.Size]byte

	ctx, cancel := context.WithTimeout(ctx, uidMapListMaxDur)
	defer cancel()

	opts := metav1.ListOptions{LabelSelector: vector.CPServiceClassSelector}
	if nodeName != "" {
		opts.FieldSelector = "spec.nodeName=" + nodeName
	}

	pods, err := client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, opts)
	if err != nil {
		return "", empty, 0, fmt.Errorf("list cmkv2 cp pods: %w", err)
	}

	// One namespace Get per namespace, cached across pods in the same build.
	nsCache := make(map[string]*corev1.Namespace)
	rows := make([]string, 0, len(pods.Items))

	for i := range pods.Items {
		pod := &pods.Items[i]

		namespace, cached := nsCache[pod.Namespace]
		if !cached {
			namespace, err = client.CoreV1().Namespaces().Get(ctx, pod.Namespace, metav1.GetOptions{})
			if err != nil {
				return "", empty, 0, fmt.Errorf("get namespace %s: %w", pod.Namespace, err)
			}

			nsCache[pod.Namespace] = namespace
		}

		cluster := namespace.Labels[cpLabelClusterID]
		project := namespace.Labels[cpLabelProjectID]
		org := namespace.Labels[cpLabelOrgID]
		if cluster == "" || project == "" || org == "" {
			// Missing labels -> cannot attribute; skip so we never write a half-attributed row.
			continue
		}

		rows = append(rows, strings.Join([]string{string(pod.UID), cluster, project, org}, ","))
	}

	sort.Strings(rows) // Deterministic order -> stable hash.

	var builder strings.Builder

	builder.WriteString(uidMapCSVHeader)
	for _, row := range rows {
		builder.WriteString(row)
		builder.WriteByte('\n')
	}

	out := builder.String()

	return out, sha256.Sum256([]byte(out)), len(rows), nil
}
