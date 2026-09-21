// K8s-specific Vector config generation.
package vector

import (
	"fmt"
	"net"
	"regexp"
	"strconv"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// PodType classifies a discovered Kubernetes pod for Vector config generation.
type PodType string

const (
	PodTypeDCGM   PodType = "dcgm_exporter"
	PodTypeAMD    PodType = "amd_exporter"
	PodTypeKSM    PodType = "kube_state_metrics"
	PodTypeSlurm  PodType = "slurm_metrics"
	PodTypeCME    PodType = "crusoe_metrics_exporter"
	PodTypeCustom PodType = "custom_metrics"
)

// ClassifiedPod holds a discovered pod's identity and scrape metadata.
type ClassifiedPod struct {
	Name string
	IP   string
	Type PodType
	// Custom metrics fields
	Port           int    // annotation crusoe.ai/port or default
	Path           string // annotation crusoe.ai/path or default
	AppID          string // annotation crusoe.ai/app_id
	DeploymentName string // inferred from pod name
}

// NodeLabels holds Crusoe-specific labels read from the K8s node at startup.
type NodeLabels struct {
	VMID         string
	NodepoolID   string
	InstanceType string
	PodID        string
	ProjectID    string
	Hostname     string
}

// ExporterConfig holds runtime parameters for a single exporter type.
type ExporterConfig struct {
	Enabled        bool
	Port           int
	Paths          []string
	ScrapeInterval int // seconds
}

// BuildEndpoints returns full scrape URLs for a pod IP.
func (e ExporterConfig) BuildEndpoints(podIP string) []string {
	eps := make([]string, len(e.Paths))
	for i, path := range e.Paths {
		eps[i] = "http://" + net.JoinHostPort(podIP, strconv.Itoa(e.Port)) + path
	}

	return eps
}

// ProxyConfig holds optional HTTP proxy settings for sinks.
type ProxyConfig struct {
	Enabled bool   `yaml:"enabled"`
	HTTP    string `yaml:"http,omitempty"`
	HTTPS   string `yaml:"https,omitempty"`
	NoProxy string `yaml:"noProxy,omitempty"`
}

func (p ProxyConfig) toMap() map[string]any {
	result := map[string]any{"enabled": p.Enabled}
	if p.HTTP != "" {
		result["http"] = p.HTTP
	}
	if p.HTTPS != "" {
		result["https"] = p.HTTPS
	}
	if p.NoProxy != "" {
		result["no_proxy"] = p.NoProxy
	}

	return result
}

// K8sConfig holds all runtime configuration for K8s Vector config generation.
type K8sConfig struct {
	DCGM  ExporterConfig
	AMD   ExporterConfig // AMD uses Paths[0] for single path
	KSM   ExporterConfig
	Slurm ExporterConfig
	CME   ExporterConfig

	CustomMetricsEnabled       bool
	CustomMetricsDefaultPort   int
	CustomMetricsDefaultPath   string
	CustomMetricsDefaultScrape int // default scrape interval for custom metrics
	LogsEnabled                bool

	// OperatorLogNamespaces are the namespaces whose GPU / network operator
	// Deployment logs are collected. Empty disables operator log collection.
	OperatorLogNamespaces []string

	SinkEndpoint string // base URL, e.g. "https://cms-monitoring.crusoecloud.com"
	Proxy        ProxyConfig

	// LogsEndpoint and MetricsEndpoint are control-plane base-URL overrides
	// (from config.apply) for the log and metric sinks respectively. They may
	// differ, so logs and metrics can be redirected independently. When empty
	// the sinks fall back to SinkEndpoint.
	LogsEndpoint    string
	MetricsEndpoint string

	// IngestionBlocked, when true (from ingestion.block), strips every sink
	// except the local internal-metrics exporter so nothing is forwarded off-host.
	IngestionBlocked bool

	// RateLimits (from rate_limit.set) maps a sink name to its forwarding cap in
	// requests per minute; the RateLimitAll key sets the default for sinks
	// without their own entry. Empty leaves Vector's default (unlimited).
	RateLimits map[string]int

	NodeLabels NodeLabels
}

// Base-URL helpers: control-plane overrides win over the deployment default.
func (c K8sConfig) logsBase() string {
	if c.LogsEndpoint != "" {
		return c.LogsEndpoint
	}

	return c.SinkEndpoint
}

func (c K8sConfig) metricsBase() string {
	if c.MetricsEndpoint != "" {
		return c.MetricsEndpoint
	}

	return c.SinkEndpoint
}

// Derived endpoint helpers.
func (c K8sConfig) infraEndpoint() string   { return c.metricsBase() + "/ingest" }
func (c K8sConfig) clusterEndpoint() string { return c.metricsBase() + "/cluster" }
func (c K8sConfig) customEndpoint() string  { return c.metricsBase() + "/custom" }
func (c K8sConfig) logsEndpoint() string    { return c.logsBase() + "/logs/ingest" }

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const (
	scrapeTimeoutPct       = 0.7
	scrapeIntervalMinK8s   = 5
	defaultCustomScrapeInt = 30

	operatorLogsSourceName    = "operator_kubernetes_logs"
	operatorLogsTransformName = "parse_operator_logs"
	// pod-template-hash is stamped on every Deployment pod and on no DaemonSet pod, so
	// it selects the operator Deployments without depending on names, which NVIDIA can change.
	operatorDeploymentSelector = "pod-template-hash"

	dcgmSourceName           = "dcgm_exporter_scrape"
	amdSourceName            = "amd_exporter_scrape"
	nodeMetricsTransformName = "enrich_node_metrics"
	amdFilterTransformName   = "amd_allowed_filter"
)

var sanitizeRe = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// SanitizeName replaces invalid Vector component name characters with underscores.
func SanitizeName(name string) string {
	return sanitizeRe.ReplaceAllString(name, "_")
}

// ---------------------------------------------------------------------------
// Top-level generator
// ---------------------------------------------------------------------------

// GenerateK8sBase returns the static K8s base config: data_dir, api, host_metrics,
// internal_metrics pipeline, node metrics pipeline, and the log pipeline.
// This is written to /etc/vector-base/vector.yaml at startup.
func GenerateK8sBase(cfg K8sConfig) map[string]any {
	baseCfg := defaultBaseConfig("/vector-data-dir")
	sources := ensureMap(baseCfg, "sources")
	transforms := ensureMap(baseCfg, "transforms")
	sinks := ensureMap(baseCfg, "sinks")

	buildStaticConfig(sources, transforms, sinks, cfg)
	applyLogs(sources, transforms, sinks, cfg)

	return baseCfg
}

// ApplyK8s overlays dynamic pod-dependent Vector configuration onto baseCfg.
// Adds DCGM, AMD, KSM, Slurm, CME, and custom metrics pipelines based on
// discovered pods and ConfigMap rules.
func ApplyK8s(baseCfg map[string]any, pods []ClassifiedPod, cmData map[string]string, cfg K8sConfig) {
	sources := ensureMap(baseCfg, "sources")
	transforms := ensureMap(baseCfg, "transforms")
	sinks := ensureMap(baseCfg, "sinks")

	buildDynamicConfig(sources, transforms, sinks, pods, cmData, cfg)

	applyRateLimit(sinks, cfg.RateLimits)

	if cfg.IngestionBlocked {
		removeExternalSinks(sinks)
	}
}

// GenerateK8s builds the complete K8s Vector config and returns YAML bytes.
// It creates a base config, applies the dynamic overlay, and marshals to YAML.
func GenerateK8s(pods []ClassifiedPod, cmData map[string]string, cfg K8sConfig) ([]byte, error) {
	baseCfg := GenerateK8sBase(cfg)
	ApplyK8s(baseCfg, pods, cmData, cfg)

	out, err := yaml.Marshal(baseCfg)
	if err != nil {
		return nil, fmt.Errorf("marshaling k8s vector config: %w", err)
	}

	return out, nil
}

func buildStaticConfig(sources, transforms, sinks map[string]any, cfg K8sConfig) {
	// Host metrics pipeline
	sources["host_metrics"] = hostMetricsSource()
	transforms[nodeMetricsTransformName] = remapTransform(
		[]string{"host_metrics"}, buildNodeMetricsTransformVRL(cfg.NodeLabels),
	)

	// Internal metrics pipeline
	sources["internal_metrics"] = internalMetricsSource()
	transforms["filter_internal_metrics"] = filterTransform([]string{"internal_metrics"}, vrlFilterInternalMetrics)
	transforms["add_internal_labels"] = remapTransform([]string{"filter_internal_metrics"}, vrlAddInternalLabelsK8s)
	sinks[internalMetricsExporterSinkName] = internalMetricsExporterSink()

	// Node metrics sink
	nodeMetricsSink := buildPromRemoteWriteSink(
		cfg.infraEndpoint(), "cri:vm/${VM_ID}", cfg.Proxy,
	)
	nodeMetricsSink["inputs"] = []string{nodeMetricsTransformName, "add_internal_labels"}
	sinks["cms_gateway_node_metrics"] = nodeMetricsSink
}

type partitionedPods struct {
	dcgmIP  string
	amdIP   string
	ksmIP   string
	slurmIP string
	cmeIP   string
	custom  []ClassifiedPod
}

func partitionPods(pods []ClassifiedPod) partitionedPods {
	var result partitionedPods
	for _, pod := range pods {
		switch pod.Type {
		case PodTypeDCGM:
			result.dcgmIP = pod.IP
		case PodTypeAMD:
			result.amdIP = pod.IP
		case PodTypeKSM:
			result.ksmIP = pod.IP
		case PodTypeSlurm:
			result.slurmIP = pod.IP
		case PodTypeCME:
			result.cmeIP = pod.IP
		case PodTypeCustom:
			result.custom = append(result.custom, pod)
		}
	}

	return result
}

func buildDynamicConfig(
	sources, transforms, sinks map[string]any,
	pods []ClassifiedPod,
	cmData map[string]string,
	cfg K8sConfig,
) {
	podsByType := partitionPods(pods)

	applyDCGM(sources, transforms, podsByType.dcgmIP, cfg)
	applyAMD(sources, transforms, podsByType.amdIP, cfg)

	applyClusterExporter(sources, transforms, sinks, podsByType.ksmIP, clusterExporterSpec{
		runtime:       cfg.KSM,
		sourceName:    "kube_state_metrics_scrape",
		transformName: "enrich_kube_state_metrics",
		sinkName:      "kube_state_metrics_sink",
		transformVRL:  vrlEnrichKSM,
		sinkConfig: buildPromRemoteWriteSink(
			cfg.clusterEndpoint(), "cri:cmk/${CRUSOE_CLUSTER_ID}", cfg.Proxy,
		),
	})
	applyClusterExporter(sources, transforms, sinks, podsByType.slurmIP, clusterExporterSpec{
		runtime:       cfg.Slurm,
		sourceName:    "slurm_metrics_scrape",
		transformName: "enrich_slurm_metrics",
		sinkName:      "slurm_metrics_sink",
		transformVRL:  vrlEnrichSlurm,
		sinkConfig: buildPromRemoteWriteSink(
			cfg.clusterEndpoint(), "cri:cmk/${CRUSOE_CLUSTER_ID}", cfg.Proxy,
		),
	})
	applyClusterExporter(sources, transforms, sinks, podsByType.cmeIP, clusterExporterSpec{
		runtime:       cfg.CME,
		sourceName:    "crusoe_metrics_exporter_scrape",
		transformName: "enrich_crusoe_metrics_exporter",
		sinkName:      "crusoe_metrics_exporter_sink",
		transformVRL:  vrlEnrichCME,
		sinkConfig: buildPromRemoteWriteSink(
			cfg.infraEndpoint(), "cri:vm/${VM_ID}", cfg.Proxy,
		),
	})

	applyCustomMetrics(sources, transforms, sinks, podsByType.custom, cmData, cfg)
}

// ---------------------------------------------------------------------------
// DCGM exporter
// ---------------------------------------------------------------------------

func applyDCGM(sources, transforms map[string]any, podIP string, cfg K8sConfig) {
	if podIP == "" || !cfg.DCGM.Enabled {
		return
	}
	endpoint := cfg.DCGM.BuildEndpoints(podIP)[0]
	sources[dcgmSourceName] = map[string]any{
		"type":                 "prometheus_scrape",
		"endpoints":            []string{endpoint},
		"scrape_interval_secs": cfg.DCGM.ScrapeInterval,
		"scrape_timeout_secs":  int(float64(cfg.DCGM.ScrapeInterval) * scrapeTimeoutPct),
	}
	wireIntoTransform(transforms, nodeMetricsTransformName, dcgmSourceName)
}

// ---------------------------------------------------------------------------
// AMD exporter
// ---------------------------------------------------------------------------

func applyAMD(sources, transforms map[string]any, podIP string, cfg K8sConfig) {
	if podIP == "" || !cfg.AMD.Enabled {
		return
	}
	endpoint := cfg.AMD.BuildEndpoints(podIP)[0]
	sources[amdSourceName] = map[string]any{
		"type":                 "prometheus_scrape",
		"endpoints":            []string{endpoint},
		"scrape_interval_secs": cfg.AMD.ScrapeInterval,
		"scrape_timeout_secs":  int(float64(cfg.AMD.ScrapeInterval) * scrapeTimeoutPct),
	}
	transforms[amdFilterTransformName] = filterTransform([]string{amdSourceName}, vrlAmdAllowlistFilter)
	wireIntoTransform(transforms, nodeMetricsTransformName, amdFilterTransformName)
}

// ---------------------------------------------------------------------------
// Cluster-scoped exporters (KSM, Slurm, CME)
// ---------------------------------------------------------------------------

type clusterExporterSpec struct {
	runtime       ExporterConfig
	sourceName    string
	transformName string
	sinkName      string
	transformVRL  string
	sinkConfig    map[string]any
}

func applyClusterExporter(sources, transforms, sinks map[string]any, podIP string, spec clusterExporterSpec) {
	if podIP == "" || !spec.runtime.Enabled {
		return
	}
	sources[spec.sourceName] = map[string]any{
		"type":                 "prometheus_scrape",
		"endpoints":            spec.runtime.BuildEndpoints(podIP),
		"scrape_interval_secs": spec.runtime.ScrapeInterval,
		"scrape_timeout_secs":  int(float64(spec.runtime.ScrapeInterval) * scrapeTimeoutPct),
	}
	transforms[spec.transformName] = map[string]any{
		"type":   "remap",
		"inputs": []string{spec.sourceName},
		"source": spec.transformVRL,
	}
	sink := copyMap(spec.sinkConfig)
	sink["inputs"] = []string{spec.transformName}
	sinks[spec.sinkName] = sink
}

// ---------------------------------------------------------------------------
// Custom metrics
// ---------------------------------------------------------------------------

func applyCustomMetrics(
	sources, transforms, sinks map[string]any,
	pods []ClassifiedPod,
	cmData map[string]string,
	cfg K8sConfig,
) {
	if !cfg.CustomMetricsEnabled || len(pods) == 0 {
		return
	}

	baseSinkConfig := buildPromRemoteWriteSink(
		cfg.customEndpoint(), "cri:custom_metrics/${CRUSOE_CLUSTER_ID}", cfg.Proxy,
	)

	for _, pod := range pods {
		applyCustomMetricsPod(sources, transforms, sinks, pod, cmData, cfg, baseSinkConfig)
	}
}

func applyCustomMetricsPod(
	sources, transforms, sinks map[string]any,
	pod ClassifiedPod,
	cmData map[string]string,
	cfg K8sConfig,
	baseSinkConfig map[string]any,
) {
	sanitized := SanitizeName(pod.Name)
	sourceName := sanitized + "_scrape"
	transformName := sanitized + "_transform"
	sinkName := sanitized + "_sink"

	deploymentCfg := getDeploymentMetricsConfig(pod.DeploymentName, cmData)
	scrapeInterval := resolveCustomScrapeInterval(deploymentCfg, cfg)

	endpoint := "http://" + net.JoinHostPort(pod.IP, strconv.Itoa(pod.Port)) + pod.Path
	sources[sourceName] = map[string]any{
		"type":                 "prometheus_scrape",
		"endpoints":            []string{endpoint},
		"scrape_interval_secs": scrapeInterval,
		"scrape_timeout_secs":  int(float64(scrapeInterval) * scrapeTimeoutPct),
	}

	transformVRL := buildCustomMetricsTransformVRL(deploymentCfg, pod, cfg.NodeLabels)
	transforms[transformName] = map[string]any{
		"type":          "remap",
		"inputs":        []string{sourceName},
		"drop_on_abort": true,
		"source":        transformVRL,
	}

	sink := copyMap(baseSinkConfig)
	sink["inputs"] = []string{transformName}
	if pod.AppID != "" {
		sink["endpoint"] = cfg.customEndpoint() + "/" + pod.AppID
	}
	sinks[sinkName] = sink
}

func resolveCustomScrapeInterval(deploymentCfg map[string]any, cfg K8sConfig) int {
	scrapeInterval := defaultCustomScrapeInt
	if cfg.CustomMetricsDefaultScrape > 0 {
		scrapeInterval = cfg.CustomMetricsDefaultScrape
	}
	if val, exists := deploymentCfg["scrape_interval_secs"]; exists {
		if intVal, isNum := toInt(val); isNum {
			if intVal < scrapeIntervalMinK8s {
				intVal = scrapeIntervalMinK8s
			}
			scrapeInterval = intVal
		}
	}

	return scrapeInterval
}

// getDeploymentMetricsConfig resolves per-deployment custom metrics rules from
// the crusoe-custom-metrics-config ConfigMap data.
func getDeploymentMetricsConfig(deploymentName string, cmData map[string]string) map[string]any {
	if deploymentName == "" {
		return nil
	}
	configYAML, found := cmData["custom-metrics-config.yaml"]
	if !found || configYAML == "" {
		return nil
	}
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(configYAML), &parsed); err != nil {
		return nil
	}
	depCfg, isMap := parsed[deploymentName].(map[string]any)
	if !isMap {
		return nil
	}

	return depCfg
}

