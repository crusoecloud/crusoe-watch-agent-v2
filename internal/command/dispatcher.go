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

// ExecRecorder persists in-flight command executions for crash recovery.
type ExecRecorder interface {
	Begin(execID, command string) error
	Complete(execID string) error
}

type inflightCmd struct {
	id          string
	command     string
	cancel      context.CancelFunc
	longRunning bool
	done        chan struct{} // done is closed when run() has finished (delivered or skipped its result).
	delivered   bool          // delivered guards against double-delivery between run() and Interrupt().
}

// Dispatcher routes commands to handlers, running each asynchronously with a
// timeout and tracking in-flight executions for dedup and graceful shutdown.
type Dispatcher struct {
	handlers map[string]Handler
	sink     ResultSink
	recorder ExecRecorder
	logger   *slog.Logger

	mu           sync.Mutex
	inflight     map[string]*inflightCmd
	shuttingDown bool
}

// NewDispatcher creates a Dispatcher delivering results to sink and recording
// in-flight executions to recorder for crash recovery.
func NewDispatcher(sink ResultSink, recorder ExecRecorder, logger *slog.Logger) *Dispatcher {
	return &Dispatcher{
		handlers: make(map[string]Handler),
		sink:     sink,
		recorder: recorder,
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
	inf := &inflightCmd{
		id:          execID,
		command:     cmd.GetCommand(),
		cancel:      cancel,
		longRunning: handler.Timeout() > Instant,
		done:        make(chan struct{}),
	}
	d.inflight[execID] = inf
	d.mu.Unlock()

	go d.run(runCtx, inf, cmd, handler)
}

func (d *Dispatcher) run(ctx context.Context, inf *inflightCmd, cmd *pb.CwaCommand, handler Handler) {
	defer inf.cancel()
	defer close(inf.done)

	d.logger.Info("executing command", "command", cmd.GetCommand(), "execution_id", inf.id)

	// Record the execution before it starts (Best effort, store failure must not stop the command from running).
	if d.recorder != nil {
		if err := d.recorder.Begin(inf.id, inf.command); err != nil {
			d.logger.Warn("failed to record command start for crash recovery",
				"execution_id", inf.id, "error", err)
		}
	}

	err := handler.Run(ctx, cmd.GetParameters())

	if errors.Is(ctx.Err(), context.Canceled) {
		return
	}

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("%w after %s", errCommandTimedOut, handler.Timeout())
	}

	status, reason := succeededStatus, ""
	if err != nil {
		status, reason = failedStatus, err.Error()
	}

	d.logger.Info("command completed",
		"command", cmd.GetCommand(), "execution_id", inf.id, "status", status, "reason", reason)

	d.deliverOnce(inf, status, reason)
}

// Interrupt handles in-flight commands on graceful shutdown (SIGTERM).
// Instant commands are given up to grace to finish. Long-running commands are cancelled immediately.
// Interrupt blocks until every in-flight command has a terminal result to flush a final heartbeat.
func (d *Dispatcher) Interrupt(grace time.Duration) {
	d.mu.Lock()
	d.shuttingDown = true

	inflight := make([]*inflightCmd, 0, len(d.inflight))
	for _, inf := range d.inflight {
		inflight = append(inflight, inf)
	}
	d.mu.Unlock()

	// Shared window: all instant commands race the same deadline.
	graceCtx, graceCancel := context.WithTimeout(context.Background(), grace)
	defer graceCancel()

	for _, inf := range inflight {
		if inf.longRunning {
			// Long-running commands cannot finish within the grace window, so interrupt them immediately.
			inf.cancel()
			d.deliverOnce(inf, interruptedStatus, "interrupted by agent shutdown")

			continue
		}

		// Instant commands may finish on their own within the grace window; wait and interrupt.
		select {
		case <-inf.done:
			// run() delivered the command's real result.
		case <-graceCtx.Done():
			inf.cancel()
			d.deliverOnce(inf, interruptedStatus, "interrupted by agent shutdown")
		}
	}
}

// Result status shorthands.
const (
	succeededStatus   = pb.CwaCommandResultStatus_CWA_COMMAND_RESULT_STATUS_SUCCEEDED
	failedStatus      = pb.CwaCommandResultStatus_CWA_COMMAND_RESULT_STATUS_FAILED
	interruptedStatus = pb.CwaCommandResultStatus_CWA_COMMAND_RESULT_STATUS_INTERRUPTED
)

// deliverOnce delivers a terminal result for inf exactly once, coordinating the
// normal-completion path (run) with the shutdown path (Interrupt).
func (d *Dispatcher) deliverOnce(inf *inflightCmd, status pb.CwaCommandResultStatus, reason string) {
	d.mu.Lock()
	if inf.delivered {
		d.mu.Unlock()

		return
	}
	inf.delivered = true
	delete(d.inflight, inf.id)
	d.mu.Unlock()

	// Clear the crash-recovery record before the result reaches the heartbeat.
	if d.recorder != nil {
		if err := d.recorder.Complete(inf.id); err != nil {
			d.logger.Warn("failed to clear command crash-recovery record",
				"execution_id", inf.id, "error", err)
		}
	}

	d.deliver(inf.id, inf.command, status, reason)
}

func (d *Dispatcher) deliver(execID, command string, status pb.CwaCommandResultStatus, reason string) {
	d.sink.DeliverResult(&pb.CwaCommandResult{
		ExecutionId: execID,
		Command:     command,
		Status:      status,
		Reason:      reason,
		CompletedAt: timestamppb.Now(),
	})
}
