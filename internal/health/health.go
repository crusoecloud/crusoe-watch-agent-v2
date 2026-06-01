// Package health collects component health status for heartbeat payloads.
package health

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/version"
	pb "gitlab.com/crusoeenergy/schemas/api/island/v2/observability"
)

const (
	defaultVectorAPIPort     = "8686"
	defaultVectorMetricsPort = "9598"
	defaultCwaUpdaterPort    = "8786"
	httpTimeout              = 3 * time.Second
	k8sUpdaterPollInterval   = 5 // poll cwa-updater every 5th heartbeat on K8s
)

// updaterHealthResponse is the expected JSON from cwa-updater's /health endpoint.
type updaterHealthResponse struct {
	Version string `json:"version"`
}

// Collector gathers health from all components.
type Collector struct {
	client            *http.Client
	logger            *slog.Logger
	vectorHealthURL   string
	vectorMetricsURL  string
	updaterHealthURL  string
	isK8s             bool
	tickCount         int
	updaterPollOffset int // random offset so DaemonSet pods don't all poll on the same tick
	cachedUpdater     *pb.CwaUpdaterHealth
}

func cryptoRandIntn(n int) int {
	val, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}

	return int(val.Int64())
}

func getEnvOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return def
}

// NewCollector creates a health Collector.
func NewCollector(logger *slog.Logger, installType pb.CwaInstallType) *Collector {
	vectorPort := getEnvOrDefault("VECTOR_API_PORT", defaultVectorAPIPort)
	vectorMetricsPort := getEnvOrDefault("VECTOR_METRICS_PORT", defaultVectorMetricsPort)
	updaterPort := getEnvOrDefault("CWA_UPDATER_PORT", defaultCwaUpdaterPort)

	return &Collector{
		client:            &http.Client{Timeout: httpTimeout},
		logger:            logger,
		vectorHealthURL:   "http://localhost:" + vectorPort + "/health",
		vectorMetricsURL:  "http://localhost:" + vectorMetricsPort + "/metrics",
		updaterHealthURL:  "http://localhost:" + updaterPort + "/health",
		isK8s:             installType == pb.CwaInstallType_CWA_INSTALL_TYPE_KUBERNETES,
		updaterPollOffset: cryptoRandIntn(k8sUpdaterPollInterval),
	}
}

// Collect returns the current health of all components.
func (c *Collector) Collect(ctx context.Context) *pb.CwaComponentsHealth {
	c.tickCount++

	return &pb.CwaComponentsHealth{
		CwaManager: c.collectCwaManager(),
		Vector:     c.collectVector(ctx),
		CwaUpdater: c.collectCwaUpdaterThrottled(ctx),
	}
}

func (c *Collector) collectCwaManager() *pb.CwaManagerHealth {
	return &pb.CwaManagerHealth{
		Status:  pb.CwaComponentStatus_CWA_COMPONENT_STATUS_HEALTHY, // self reported
		Version: version.Version,
	}
}

func (c *Collector) collectCwaUpdaterThrottled(ctx context.Context) *pb.CwaUpdaterHealth {
	// On K8s, poll every 5th heartbeat to avoid fan-in from all DaemonSet pods.
	// Each pod picks a random offset at startup so polls are staggered across the cluster.
	// On VM/Docker, poll every tick.
	if c.isK8s && c.tickCount%k8sUpdaterPollInterval != c.updaterPollOffset && c.cachedUpdater != nil {
		return c.cachedUpdater
	}

	result := c.collectCwaUpdater(ctx)
	c.cachedUpdater = result

	return result
}

func (c *Collector) collectVector(ctx context.Context) *pb.CwaVectorHealth {
	health := &pb.CwaVectorHealth{
		Status:  pb.CwaComponentStatus_CWA_COMPONENT_STATUS_UNKNOWN,
		Version: version.Version, // TODO: Source from cwa-updater once it can upgrade components independently.
	}

	health.Status = c.checkVectorHealth(ctx)
	errorCount, ok := c.queryVectorErrorCount(ctx)
	if ok {
		health.ErrorCount = errorCount
		health.LastScrapeSuccess = timestamppb.Now()
	} else {
		health.ErrorCount = -1
	}

	return health
}

// httpGet performs a GET request and returns the response.
func (c *Collector) httpGet(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}

	return resp, nil
}

// checkVectorHealth calls Vector's /health endpoint to determine status.
func (c *Collector) checkVectorHealth(ctx context.Context) pb.CwaComponentStatus {
	resp, err := c.httpGet(ctx, c.vectorHealthURL)
	if err != nil {
		c.logger.Debug("vector health check failed", "error", err)

		return pb.CwaComponentStatus_CWA_COMPONENT_STATUS_UNKNOWN
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.logger.Debug("vector unhealthy", "status_code", resp.StatusCode)

		return pb.CwaComponentStatus_CWA_COMPONENT_STATUS_UNHEALTHY
	}

	return pb.CwaComponentStatus_CWA_COMPONENT_STATUS_HEALTHY
}

// queryVectorErrorCount scrapes Vector's prometheus_exporter sink for the
// component_errors_total metric and returns the sum across all components.
// The bool return indicates whether the scrape succeeded.
func (c *Collector) queryVectorErrorCount(ctx context.Context) (int64, bool) {
	resp, err := c.httpGet(ctx, c.vectorMetricsURL)
	if err != nil {
		c.logger.Debug("vector metrics request failed", "error", err)

		return 0, false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.logger.Debug("vector metrics endpoint returned non-OK status", "status_code", resp.StatusCode)

		return 0, false
	}

	var total float64

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "component_errors_total{") {
			continue
		}

		// Prometheus exposition format: metric_name{labels} value [timestamp]
		parts := strings.Fields(line)
		if len(parts) >= 2 { //nolint:mnd // minimum fields: metric name + value
			val, parseErr := strconv.ParseFloat(parts[1], 64)
			if parseErr == nil {
				total += val
			}
		}
	}

	if err := scanner.Err(); err != nil {
		c.logger.Debug("error reading vector metrics response", "error", err)

		return 0, false
	}

	return int64(total), true
}

func (c *Collector) collectCwaUpdater(ctx context.Context) *pb.CwaUpdaterHealth {
	resp, err := c.httpGet(ctx, c.updaterHealthURL)
	if err != nil {
		c.logger.Debug("cwa-updater health check failed", "error", err)

		return &pb.CwaUpdaterHealth{
			Status: pb.CwaComponentStatus_CWA_COMPONENT_STATUS_UNKNOWN,
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.logger.Debug("cwa-updater unhealthy", "status_code", resp.StatusCode)

		return &pb.CwaUpdaterHealth{
			Status:   pb.CwaComponentStatus_CWA_COMPONENT_STATUS_UNHEALTHY,
			LastSeen: timestamppb.Now(),
		}
	}

	health := &pb.CwaUpdaterHealth{
		Status:   pb.CwaComponentStatus_CWA_COMPONENT_STATUS_HEALTHY,
		LastSeen: timestamppb.Now(),
	}

	var body updaterHealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err == nil {
		health.Version = body.Version
	} else {
		c.logger.Debug("failed to decode cwa-updater health response", "error", err)
	}

	return health
}
