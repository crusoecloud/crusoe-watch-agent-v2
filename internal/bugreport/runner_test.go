package bugreport

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/vector"
)

// fakeGen is a reportGenerator whose behavior the test controls.
type fakeGen struct {
	path   string
	err    error
	gotGPU vector.GPUType
	gotEvt string
	called bool
}

func (f *fakeGen) Generate(_ context.Context, gpu vector.GPUType, eventID string) (string, error) {
	f.called = true
	f.gotGPU = gpu
	f.gotEvt = eventID

	return f.path, f.err
}

// serveRunner serves gen on a unix socket in a temp dir and returns the socket path.
// The server stops when the test ends.
func serveRunner(t *testing.T, gen reportGenerator) string {
	t.Helper()

	// Unix socket paths are capped (~104 bytes on macOS). t.TempDir() embeds the long
	// test name under $TMPDIR and can exceed that, so use a short temp dir instead.
	dir, err := os.MkdirTemp("", "rr")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	socket := filepath.Join(dir, "r.sock")
	srv := NewRunnerServer(gen, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() { _ = srv.Serve(ctx, socket) }()

	// Serve creates the socket asynchronously; wait for it before returning.
	require.Eventually(t, func() bool {
		_, err := os.Stat(socket)

		return err == nil
	}, 2*time.Second, 10*time.Millisecond, "runner socket never appeared")

	return socket
}

// startRunner serves gen and returns a client generator wired to its socket.
func startRunner(t *testing.T, gen reportGenerator) *RunnerClient {
	t.Helper()

	return NewRunnerClient(serveRunner(t, gen))
}

// socketClient builds a raw HTTP client dialing socket, for tests that send
// requests the RunnerClient would never produce (wrong method, malformed body).
func socketClient(socket string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer

				return dialer.DialContext(ctx, "unix", socket)
			},
		},
	}
}

func TestRunnerClient_RoundTrip(t *testing.T) {
	gen := &fakeGen{path: "/etc/crusoe/bug-reports/report.log.gz"}
	client := startRunner(t, gen)

	path, err := client.Generate(context.Background(), vector.GPUNvidia, "evt-42")
	require.NoError(t, err)

	// The archive path round-trips, and the request reached the generator intact.
	assert.Equal(t, "/etc/crusoe/bug-reports/report.log.gz", path)
	assert.True(t, gen.called)
	assert.Equal(t, vector.GPUNvidia, gen.gotGPU)
	assert.Equal(t, "evt-42", gen.gotEvt)
}

func TestRunnerServer_RejectsNonPOST(t *testing.T) {
	gen := &fakeGen{}
	socket := serveRunner(t, gen)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://report-runner"+collectPath, nil)
	require.NoError(t, err)

	resp, err := socketClient(socket).Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	// Only POST triggers collection; anything else is rejected without touching the generator.
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	assert.False(t, gen.called)
}

func TestRunnerServer_RejectsMalformedJSON(t *testing.T) {
	gen := &fakeGen{}
	socket := serveRunner(t, gen)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://report-runner"+collectPath, strings.NewReader("{not json"))
	require.NoError(t, err)

	resp, err := socketClient(socket).Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	// An undecodable body is a client error, and no collection is attempted.
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.False(t, gen.called)

	var out CollectResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.Contains(t, out.Error, "invalid request")
}

func TestRunnerClient_PropagatesFailureVerbatim(t *testing.T) {
	// The runner-side error already carries a CWA-BR code; it must survive the
	// round trip as the leading token so the coordinator can parse it.
	gen := &fakeGen{err: CodeScriptUnavailable.Errorf("nvidia-bug-report.sh missing")}
	client := startRunner(t, gen)

	_, err := client.Generate(context.Background(), vector.GPUNvidia, "evt-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), string(CodeScriptUnavailable))
}
