// Package identity resolves and persists the agent's identity fields
// (vm_id, install_type, agent_id) used in registration and heartbeats.
package identity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	pb "gitlab.com/crusoeenergy/schemas/api/island/v2/observability"
)

// errNoVMID is returned when the VM UUID cannot be read from any source.
var errNoVMID = errors.New("could not read VM UUID from DMI files or dmidecode")

const (
	// DMI paths for reading the VM UUID.
	dmiPathVM  = "/sys/class/dmi/id/product_uuid"
	dmiPathK8s = "/host/sys/class/dmi/id/product_uuid"

	// File where the agent_id is persisted after registration.
	agentIDPath = "/etc/crusoe/.agent-id"

	// Install mode file written by the VM installer script.
	installModeFile = "/etc/crusoe/crusoe_watch_agent/.install-mode"

	// Environment variables.
	envK8sServiceHost = "KUBERNETES_SERVICE_HOST"
	// envNodeName carries the host FQDN. In K8s it's set via the downward API
	// (spec.nodeName); on VMs the installer writes `hostname -f` into the .env.
	envNodeName = "NODE_NAME"
	// envProjectID is sourced from the crusoe-secrets Secret via envFrom in K8s
	// mode (the same secret Vector consumes). Unset on VMs.
	envProjectID = "CRUSOE_PROJECT_ID"
)

// Identity holds the resolved identity fields for this agent.
type Identity struct {
	VMID        string
	InstallType pb.CwaInstallType
	AgentID     string
	Region      string
	ProjectID   string
}

// Resolver resolves identity fields from the local environment.
type Resolver struct{}

// NewResolver creates a Resolver.
func NewResolver() *Resolver {
	return &Resolver{}
}

// Resolve reads identity fields from the local environment.
// agent_id is loaded from disk if previously persisted; otherwise left empty
// (the caller must Register and then call PersistAgentID).
func (r *Resolver) Resolve(ctx context.Context) (*Identity, error) {
	vmID, err := readVMID(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading vm_id: %w", err)
	}

	identity := &Identity{
		VMID:        vmID,
		InstallType: detectInstallType(),
		Region:      readRegion(),
		ProjectID:   readProjectID(),
	}

	agentID, err := os.ReadFile(agentIDPath)
	if err == nil {
		identity.AgentID = strings.TrimSpace(string(agentID))
	}
	// If the file doesn't exist, AgentID stays empty — caller must register.

	return identity, nil
}

// PersistAgentID writes the agent_id to disk so it survives restarts.
func (r *Resolver) PersistAgentID(agentID string) error {
	dir := filepath.Dir(agentIDPath)

	//nolint:mnd // 0o750 restricts access to owner and group
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating agent-id directory: %w", err)
	}

	//nolint:mnd // 0o600 is restrictive file permissions for secrets
	if err := os.WriteFile(agentIDPath, []byte(agentID+"\n"), 0o600); err != nil {
		return fmt.Errorf("writing agent-id: %w", err)
	}

	return nil
}

// Registered returns true if the agent has a persisted agent_id.
func (r *Resolver) Registered() bool {
	_, err := os.Stat(agentIDPath)

	return err == nil
}

func readProjectID() string {
	return strings.TrimSpace(os.Getenv(envProjectID))
}

// readRegion parses the Crusoe region from NODE_NAME.
// "<host>.us-east1-a.compute.internal" → "us-east1-a".
func readRegion() string {
	name := strings.TrimSpace(os.Getenv(envNodeName))

	_, domain, ok := strings.Cut(name, ".")
	if !ok {
		return ""
	}

	region, _, _ := strings.Cut(domain, ".")

	return region
}

func readVMID(ctx context.Context) (string, error) {
	// In Kubernetes, the host filesystem is mounted at /host.
	for _, path := range []string{dmiPathK8s, dmiPathVM} {
		data, err := os.ReadFile(path)
		if err == nil {
			if id := strings.TrimSpace(string(data)); id != "" {
				return id, nil
			}
		}
	}

	// Fall back to dmidecode (works regardless of sysfs permissions).
	out, err := exec.CommandContext(ctx, "dmidecode", "-s", "system-uuid").Output()
	if err == nil {
		if id := strings.TrimSpace(string(out)); id != "" {
			return id, nil
		}
	}

	return "", errNoVMID
}

func detectInstallType() pb.CwaInstallType {
	if os.Getenv(envK8sServiceHost) != "" {
		return pb.CwaInstallType_CWA_INSTALL_TYPE_KUBERNETES
	}

	// On VMs, the installer persists "docker" or "native" to the install-mode file.
	data, err := os.ReadFile(installModeFile)
	if err != nil {
		return pb.CwaInstallType_CWA_INSTALL_TYPE_UNSPECIFIED
	}

	mode := strings.TrimSpace(string(data))

	switch mode {
	case "docker":
		return pb.CwaInstallType_CWA_INSTALL_TYPE_DOCKER
	case "native":
		return pb.CwaInstallType_CWA_INSTALL_TYPE_SYSTEMD
	default:
		return pb.CwaInstallType_CWA_INSTALL_TYPE_UNSPECIFIED
	}
}
