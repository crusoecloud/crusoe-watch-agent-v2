package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/atomicfile"
)

// configKey holds the serialised RuntimeConfig in the handoff ConfigMap, so no
// new object or RBAC rule is needed.
const configKey = "config"

// Bounds on the tunable values: too short a window rolls every upgrade back, an
// unbounded one holds a host in upgrade_in_progress. The floor fits an upgrade
// with a retry or two, not sustained download failures; the window caps those.
const (
	minRollbackTimeoutMin = 5
	maxRollbackTimeoutMin = 120
	maxBackoffMaxSec      = 3600
)

// Configurable field names. These are both the JSON keys and the
// configure-updater parameter names.
const (
	FieldDownloadURLBase       = "download_url_base"
	FieldRollbackTimeoutMin    = "rollback_timeout_min"
	FieldDownloadBackoffMaxSec = "download_backoff_max_sec"
)

// ErrInvalidConfig marks a configuration cwa-updater refuses. It never succeeds
// on retry.
var ErrInvalidConfig = errors.New("invalid updater configuration")

// RuntimeConfig is cwa-updater's tunable configuration, delivered by the
// configure-updater command and read back at startup. Every field is optional; an
// unset one keeps the default. A known field this executor does not act on
// is still stored, so a later build honours what was already sent.
//
//nolint:tagliatelle // snake_case is the configure-updater wire contract
type RuntimeConfig struct {
	// DownloadURLBase points the VM installer at an internal artifact mirror.
	DownloadURLBase string `json:"download_url_base,omitempty"`
	// RollbackTimeoutMin bounds an upgrade attempt, health gate included.
	RollbackTimeoutMin int `json:"rollback_timeout_min,omitempty"`
	// DownloadBackoffMaxSec caps the wait between retries: registry calls on
	// Kubernetes, release fetches on VM.
	DownloadBackoffMaxSec int `json:"download_backoff_max_sec,omitempty"`
}

// RollbackTimeout is RollbackTimeoutMin as a duration; zero when unset.
func (c RuntimeConfig) RollbackTimeout() time.Duration {
	return time.Duration(c.RollbackTimeoutMin) * time.Minute
}

// attemptWindow is the deadline one upgrade or rollback attempt gets: the
// delivered rollback_timeout_min when set, fallback otherwise.
func attemptWindow(settings *Holder, fallback time.Duration) time.Duration {
	if applied := settings.Get().RollbackTimeout(); applied > 0 {
		return applied
	}

	return fallback
}

// validateDownloadURLBase rejects a mirror the installer could not fetch from.
func (c RuntimeConfig) validateDownloadURLBase() error {
	if c.DownloadURLBase == "" {
		return nil
	}

	parsed, err := url.Parse(c.DownloadURLBase)

	switch {
	case err != nil:
		return fmt.Errorf("%w: download_url_base: %w", ErrInvalidConfig, err)
	case parsed.Scheme != "https":
		// Signatures are checked anyway, but plaintext leaks who upgrades to what.
		return fmt.Errorf("%w: download_url_base must be https, got %q",
			ErrInvalidConfig, c.DownloadURLBase)
	case parsed.Host == "":
		return fmt.Errorf("%w: download_url_base has no host: %q", ErrInvalidConfig, c.DownloadURLBase)
	}

	return nil
}

// Validate rejects values that would leave cwa-updater unable to upgrade a host.
func (c RuntimeConfig) Validate() error {
	if err := c.validateDownloadURLBase(); err != nil {
		return err
	}

	if c.RollbackTimeoutMin != 0 &&
		(c.RollbackTimeoutMin < minRollbackTimeoutMin || c.RollbackTimeoutMin > maxRollbackTimeoutMin) {

		return fmt.Errorf("%w: rollback_timeout_min must be between %d and %d, got %d",
			ErrInvalidConfig, minRollbackTimeoutMin, maxRollbackTimeoutMin, c.RollbackTimeoutMin)
	}

	if c.DownloadBackoffMaxSec < 0 || c.DownloadBackoffMaxSec > maxBackoffMaxSec {
		return fmt.Errorf("%w: download_backoff_max_sec must be between 0 and %d, got %d",
			ErrInvalidConfig, maxBackoffMaxSec, c.DownloadBackoffMaxSec)
	}

	return nil
}

// setFields names the fields the config sets, in declaration order.
func (c RuntimeConfig) setFields() []string {
	var fields []string

	if c.DownloadURLBase != "" {
		fields = append(fields, FieldDownloadURLBase)
	}

	if c.RollbackTimeoutMin != 0 {
		fields = append(fields, FieldRollbackTimeoutMin)
	}

	if c.DownloadBackoffMaxSec != 0 {
		fields = append(fields, FieldDownloadBackoffMaxSec)
	}

	return fields
}

// ConfigAck is cwa-updater's reply to a delivered config: what is now live, and
// which of those fields it acts on. The reply itself confirms the reload.
type ConfigAck struct {
	Config      RuntimeConfig `json:"config"`
	Applied     []string      `json:"applied,omitempty"`
	Unsupported []string      `json:"unsupported,omitempty"`
}

// NewConfigAck describes cfg as cwa-updater just made it live. honored names
// the fields this executor reads; the rest are reported unsupported, so a
// result never claims a change that did not happen.
func NewConfigAck(cfg RuntimeConfig, honored []string) ConfigAck {
	ack := ConfigAck{Config: cfg}

	for _, field := range cfg.setFields() {
		if slices.Contains(honored, field) {
			ack.Applied = append(ack.Applied, field)
		} else {
			ack.Unsupported = append(ack.Unsupported, field)
		}
	}

	return ack
}

