package health

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/version"
	pb "gitlab.com/crusoeenergy/schemas/api/island/v2/observability"
)

func newTestCollector(vectorHealthURL, vectorMetricsURL, updaterURL string, isK8s bool) *Collector {
	return &Collector{
		client:            &http.Client{},
		logger:            slog.Default(),
		vectorHealthURL:   vectorHealthURL,
		vectorMetricsURL:  vectorMetricsURL,
		updaterHealthURL:  updaterURL,
		isK8s:             isK8s,
		updaterPollOffset: 0,
	}
}

func TestNewCollector_EnvOverrides(t *testing.T) {
	t.Setenv("VECTOR_API_PORT", "1111")
	t.Setenv("VECTOR_METRICS_PORT", "2222")
	t.Setenv("CWA_UPDATER_PORT", "3333")

	c := NewCollector(slog.Default(), pb.CwaInstallType_CWA_INSTALL_TYPE_DOCKER)

	assert.Equal(t, "http://localhost:1111/health", c.vectorHealthURL)
	assert.Equal(t, "http://localhost:2222/metrics", c.vectorMetricsURL)
	assert.Equal(t, "http://localhost:3333/health", c.updaterHealthURL)
	assert.False(t, c.isK8s)
}

func TestNewCollector_K8sDetection(t *testing.T) {
	c := NewCollector(slog.Default(), pb.CwaInstallType_CWA_INSTALL_TYPE_KUBERNETES)
	assert.True(t, c.isK8s)
}

func TestNewCollector_Defaults(t *testing.T) {
	t.Setenv("VECTOR_API_PORT", "")
	t.Setenv("VECTOR_METRICS_PORT", "")
	t.Setenv("CWA_UPDATER_PORT", "")

	c := NewCollector(slog.Default(), pb.CwaInstallType_CWA_INSTALL_TYPE_DOCKER)

	assert.Equal(t, "http://localhost:8686/health", c.vectorHealthURL)
	assert.Equal(t, "http://localhost:9598/metrics", c.vectorMetricsURL)
	assert.Equal(t, "http://localhost:8786/health", c.updaterHealthURL)
}

func TestCollectCwaManager(t *testing.T) {
	c := newTestCollector("", "", "", false)
	h := c.collectCwaManager()

	assert.Equal(t, pb.CwaComponentStatus_CWA_COMPONENT_STATUS_HEALTHY, h.GetStatus())
	assert.Equal(t, version.Version, h.GetVersion())
}

func TestCollectVector(t *testing.T) {
	t.Run("healthy with error count and version from metrics", func(t *testing.T) {
		healthSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer healthSrv.Close()

		metricsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintln(w, `# HELP component_errors_total Total errors`)
			fmt.Fprintln(w, `# TYPE component_errors_total counter`)
			fmt.Fprintln(w, `component_errors_total{component_id="source0",component_type="prometheus_scrape"} 3`)
			fmt.Fprintln(w, `component_errors_total{component_id="sink0",component_type="prometheus_remote_write"} 2`)
			fmt.Fprintln(w, `# HELP vector_build_info vector`)
			fmt.Fprintln(w, `# TYPE vector_build_info gauge`)
			fmt.Fprintln(w, `vector_build_info{debug="false",host="vm-host",pid="1",revision="abc",rust_version="1.75.0",version="0.55.0"} 1`)
		}))
		defer metricsSrv.Close()

		c := newTestCollector(healthSrv.URL, metricsSrv.URL, "", false)
		h := c.collectVector(context.Background())

		assert.Equal(t, pb.CwaComponentStatus_CWA_COMPONENT_STATUS_HEALTHY, h.GetStatus())
		assert.Equal(t, "0.55.0", h.GetVersion())
		assert.Equal(t, int64(5), h.GetErrorCount())
		require.NotNil(t, h.GetLastScrapeSuccess())
	})

	t.Run("unhealthy with metrics still returned", func(t *testing.T) {
		healthSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer healthSrv.Close()

		metricsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintln(w, `component_errors_total{component_id="source0"} 7`)
			fmt.Fprintln(w, `vector_build_info{version="0.55.0-debian"} 1`)
		}))
		defer metricsSrv.Close()

		c := newTestCollector(healthSrv.URL, metricsSrv.URL, "", false)
		h := c.collectVector(context.Background())

		assert.Equal(t, pb.CwaComponentStatus_CWA_COMPONENT_STATUS_UNHEALTHY, h.GetStatus())
		assert.Equal(t, int64(7), h.GetErrorCount())
		assert.Equal(t, "0.55.0-debian", h.GetVersion())
	})
}

func TestCollectVector_ConnectionError(t *testing.T) {
	c := newTestCollector("http://localhost:1", "http://localhost:1", "", false)
	h := c.collectVector(context.Background())

	assert.Equal(t, pb.CwaComponentStatus_CWA_COMPONENT_STATUS_UNKNOWN, h.GetStatus())
	assert.Equal(t, int64(-1), h.GetErrorCount())
	assert.Nil(t, h.GetLastScrapeSuccess())
}

