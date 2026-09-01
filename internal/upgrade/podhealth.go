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

// healthyStatus is what cwa-manager reports once it has reached the control plane at least once.
const healthyStatus = "healthy"

const (
	defaultVerifyInterval = 10 * time.Second
	verifyHTTPTimeout     = 5 * time.Second
)

// errNoAgents means the headless Service published no addresses to poll.
var errNoAgents = errors.New("no agent addresses published")

// errAgentUnhealthy marks an agent that answered but is not ready to report.
var errAgentUnhealthy = errors.New("agent is not healthy")

// agentHealth is the subset of cwa-manager's /health body this reads.
type agentHealth struct {
	Status string `json:"status"`
}

// EndpointVerifier polls every agent pod's /health through the addresses the
// agent chart's headless Service publishes.
type EndpointVerifier struct {
	client kubernetes.Interface
	http   *http.Client
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
		http:      &http.Client{Timeout: verifyHTTPTimeout},
		logger:    logger,
		namespace: namespace,
		service:   service,
		port:      port,
		interval:  defaultVerifyInterval,
	}
}

// Verify blocks until every published agent address reports healthy, or until
// ctx expires with the rollback window. The last failure is wrapped into the
// returned error, so the upgrade result names what held the round back.
func (v *EndpointVerifier) Verify(ctx context.Context) error {
	ticker := time.NewTicker(v.interval)
	defer ticker.Stop()

	var lastErr error

	for {
		err := v.pass(ctx)
		if err == nil {
			return nil
		}

		if !isContextErr(err) || lastErr == nil {
			lastErr = err
		}

		v.logger.Info("agents not healthy yet", "error", err)

		select {
		case <-ctx.Done():
			return fmt.Errorf("agents did not become healthy: %w", lastErr)
		case <-ticker.C:
		}
	}
}

// isContextErr reports whether err is the round being cut off rather than an answer from an agent.
func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// pass checks every published address once, stopping at the first that is not ready: the round needs all of them.
func (v *EndpointVerifier) pass(ctx context.Context) error {
	addresses, err := v.addresses(ctx)
	if err != nil {
		return err
	}

	for _, address := range addresses {
		if err := v.check(ctx, address); err != nil {
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

// check polls one agent's /health. A 200 alone is not enough: cwa-manager
// answers 200 as soon as it is listening, and an agent that never reaches the
// control plane cannot report the upgrade result the round is waiting for.
func (v *EndpointVerifier) check(ctx context.Context, address string) error {
	url := "http://" + net.JoinHostPort(address, strconv.Itoa(v.port)) + "/health"

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("building health request for %s: %w", address, err)
	}

	resp, err := v.http.Do(request)
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
