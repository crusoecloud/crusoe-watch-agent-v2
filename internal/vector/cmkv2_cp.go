// CMKv2 control-plane (CP) log collection for CWA 2.0 cell mode. Gated on cfg.CMKv2CPEnabled.
package vector

const (
	cmkv2AuditSourceName       = "cmkv2_audit"
	cmkv2OperationalSourceName = "cmkv2_operational"
	cmkv2SummarizeTransform    = "cmkv2_summarize"
	cmkv2AttributeTransform    = "cmkv2_attribute"
	cmkv2TagOperationalName    = "cmkv2_tag_operational"
	cmkv2UIDMapTable           = "uid_map"
	cmkv2CMLSinkName           = "cmkv2_cml"

	// CPServiceClassSelector selects CMKv2 control-plane pods.
	CPServiceClassSelector = "policy.crusoe.ai/public-service-class=cmkv2-control-plane"
)

// cmlEndpoint returns the CML cross-tenant internal-ingest URL for CP logs.
func (c K8sConfig) cmlEndpoint() string {
	base := c.CMLEndpoint
	if base == "" {
		base = c.logsBase()
	}

	return base + "/internal/logs/ingest"
}

// applyCMKv2CP adds the CMKv2 CP log pipeline onto baseCfg. Gated on CMKv2CPEnabled.
func applyCMKv2CP(baseCfg map[string]any, cfg K8sConfig) {
	if !cfg.CMKv2CPEnabled {
		return
	}

	sources := ensureMap(baseCfg, "sources")
	transforms := ensureMap(baseCfg, "transforms")
	sinks := ensureMap(baseCfg, "sinks")
	tables := ensureMap(baseCfg, "enrichment_tables")

	// uid -> tenant enrichment table, written by cwa-manager and reloaded via --watch-config.
	tables[cmkv2UIDMapTable] = map[string]any{
		"type": "file",
		"file": map[string]any{
			"path":     "/etc/vector/uid_map.csv",
			"encoding": map[string]any{"type": "csv"},
		},
		"schema": map[string]any{
			"uid":        "string",
			"cluster_id": "string",
			"project_id": "string",
			"org_id":     "string",
		},
	}

	// Audit files in the CP pod's audit-logs emptyDir: legacy flat + path-encoded layouts.
	sources[cmkv2AuditSourceName] = map[string]any{
		"type": "file",
		"include": []string{
			"/var/lib/kubelet/pods/*/volumes/kubernetes.io~empty-dir/audit-logs/audit*.log",
			"/var/lib/kubelet/pods/*/volumes/kubernetes.io~empty-dir/audit-logs/*/*/audit*.log",
		},
		"read_from": "beginning",
	}

	// Operational: CP component stdout, scoped to tenant namespaces and the 5 CP containers.
	sources[cmkv2OperationalSourceName] = buildLogSource(k8sLogSourceOpts{
		namespaceLabelSelector: "crusoe.ai/cluster-id",
		globs: []string{
			"**/kube-apiserver/*.log",
			"**/etcd/*.log",
			"**/kube-controller-manager/*.log",
			"**/kube-scheduler/*.log",
			"**/konnectivity-server/*.log",
		},
		readFrom: "end",
	})

	// Audit summary: parse the audit JSON and build _msg (incl. objectRef.subresource).
	transforms[cmkv2SummarizeTransform] = remapTransform(
		[]string{cmkv2AuditSourceName}, vrlCMKv2AuditSummarize,
	)

	// Audit attribution (path-encoded + uid_map fallback). Operational is attributed separately.
	transforms[cmkv2AttributeTransform] = remapTransform(
		[]string{cmkv2SummarizeTransform}, vrlCMKv2Attribute,
	)

	// Operational: attribute from namespace labels + build _msg/log_source/level (no uid_map).
	transforms[cmkv2TagOperationalName] = remapTransform(
		[]string{cmkv2OperationalSourceName}, vrlCMKv2OperationalTag,
	)

	sinks[cmkv2CMLSinkName] = buildCMLSink(cfg)

	// Expose internal metrics so drops are visible (component_discarded_events_total per transform).
	sources["internal_metrics"] = internalMetricsSource()
	sinks[internalMetricsExporterSinkName] = internalMetricsExporterSink()
}

// buildCMLSink is the cross-tenant CP log sink to CML internal-ingest (dedicated CP writer token).
func buildCMLSink(cfg K8sConfig) map[string]any {
	opts := httpLogSinkOpts{
		inputs:  []string{cmkv2AttributeTransform, cmkv2TagOperationalName},
		uri:     cfg.cmlEndpoint(),
		token:   "${CMKV2_CP_LOG_TOKEN}",
		headers: map[string]any{"X-Crusoe-CISv2-Cluster": "${CRUSOE_CELL:-unknown}"},
		tls:     tlsConfig(cfg.Proxy.Enabled),
		// Disk buffer so CP logs (incl. audit) survive a CML outage instead of dropping.
		buffer: diskBufferConfig(),
	}
	if cfg.Proxy.Enabled {
		opts.proxy = cfg.Proxy.toMap()
	}

	return buildHTTPLogSink(opts)
}
