package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// modeFile writes content to a mode file and returns its path.
func modeFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), ".install-mode")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

// offCluster clears the cluster signal. CI runners are themselves pods, so the
// VM cases have to say so rather than inherit the runner's environment.
func offCluster(t *testing.T) {
	t.Helper()
	t.Setenv(envK8sServiceHost, "")
}

// In a pod the mode file is absent, and the cluster environment is the answer.
func TestDetectInstallModeKubernetes(t *testing.T) {
	t.Setenv(envK8sServiceHost, "10.0.0.1")

	mode, err := detectInstallMode("/nonexistent")
	require.NoError(t, err)
	assert.Equal(t, modeKubernetes, mode)
}

// The cluster environment wins: a node whose host filesystem carries a stale
// mode file from a VM install must not be mistaken for a VM.
func TestDetectInstallModeKubernetesWins(t *testing.T) {
	t.Setenv(envK8sServiceHost, "10.0.0.1")

	mode, err := detectInstallMode(modeFile(t, "native"))
	require.NoError(t, err)
	assert.Equal(t, modeKubernetes, mode)
}

func TestDetectInstallModeVM(t *testing.T) {
	offCluster(t)

	for _, tc := range []struct {
		content string
		want    installMode
	}{
		{"native", modeNative},
		{"docker", modeDocker},
		{"native\n", modeNative},
		{"  docker  \n", modeDocker},
	} {
		mode, err := detectInstallMode(modeFile(t, tc.content))
		require.NoError(t, err, tc.content)
		assert.Equal(t, tc.want, mode, tc.content)
	}
}

// An updater that cannot tell where its state lives must not start: reporting
// idle would lose an upgrade that was in flight.
func TestDetectInstallModeUnreadable(t *testing.T) {
	offCluster(t)

	_, err := detectInstallMode(filepath.Join(t.TempDir(), "absent"))
	assert.ErrorIs(t, err, errUnknownInstallMode)
}

func TestDetectInstallModeUnrecognised(t *testing.T) {
	offCluster(t)

	for _, content := range []string{"", "kubernetes", "systemd", "garbage"} {
		_, err := detectInstallMode(modeFile(t, content))
		assert.ErrorIs(t, err, errUnknownInstallMode, content)
	}
}

// The record must outlive a reboot, so it cannot live on the /run tmpfs.
func TestVMStatePathIsPersistent(t *testing.T) {
	assert.False(t, strings.HasPrefix(vmStatePath, "/run/"), vmStatePath)
}

// Until the bundle executor lands a VM upgrade fails, but the rollback that
// follows reports clean: nothing was installed, so the host is still on
// rollback_version.
func TestVMExecutorPlaceholder(t *testing.T) {
	assert.ErrorIs(t, vmExecutor{}.Upgrade(context.Background(), nil), errNoVMExecutor)
	assert.NoError(t, vmExecutor{}.Rollback(context.Background(), nil))
}
