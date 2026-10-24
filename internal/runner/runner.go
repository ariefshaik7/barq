// Package runner executes a single step of a Barq pipeline.
//
// The runner owns the lifecycle of one step: it starts the worker,
// attaches to the log stream, waits for the exit code and cleans the
// worker up. The ordering is deliberate and matters:
//
//  1. The log stream is attached and drained *before* the exit code is
//     awaited. Some backends close the log stream when the process
//     exits, so draining afterwards loses output.
//  2. The cleanup context is separate from the work context. A
//     canceled workflow must still be able to reach the backend to
//     stop and remove the worker.
//  3. A step that fails is a successfully executed step from the
//     runner's point of view. The failure is reported through the
//     step state, not as a runtime error.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

// logBufferPool provides reusable buffers for log streaming to reduce
// heap allocations. The buffer size (64KB) is chosen to balance
// syscall overhead against memory pressure for concurrent steps.
var logBufferPool = sync.Pool{
	New: func() any {
		return make([]byte, 64*1024)
	},
}

// pendingLinePool provides reusable byte slices for accumulating
// partial lines across Read calls.
var pendingLinePool = sync.Pool{
	New: func() any {
		return make([]byte, 0, 4*1024)
	},
}

// ErrCanceled is returned when a step was canceled.
var ErrCanceled = errors.New("the step was canceled")

// ErrStepFailed is returned when a step exited with a non-zero code.
//
// It carries the exit code so that the caller can report it without
// parsing an error string.
type ErrStepFailed struct {
	StepID   string
	ExitCode int
}

// Error implements error.
func (e *ErrStepFailed) Error() string {
	return fmt.Sprintf("step %q failed with exit code %d", e.StepID, e.ExitCode)
}

// ErrOutOfMemory is returned when the worker was killed for using too
// much memory.
type ErrOutOfMemory struct {
	StepID   string
	ExitCode int
}

// Error implements error.
func (e *ErrOutOfMemory) Error() string {
	return fmt.Sprintf("step %q was killed for exceeding its memory limit", e.StepID)
}

// State is the outcome of a step.
type State struct {
	// ExitCode is the exit code of the worker.
	ExitCode int
	// Exited reports whether the worker finished.
	Exited bool
	// OOMKilled reports whether the worker was killed for memory.
	OOMKilled bool
	// Started is when the worker started.
	Started time.Time
	// Finished is when the worker finished.
	Finished time.Time
	// Err is the error that ended the step, if any.
	Err error
}

// Backend is the execution backend a runner drives.
//
// The interface is deliberately small and mirrors the lifecycle of a
// worker: set up, start, tail, wait, destroy.
type Backend interface {
	// SetupWorkflow prepares the resources a workflow needs.
	SetupWorkflow(ctx context.Context, workflowID string) error
	// StartStep starts a worker for the step.
	StartStep(ctx context.Context, step Step) error
	// TailStep opens the log stream of a running worker.
	TailStep(ctx context.Context, step Step) (io.ReadCloser, error)
	// WaitStep waits for the worker to finish and reports its state.
	WaitStep(ctx context.Context, step Step) (State, error)
	// DestroyStep stops and removes the worker.
	DestroyStep(ctx context.Context, step Step) error
	// DestroyWorkflow removes the resources of a workflow.
	DestroyWorkflow(ctx context.Context, workflowID string) error
}

// Step is the description of a step a runner executes.
type Step struct {
	// ID is the compiled step identifier.
	ID string
	// Name is the human readable label.
	Name string
	// Image is the container image.
	Image string
	// Commands are the shell commands to execute.
	Commands []string
	// Env is the environment of the worker.
	Env map[string]string
	// WorkingDir is the working directory.
	WorkingDir string
	// TimeoutSeconds bounds the step.
	TimeoutSeconds int
	// PodName is the deterministic name of the worker.
	PodName string
	// RunID identifies the run.
	RunID string
	// Attempt is the zero based attempt number.
	Attempt int
	// Services are the sidecar containers of the step.
	Services []Sidecar
}

// Sidecar is a container that runs alongside the worker.
type Sidecar struct {
	// ID identifies the sidecar.
	ID string
	// Image is the container image.
	Image string
	// Commands are the shell commands to execute.
	Commands []string
	// Env is the environment of the sidecar.
	Env map[string]string
	// Ports are the ports the sidecar exposes.
	Ports []string
}

// FailureMode is the behaviour when a step fails.
type FailureMode string

const (
	// FailureFail stops the pipeline. It is the default.
	FailureFail FailureMode = "fail"
	// FailureIgnore lets the pipeline continue.
	FailureIgnore FailureMode = "ignore"
	// FailureCancel cancels the whole pipeline.
	FailureCancel FailureMode = "cancel"
)

