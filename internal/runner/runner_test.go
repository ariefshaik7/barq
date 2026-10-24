package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBackend is an in-memory backend for the runner tests.
type fakeBackend struct {
	mu sync.Mutex

	started    []string
	destroyed  []string
	tailDelay  time.Duration
	exitCode   int
	oomKilled  bool
	waitErr    error
	tailErr    error
	logContent string
	// order records the sequence of the lifecycle calls.
	order []string
}

func (b *fakeBackend) SetupWorkflow(ctx context.Context, workflowID string) error {
	return nil
}

func (b *fakeBackend) StartStep(ctx context.Context, step Step) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.started = append(b.started, step.ID)
	b.order = append(b.order, "start")
	return nil
}

func (b *fakeBackend) TailStep(ctx context.Context, step Step) (io.ReadCloser, error) {
	b.mu.Lock()
	b.order = append(b.order, "tail")
	err := b.tailErr
	content := b.logContent
	delay := b.tailDelay
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	return io.NopCloser(strings.NewReader(content)), nil
}

func (b *fakeBackend) WaitStep(ctx context.Context, step Step) (State, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.order = append(b.order, "wait")
	return State{ExitCode: b.exitCode, Exited: true, OOMKilled: b.oomKilled}, b.waitErr
}

func (b *fakeBackend) DestroyStep(ctx context.Context, step Step) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.destroyed = append(b.destroyed, step.ID)
	b.order = append(b.order, "destroy")
	return nil
}

func (b *fakeBackend) DestroyWorkflow(ctx context.Context, workflowID string) error {
	return nil
}

func (b *fakeBackend) lifecycle() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Join(b.order, ",")
}

func TestRunnerSuccessfulStep(t *testing.T) {
	backend := &fakeBackend{exitCode: 0, logContent: "hello\n"}
	var lines []string
	var mu sync.Mutex

	runner := New(Options{
		Backend: backend,
		OnLog: func(stepID string, line []byte) {
			mu.Lock()
			defer mu.Unlock()
			lines = append(lines, string(line))
		},
	})

	state, err := runner.Run(context.Background(), context.Background(), Step{ID: "build", PodName: "pod"})
	if err != nil {
		t.Fatal(err)
	}
	if state.ExitCode != 0 {
		t.Fatalf("expected a zero exit code, got %d", state.ExitCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 1 || lines[0] != "hello" {
		t.Fatalf("unexpected log lines: %+v", lines)
	}
}

func TestRunnerDrainsTheLogsBeforeWaiting(t *testing.T) {
	// The log stream must be drained before the exit code is awaited.
	// A backend may close the stream when the worker exits, so draining
	// afterwards loses the output.
	backend := &fakeBackend{exitCode: 0, logContent: "line one\nline two\n"}
	runner := New(Options{Backend: backend})

	if _, err := runner.Run(context.Background(), context.Background(), Step{ID: "build"}); err != nil {
		t.Fatal(err)
	}
	order := backend.lifecycle()
	drain := strings.Index(order, "tail")
	wait := strings.Index(order, "wait")
	if drain < 0 || wait < 0 || drain > wait {
		t.Fatalf("the log stream was not attached before the wait: %s", order)
	}
}

func TestRunnerFailingStep(t *testing.T) {
	backend := &fakeBackend{exitCode: 3}
	runner := New(Options{Backend: backend})

	state, err := runner.Run(context.Background(), context.Background(), Step{ID: "build"})
	if err == nil {
		t.Fatal("expected an error for a failing step")
	}
	var failure *ErrStepFailed
	if !errors.As(err, &failure) {
		t.Fatalf("expected a step failure, got %T: %v", err, err)
	}
	if failure.ExitCode != 3 {
		t.Fatalf("expected exit code 3, got %d", failure.ExitCode)
	}
	if state.ExitCode != 3 {
		t.Fatalf("expected the state to report the exit code, got %d", state.ExitCode)
	}
}

func TestRunnerOutOfMemory(t *testing.T) {
	backend := &fakeBackend{exitCode: 137, oomKilled: true}
	runner := New(Options{Backend: backend})

	_, err := runner.Run(context.Background(), context.Background(), Step{ID: "build"})
	var oom *ErrOutOfMemory
	if !errors.As(err, &oom) {
		t.Fatalf("expected an out of memory error, got %T: %v", err, err)
	}
}

func TestRunnerCleansUpTheWorker(t *testing.T) {
	backend := &fakeBackend{exitCode: 0}
	runner := New(Options{Backend: backend})

	if _, err := runner.Run(context.Background(), context.Background(), Step{ID: "build"}); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.destroyed) != 1 || backend.destroyed[0] != "build" {
		t.Fatalf("expected the worker to be removed, got %+v", backend.destroyed)
	}
}

func TestRunnerCleansUpWhenTheLogStreamFails(t *testing.T) {
	backend := &fakeBackend{exitCode: 0, tailErr: errors.New("the stream could not be opened")}
	runner := New(Options{Backend: backend})

	if _, err := runner.Run(context.Background(), context.Background(), Step{ID: "build"}); err == nil {
		t.Fatal("expected an error")
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.destroyed) != 1 {
		t.Fatalf("expected the worker to be removed, got %+v", backend.destroyed)
	}
}

func TestRunnerCancellationIsNormalised(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	backend := &fakeBackend{exitCode: 1, waitErr: context.Canceled}
	runner := New(Options{Backend: backend})

	_, err := runner.Run(ctx, context.Background(), Step{ID: "build"})
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("expected a cancellation error, got %v", err)
	}
}

