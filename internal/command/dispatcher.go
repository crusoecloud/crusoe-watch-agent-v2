// Package command implements the control-plane command dispatch framework for
// cwa-manager: an async, timeout-bounded dispatcher. Commands arrive on the
// heartbeat response and their results are delivered back into the heartbeat's
// pending-results ACK loop.
package command

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "gitlab.com/crusoeenergy/schemas/api/island/v2/observability"
)

// Command timeout classes.
const (
	Instant     = 30 * time.Second
	LongRunning = 10 * time.Minute
)

var errCommandTimedOut = errors.New("command timed out")

// Handler executes a single command. A nil error is reported as SUCCEEDED; a
// non-nil error as FAILED with the error message as the reason.
type Handler interface {
	Timeout() time.Duration
	Run(ctx context.Context, params map[string]string) error
}

// ResultSink receives finished command results. heartbeat.Loop implements it by
// placing the result in its pending-results map for the next heartbeat.
type ResultSink interface {
	DeliverResult(*pb.CwaCommandResult)
}

type inflightCmd struct {
	command string
	cancel  context.CancelFunc
}

// Dispatcher routes commands to handlers, running each asynchronously with a
// timeout and tracking in-flight executions for dedup and graceful shutdown.
type Dispatcher struct {
	handlers map[string]Handler
	sink     ResultSink
	logger   *slog.Logger

	mu           sync.Mutex
	inflight     map[string]*inflightCmd
	shuttingDown bool
}

// NewDispatcher creates a Dispatcher delivering results to sink.
func NewDispatcher(sink ResultSink, logger *slog.Logger) *Dispatcher {
	return &Dispatcher{
		handlers: make(map[string]Handler),
		sink:     sink,
		logger:   logger,
		inflight: make(map[string]*inflightCmd),
	}
}

// Register associates a handler with a command name.
func (d *Dispatcher) Register(name string, h Handler) {
	d.handlers[name] = h
}

// Dispatch runs a command asynchronously. It never blocks the caller (the
// heartbeat loop). Commands already in flight are ignored (the coordinator
// re-echoes a command until it sees the result). Unknown commands are still
// acknowledged with a FAILED result so the coordinator stops echoing them.
func (d *Dispatcher) Dispatch(ctx context.Context, cmd *pb.CwaCommand) {
	execID := cmd.GetExecutionId()

	d.mu.Lock()

	if d.shuttingDown {
		d.mu.Unlock()

		return
	}

	if _, running := d.inflight[execID]; running {
		d.mu.Unlock()

		return
	}

	handler, ok := d.handlers[cmd.GetCommand()]
	if !ok {
		d.mu.Unlock()
		d.logger.Warn("unknown command", "command", cmd.GetCommand(), "execution_id", execID)
		d.deliver(execID, cmd.GetCommand(), failedStatus, "unknown command: "+cmd.GetCommand())

		return
	}

	// Detach from the caller's cancellation: a command outlives the heartbeat
	// stream that delivered it and is bounded by its own timeout (or Interrupt).
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), handler.Timeout())
	d.inflight[execID] = &inflightCmd{command: cmd.GetCommand(), cancel: cancel}
	d.mu.Unlock()

	go d.run(runCtx, cancel, cmd, handler)
}

func (d *Dispatcher) run(ctx context.Context, cancel context.CancelFunc, cmd *pb.CwaCommand, handler Handler) {
	defer cancel()

	execID := cmd.GetExecutionId()

	d.logger.Info("executing command", "command", cmd.GetCommand(), "execution_id", execID)

	err := handler.Run(ctx, cmd.GetParameters())
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("%w after %s", errCommandTimedOut, handler.Timeout())
	}

	d.mu.Lock()
	shuttingDown := d.shuttingDown
	d.mu.Unlock()

	// During shutdown, Interrupt() owns the (INTERRUPTED) result for in-flight commands.
	if shuttingDown {
		return
	}

	status, reason := succeededStatus, ""
	if err != nil {
		status, reason = failedStatus, err.Error()
	}

	// Deliver before clearing in-flight.
	d.deliver(execID, cmd.GetCommand(), status, reason)

	d.mu.Lock()
	delete(d.inflight, execID)
	d.mu.Unlock()
}

// Interrupt cancels all in-flight commands and reports them as INTERRUPTED. It
// is called on graceful shutdown (SIGTERM); the caller then flushes a final
// heartbeat carrying these results so the control plane applies its retry policy.
func (d *Dispatcher) Interrupt() {
	d.mu.Lock()
	d.shuttingDown = true

	type interruptedCmd struct{ id, command string }

	interrupted := make([]interruptedCmd, 0, len(d.inflight))

	for id, inf := range d.inflight {
		inf.cancel()
		interrupted = append(interrupted, interruptedCmd{id: id, command: inf.command})
	}

	d.inflight = make(map[string]*inflightCmd)
	d.mu.Unlock()

	for _, c := range interrupted {
		d.deliver(c.id, c.command, interruptedStatus, "interrupted by agent shutdown")
	}
}

// Result status shorthands.
const (
	succeededStatus   = pb.CwaCommandResultStatus_CWA_COMMAND_RESULT_STATUS_SUCCEEDED
	failedStatus      = pb.CwaCommandResultStatus_CWA_COMMAND_RESULT_STATUS_FAILED
	interruptedStatus = pb.CwaCommandResultStatus_CWA_COMMAND_RESULT_STATUS_INTERRUPTED
)

func (d *Dispatcher) deliver(execID, command string, status pb.CwaCommandResultStatus, reason string) {
	d.sink.DeliverResult(&pb.CwaCommandResult{
		ExecutionId: execID,
		Command:     command,
		Status:      status,
		Reason:      reason,
		CompletedAt: timestamppb.Now(),
	})
}