// Options configures a runner.
type Options struct {
	// Backend executes the step.
	Backend Backend
	// Logger receives the runner's diagnostics.
	Logger *slog.Logger
	// OnLog receives every log line the worker produced. It is called
	// from the log streaming goroutine, so it must be safe for
	// concurrent use and must not block.
	OnLog func(stepID string, line []byte)
	// OnState is called when a step changes state. It is the tracer
	// hook: the controller uses it to persist the outcome.
	OnState func(stepID string, state State)
	// WorkflowID identifies the workflow the step belongs to.
	WorkflowID string
	// Failure is the behaviour when the step fails.
	Failure FailureMode
}

// Runner executes steps.
type Runner struct {
	options Options
	logger  *slog.Logger
}

// New creates a runner.
func New(options Options) *Runner {
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if options.Failure == "" {
		options.Failure = FailureFail
	}
	return &Runner{options: options, logger: logger}
}

// Run executes a step to completion.
//
// The work context controls the execution. The cleanup context is used
// for the destruction of the worker, so that a canceled workflow can
// still remove it.
func (r *Runner) Run(ctx context.Context, cleanupCtx context.Context, step Step) (State, error) {
	if r.options.Backend == nil {
		return State{}, fmt.Errorf("the runner requires a backend")
	}

	if err := r.options.Backend.StartStep(ctx, step); err != nil {
		return State{Err: err}, err
	}

	// The log stream is attached immediately. It must be drained before
	// the exit code is awaited, because a backend may close the stream
	// when the worker exits.
	stream, err := r.options.Backend.TailStep(ctx, step)
	if err != nil {
		// The worker started but the log stream could not be opened.
		// The worker is still cleaned up, and the failure is reported.
		if destroyErr := r.options.Backend.DestroyStep(cleanupCtx, step); destroyErr != nil {
			r.logger.Warn("the worker could not be removed", "step", step.ID, "error", destroyErr)
		}
		return State{Err: err}, err
	}

	var drained sync.WaitGroup
	drained.Add(1)
	go func() {
		defer drained.Done()
		r.streamLogs(step, stream)
	}()

	state, waitErr := r.options.Backend.WaitStep(ctx, step)

	// The log stream is drained first, so that no output is lost.
	drained.Wait()

	// The cleanup context is used deliberately. The work context may
	// already be canceled, but the worker still has to be removed.
	if err := r.options.Backend.DestroyStep(cleanupCtx, step); err != nil {
		r.logger.Warn("the worker could not be removed", "step", step.ID, "error", err)
	}

	state.Exited = true
	if state.Started.IsZero() {
		state.Started = time.Now()
	}
	state.Finished = time.Now()
	if waitErr != nil {
		if r.canceled(ctx) || errors.Is(waitErr, context.Canceled) {
			state.Err = ErrCanceled
			waitErr = ErrCanceled
		} else {
			state.Err = waitErr
		}
	}
	if state.OOMKilled {
		state.Err = &ErrOutOfMemory{StepID: step.ID, ExitCode: state.ExitCode}
		waitErr = state.Err
	} else if state.ExitCode != 0 && waitErr == nil {
		waitErr = &ErrStepFailed{StepID: step.ID, ExitCode: state.ExitCode}
		state.Err = waitErr
	}

	if r.options.OnState != nil {
		r.options.OnState(step.ID, state)
	}
	return state, waitErr
}

// streamLogs copies the log stream into the log handler.
//
// A panic in the handler must not take the worker down, so it is
// recovered here.
//
// Uses pooled buffers to minimize heap allocations under high
// concurrency. The pending line buffer is also pooled to avoid
// allocations when accumulating partial lines across Read calls.
func (r *Runner) streamLogs(step Step, stream io.ReadCloser) {
	defer stream.Close()

	buffer := logBufferPool.Get().([]byte)
	defer logBufferPool.Put(buffer)

	pending := pendingLinePool.Get().([]byte)
	defer pendingLinePool.Put(pending[:0])

	for {
		count, err := stream.Read(buffer)
		if count > 0 {
			pending = append(pending, buffer[:count]...)
			for {
				index := indexByte(pending, '\n')
				if index < 0 {
					break
				}
				line := pending[:index]
				pending = pending[index+1:]
				r.emit(step, line)
			}
		}
		if err != nil {
			break
		}
	}
	if len(pending) > 0 {
		r.emit(step, pending)
	}
}

// emit delivers a log line.
func (r *Runner) emit(step Step, line []byte) {
	defer func() {
		if recovered := recover(); recovered != nil {
			r.logger.Error("the log handler panicked", "step", step.ID, "panic", recovered)
		}
	}()
	if r.options.OnLog != nil {
		r.options.OnLog(step.ID, line)
	}
}

// canceled reports whether the context was canceled.
func (r *Runner) canceled(ctx context.Context) bool {
	return ctx.Err() != nil
}

// indexByte finds a byte in a slice. It exists so that the runner does
// not need the bytes package for a single search.
func indexByte(data []byte, target byte) int {
	for index := 0; index < len(data); index++ {
		if data[index] == target {
			return index
		}
	}
	return -1
}
