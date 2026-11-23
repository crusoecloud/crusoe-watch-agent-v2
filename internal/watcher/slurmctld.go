package watcher

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"
)

const (
	slurmctldReadyzPath    = "/readyz"
	slurmctldReadyzTimeout = 3 * time.Second

	// slurmctldRecheckInterval re-runs the readiness check without a pod event. A failover moves
	// Slinky's active label and so fires a pod event, but the label can lag behind slurmctld.
	slurmctldRecheckInterval = 60 * time.Second
)

// errUnexpectedReadyzStatus means /readyz answered with a code that is neither active nor standby.
var errUnexpectedReadyzStatus = errors.New("unexpected slurmctld /readyz status")

// slurmctldCheck reports whether the slurmctld at ip:port is the active controller.
type slurmctldCheck func(ctx context.Context, ip string, port int) (bool, error)

// newSlurmctldCheck returns a check that asks slurmctld's /readyz.
//
// A Slurm cluster with controller HA runs a backup slurmctld in standby. A standby has no job
// list, and Slurm 25.11.2 to 25.11.6 crash with SIGSEGV when it serves /metrics/jobs, so only the
// active controller is scraped. The active controller answers 2xx on /readyz. A standby answers
// 500, or 303 on Slurm 25.11.7. A 404 means this Slurm has no /readyz, so the pod is scraped as
// before. Any other code is an error, so the caller logs it instead of treating it as a standby.
//
// The pod object cannot answer this. With HA, Slinky 1.3 probes readiness on /livez, so the
// standby is Ready too. Slinky 1.1.3 never sets the controller.slinky.slurm.net/active label, so
// its only controller looks like a standby.
//
// The client ignores HTTP_PROXY, because a pod IP must not go through the egress proxy. It does
// not follow redirects. It does not keep connections open, because the check runs once a minute
// and slurmctld pod IPs change on restart and failover.
func newSlurmctldCheck() slurmctldCheck {
	client := &http.Client{
		Timeout:   slurmctldReadyzTimeout,
		Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return func(ctx context.Context, ip string, port int) (bool, error) {
		url := "http://" + net.JoinHostPort(ip, strconv.Itoa(port)) + slurmctldReadyzPath

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false, fmt.Errorf("building slurmctld readiness request: %w", err)
		}

		resp, err := client.Do(req)
		if err != nil {
			return false, fmt.Errorf("slurmctld readiness request: %w", err)
		}
		defer resp.Body.Close()

		switch code := resp.StatusCode; {
		case code >= http.StatusOK && code < http.StatusMultipleChoices, code == http.StatusNotFound:
			return true, nil
		case code == http.StatusInternalServerError, code == http.StatusSeeOther:
			return false, nil
		default:
			return false, fmt.Errorf("%w: %d", errUnexpectedReadyzStatus, code)
		}
	}
}