// ---------------------------------------------------------------------------
// Logs pipeline
// ---------------------------------------------------------------------------

func applyLogs(sources, transforms, sinks map[string]any, cfg K8sConfig) {
	if !cfg.LogsEnabled {
		return
	}

	sources["journald_logs"] = map[string]any{
		"type":              "journald",
		"journal_directory": "/var/log/journal",
		"since_now":         true,
	}
	sources["vector_internal_logs"] = map[string]any{
		"type": "internal_logs",
	}
	transforms["filter_journald_noise"] = filterTransform([]string{"journald_logs"}, vrlFilterJournaldNoise)
	transforms["parse_journald_logs"] = remapTransform([]string{"filter_journald_noise"}, vrlParseJournaldLogsK8s)
	transforms["parse_internal_logs"] = remapTransform([]string{"vector_internal_logs"}, vrlParseInternalLogs)
	enrichInputs := []string{"parse_journald_logs", "parse_internal_logs"}

	// The glob's leading wildcard is <namespace>_<pod>_<uid>, so each source picks up its container in every namespace.
	for _, spec := range []k8sLogSpec{
		{
			sourceName:    "cwa_manager_logs",
			transformName: "parse_cwa_manager_logs",
			globs:         []string{"/var/log/pods/*/cwa-manager/*.log"},
			readFrom:      "beginning",
			transformVRL:  vrlParseCwaManagerLogsK8s,
		},
		{
			sourceName:    "report_runner_logs",
			transformName: "parse_report_runner_logs",
			globs:         []string{"/var/log/pods/*/report-runner/*.log"},
			readFrom:      "beginning",
			transformVRL:  vrlParseReportRunnerLogsK8s,
		},
		{
			sourceName:    "cwa_updater_logs",
			transformName: "parse_cwa_updater_logs",
			globs:         []string{"/var/log/pods/*/cwa-updater/*.log"},
			readFrom:      "beginning",
			transformVRL:  vrlParseCwaUpdaterLogsK8s,
		},
	} {
		enrichInputs = append(enrichInputs, applyK8sLog(sources, transforms, spec))
	}
	if operatorTransform := applyOperatorLogs(sources, transforms, cfg); operatorTransform != "" {
		enrichInputs = append(enrichInputs, operatorTransform)
	}
	transforms["enrich_logs"] = remapTransform(enrichInputs, vrlEnrichLogsK8s)

	sinks["crusoe_ingest"] = buildLogsSink(cfg)
}

