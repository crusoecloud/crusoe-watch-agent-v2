package upgrade

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	testNamespace = "crusoe-system"
	testConfigMap = "cwa-upgrade-handoff"
)

func handoffConfigMap(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testConfigMap, Namespace: testNamespace},
		Data:       data,
	}
}

func newTestStore(objects ...runtime.Object) *ConfigMapStore {
	return NewConfigMapStore(fake.NewSimpleClientset(objects...), testNamespace, testConfigMap)
}

func TestConfigMapStoreLoadNoState(t *testing.T) {
	// The chart creates the ConfigMap empty; that is the idle case, not an error.
	store := newTestStore(handoffConfigMap(nil))

	_, err := store.Load(context.Background())
	require.ErrorIs(t, err, ErrNoState)

	store = newTestStore(handoffConfigMap(map[string]string{stateKey: ""}))
	_, err = store.Load(context.Background())
	require.ErrorIs(t, err, ErrNoState)
}

func TestConfigMapStoreMissingConfigMap(t *testing.T) {
	store := newTestStore()

	_, err := store.Load(context.Background())
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNoState)
}

func TestConfigMapStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(handoffConfigMap(nil))

	requested := time.Now().UTC().Truncate(time.Second)
	state := &State{
		Phase:           PhaseInProgress,
		TargetVersion:   "v2.1.0",
		RollbackVersion: "v2.0.3",
		AgentExecutions: map[string]string{"agent-1": "cmd-1", "agent-2": "cmd-2"},
		ExpectedCount:   2,
		CollectDeadline: requested.Add(15 * time.Minute),
		RequestedAt:     requested,
		Result:          &Result{Status: ResultFailed, Reason: "boom", CompletedAt: requested},
	}

	require.NoError(t, store.Save(ctx, state))

	loaded, err := store.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, PhaseInProgress, loaded.Phase)
	assert.Equal(t, "v2.1.0", loaded.TargetVersion)
	assert.Equal(t, map[string]string{"agent-1": "cmd-1", "agent-2": "cmd-2"}, loaded.AgentExecutions)
	assert.Equal(t, 2, loaded.ExpectedCount)
	assert.True(t, requested.Equal(loaded.RequestedAt))
	require.NotNil(t, loaded.Result)
	assert.Equal(t, "boom", loaded.Result.Reason)
}

func TestConfigMapStoreClear(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(handoffConfigMap(map[string]string{
		stateKey:    `{"phase":"failed","target_version":"v2.1.0"}`,
		"unrelated": "keep me",
	}))

	require.NoError(t, store.Clear(ctx))

	_, err := store.Load(ctx)
	require.ErrorIs(t, err, ErrNoState)

	// Clearing must not disturb other keys, or delete the chart-owned ConfigMap.
	configMap, err := store.client.CoreV1().ConfigMaps(testNamespace).
		Get(ctx, testConfigMap, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "keep me", configMap.Data["unrelated"])
}

func TestConfigMapStoreLoadCorruptState(t *testing.T) {
	store := newTestStore(handoffConfigMap(map[string]string{stateKey: "{not json"}))

	// ErrCorruptState, not a plain error: it is what tells the service to serve
	// the failure instead of exiting into a crash loop that re-reads the same bytes.
	_, err := store.Load(context.Background())
	require.ErrorIs(t, err, ErrCorruptState)
	require.NotErrorIs(t, err, ErrNoState)
}

// A state persisted without any handoffs still loads with a usable map.
func TestConfigMapStoreLoadNilExecutions(t *testing.T) {
	store := newTestStore(handoffConfigMap(map[string]string{
		stateKey: `{"phase":"pending","target_version":"v2.1.0"}`,
	}))

	loaded, err := store.Load(context.Background())
	require.NoError(t, err)
	require.NotNil(t, loaded.AgentExecutions)

	loaded.AgentExecutions["agent-1"] = "cmd-1"
	assert.Len(t, loaded.AgentExecutions, 1)
}

func TestDaemonSetCounterReadyCount(t *testing.T) {
	daemonSet := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "crusoe-watch-agent", Namespace: testNamespace},
		Status:     appsv1.DaemonSetStatus{NumberReady: 6, DesiredNumberScheduled: 8},
	}
	counter := NewDaemonSetCounter(fake.NewSimpleClientset(daemonSet), testNamespace, "crusoe-watch-agent")

	// numberReady, not desired: only ready pods were dispatched this round.
	ready, err := counter.ReadyCount(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 6, ready)
}

func TestDaemonSetCounterMissingDaemonSet(t *testing.T) {
	counter := NewDaemonSetCounter(fake.NewSimpleClientset(), testNamespace, "crusoe-watch-agent")

	_, err := counter.ReadyCount(context.Background())
	require.Error(t, err)
}