func TestCheckVectorHealth(t *testing.T) {
	t.Run("healthy on 200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		c := newTestCollector(srv.URL, "", "", false)
		assert.Equal(t, pb.CwaComponentStatus_CWA_COMPONENT_STATUS_HEALTHY, c.checkVectorHealth(context.Background()))
	})

	t.Run("unhealthy on 503", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()

		c := newTestCollector(srv.URL, "", "", false)
		assert.Equal(t, pb.CwaComponentStatus_CWA_COMPONENT_STATUS_UNHEALTHY, c.checkVectorHealth(context.Background()))
	})

	t.Run("unknown on connection error", func(t *testing.T) {
		c := newTestCollector("http://localhost:1", "", "", false)
		assert.Equal(t, pb.CwaComponentStatus_CWA_COMPONENT_STATUS_UNKNOWN, c.checkVectorHealth(context.Background()))
	})
}

func TestCollectVector_HealthUpMetricsDown(t *testing.T) {
	healthSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer healthSrv.Close()

	c := newTestCollector(healthSrv.URL, "http://localhost:1", "", false)
	h := c.collectVector(context.Background())

	assert.Equal(t, pb.CwaComponentStatus_CWA_COMPONENT_STATUS_HEALTHY, h.GetStatus())
	assert.Equal(t, int64(-1), h.GetErrorCount(), "error count should be -1 when metrics unreachable")
	assert.Nil(t, h.GetLastScrapeSuccess(), "last_scrape_success should be nil when metrics unreachable")
	assert.Empty(t, h.GetVersion(), "version should be empty when metrics scrape fails")
}

func TestScrapeVectorMetrics(t *testing.T) {
	t.Run("sums component_errors_total and extracts version", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintln(w, `# HELP component_errors_total Total errors`)
			fmt.Fprintln(w, `# TYPE component_errors_total counter`)
			fmt.Fprintln(w, `component_errors_total{component_id="src1"} 10`)
			fmt.Fprintln(w, `component_errors_total{component_id="src2"} 32`)
			fmt.Fprintln(w, `component_sent_events_total{component_id="src1"} 9999`)
			fmt.Fprintln(w, `vector_build_info{debug="false",version="0.55.0",rust_version="1.75.0"} 1`)
		}))
		defer srv.Close()

		c := &Collector{
			client:           &http.Client{},
			logger:           slog.Default(),
			vectorMetricsURL: srv.URL,
		}

		count, ver, ok := c.scrapeVectorMetrics(context.Background())
		assert.True(t, ok)
		assert.Equal(t, int64(42), count)
		assert.Equal(t, "0.55.0", ver)
	})

	t.Run("returns false on connection error", func(t *testing.T) {
		c := &Collector{
			client:           &http.Client{},
			logger:           slog.Default(),
			vectorMetricsURL: "http://localhost:1/metrics",
		}

		count, ver, ok := c.scrapeVectorMetrics(context.Background())
		assert.False(t, ok)
		assert.Equal(t, int64(0), count)
		assert.Empty(t, ver)
	})

	t.Run("returns zero with ok when no error metrics present", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintln(w, `# HELP uptime_seconds Vector uptime`)
			fmt.Fprintln(w, `uptime_seconds 3600`)
		}))
		defer srv.Close()

		c := &Collector{
			client:           &http.Client{},
			logger:           slog.Default(),
			vectorMetricsURL: srv.URL,
		}

		count, ver, ok := c.scrapeVectorMetrics(context.Background())
		assert.True(t, ok)
		assert.Equal(t, int64(0), count)
		assert.Empty(t, ver, "version is empty when vector_build_info not present")
	})
}

func TestScrapeVectorMetrics_IgnoresMalformedValues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, `component_errors_total{component_id="src1"} 10`)
		fmt.Fprintln(w, `component_errors_total{component_id="src2"} not_a_number`)
		fmt.Fprintln(w, `component_errors_total{component_id="src3"} 5`)
	}))
	defer srv.Close()

	c := &Collector{
		client:           &http.Client{},
		logger:           slog.Default(),
		vectorMetricsURL: srv.URL,
	}

	count, _, ok := c.scrapeVectorMetrics(context.Background())
	assert.True(t, ok)
	assert.Equal(t, int64(15), count, "should skip malformed values and sum the rest")
}

func TestScrapeVectorMetrics_IgnoresSimilarMetricNames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, `component_errors_total{component_id="src1"} 10`)
		fmt.Fprintln(w, `component_errors_total_bytes{component_id="src1"} 9999`)
	}))
	defer srv.Close()

	c := &Collector{
		client:           &http.Client{},
		logger:           slog.Default(),
		vectorMetricsURL: srv.URL,
	}

	count, _, ok := c.scrapeVectorMetrics(context.Background())
	assert.True(t, ok)
	assert.Equal(t, int64(10), count, "should not match component_errors_total_bytes")
}

