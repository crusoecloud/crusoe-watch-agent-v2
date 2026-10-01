package command

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	pb "gitlab.com/crusoeenergy/schemas/api/island/v2/observability"
)

// CredentialsCleanupCommand removes the host's legacy static bearer-token credential.
const CredentialsCleanupCommand = "credentials.cleanup"

// K8s Secret holding the legacy static monitoring token. The name and namespace
// match the chart's cwa.monitoringSecretName helper and default namespace.
const (
	monitoringSecretNamespace = "crusoe-system"
	monitoringSecretName      = "crusoe-monitoring-token"
)

// Result payloads distinguishing a deletion from an already-clean host.
const (
	credentialRemoved = "removed"
	credentialAbsent  = "already absent"
)

var (
	errSecretDeleterUnavailable = errors.New("k8s secret client unavailable")
	errTokenPathUnset           = errors.New("monitoring token path not configured")
)

// CredentialsCleanup handles credentials.cleanup: it removes the legacy static
// bearer-token credential from the host (file on VMs, a Secret on K8s).
//
// No server-side revocation happens here: the token's public identifier is not
// stored alongside it and cannot be recovered after creation.
//
// Removal is idempotent, an already-absent credential still succeeds.
type CredentialsCleanup struct {
	deps Deps
}

// NewCredentialsCleanup creates a credentials.cleanup handler.
func NewCredentialsCleanup(deps Deps) *CredentialsCleanup {
	return &CredentialsCleanup{deps: deps}
}

// Timeout returns the instant class (credentials.cleanup is a 30s command).
func (c *CredentialsCleanup) Timeout() time.Duration { return Instant }

// Run removes the legacy static credential for the host's install type.
func (c *CredentialsCleanup) Run(ctx context.Context, _ map[string]string) (string, error) {
	switch c.deps.InstallType {
	case pb.CwaInstallType_CWA_INSTALL_TYPE_KUBERNETES:
		return c.cleanupK8s(ctx)
	case pb.CwaInstallType_CWA_INSTALL_TYPE_DOCKER, pb.CwaInstallType_CWA_INSTALL_TYPE_SYSTEMD:
		return c.cleanupVM()
	case pb.CwaInstallType_CWA_INSTALL_TYPE_UNSPECIFIED:
	}

	return "", fmt.Errorf("%w: %s", errUnsupportedInstallType, c.deps.InstallType)
}

// cleanupVM deletes the monitoring-token file.
func (c *CredentialsCleanup) cleanupVM() (string, error) {
	if c.deps.MonitoringTokenPath == "" {
		return "", errTokenPathUnset
	}

	if err := os.Remove(c.deps.MonitoringTokenPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return credentialAbsent, nil
		}

		return "", fmt.Errorf("removing monitoring token: %w", err)
	}

	return credentialRemoved, nil
}

// cleanupK8s deletes the monitoring-token Secret.
func (c *CredentialsCleanup) cleanupK8s(ctx context.Context) (string, error) {
	if c.deps.SecretDeleter == nil {
		return "", errSecretDeleterUnavailable
	}

	removed, err := c.deps.SecretDeleter.DeleteSecret(ctx, monitoringSecretNamespace, monitoringSecretName)
	if err != nil {
		return "", fmt.Errorf("deleting monitoring secret: %w", err)
	}

	if !removed {
		return credentialAbsent, nil
	}

	return credentialRemoved, nil
}