// ConfigStore is where a RuntimeConfig outlives the process: a local file on VM
// targets, the handoff ConfigMap on Kubernetes. Load returns the zero config
// when none is recorded.
type ConfigStore interface {
	Load(ctx context.Context) (RuntimeConfig, error)
	Save(ctx context.Context, cfg RuntimeConfig) error
}

// Holder owns the live RuntimeConfig. Executors read it per run, so an applied
// change takes effect on the next upgrade without rebuilding anything.
type Holder struct {
	store   ConfigStore
	honored []string

	mu  sync.RWMutex
	cfg RuntimeConfig
}

// NewHolder returns a Holder backed by store, holding the zero config until
// Load. honored names the fields this executor reads.
func NewHolder(store ConfigStore, honored []string) *Holder {
	return &Holder{store: store, honored: honored}
}

// Ack describes the live config against what this executor honors.
func (h *Holder) Ack() ConfigAck {
	return NewConfigAck(h.Get(), h.honored)
}

// Get returns the live configuration; a nil Holder reads as the zero config.
func (h *Holder) Get() RuntimeConfig {
	if h == nil {
		return RuntimeConfig{}
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	return h.cfg
}

// Load reads the persisted configuration in at startup.
func (h *Holder) Load(ctx context.Context) error {
	cfg, err := h.store.Load(ctx)
	if err != nil {
		return fmt.Errorf("loading the updater config: %w", err)
	}

	// A hand-edited file can be invalid; keep the current config over a bad one.
	if err := cfg.Validate(); err != nil {
		return err
	}

	h.mu.Lock()
	h.cfg = cfg
	h.mu.Unlock()

	return nil
}

// Apply validates cfg, persists it, then makes it live. The delivery carries the
// whole configuration, so this replaces rather than merges: an omitted field
// returns to its default, and a drifted host converges on the next command.
//
// Persisting first means a crash loses nothing: the next start reads what was stored.
func (h *Holder) Apply(ctx context.Context, cfg RuntimeConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	if err := h.store.Save(ctx, cfg); err != nil {
		return fmt.Errorf("persisting the updater config: %w", err)
	}

	h.mu.Lock()
	h.cfg = cfg
	h.mu.Unlock()

	return nil
}

// ---------------------------------------------------------------------------
// VM: a local file
// ---------------------------------------------------------------------------

// FileConfigStore persists the config next to the upgrade record, root-only: it
// names what gets installed.
type FileConfigStore struct {
	path string
}

// NewFileConfigStore returns a store backed by the file at path.
func NewFileConfigStore(path string) *FileConfigStore {
	return &FileConfigStore{path: path}
}

// Load reads the persisted config, returning the zero config when none exists.
func (s *FileConfigStore) Load(context.Context) (RuntimeConfig, error) {
	data, err := os.ReadFile(s.path)

	switch {
	case errors.Is(err, os.ErrNotExist):
		return RuntimeConfig{}, nil
	case err != nil:
		return RuntimeConfig{}, fmt.Errorf("reading updater config %s: %w", s.path, err)
	case len(strings.TrimSpace(string(data))) == 0:
		return RuntimeConfig{}, nil
	}

	var cfg RuntimeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return RuntimeConfig{}, fmt.Errorf("%w: parsing %s: %w", ErrInvalidConfig, s.path, err)
	}

	return cfg, nil
}

// Save durably records cfg.
func (s *FileConfigStore) Save(_ context.Context, cfg RuntimeConfig) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling updater config: %w", err)
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, stateDirMode); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	if err := atomicfile.Write(s.path, data, stateFileMode); err != nil {
		return fmt.Errorf("writing updater config %s: %w", s.path, err)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Kubernetes: a key in the handoff ConfigMap
// ---------------------------------------------------------------------------

// ConfigMapConfigStore persists the config in the handoff ConfigMap, which the
// chart keeps across a pod restart. Kubernetes only.
type ConfigMapConfigStore struct {
	client    kubernetes.Interface
	namespace string
	name      string
}

// NewConfigMapConfigStore returns a store backed by the named ConfigMap.
func NewConfigMapConfigStore(client kubernetes.Interface, namespace, name string) *ConfigMapConfigStore {
	return &ConfigMapConfigStore{client: client, namespace: namespace, name: name}
}

// Load reads the persisted config, returning the zero config when none is recorded.
func (s *ConfigMapConfigStore) Load(ctx context.Context) (RuntimeConfig, error) {
	configMap, err := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("reading handoff configmap %s/%s: %w", s.namespace, s.name, err)
	}

	raw, ok := configMap.Data[configKey]
	if !ok || raw == "" {
		return RuntimeConfig{}, nil
	}

	var cfg RuntimeConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return RuntimeConfig{}, fmt.Errorf("%w: parsing %s/%s: %w",
			ErrInvalidConfig, s.namespace, s.name, err)
	}

	return cfg, nil
}

// Save records cfg, retrying the conflict a concurrent upgrade write causes.
func (s *ConfigMapConfigStore) Save(ctx context.Context, cfg RuntimeConfig) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling updater config: %w", err)
	}

	return updateConfigMapKey(ctx, s.client, s.namespace, s.name, configKey, string(data))
}
