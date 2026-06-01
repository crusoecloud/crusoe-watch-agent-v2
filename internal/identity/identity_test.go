package identity

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	pb "gitlab.com/crusoeenergy/schemas/api/island/v2/observability"
)

func TestDetectInstallType(t *testing.T) {
	t.Run("kubernetes when env var set", func(t *testing.T) {
		t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
		assert.Equal(t, pb.CwaInstallType_CWA_INSTALL_TYPE_KUBERNETES, detectInstallType())
	})

	t.Run("unspecified when no env and no file", func(t *testing.T) {
		t.Setenv("KUBERNETES_SERVICE_HOST", "")
		assert.Equal(t, pb.CwaInstallType_CWA_INSTALL_TYPE_UNSPECIFIED, detectInstallType())
	})
}

func TestDetectInstallType_KubernetesWins(t *testing.T) {
	// Even if install-mode file existed, K8s env var takes precedence.
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	assert.Equal(t, pb.CwaInstallType_CWA_INSTALL_TYPE_KUBERNETES, detectInstallType())
}

func TestErrNoVMID(t *testing.T) {
	assert.NotNil(t, errNoVMID)
	assert.Equal(t, "could not read VM UUID from DMI files or dmidecode", errNoVMID.Error())
}

func TestReadVMID_FailsGracefully(t *testing.T) {
	// On dev machines, DMI files and dmidecode won't be available.
	_, err := readVMID(context.Background())
	if err == nil {
		return // running on a Linux VM where DMI is readable
	}

	assert.ErrorIs(t, err, errNoVMID)
}

func TestReadVMID_RespectsContextCancellation(t *testing.T) {
	// readVMID tries DMI files first (no context check), then falls back to dmidecode.
	// On CI/VM runners where DMI files are readable, readVMID succeeds before
	// reaching the context-aware dmidecode path, so we skip in that case.
	if _, err := readVMID(context.Background()); err == nil {
		t.Skip("DMI files are readable; context cancellation only affects dmidecode fallback")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := readVMID(ctx)
	assert.Error(t, err)
}

func TestNewResolver(t *testing.T) {
	r := NewResolver()
	assert.NotNil(t, r)
}

func TestRegistered_FalseWhenNoFile(t *testing.T) {
	// On dev machines, /etc/crusoe/.agent-id doesn't exist.
	r := NewResolver()
	assert.False(t, r.Registered())
}