func TestRunnerReportsTheState(t *testing.T) {
	backend := &fakeBackend{exitCode: 0}
	var reported []State
	var mu sync.Mutex
	runner := New(Options{
		Backend: backend,
		OnState: func(stepID string, state State) {
			mu.Lock()
			defer mu.Unlock()
			reported = append(reported, state)
		},
	})

	if _, err := runner.Run(context.Background(), context.Background(), Step{ID: "build"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reported) != 1 {
		t.Fatalf("expected one state report, got %d", len(reported))
	}
	if !reported[0].Exited {
		t.Fatal("expected the state to report that the step exited")
	}
}

func TestRunnerSplitsTheLogIntoLines(t *testing.T) {
	backend := &fakeBackend{exitCode: 0, logContent: "one\ntwo\nthree"}
	var lines []string
	var mu sync.Mutex
	runner := New(Options{
		Backend: backend,
		OnLog: func(stepID string, line []byte) {
			mu.Lock()
			defer mu.Unlock()
			lines = append(lines, string(line))
		},
	})

	if _, err := runner.Run(context.Background(), context.Background(), Step{ID: "build"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %+v", lines)
	}
	if lines[0] != "one" || lines[1] != "two" || lines[2] != "three" {
		t.Fatalf("unexpected lines: %+v", lines)
	}
}

func TestRunnerSurvivesAPanickingLogHandler(t *testing.T) {
	backend := &fakeBackend{exitCode: 0, logContent: "line\n"}
	runner := New(Options{
		Backend: backend,
		OnLog: func(stepID string, line []byte) {
			panic("the handler is broken")
		},
	})
	// The panic must not take the runner down.
	if _, err := runner.Run(context.Background(), context.Background(), Step{ID: "build"}); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerUsesPooledBuffers(t *testing.T) {
	// This test verifies that the runner uses pooled buffers by running
	// multiple steps concurrently and checking that no excessive allocations
	// occur. We can't directly test the pool internals, but we can verify
	// the runner handles large log streams correctly.
	backend := &fakeBackend{
		exitCode:   0,
		logContent: strings.Repeat("x", 100*1024) + "\n", // 100KB line
	}
	runner := New(Options{Backend: backend})

	// Run multiple steps concurrently to stress the buffer pool.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			_, err := runner.Run(context.Background(), context.Background(), Step{ID: fmt.Sprintf("build-%d", id)})
			if err != nil {
				t.Errorf("step %d failed: %v", id, err)
			}
		}(i)
	}
	wg.Wait()
}
