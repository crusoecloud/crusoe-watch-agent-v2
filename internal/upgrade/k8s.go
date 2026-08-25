package upgrade

import (
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// stateKey is the ConfigMap data key holding the serialised State.
const stateKey = "state"

// DaemonSetCounter reports the agent DaemonSet's numberReady, which is how many
// pods the control plane could have dispatched this upgrade round to.
type DaemonSetCounter struct {
	client    kubernetes.Interface
	namespace string
	name      string
}

// NewDaemonSetCounter returns a counter reading the named agent DaemonSet.
func NewDaemonSetCounter(client kubernetes.Interface, namespace, name string) *DaemonSetCounter {
	return &DaemonSetCounter{client: client, namespace: namespace, name: name}
}

// ReadyCount reads numberReady from the DaemonSet's status subresource.
func (c *DaemonSetCounter) ReadyCount(ctx context.Context) (int, error) {
	daemonSet, err := c.client.AppsV1().DaemonSets(c.namespace).Get(ctx, c.name, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("reading daemonset %s/%s: %w", c.namespace, c.name, err)
	}

	return int(daemonSet.Status.NumberReady), nil
}

// ConfigMapStore persists upgrade state in the handoff ConfigMap. The cwa-updater
// chart creates it with helm.sh/resource-policy: keep, so state outlives both a
// pod restart and a chart reinstall mid-upgrade. Kubernetes only.
type ConfigMapStore struct {
	client    kubernetes.Interface
	namespace string
	name      string
}

// NewConfigMapStore returns a store backed by the named ConfigMap.
func NewConfigMapStore(client kubernetes.Interface, namespace, name string) *ConfigMapStore {
	return &ConfigMapStore{client: client, namespace: namespace, name: name}
}

// Load reads the persisted state, returning ErrNoState when none is recorded.
func (s *ConfigMapStore) Load(ctx context.Context) (*State, error) {
	configMap, err := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading handoff configmap %s/%s: %w", s.namespace, s.name, err)
	}

	raw, ok := configMap.Data[stateKey]
	if !ok || raw == "" {
		return nil, ErrNoState
	}

	var state State
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return nil, fmt.Errorf("%w: parsing %s/%s: %w", ErrCorruptState, s.namespace, s.name, err)
	}

	if state.AgentExecutions == nil {
		state.AgentExecutions = make(map[string]string)
	}

	return &state, nil
}

// Save durably records state. Concurrent writers on the ConfigMap are resolved by
// re-reading and retrying: cwa-updater is single-replica, so a conflict only comes
// from a helm operation touching the same object.
func (s *ConfigMapStore) Save(ctx context.Context, state *State) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshaling upgrade state: %w", err)
	}

	return s.update(ctx, string(data))
}

// Clear drops the persisted state, returning cwa-updater to idle. The ConfigMap
// itself is left in place: the chart owns it, and cwa-updater has no delete verb.
func (s *ConfigMapStore) Clear(ctx context.Context) error {
	return s.update(ctx, "")
}

// update writes value under stateKey, or removes the key when value is empty.
// Wrapping inside the closure is safe: RetryOnConflict detects a conflict with
// errors.As, which sees through %w.
func (s *ConfigMapStore) update(ctx context.Context, value string) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		configMap, getErr := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("reading for update: %w", getErr)
		}

		if value == "" {
			delete(configMap.Data, stateKey)
		} else {
			if configMap.Data == nil {
				configMap.Data = make(map[string]string, 1)
			}

			configMap.Data[stateKey] = value
		}

		if _, updateErr := s.client.CoreV1().ConfigMaps(s.namespace).
			Update(ctx, configMap, metav1.UpdateOptions{}); updateErr != nil {
			return fmt.Errorf("updating: %w", updateErr)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("writing handoff configmap %s/%s: %w", s.namespace, s.name, err)
	}

	return nil
}
