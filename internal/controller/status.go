package controller

import (
	"fmt"
	"strings"
)

// Status is the lifecycle state of a run or a step.
//
// The states form a total order by severity. When two states are
// merged, the more severe one wins. That rule is what makes an
// aggregate status well defined: a pipeline with one failed step and
// nine successful steps is failed, and a pipeline with one canceled
// step and one failed step is failed rather than canceled.
type Status string

const (
	// StatusPending means the work has not started.
	StatusPending Status = "PENDING"
	// StatusRunning means the work is executing.
	StatusRunning Status = "RUNNING"
	// StatusSuccess means the work completed successfully.
	StatusSuccess Status = "SUCCESS"
	// StatusFailure means the work failed.
	StatusFailure Status = "FAILURE"
	// StatusSkipped means the work was skipped by a condition.
	StatusSkipped Status = "SKIPPED"
	// StatusCanceled means the work was canceled.
	StatusCanceled Status = "CANCELED"
	// StatusKilled means the work was terminated.
	StatusKilled Status = "KILLED"
	// StatusError means the work could not be run at all.
	StatusError Status = "ERROR"
	// StatusBlocked means the work is waiting for an approval.
	StatusBlocked Status = "BLOCKED"
	// StatusCached means the work was satisfied from the cache.
	StatusCached Status = "CACHED"
	// StatusSpotMigrating means the worker was preempted and the
	// work is being rescheduled.
	StatusSpotMigrating Status = "SPOT_MIGRATING"
)

// statusPriority orders the statuses. The lower the index, the more
// severe the status.
var statusPriority = []Status{
	StatusError,
	StatusBlocked,
	StatusKilled,
	StatusCanceled,
	StatusFailure,
	StatusSpotMigrating,
	StatusRunning,
	StatusPending,
	StatusCached,
	StatusSuccess,
	StatusSkipped,
}

// priorityIndex returns the rank of a status.
func priorityIndex(status Status) int {
	for index, candidate := range statusPriority {
		if candidate == status {
			return index
		}
	}
	// An unknown status is treated as an error, because an unknown
	// state must never be silently reported as a success.
	return 0
}

// MergeStatus combines two statuses and returns the more severe one.
//
// The rule is applied bottom up: step statuses merge into a workflow
// status, and workflow statuses merge into a pipeline status.
func MergeStatus(a, b Status) Status {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	// A canceled status that is merged with another status becomes
	// killed, because the cancellation was not the final outcome of
	// the work. Two cancellations remain a cancellation.
	if a == StatusCanceled && b == StatusCanceled {
		return StatusCanceled
	}
	if a == StatusCanceled {
		a = StatusKilled
	}
	if b == StatusCanceled {
		b = StatusKilled
	}
	if priorityIndex(a) <= priorityIndex(b) {
		return a
	}
	return b
}

// MergeStatuses folds a list of statuses into one.
func MergeStatuses(statuses []Status) Status {
	result := Status("")
	for _, status := range statuses {
		result = MergeStatus(result, status)
	}
	if result == "" {
		return StatusPending
	}
	return result
}

// IsTerminal reports whether a status is final.
func (s Status) IsTerminal() bool {
	switch s {
	case StatusSuccess, StatusFailure, StatusCanceled, StatusKilled, StatusError, StatusSkipped:
		return true
	}
	return false
}

// IsFailure reports whether a status represents a failure.
func (s Status) IsFailure() bool {
	switch s {
	case StatusFailure, StatusError, StatusKilled:
		return true
	}
	return false
}

// String implements fmt.Stringer.
func (s Status) String() string { return string(s) }

// ParseStatus parses a status from its wire representation.
func ParseStatus(value string) (Status, error) {
	status := Status(strings.ToUpper(strings.TrimSpace(value)))
	switch status {
	case StatusPending, StatusRunning, StatusSuccess, StatusFailure, StatusSkipped,
		StatusCanceled, StatusKilled, StatusError, StatusBlocked:
		return status, nil
	}
	return "", fmt.Errorf("unknown status %q", value)
}
