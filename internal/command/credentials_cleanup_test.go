package command

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "gitlab.com/crusoeenergy/schemas/api/island/v2/observability"
)

type fakeSecretDeleter struct {
	namespace string
	name      string
	called    bool
	removed   bool
	err       error
}

func (f *fakeSecretDeleter) DeleteSecret(_ context.Context, namespace, name string) (bool, error) {
	f.called = true
	f.namespace = namespace
	f.name = name

	return f.removed, f.err
}

func TestCredentialsCleanup_VM_RemovesToken(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "secrets", ".monitoring-token")
	require.NoError(t, os.MkdirAll(filepath.Dir(tokenPath), 0o750))
	require.NoError(t, os.WriteFile(tokenPath, []byte("legacy-token\n"), 0o600))

	h := NewCredentialsCleanup(Deps{
		InstallType:         pb.CwaInstallType_CWA_INSTALL_TYPE_DOCKER,
		MonitoringTokenPath: tokenPath,
	})

	result, err := h.Run(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, credentialRemoved, result)
	assert.NoFileExists(t, tokenPath)
}

func TestCredentialsCleanup_VM_AlreadyAbsent(t *testing.T) {
	h := NewCredentialsCleanup(Deps{
		InstallType:         pb.CwaInstallType_CWA_INSTALL_TYPE_SYSTEMD,
		MonitoringTokenPath: filepath.Join(t.TempDir(), "secrets", ".monitoring-token"),
	})

	result, err := h.Run(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, credentialAbsent, result)
}

func TestCredentialsCleanup_VM_UnsetPath(t *testing.T) {
	// An empty path is a misconfig, not an already-absent credential.
	h := NewCredentialsCleanup(Deps{InstallType: pb.CwaInstallType_CWA_INSTALL_TYPE_DOCKER})

	_, err := h.Run(context.Background(), nil)
	assert.ErrorIs(t, err, errTokenPathUnset)
}

func TestCredentialsCleanup_VM_RemoveError(t *testing.T) {
	// A non-empty directory can't be removed by os.Remove, surfacing an error
	// that is neither success nor already-absent.
	tokenPath := filepath.Join(t.TempDir(), "token-dir")
	require.NoError(t, os.MkdirAll(filepath.Join(tokenPath, "child"), 0o750))

	h := NewCredentialsCleanup(Deps{
		InstallType:         pb.CwaInstallType_CWA_INSTALL_TYPE_DOCKER,
		MonitoringTokenPath: tokenPath,
	})

	_, err := h.Run(context.Background(), nil)
	assert.Error(t, err)
}

func TestCredentialsCleanup_K8s_DeletesSecret(t *testing.T) {
	del := &fakeSecretDeleter{removed: true}

	h := NewCredentialsCleanup(Deps{
		InstallType:   pb.CwaInstallType_CWA_INSTALL_TYPE_KUBERNETES,
		SecretDeleter: del,
	})

	result, err := h.Run(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, credentialRemoved, result)
	assert.True(t, del.called)
	assert.Equal(t, "crusoe-system", del.namespace)
	assert.Equal(t, "crusoe-monitoring-token", del.name)
}

func TestCredentialsCleanup_K8s_AlreadyAbsent(t *testing.T) {
	h := NewCredentialsCleanup(Deps{
		InstallType:   pb.CwaInstallType_CWA_INSTALL_TYPE_KUBERNETES,
		SecretDeleter: &fakeSecretDeleter{removed: false},
	})

	result, err := h.Run(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, credentialAbsent, result)
}

func TestCredentialsCleanup_K8s_DeleteError(t *testing.T) {
	h := NewCredentialsCleanup(Deps{
		InstallType:   pb.CwaInstallType_CWA_INSTALL_TYPE_KUBERNETES,
		SecretDeleter: &fakeSecretDeleter{err: errors.New("api down")},
	})

	_, err := h.Run(context.Background(), nil)
	assert.Error(t, err)
}

func TestCredentialsCleanup_K8s_NoDeleter(t *testing.T) {
	// Degraded agent: the in-cluster client failed to build at startup.
	h := NewCredentialsCleanup(Deps{InstallType: pb.CwaInstallType_CWA_INSTALL_TYPE_KUBERNETES})

	_, err := h.Run(context.Background(), nil)
	assert.ErrorIs(t, err, errSecretDeleterUnavailable)
}

func TestCredentialsCleanup_UnsupportedInstallType(t *testing.T) {
	h := NewCredentialsCleanup(Deps{InstallType: pb.CwaInstallType_CWA_INSTALL_TYPE_UNSPECIFIED})

	_, err := h.Run(context.Background(), nil)
	assert.ErrorIs(t, err, errUnsupportedInstallType)
}

func TestCredentialsCleanup_Timeout(t *testing.T) {
	assert.Equal(t, Instant, NewCredentialsCleanup(Deps{}).Timeout())
}