// k8sLogSpec describes one kubernetes_logs source and the transform that parses it.
// Every source carries its own VRL, so namespaces and containers added later are not
// forced through a shared transform.
type k8sLogSpec struct {
	sourceName    string
	transformName string
	globs         []string
	// labelSelector is an optional extra_label_selector narrowing which pods are read.
	labelSelector string
	// readFrom is Vector's read_from: "beginning" for the whole file, "end" for new lines only.
	readFrom     string
	transformVRL string
}

// applyK8sLog wires a kubernetes_logs source into its parse transform and returns the
// transform name, for feeding into enrich_logs. Names are Vector's global component
// namespace: a reused name overwrites the earlier component.
func applyK8sLog(sources, transforms map[string]any, spec k8sLogSpec) string {
	source := map[string]any{
		"type":                        "kubernetes_logs",
		"include_paths_glob_patterns": spec.globs,
		"read_from":                   spec.readFrom,
	}
	if spec.labelSelector != "" {
		source["extra_label_selector"] = spec.labelSelector
	}
	sources[spec.sourceName] = source
	transforms[spec.transformName] = remapTransform([]string{spec.sourceName}, spec.transformVRL)

	return spec.transformName
}

// applyOperatorLogs adds the GPU / network operator Deployment log pipeline and returns its
// transform name, or "" when no namespaces are configured. It gets its own source so the
// Deployment-only label selector stays scoped to these namespaces: DaemonSet log files are
// never opened, so the glob and the selector are exact and no filter transform is needed.
func applyOperatorLogs(sources, transforms map[string]any, cfg K8sConfig) string {
	if len(cfg.OperatorLogNamespaces) == 0 {
		return ""
	}

	globs := make([]string, len(cfg.OperatorLogNamespaces))
	for i, namespace := range cfg.OperatorLogNamespaces {
		globs[i] = "/var/log/pods/" + namespace + "_*/*/*.log"
	}

	return applyK8sLog(sources, transforms, k8sLogSpec{
		sourceName:    operatorLogsSourceName,
		transformName: operatorLogsTransformName,
		globs:         globs,
		labelSelector: operatorDeploymentSelector,
		// Collect from agent install forward.
		readFrom:     "end",
		transformVRL: vrlParseOperatorLogsK8s,
	})
}

