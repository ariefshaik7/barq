package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"barq/internal/runner"
)

// Step describes a step for the backend.
type Step = runner.Step

// StepState is the outcome of a Kubernetes worker.
type StepState = runner.State

// Sidecar is a container that runs alongside the worker.
type Sidecar = runner.Sidecar

// SetupWorkflow prepares the resources a workflow needs.
//
// The Kubernetes backend has no per-workflow resources: every worker
// gets its own Pod and its own ephemeral workspace volume. The method
// exists so that the lifecycle is uniform across backends and so that
// a future shared-workspace implementation has a place to hook in.
func (c *Client) SetupWorkflow(ctx context.Context, workflowID string) error {
	return nil
}

// StartStep creates the worker Pod for a step.
func (c *Client) StartStep(ctx context.Context, step Step) error {
	worker := Worker{
		Name:            step.PodName,
		RunID:           step.RunID,
		StepID:          step.ID,
		Attempt:         step.Attempt,
		Image:           step.Image,
		Commands:        step.Commands,
		Env:             step.Env,
		WorkingDir:      step.WorkingDir,
		TimeoutSeconds:  step.TimeoutSeconds,
		WorkspaceUpload: step.Env["BARQ_WORKSPACE_UPLOAD"],
		Services:        step.Services,
	}
	if downloads := step.Env["BARQ_WORKSPACE_DOWNLOADS"]; downloads != "" {
		worker.WorkspaceDownloads = strings.Split(downloads, "\n")
	}
	if step.Env["BARQ_NO_WORKSPACE"] == "1" {
		worker.NoWorkspace = true
	}
	return c.Spawn(ctx, worker)
}

// TailStep opens the log stream of a worker.
//
// The stream is retried, because the API server rejects a log request
// for a Pod whose container has not started yet.
func (c *Client) TailStep(ctx context.Context, step Step) (io.ReadCloser, error) {
	var lastErr error
	for attempt := 0; attempt < 30; attempt++ {
		stream, err := c.PodLogs(ctx, step.PodName, time.Time{})
		if err == nil {
			return stream, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("the log stream for %q could not be opened: %w", step.PodName, lastErr)
}

// WaitStep waits for a worker to finish.
//
// The API server can report a phase before the container's terminated
// state is populated, so the read is retried until the exit code is
// available.
func (c *Client) WaitStep(ctx context.Context, step Step) (StepState, error) {
	deadline := time.Now().Add(30 * time.Minute)
	var lastErr error
	for {
		pod, err := c.GetPod(ctx, step.PodName)
		if err != nil {
			if IsNotFound(err) {
				// The worker is gone. It was either removed by an
				// operator or the node was lost. The exit code is
				// unknown, so the step is reported as failed.
				return StepState{ExitCode: 137, Exited: true}, nil
			}
			lastErr = err
		} else {
			for _, status := range pod.Status.ContainerStatuses {
				if status.State.Terminated == nil {
					break
				}
				terminated := status.State.Terminated
				state := StepState{
					ExitCode:  terminated.ExitCode,
					Exited:    true,
					OOMKilled: terminated.Reason == "OOMKilled",
				}
				if terminated.StartedAt != "" {
					if parsed, err := time.Parse(time.RFC3339, terminated.StartedAt); err == nil {
						state.Started = parsed
					}
				}
				if terminated.FinishedAt != "" {
					if parsed, err := time.Parse(time.RFC3339, terminated.FinishedAt); err == nil {
						state.Finished = parsed
					}
				}
				return state, nil
			}
			if pod.Status.Phase == "Failed" {
				// The Pod failed without a terminated container state.
				// The most common cause is an image that could not be
				// pulled, which the kubelet reports as exit code 1.
				return StepState{ExitCode: 1, Exited: true}, nil
			}
			lastErr = fmt.Errorf("the worker %q has not reported an exit code yet", step.PodName)
		}

		if time.Now().After(deadline) {
			if lastErr == nil {
				lastErr = fmt.Errorf("the worker %q did not finish in time", step.PodName)
			}
			return StepState{}, lastErr
		}
		select {
		case <-ctx.Done():
			return StepState{}, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// DestroyStep stops and removes a worker.
//
// A worker that is already gone is not an error, because the step may
// have been cancelled before it started.
func (c *Client) DestroyStep(ctx context.Context, step Step) error {
	err := c.DeletePod(ctx, step.PodName)
	if err != nil && IsNotFound(err) {
		return nil
	}
	return err
}

// DestroyWorkflow removes the resources of a workflow.
//
// The Kubernetes backend has no per-workflow resources, so the method
// only removes the workers that still exist.
func (c *Client) DestroyWorkflow(ctx context.Context, workflowID string) error {
	pods, err := c.ListRunPods(ctx, workflowID)
	if err != nil {
		return err
	}
	for _, pod := range pods {
		if err := c.DeletePod(ctx, pod.Metadata.Name); err != nil && !IsNotFound(err) {
			return err
		}
	}
	return nil
}

// ErrNotRunning is returned when a worker is not running.
var ErrNotRunning = errors.New("the worker is not running")

// WorkerIDs returns the identifiers of the workers of a run, sorted.
func (c *Client) WorkerIDs(ctx context.Context, runID string) ([]string, error) {
	pods, err := c.ListRunPods(ctx, runID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(pods))
	for _, pod := range pods {
		ids = append(ids, pod.Metadata.Name)
	}
	sort.Strings(ids)
	return ids, nil
}

// parseAttempt reads the attempt label of a worker.
func parseAttempt(labels map[string]string) int {
	value, err := strconv.Atoi(labels["barq.io/attempt"])
	if err != nil {
		return 0
	}
	return value
}
