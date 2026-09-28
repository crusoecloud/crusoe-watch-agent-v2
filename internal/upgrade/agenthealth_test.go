package upgrade

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

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

// hostPort splits a test server's address so it can be polled, or published as
// a Kubernetes endpoint.
func hostPort(t *testing.T, srv *httptest.Server) (string, int) {
	t.Helper()

	parsed, err := url.Parse(srv.URL)
	require.NoError(t, err)

	port, err := strconv.Atoi(parsed.Port())
	require.NoError(t, err)

	return parsed.Hostname(), port
}

// hostVerifier polls host:port, checking often enough that the tests do not wait
// out the production interval.
func hostVerifier(host string, port int) *HostVerifier {
	verifier := NewHostVerifier(discardLogger(), host, port)
	verifier.interval = time.Millisecond

	return verifier
}

// closedPort is a port nothing is listening on, for the connection-refused case.
func closedPort(t *testing.T) (string, int) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	host, rawPort, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	require.NoError(t, listener.Close())

	port, err := strconv.Atoi(rawPort)
	require.NoError(t, err)

	return host, port
}

// ---------------------------------------------------------------------------
// VM
// ---------------------------------------------------------------------------

func TestHostVerifierPassesOnAHealthyAgent(t *testing.T) {
	host, port := hostPort(t, agentServer(t, http.StatusOK, `{"status":"healthy"}`))

	require.NoError(t, hostVerifier(host, port).Verify(context.Background()))
}

// A 200 alone is not enough: cwa-manager answers as soon as it is listening, and
// an agent that has not reached the control plane cannot report the result.
func TestHostVerifierWaitsOutAnUnhealthyAgent(t *testing.T) {
	host, port := hostPort(t, agentServer(t, http.StatusOK, `{"status":"degraded"}`))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := hostVerifier(host, port).Verify(ctx)

	require.ErrorIs(t, err, errAgentUnhealthy)
	assert.Contains(t, err.Error(), "degraded", "the reason the agent was unhealthy was lost")
}

// The reported reason must name what kept the agent from coming back, not the
// deadline that ended the wait.
func TestHostVerifierReportsTheAgentFailureNotTheDeadline(t *testing.T) {
	host, port := closedPort(t)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := hostVerifier(host, port).Verify(ctx)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "connect", "the connection failure was replaced by the deadline")
}

// An agent that comes back part way through the window still passes.
func TestHostVerifierPassesOnceTheAgentRecovers(t *testing.T) {
	var polls atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if polls.Add(1) < 3 {
			_, _ = w.Write([]byte(`{"status":"starting"}`))

			return
		}

		_, _ = w.Write([]byte(`{"status":"healthy"}`))
	}))
	t.Cleanup(server.Close)

	host, port := hostPort(t, server)

	require.NoError(t, hostVerifier(host, port).Verify(context.Background()))
	assert.GreaterOrEqual(t, polls.Load(), int64(3))
}

// ---------------------------------------------------------------------------
// Kubernetes
// ---------------------------------------------------------------------------

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

func newTestVerifier(port int, objects ...runtime.Object) *EndpointVerifier {
	verifier := NewEndpointVerifier(
		fake.NewSimpleClientset(objects...),
		discardLogger(),
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
