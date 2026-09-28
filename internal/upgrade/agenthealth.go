package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// healthyStatus is cwa-manager's /health status once it has heartbeated at least once.
const healthyStatus = "healthy"

const (
	defaultVerifyInterval = 10 * time.Second
	verifyHTTPTimeout     = 5 * time.Second
)

// errAgentUnhealthy marks an agent that answered but is not ready to report.
var errAgentUnhealthy = errors.New("agent is not healthy")

// agentHealth is the subset of cwa-manager's /health body this reads.
type agentHealth struct {
	Status string `json:"status"`
}

// healthPoller polls a cwa-manager /health endpoint.
type healthPoller struct {
	http *http.Client
}

// newHealthPoller returns a poller bounded by the per-request health timeout.
func newHealthPoller() healthPoller {
	return healthPoller{http: &http.Client{Timeout: verifyHTTPTimeout}}
}

// healthAddress is one agent's /health URL.
func healthAddress(host string, port int) string {
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/health"
}

// check polls one agent's /health. A 200 alone is not enough: cwa-manager
// answers 200 as soon as it is listening, and an agent that never reaches the
// control plane cannot report the upgrade result the round is waiting for.
func (p healthPoller) check(ctx context.Context, address string, port int) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, healthAddress(address, port), nil)
	if err != nil {
		return fmt.Errorf("building health request for %s: %w", address, err)
	}

	resp, err := p.http.Do(request)
	if err != nil {
		return fmt.Errorf("polling %s: %w", address, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s answered %d", errAgentUnhealthy, address, resp.StatusCode)
	}

	var body agentHealth
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("decoding health from %s: %w", address, err)
	}

	if body.Status != healthyStatus {
		return fmt.Errorf("%w: %s reports %q and has not reached the control plane",
			errAgentUnhealthy, address, body.Status)
	}

	return nil
}

// pollUntilHealthy runs pass until it succeeds or ctx expires with the rollback
// window, naming the agents being waited on as what. The last failure is wrapped
// into the returned error, so the upgrade result reports what held the round back
// rather than the deadline that ended it.
//
// Both verifiers below share this.
func pollUntilHealthy(
	ctx context.Context, logger *slog.Logger, interval time.Duration,
	what string, pass func(context.Context) error,
) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastErr error

	for {
		err := pass(ctx)
		if err == nil {
			return nil
		}

		// A cut-off round must not displace the reason an agent was unhealthy.
		if !isContextErr(err) || lastErr == nil {
			lastErr = err
		}

		logger.Info("not healthy yet", "waiting_on", what, "error", err)

		select {
		case <-ctx.Done():
			return fmt.Errorf("%s did not become healthy: %w", what, lastErr)
		case <-ticker.C:
		}
	}
}

// isContextErr reports whether err is the round being cut off rather than an answer from an agent.
func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// ---------------------------------------------------------------------------
// VM: one agent at a known address
// ---------------------------------------------------------------------------

// HostVerifier polls the one cwa-manager on this host, which needs no discovery:
// a VM runs exactly one, on the host network.
type HostVerifier struct {
	poller healthPoller
	logger *slog.Logger

	host     string
	port     int
	interval time.Duration
}

// NewHostVerifier returns a Verifier polling cwa-manager's /health at host:port.
func NewHostVerifier(logger *slog.Logger, host string, port int) *HostVerifier {
	return &HostVerifier{
		poller:   newHealthPoller(),
		logger:   logger,
		host:     host,
		port:     port,
		interval: defaultVerifyInterval,
	}
}

// Verify blocks until the agent reports healthy, or until the window is spent.
func (v *HostVerifier) Verify(ctx context.Context) error {
	return pollUntilHealthy(ctx, v.logger, v.interval, v.host,
		func(ctx context.Context) error { return v.poller.check(ctx, v.host, v.port) })
}

// ---------------------------------------------------------------------------
// Kubernetes: every pod behind the agent chart's headless Service
// ---------------------------------------------------------------------------

// errNoAgents means the headless Service published no addresses to poll.
var errNoAgents = errors.New("no agent addresses published")

// EndpointVerifier polls every agent pod's /health through the addresses the
// agent chart's headless Service publishes.
type EndpointVerifier struct {
	client kubernetes.Interface
	poller healthPoller
	logger *slog.Logger

	namespace string
	service   string
	port      int
	interval  time.Duration
}

// NewEndpointVerifier returns a Verifier polling the named headless Service.
func NewEndpointVerifier(
	client kubernetes.Interface, logger *slog.Logger, namespace, service string, port int,
) *EndpointVerifier {
	return &EndpointVerifier{
		client:    client,
		poller:    newHealthPoller(),
		logger:    logger,
		namespace: namespace,
		service:   service,
		port:      port,
		interval:  defaultVerifyInterval,
	}
}

// Verify blocks until every published agent address reports healthy, or until
// the window is spent.
func (v *EndpointVerifier) Verify(ctx context.Context) error {
	return pollUntilHealthy(ctx, v.logger, v.interval, v.service, v.pass)
}

// pass checks every published address once, stopping at the first that is not ready: the round needs all of them.
func (v *EndpointVerifier) pass(ctx context.Context) error {
	addresses, err := v.addresses(ctx)
	if err != nil {
		return err
	}

	for _, address := range addresses {
		if err := v.poller.check(ctx, address, v.port); err != nil {
			return err
		}
	}

	v.logger.Info("all agents healthy after upgrade", "agents", len(addresses))

	return nil
}

// addresses lists the agent pod addresses behind the headless Service. The
// Service sets publishNotReadyAddresses, so a pod that is rolling or stuck is
// listed and polled rather than dropped from the round for being NotReady.
func (v *EndpointVerifier) addresses(ctx context.Context) ([]string, error) {
	endpointSlices, err := v.client.DiscoveryV1().EndpointSlices(v.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: discoveryv1.LabelServiceName + "=" + v.service,
	})
	if err != nil {
		return nil, fmt.Errorf("listing endpointslices for %s/%s: %w", v.namespace, v.service, err)
	}

	addresses := endpointAddresses(endpointSlices.Items)
	if len(addresses) == 0 {
		return nil, fmt.Errorf("%w by %s/%s", errNoAgents, v.namespace, v.service)
	}

	return addresses, nil
}

// endpointAddresses flattens the slices, dropping repeats: a dual-stack Service
// gets one slice per address family, and the agents are hostNetwork.
func endpointAddresses(endpointSlices []discoveryv1.EndpointSlice) []string {
	var addresses []string

	seen := make(map[string]struct{})

	for i := range endpointSlices {
		for _, endpoint := range endpointSlices[i].Endpoints {
			for _, address := range endpoint.Addresses {
				if _, ok := seen[address]; ok {
					continue
				}

				seen[address] = struct{}{}
				addresses = append(addresses, address)
			}
		}
	}

	return addresses
}