func buildLogsSink(cfg K8sConfig) map[string]any {
	sinkConfig := map[string]any{
		"type":        "http",
		"inputs":      []string{"enrich_logs"},
		"uri":         cfg.logsEndpoint(),
		"framing":     map[string]any{"method": "newline_delimited"},
		"compression": "snappy",
		"healthcheck": map[string]any{"enabled": false},
		"request": map[string]any{
			"headers": map[string]any{
				"X-Crusoe-Vm-Id": "${VM_ID:-unknown}",
				"User-Agent":     "CrusoeWatchAgent/CMK-${AGENT_VERSION}",
			},
			"timeout_secs": requestTimeoutSecs,
		},
		"auth":     map[string]any{"strategy": "bearer", "token": "${CRUSOE_MONITORING_TOKEN}"},
		"encoding": map[string]any{"codec": "json"},
		"batch":    map[string]any{"max_bytes": logBatchMaxBytes},
		"tls":      tlsConfig(cfg.Proxy.Enabled),
	}
	if cfg.Proxy.Enabled {
		sinkConfig["proxy"] = cfg.Proxy.toMap()
	}

	return sinkConfig
}

// ---------------------------------------------------------------------------
// Sink builders
// ---------------------------------------------------------------------------

func buildPromRemoteWriteSink(endpoint, tenantID string, proxy ProxyConfig) map[string]any {
	cfg := map[string]any{
		"type":        "prometheus_remote_write",
		"endpoint":    endpoint,
		"tenant_id":   tenantID,
		"auth":        map[string]any{"strategy": "bearer", "token": "${CRUSOE_MONITORING_TOKEN}"},
		"healthcheck": map[string]any{"enabled": false},
		"compression": "snappy",
		"request": map[string]any{
			"headers": map[string]any{
				"X-Crusoe-Vm-Id": "${VM_ID:-unknown}",
				"User-Agent":     "CrusoeWatchAgent/CMK-${AGENT_VERSION}",
			},
			"concurrency":  "adaptive",
			"timeout_secs": requestTimeoutSecs,
		},
		"batch":  map[string]any{"max_bytes": metricBatchMaxBytes, "aggregate": false},
		"buffer": diskBufferConfig(),
		"tls":    tlsConfig(proxy.Enabled),
	}
	if proxy.Enabled {
		cfg["proxy"] = proxy.toMap()
	}

	return cfg
}

// ---------------------------------------------------------------------------
// K8s-only helpers
// ---------------------------------------------------------------------------

func copyMap(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for key, val := range src {
		dst[key] = val
	}

	return dst
}

func toStringSlice(val any) ([]string, bool) {
	if val == nil {
		return nil, false
	}
	switch slice := val.(type) {
	case []string:
		return slice, true
	case []any:
		out := make([]string, 0, len(slice))
		for _, item := range slice {
			if str, isStr := item.(string); isStr {
				out = append(out, str)
			}
		}

		return out, len(out) > 0
	default:
		return nil, false
	}
}

func toInt(val any) (int, bool) {
	switch num := val.(type) {
	case int:
		return num, true
	case int64:
		return int(num), true
	case float64:
		return int(num), true
	default:
		return 0, false
	}
}
