package upgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Where cwa-manager reaches cwa-updater: a per-cluster Service on Kubernetes,
// a local peer on VM targets.
const (
	HostEnv     = "CWA_UPDATER_HOST"
	PortEnv     = "CWA_UPDATER_PORT"
	DefaultHost = "localhost"
	DefaultPort = "8786"
)

// Routes cwa-updater serves the handoff API on.
const (
	statusRoute  = "/status"
	upgradeRoute = "/upgrade"
)

// clientTimeout bounds one call; every route answers from state already in memory.
const clientTimeout = 10 * time.Second

// maxResponseBytes caps a reply. Only agent_executions grows with the fleet,
// well under a kilobyte per node.
const maxResponseBytes = 1 << 20

// ErrUpdaterUnavailable marks a call that never reached cwa-updater, or a 5xx.
// Unlike a refusal it may succeed on retry.
var ErrUpdaterUnavailable = errors.New("cwa-updater is unavailable")

// Client is cwa-manager's side of the handoff: hand an upgrade off, read back
// the result, acknowledge it.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient returns a Client addressing cwa-updater at host:port.
func NewClient(host, port string) *Client {
	return &Client{
		baseURL: "http://" + host + ":" + port,
		http:    &http.Client{Timeout: clientTimeout},
	}
}

// Handoff posts this agent's handoff. A nil error means cwa-updater has it
// durably recorded and owns the upgrade from here; it is not an upgrade result.
func (c *Client) Handoff(ctx context.Context, req *Request) (Acceptance, error) {
	if err := req.Validate(); err != nil {
		return Acceptance{}, err
	}

	body, err := json.Marshal(req)
	if err != nil {
		return Acceptance{}, fmt.Errorf("encoding the handoff: %w", err)
	}

	resp, err := c.do(ctx, http.MethodPost, upgradeRoute, body)
	if err != nil {
		return Acceptance{}, err
	}
	defer closeBody(resp)

	if resp.StatusCode != http.StatusOK {
		return Acceptance{}, refusal(resp)
	}

	var acceptance Acceptance
	if err := decode(resp, &acceptance); err != nil {
		return Acceptance{}, err
	}

	return acceptance, nil
}

// Status reads the upgrade cwa-updater holds. ErrNoState means idle, the steady state.
func (c *Client) Status(ctx context.Context) (*StatusView, error) {
	resp, err := c.do(ctx, http.MethodGet, statusRoute, nil)
	if err != nil {
		return nil, err
	}
	defer closeBody(resp)

	switch {
	case resp.StatusCode == http.StatusNoContent:
		return nil, ErrNoState
	case resp.StatusCode != http.StatusOK:
		return nil, refusal(resp)
	}

	view := &StatusView{}
	if err := decode(resp, view); err != nil {
		return nil, err
	}

	return view, nil
}

// Acknowledge clears a reported result, which frees cwa-updater to accept the next round. Idempotent.
func (c *Client) Acknowledge(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodDelete, statusRoute, nil)
	if err != nil {
		return err
	}
	defer closeBody(resp)

	if resp.StatusCode != http.StatusNoContent {
		return refusal(resp)
	}

	return nil
}

// do issues one request. Transport failures and 5xx both come back as
// ErrUpdaterUnavailable, so callers have a single condition to retry on.
func (c *Client) do(ctx context.Context, method, route string, body []byte) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+route, reader)
	if err != nil {
		return nil, fmt.Errorf("building %s %s: %w", method, route, err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s %s: %w", ErrUpdaterUnavailable, method, route, err)
	}

	return resp, nil
}

// closeBody closes a response body once it has been decoded or refused.
func closeBody(resp *http.Response) {
	_ = resp.Body.Close()
}

// decode reads a JSON reply into out.
func decode(resp *http.Response, out any) error {
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out); err != nil {
		return fmt.Errorf("decoding the reply to %s: %w", resp.Request.URL.Path, err)
	}

	return nil
}

// refusal maps a status onto the sentinel a caller branches on, keeping
// cwa-updater's own reason.
func refusal(resp *http.Response) error {
	sentinel := ErrUpdaterUnavailable

	switch resp.StatusCode {
	case http.StatusBadRequest:
		sentinel = ErrInvalidRequest
	case http.StatusConflict:
		sentinel = ErrConflict
	}

	return fmt.Errorf("%w: %s", sentinel, refusalReason(resp, sentinel))
}

// refusalReason reads the error body, falling back to the status line. The
// sentinel's own text is trimmed: cwa-updater wraps these same errors, so it
// would otherwise appear twice.
func refusalReason(resp *http.Response, sentinel error) string {
	var body struct {
		Error string `json:"error"`
	}

	err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&body)
	if err != nil || body.Error == "" {
		return "HTTP " + resp.Status
	}

	return strings.TrimPrefix(body.Error, sentinel.Error()+": ")
}