func TestScrapeVectorMetrics_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := &Collector{
		client:           &http.Client{},
		logger:           slog.Default(),
		vectorMetricsURL: srv.URL,
	}

	count, ver, ok := c.scrapeVectorMetrics(context.Background())
	assert.False(t, ok, "non-OK HTTP status should fail the scrape")
	assert.Equal(t, int64(0), count)
	assert.Empty(t, ver)
}

func TestExtractPromLabel(t *testing.T) {
	// rust_version listed BEFORE version to catch suffix-collision bugs.
	line := `vector_build_info{debug="false",host="vm-host",rust_version="1.75.0",version="0.55.0-debian"} 1`

	assert.Equal(t, "0.55.0-debian", extractPromLabel(line, "version"))
	assert.Equal(t, "1.75.0", extractPromLabel(line, "rust_version"))
	assert.Equal(t, "false", extractPromLabel(line, "debug"))
	assert.Equal(t, "", extractPromLabel(line, "missing"))
}

func TestCollectCwaUpdater_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	c := newTestCollector("", "", srv.URL, false)
	h := c.collectCwaUpdater(context.Background())

	assert.Equal(t, pb.CwaComponentStatus_CWA_COMPONENT_STATUS_HEALTHY, h.GetStatus())
	assert.Empty(t, h.GetVersion(), "version should be empty when JSON decode fails")
	require.NotNil(t, h.GetLastSeen())
}

func TestCollectCwaUpdater(t *testing.T) {
	tests := []struct {
		name           string
		handler        http.HandlerFunc
		expectedStatus pb.CwaComponentStatus
		checkVersion   string
		checkLastSeen  bool
	}{
		{
			name: "healthy with version",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(updaterHealthResponse{Version: "1.2.3"})
			},
			expectedStatus: pb.CwaComponentStatus_CWA_COMPONENT_STATUS_HEALTHY,
			checkVersion:   "1.2.3",
			checkLastSeen:  true,
		},
		{
			name: "unhealthy status code",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			expectedStatus: pb.CwaComponentStatus_CWA_COMPONENT_STATUS_UNHEALTHY,
			checkLastSeen:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()

			c := newTestCollector("", "", srv.URL, false)
			h := c.collectCwaUpdater(context.Background())

			assert.Equal(t, tt.expectedStatus, h.GetStatus())

			if tt.checkVersion != "" {
				assert.Equal(t, tt.checkVersion, h.GetVersion())
			}

			if tt.checkLastSeen {
				require.NotNil(t, h.GetLastSeen())
			}
		})
	}
}

func TestCollectCwaUpdater_ConnectionError(t *testing.T) {
	c := newTestCollector("", "", "http://localhost:1", false)
	h := c.collectCwaUpdater(context.Background())

	assert.Equal(t, pb.CwaComponentStatus_CWA_COMPONENT_STATUS_UNKNOWN, h.GetStatus())
}

func TestCollectCwaUpdaterThrottled_VMPollsEveryTick(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(updaterHealthResponse{Version: "1.0.0"})
	}))
	defer srv.Close()

	c := newTestCollector("", "", srv.URL, false)

	for i := 0; i < 5; i++ {
		c.tickCount = i + 1
		c.collectCwaUpdaterThrottled(context.Background())
	}

	assert.Equal(t, 5, callCount, "VM mode should poll every tick")
}

func TestCollectCwaUpdaterThrottled_K8sThrottles(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(updaterHealthResponse{Version: "1.0.0"})
	}))
	defer srv.Close()

	c := newTestCollector("", "", srv.URL, true)
	c.updaterPollOffset = 0

	for i := 0; i < 10; i++ {
		c.tickCount = i + 1
		c.collectCwaUpdaterThrottled(context.Background())
	}

	// tick 1: no cache → polls. ticks 5,10: offset match → polls. Others: cached.
	assert.Equal(t, 3, callCount, "K8s mode should throttle to every 5th tick")
}

func TestCollect_ReturnsAllComponents(t *testing.T) {
	healthSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer healthSrv.Close()

	metricsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, `component_errors_total{component_id="src0"} 0`)
	}))
	defer metricsSrv.Close()

	updaterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(updaterHealthResponse{Version: "1.0.0"})
	}))
	defer updaterSrv.Close()

	c := newTestCollector(healthSrv.URL, metricsSrv.URL, updaterSrv.URL, false)
	h := c.Collect(context.Background())

	require.NotNil(t, h.GetCwaManager())
	require.NotNil(t, h.GetVector())
	require.NotNil(t, h.GetCwaUpdater())
}

func TestCollect_IncrementsTickCount(t *testing.T) {
	c := newTestCollector("http://localhost:1", "http://localhost:1", "http://localhost:1", false)

	c.Collect(context.Background())
	c.Collect(context.Background())
	c.Collect(context.Background())

	assert.Equal(t, 3, c.tickCount)
}

func TestCryptoRandIntn(t *testing.T) {
	for i := 0; i < 100; i++ {
		v := cryptoRandIntn(5)
		assert.GreaterOrEqual(t, v, 0)
		assert.Less(t, v, 5)
	}
}
