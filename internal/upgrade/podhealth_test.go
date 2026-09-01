package upgrade

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

const testHealthService = "crusoe-watch-agent"

// agentEndpoints is the headless Service's EndpointSlice over the given addresses.
func agentEndpoints(ready, notReady []string) *discoveryv1.EndpointSlice {
	endpoints := func(ips []string, isReady bool) []discoveryv1.Endpoint {
		out := make([]discoveryv1.Endpoint, 0, len(ips))
		for _, ip := range ips {
			out = append(out, discoveryv1.Endpoint{
				Addresses:  []string{ip},
				Conditions: discoveryv1.EndpointConditions{Ready: &isReady},
			})
		}

		return out
	}

	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testHealthService + "-abcde",
			Namespace: testNamespace,
			Labels:    map[string]string{discoveryv1.LabelServiceName: testHealthService},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   append(endpoints(ready, true), endpoints(notReady, false)...),
	}
}

// agentServer serves one agent's /health with the given body.
func agentServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	return srv
}

// hostPort splits a test server's address so it can be published as an endpoint.
func hostPort(t *testing.T, srv *httptest.Server) (string, int) {
	t.Helper()

	parsed, err := url.Parse(srv.URL)
	require.NoError(t, err)

	port, err := strconv.Atoi(parsed.Port())
	require.NoError(t, err)

	return parsed.Hostname(), port
}

func newTestVerifier(port int, objects ...runtime.Object) *EndpointVerifier {
	verifier := NewEndpointVerifier(
		fake.NewSimpleClientset(objects...),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		testNamespace, testHealthService, port,
	)
	// Keep the retry loop from dominating test runtime.
	verifier.interval = time.Millisecond

	return verifier
}

// The happy path: every published agent has heartbeated, so the upgrade holds.
func TestVerifyPassesWhenAgentsAreHealthy(t *testing.T) {
	srv := agentServer(t, http.StatusOK,
		`{"status":"healthy","version":"v2.1.0","last_heartbeat":"2026-08-31T10:00:00Z"}`)
	host, port := hostPort(t, srv)

	verifier := newTestVerifier(port, agentEndpoints([]string{host}, nil))

	require.NoError(t, verifier.Verify(context.Background()))
}

// cwa-manager answers 200 as soon as it is listening. An agent still "starting"
// has not reached the control plane and cannot report the upgrade result, so the
// round must not be called complete on its account.
func TestVerifyFailsWhileAgentIsStarting(t *testing.T) {
	srv := agentServer(t, http.StatusOK, `{"status":"starting","version":"v2.1.0"}`)
	host, port := hostPort(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := newTestVerifier(port, agentEndpoints([]string{host}, nil)).Verify(ctx)
	require.ErrorIs(t, err, errAgentUnhealthy)
	// The reason names the state the agent was stuck in, not just "timed out".
	assert.Contains(t, err.Error(), "starting")
}

// A pod that is rolling or wedged is published as NotReady by the Service, and
// dropping it from the round would call a partial rollout a success.
func TestVerifyPollsNotReadyAddresses(t *testing.T) {
	healthy := agentServer(t, http.StatusOK, `{"status":"healthy"}`)
	host, port := hostPort(t, healthy)

	unreachable := agentEndpoints([]string{host}, []string{"10.0.0.99"})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	require.Error(t, newTestVerifier(port, unreachable).Verify(ctx))
}

// An empty slice means the rollout has not published anything yet; it is a
// reason to keep waiting, and a reason to fail if it never changes.
func TestVerifyFailsWhenNoAgentsPublished(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := newTestVerifier(8787, agentEndpoints(nil, nil)).Verify(ctx)
	require.ErrorIs(t, err, errNoAgents)
}

func TestVerifyFailsOnNon200(t *testing.T) {
	srv := agentServer(t, http.StatusServiceUnavailable, `{}`)
	host, port := hostPort(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := newTestVerifier(port, agentEndpoints([]string{host}, nil)).Verify(ctx)
	require.ErrorIs(t, err, errAgentUnhealthy)
	assert.Contains(t, err.Error(), "503")
}

// A missing Service is a configuration error, and it must not read as "healthy".
func TestVerifyFailsWhenServiceMissing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := newTestVerifier(8787).Verify(ctx)
	require.ErrorIs(t, err, errNoAgents)
}
