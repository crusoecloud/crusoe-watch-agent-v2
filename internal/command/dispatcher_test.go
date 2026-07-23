package command

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "gitlab.com/crusoeenergy/schemas/api/island/v2/observability"
)

// fakeSink captures delivered results.
type fakeSink struct {
	mu      sync.Mutex
	results map[string]*pb.CwaCommandResult
}

func newFakeSink() *fakeSink {
	return &fakeSink{results: make(map[string]*pb.CwaCommandResult)}
}

func (f *fakeSink) DeliverResult(r *pb.CwaCommandResult) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.results[r.GetExecutionId()] = r
}

func (f *fakeSink) get(id string) *pb.CwaCommandResult {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.results[id]
}

// fakeHandler is a controllable Handler.
type fakeHandler struct {
	timeout time.Duration
	err     error
	runs    atomic.Int32

	startOnce sync.Once
	started   chan struct{} // closed when Run begins
	block     chan struct{} // if non-nil, Run blocks until closed or ctx done
}

func (h *fakeHandler) Timeout() time.Duration { return h.timeout }

func (h *fakeHandler) Run(ctx context.Context, _ map[string]string) error {
	h.runs.Add(1)

	if h.started != nil {
		h.startOnce.Do(func() { close(h.started) })
	}

	if h.block != nil {
		select {
		case <-h.block:
		case <-ctx.Done():
			return errors.New("cancelled")
		}
	}

	return h.err
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestDispatcher(t *testing.T, sink ResultSink) *Dispatcher {
	t.Helper()

	return NewDispatcher(sink, nil, discardLogger())
}

func cmd(id, name string) *pb.CwaCommand {
	return &pb.CwaCommand{ExecutionId: id, Command: name}
}

func TestDispatch_RunsHandlerAndDelivers(t *testing.T) {
	sink := newFakeSink()
	d := newTestDispatcher(t, sink)

	h := &fakeHandler{timeout: Instant}
	d.Register("config.apply", h)

	d.Dispatch(context.Background(), cmd("exec-1", "config.apply"))

	require.Eventually(t, func() bool { return sink.get("exec-1") != nil }, time.Second, 5*time.Millisecond)

	r := sink.get("exec-1")
	assert.Equal(t, pb.CwaCommandResultStatus_CWA_COMMAND_RESULT_STATUS_SUCCEEDED, r.GetStatus())
	assert.NotNil(t, r.GetCompletedAt())
	assert.Equal(t, int32(1), h.runs.Load())
}

func TestDispatch_RecordsInProgressThenClears(t *testing.T) {
	sink := newFakeSink()
	store := NewExecStore(t.TempDir())
	d := NewDispatcher(sink, store, discardLogger())

	h := &fakeHandler{timeout: Instant, started: make(chan struct{}), block: make(chan struct{})}
	d.Register("report.bug", h)

	d.Dispatch(context.Background(), cmd("exec-1", "report.bug"))
	<-h.started // handler running: the in-progress record must exist

	records, err := store.List()
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, "exec-1", records[0].ExecutionID)
	assert.Equal(t, ExecStatusInProgress, records[0].Status)

	close(h.block)
	require.Eventually(t, func() bool { return sink.get("exec-1") != nil }, time.Second, 5*time.Millisecond)

	// The result is only delivered after the record is cleared.
	records, err = store.List()
	require.NoError(t, err)
	assert.Empty(t, records, "record must be cleared on completion")
}

func TestDispatch_HandlerErrorIsFailed(t *testing.T) {
	sink := newFakeSink()
	d := newTestDispatcher(t, sink)
	d.Register("config.apply", &fakeHandler{timeout: Instant, err: errors.New("boom")})

	d.Dispatch(context.Background(), cmd("exec-1", "config.apply"))

	require.Eventually(t, func() bool { return sink.get("exec-1") != nil }, time.Second, 5*time.Millisecond)

	r := sink.get("exec-1")
	assert.Equal(t, pb.CwaCommandResultStatus_CWA_COMMAND_RESULT_STATUS_FAILED, r.GetStatus())
	assert.Equal(t, "boom", r.GetReason())
}

func TestDispatch_UnknownCommandStillAcked(t *testing.T) {
	sink := newFakeSink()
	d := newTestDispatcher(t, sink)

	d.Dispatch(context.Background(), cmd("exec-1", "does.not.exist"))

	r := sink.get("exec-1")
	require.NotNil(t, r, "unknown command must still deliver a result so the coordinator stops echoing")
	assert.Equal(t, pb.CwaCommandResultStatus_CWA_COMMAND_RESULT_STATUS_FAILED, r.GetStatus())
}

