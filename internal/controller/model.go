package controller

import (
	"time"

	"barq/internal/dag"
	"barq/internal/yaml"
)

// RunStatus is the lifecycle state of a run.
//
// It is an alias of Status so that the same severity ordering applies
// to runs and to steps.
type RunStatus = Status

// StepStatus is the lifecycle state of a single step.
//
// It is an alias of Status so that the same severity ordering applies
// to runs and to steps.
type StepStatus = Status

// The step states, expressed in terms of the shared status type.
const (
	// StepQueued means the step is waiting for its dependencies.
	StepQueued = StatusPending
	// StepRunning means the step has a live worker.
	StepRunning = StatusRunning
	// StepCached means the step was satisfied from the Merkle cache.
	StepCached = StatusCached
	// StepSpotMigrating means the worker was preempted and is
	// rescheduled.
	StepSpotMigrating = StatusSpotMigrating
	// StepSuccess means the step completed successfully.
	StepSuccess = StatusSuccess
	// StepFailed means the step failed.
	StepFailed = StatusFailure
	// StepCanceled means the step was canceled.
	StepCanceled = StatusCanceled
	// StepSkipped means the step was skipped by a condition.
	StepSkipped = StatusSkipped
)

// The run states, expressed in terms of the shared status type.
const (
	// RunQueued means the run has been accepted but not started.
	RunQueued = StatusPending
	// RunRunning means at least one step is executing.
	RunRunning = StatusRunning
	// RunSuccess means every step completed successfully.
	RunSuccess = StatusSuccess
	// RunFailed means at least one step failed.
	RunFailed = StatusFailure
	// RunCanceled means the run was stopped by an operator.
	RunCanceled = StatusCanceled
)

// StepState is the persisted state of one compiled step.
type StepState struct {
	ID            string     `json:"id"`
	Wave          int        `json:"wave"`
	Status        StepStatus `json:"status"`
	PodName       string     `json:"pod_name,omitempty"`
	Attempt       int        `json:"attempt"`
	ExitCode      *int       `json:"exit_code,omitempty"`
	Message       string     `json:"message,omitempty"`
	Merkle        string     `json:"merkle,omitempty"`
	CacheHit      bool       `json:"cache_hit"`
	WorkspaceKey  string     `json:"workspace_key,omitempty"`
	CheckpointKey string     `json:"checkpoint_key,omitempty"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	DurationMS    int64      `json:"duration_ms"`
	StorageTier   string     `json:"storage_tier,omitempty"`
	// BypassCache forces the step to execute even when a cache entry
	// exists. A rebuild sets it so that a rebuild actually rebuilds.
	BypassCache bool `json:"bypass_cache,omitempty"`
}

// Clone returns a copy of the step state.
func (s *StepState) Clone() *StepState {
	if s == nil {
		return nil
	}
	copied := *s
	return &copied
}

// RunState is the persisted state of one pipeline run.
type RunState struct {
	ID          string                `json:"id"`
	Name        string                `json:"name"`
	Status      RunStatus             `json:"status"`
	Revision    uint64                `json:"revision"`
	TriggerID   string                `json:"trigger_id,omitempty"`
	Source      string                `json:"source,omitempty"`
	Pipeline    yaml.PipelineSpec     `json:"pipeline"`
	Compiled    dag.Compiled          `json:"compiled"`
	Steps       map[string]*StepState `json:"steps"`
	CreatedAt   time.Time             `json:"created_at"`
	StartedAt   *time.Time            `json:"started_at,omitempty"`
	FinishedAt  *time.Time            `json:"finished_at,omitempty"`
	Archived    bool                  `json:"archived"`
	StorageTier string                `json:"storage_tier,omitempty"`
	LastLogAt   *time.Time            `json:"last_log_at,omitempty"`
	Message     string                `json:"message,omitempty"`
}

// NewRunState builds the initial state of a run.
func NewRunState(id string, spec yaml.PipelineSpec, compiled dag.Compiled) *RunState {
	state := &RunState{
		ID:        id,
		Name:      spec.Name,
		Status:    RunQueued,
		Pipeline:  spec,
		Compiled:  compiled,
		Steps:     make(map[string]*StepState, len(compiled.Steps)),
		CreatedAt: time.Now().UTC(),
	}
	for _, step := range compiled.Steps {
		state.Steps[step.ID] = &StepState{
			ID:      step.ID,
			Wave:    step.Wave,
			Status:  StepQueued,
			Message: "waiting for dependencies",
		}
	}
	return state
}

// Clone returns a deep copy of the run state.
func (r *RunState) Clone() *RunState {
	clone := *r
	clone.Steps = make(map[string]*StepState, len(r.Steps))
	for id, step := range r.Steps {
		copied := *step
		clone.Steps[id] = &copied
	}
	return &clone
}

// AllSteps returns every step ordered by wave and identifier.
func (r *RunState) AllSteps() []*StepState {
	result := make([]*StepState, 0, len(r.Steps))
	for _, step := range r.Steps {
		result = append(result, step)
	}
	sortSteps(result)
	return result
}

func sortSteps(steps []*StepState) {
	for i := 1; i < len(steps); i++ {
		for j := i; j > 0; j-- {
			left, right := steps[j-1], steps[j]
			if left.Wave < right.Wave || (left.Wave == right.Wave && left.ID <= right.ID) {
				break
			}
			steps[j-1], steps[j] = steps[j], steps[j-1]
		}
	}
}

// IsTerminal reports whether the run has reached a final state.
func (r *RunState) IsTerminal() bool {
	switch r.Status {
	case RunSuccess, RunFailed, RunCanceled:
		return true
	}
	return false
}
