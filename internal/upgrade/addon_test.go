package upgrade

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

const testDaemonSet = "crusoe-watch-agent"

// agentDaemonSet is the agent DaemonSet at version, with updated and ready of desired pods rolled.
func agentDaemonSet(version string, desired, updated, ready int32) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testDaemonSet,
			Namespace:  testNamespace,
			Generation: 2,
			Labels:     map[string]string{versionLabel: version},
		},
		Status: appsv1.DaemonSetStatus{
			ObservedGeneration:     2,
			DesiredNumberScheduled: desired,
			UpdatedNumberScheduled: updated,
			NumberReady:            ready,
		},
	}
}

func newAddonExecutor(client kubernetes.Interface, verifier Verifier) *AddonExecutor {
	executor := NewAddonExecutor(AddonConfig{
		Client:    client,
		Verifier:  verifier,
		Logger:    discardLogger(),
		Namespace: testNamespace,
		DaemonSet: testDaemonSet,
	})
	executor.interval = time.Millisecond

	return executor
}

func addonState() *State {
	return &State{TargetVersion: "v2.0.2", RollbackVersion: "v2.0.1"}
}

func TestAddonUpgradeSucceedsOnceRolledOutAndHealthy(t *testing.T) {
	verifier := &fakeVerifier{}
	executor := newAddonExecutor(fake.NewSimpleClientset(agentDaemonSet("2.0.2", 3, 3, 3)), verifier)

	require.NoError(t, executor.Upgrade(context.Background(), addonState()))
	assert.Equal(t, 1, verifier.calls)
}

func TestAddonUpgradeWaitsForTheAddonManagerToApply(t *testing.T) {
	client := fake.NewSimpleClientset(agentDaemonSet("2.0.1", 3, 3, 3))
	verifier := &fakeVerifier{}
	executor := newAddonExecutor(client, verifier)

	done := make(chan error, 1)
	go func() { done <- executor.Upgrade(context.Background(), addonState()) }()

	select {
	case err := <-done:
		t.Fatalf("Upgrade returned before the daemonset moved: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	_, err := client.AppsV1().DaemonSets(testNamespace).
		Update(context.Background(), agentDaemonSet("2.0.2", 3, 3, 3), metav1.UpdateOptions{})
	require.NoError(t, err)

	require.NoError(t, <-done)
	assert.Equal(t, 1, verifier.calls)
}

func TestAddonUpgradeReportsWhyTheRolloutDidNotComplete(t *testing.T) {
	tests := []struct {
		name      string
		daemonSet *appsv1.DaemonSet
		want      string
	}{
		{"never applied", agentDaemonSet("2.0.1", 3, 3, 3), `is at version "2.0.1"`},
		{"stalled on the first batch", agentDaemonSet("2.0.2", 10, 1, 9), "1 of 10 pods updated, 9 ready"},
		{"rolled but not ready", agentDaemonSet("2.0.2", 3, 3, 2), "3 of 3 pods updated, 2 ready"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			verifier := &fakeVerifier{}
			executor := newAddonExecutor(fake.NewSimpleClientset(tc.daemonSet), verifier)
			executor.rolloutWindow = 20 * time.Millisecond

			err := executor.Upgrade(context.Background(), addonState())
			require.ErrorIs(t, err, errRolloutPending)
			assert.Contains(t, err.Error(), tc.want)
			assert.Equal(t, 0, verifier.calls)
		})
	}
}

func TestAddonUpgradeWaitsForTheLatestGeneration(t *testing.T) {
	daemonSet := agentDaemonSet("2.0.2", 3, 3, 3)
	daemonSet.Status.ObservedGeneration = 1

	executor := newAddonExecutor(fake.NewSimpleClientset(daemonSet), &fakeVerifier{})
	executor.rolloutWindow = 20 * time.Millisecond

	require.ErrorIs(t, executor.Upgrade(context.Background(), addonState()), errRolloutPending)
}

func TestAddonUpgradeFailsWhenAgentsAreUnhealthy(t *testing.T) {
	verifier := &fakeVerifier{err: errAgentUnhealthy}
	executor := newAddonExecutor(fake.NewSimpleClientset(agentDaemonSet("2.0.2", 3, 3, 3)), verifier)

	err := executor.Upgrade(context.Background(), addonState())
	require.ErrorIs(t, err, errAgentUnhealthy)
	assert.Contains(t, err.Error(), "v2.0.2")
}

func TestAddonUpgradeReportsAMissingDaemonSet(t *testing.T) {
	executor := newAddonExecutor(fake.NewSimpleClientset(), &fakeVerifier{})
	executor.rolloutWindow = 20 * time.Millisecond

	err := executor.Upgrade(context.Background(), addonState())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading daemonset")
}

func TestAddonExecutorCannotRollBack(t *testing.T) {
	executor := newAddonExecutor(fake.NewSimpleClientset(), nil)

	require.ErrorIs(t, executor.Rollback(context.Background(), addonState()), errRollbackUnavailable)
	assert.True(t, observes(executor))
	assert.False(t, observes(&fakeExecutor{}))
}