func TestDispatch_DedupInFlight(t *testing.T) {
	sink := newFakeSink()
	d := newTestDispatcher(t, sink)

	h := &fakeHandler{
		timeout: Instant,
		started: make(chan struct{}),
		block:   make(chan struct{}),
	}
	d.Register("config.apply", h)

	d.Dispatch(context.Background(), cmd("exec-1", "config.apply"))
	<-h.started // handler is running

	// Re-echo while in flight must not start a second execution.
	d.Dispatch(context.Background(), cmd("exec-1", "config.apply"))

	close(h.block)

	require.Eventually(t, func() bool { return sink.get("exec-1") != nil }, time.Second, 5*time.Millisecond)
	assert.Equal(t, int32(1), h.runs.Load(), "handler should run exactly once")
}

func TestDispatch_AsyncNonBlocking(t *testing.T) {
	sink := newFakeSink()
	d := newTestDispatcher(t, sink)

	h := &fakeHandler{
		timeout: Instant,
		started: make(chan struct{}),
		block:   make(chan struct{}),
	}
	d.Register("config.apply", h)

	d.Dispatch(context.Background(), cmd("exec-1", "config.apply")) // returns immediately
	<-h.started

	assert.Nil(t, sink.get("exec-1"), "result must not be delivered while handler is still running")

	close(h.block)
	require.Eventually(t, func() bool { return sink.get("exec-1") != nil }, time.Second, 5*time.Millisecond)
}

func TestDispatch_Timeout(t *testing.T) {
	sink := newFakeSink()
	d := newTestDispatcher(t, sink)

	// Handler blocks forever; only the context deadline unblocks it.
	h := &fakeHandler{timeout: 10 * time.Millisecond, block: make(chan struct{})}
	d.Register("slow", h)

	d.Dispatch(context.Background(), cmd("exec-1", "slow"))

	require.Eventually(t, func() bool { return sink.get("exec-1") != nil }, time.Second, 5*time.Millisecond)

	r := sink.get("exec-1")
	assert.Equal(t, pb.CwaCommandResultStatus_CWA_COMMAND_RESULT_STATUS_FAILED, r.GetStatus())
	assert.Contains(t, r.GetReason(), "timed out")
}

func TestInterrupt_LongRunningIsInterruptedImmediately(t *testing.T) {
	sink := newFakeSink()
	d := NewDispatcher(sink, nil, discardLogger())

	h := &fakeHandler{
		timeout: LongRunning,
		started: make(chan struct{}),
		block:   make(chan struct{}), // never closed; only ctx cancel unblocks
	}
	d.Register("report.bug", h)

	d.Dispatch(context.Background(), cmd("exec-1", "report.bug"))
	<-h.started

	// A generous grace must NOT be spent on a long-running command: it is
	// cancelled immediately, so Interrupt returns far sooner than the grace.
	start := time.Now()
	d.Interrupt(time.Minute)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 5*time.Second,
		"long-running command must be interrupted immediately, not waited on for the grace window")

	r := sink.get("exec-1")
	require.NotNil(t, r)
	assert.Equal(t, pb.CwaCommandResultStatus_CWA_COMMAND_RESULT_STATUS_INTERRUPTED, r.GetStatus())

	// After shutdown, further dispatches are ignored.
	d.Dispatch(context.Background(), cmd("exec-2", "report.bug"))
	assert.Nil(t, sink.get("exec-2"))
}

func TestInterrupt_InstantCommandFinishesWithinGrace(t *testing.T) {
	sink := newFakeSink()
	d := NewDispatcher(sink, nil, discardLogger())

	h := &fakeHandler{
		timeout: Instant,
		started: make(chan struct{}),
		block:   make(chan struct{}),
	}
	d.Register("config.apply", h)

	d.Dispatch(context.Background(), cmd("exec-1", "config.apply"))
	<-h.started

	// Let the handler finish just after Interrupt begins waiting.
	go func() { close(h.block) }()

	d.Interrupt(time.Second)

	r := sink.get("exec-1")
	require.NotNil(t, r)
	assert.Equal(t, pb.CwaCommandResultStatus_CWA_COMMAND_RESULT_STATUS_SUCCEEDED, r.GetStatus(),
		"an instant command that finishes within grace reports its real result")
}

func TestInterrupt_InstantCommandExceedingGraceIsInterrupted(t *testing.T) {
	sink := newFakeSink()
	d := NewDispatcher(sink, nil, discardLogger())

	h := &fakeHandler{
		timeout: Instant,
		started: make(chan struct{}),
		block:   make(chan struct{}), // never closed; only ctx cancel unblocks
	}
	d.Register("config.apply", h)

	d.Dispatch(context.Background(), cmd("exec-1", "config.apply"))
	<-h.started

	// Handler never completes on its own, so Interrupt must wait out the full
	// grace window before giving up and reporting INTERRUPTED.
	const grace = 200 * time.Millisecond

	start := time.Now()
	d.Interrupt(grace)
	elapsed := time.Since(start)

	assert.GreaterOrEqual(t, elapsed, grace,
		"instant command must be given the full grace window to finish before being interrupted")

	r := sink.get("exec-1")
	require.NotNil(t, r)
	assert.Equal(t, pb.CwaCommandResultStatus_CWA_COMMAND_RESULT_STATUS_INTERRUPTED, r.GetStatus())
}
