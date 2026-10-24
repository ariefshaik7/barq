package k8s

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Simulator is an in-process execution backend.
//
// It exists so the API, UI, lease election and reconciliation logic can
// be exercised without a Kubernetes cluster. It is EXPLICITLY synthetic:
// it never proves that a build succeeded. It must only be enabled by
// setting BARQ_MODE=simulator.
type Simulator struct {
	mu       sync.Mutex
	pods     map[string]*simPod
	leases   map[string]Lease
	revision int
	now      func() time.Time
	workRoot string
	spotRuns map[string]bool
}

type simPod struct {
	metadata ObjectMeta
	status   PodStatus
	log      *simLog
}

// NewSimulator creates a simulator backend rooted at a temporary
// directory.
func NewSimulator(workRoot string) (*Simulator, error) {
	if workRoot == "" {
		return nil, fmt.Errorf("simulator requires a work root")
	}
	if err := os.MkdirAll(workRoot, 0o700); err != nil {
		return nil, err
	}
	return &Simulator{
		pods:     make(map[string]*simPod),
		leases:   make(map[string]Lease),
		now:      time.Now,
		workRoot: workRoot,
		spotRuns: make(map[string]bool),
	}, nil
}

// Name implements Backend.
func (s *Simulator) Name() string { return "simulator" }

// SetupWorkflow implements the runner lifecycle. The simulator has no
// per-workflow resources.
func (s *Simulator) SetupWorkflow(ctx context.Context, workflowID string) error {
	return nil
}

// StartStep creates a simulated worker.
func (s *Simulator) StartStep(ctx context.Context, step Step) error {
	return s.Spawn(ctx, Worker{
		Name:       step.PodName,
		RunID:      step.RunID,
		StepID:     step.ID,
		Attempt:    step.Attempt,
		Image:      step.Image,
		Commands:   step.Commands,
		Env:        step.Env,
		WorkingDir: step.WorkingDir,
	})
}

// TailStep opens the log stream of a simulated worker.
func (s *Simulator) TailStep(ctx context.Context, step Step) (io.ReadCloser, error) {
	return s.PodLogs(ctx, step.PodName, time.Time{})
}

// WaitStep waits for a simulated worker to finish.
func (s *Simulator) WaitStep(ctx context.Context, step Step) (StepState, error) {
	deadline := time.Now().Add(30 * time.Minute)
	for {
		pod, err := s.GetPod(ctx, step.PodName)
		if err == nil {
			for _, status := range pod.Status.ContainerStatuses {
				if status.State.Terminated != nil {
					return StepState{
						ExitCode: status.State.Terminated.ExitCode,
						Exited:   true,
					}, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return StepState{}, fmt.Errorf("the simulated worker %q did not finish", step.PodName)
		}
		select {
		case <-ctx.Done():
			return StepState{}, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// DestroyStep removes a simulated worker.
func (s *Simulator) DestroyStep(ctx context.Context, step Step) error {
	err := s.DeletePod(ctx, step.PodName)
	if err != nil && IsNotFound(err) {
		return nil
	}
	return err
}

// DestroyWorkflow removes the workers of a simulated workflow.
func (s *Simulator) DestroyWorkflow(ctx context.Context, workflowID string) error {
	pods, err := s.ListRunPods(ctx, workflowID)
	if err != nil {
		return err
	}
	for _, pod := range pods {
		if err := s.DeletePod(ctx, pod.Metadata.Name); err != nil && !IsNotFound(err) {
			return err
		}
	}
	return nil
}

// Spawn implements Backend.
func (s *Simulator) Spawn(ctx context.Context, worker Worker) error {
	s.mu.Lock()
	if _, exists := s.pods[worker.Name]; exists {
		s.mu.Unlock()
		return &APIError{StatusCode: 409, Method: "POST", Path: "/pods", Body: "alreadyexists"}
	}
	pod := &simPod{
		metadata: ObjectMeta{
			Name:      worker.Name,
			Namespace: "simulator",
			Labels: map[string]string{
				"barq.io/run-id":  sanitizeLabel(worker.RunID),
				"barq.io/step-id": sanitizeLabel(worker.StepID),
				"barq.io/wave":    strconv.Itoa(worker.Wave),
				"barq.io/attempt": strconv.Itoa(worker.Attempt),
			},
		},
		status: PodStatus{Phase: "Running"},
		log:    newSimLog(),
	}
	s.pods[worker.Name] = pod
	s.mu.Unlock()

	go s.execute(worker, pod)
	return nil
}

// execute runs the worker commands in a local process.
func (s *Simulator) execute(worker Worker, pod *simPod) {
	workspace := filepath.Join(s.workRoot, "runs", worker.RunID, worker.StepID)
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		pod.log.Write([]byte("failed to create workspace: " + err.Error() + "\n"))
		pod.finish(s, 1)
		return
	}

	environment := os.Environ()
	for key, value := range worker.Env {
		environment = append(environment, key+"="+value)
	}

	exitCode := 0
	for index, command := range worker.Commands {
		// The simulator honours the same preemption contract as the
		// real worker. A command that asks to be preempted exits with
		// code 75 exactly once, on the first attempt, so that the
		// controller exercises the reschedule path and then succeeds.
		// The marker is detected before execution because the marker
		// itself is not a runnable program.
		if strings.Contains(command, spotPreemptMarker) {
			if !s.spotSimulated(worker.RunID) {
				s.markSpotSimulated(worker.RunID)
				pod.log.Write([]byte("[barq] simulated spot preemption\n"))
				pod.finish(s, 75)
				return
			}
			// The rescheduled attempt runs the marker as a no-op so
			// that the step completes successfully.
			pod.log.Write([]byte("[barq] rescheduled after simulated preemption\n"))
			continue
		}
		process := exec.Command("/bin/sh", "-c", command)
		process.Dir = workspace
		process.Env = environment
		process.Stdout = pod.log
		process.Stderr = pod.log
		err := process.Run()
		if err != nil {
			exitCode = 1
			if exitError, ok := err.(*exec.ExitError); ok {
				exitCode = exitError.ExitCode()
			}
			pod.log.Write([]byte(fmt.Sprintf("[barq] command %d failed with status %d\n", index, exitCode)))
			break
		}
	}
	pod.finish(s, exitCode)
}

// finish marks a simulated pod as terminated.
func (p *simPod) finish(simulator *Simulator, exitCode int) {
	simulator.mu.Lock()
	p.status.Phase = "Succeeded"
	if exitCode != 0 {
		p.status.Phase = "Failed"
	}
	p.status.ContainerStatuses = []ContainerStat{{
		Name: "worker",
		State: ContainerState{
			Terminated: &TerminatedState{ExitCode: exitCode},
		},
	}}
	simulator.mu.Unlock()
	p.log.close()
}

// GetPod implements Backend.
func (s *Simulator) GetPod(ctx context.Context, name string) (Pod, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pod, ok := s.pods[name]
	if !ok {
		return Pod{}, &APIError{StatusCode: 404, Method: "GET", Path: "/pods/" + name, Body: "not found"}
	}
	return Pod{Metadata: pod.metadata, Status: pod.status}, nil
}

// ListRunPods implements Backend.
func (s *Simulator) ListRunPods(ctx context.Context, runID string) ([]Pod, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []Pod
	for _, pod := range s.pods {
		if pod.metadata.Labels["barq.io/run-id"] == sanitizeLabel(runID) {
			result = append(result, Pod{Metadata: pod.metadata, Status: pod.status})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Metadata.Name < result[j].Metadata.Name })
	return result, nil
}

// DeletePod implements Backend.
func (s *Simulator) DeletePod(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pods, name)
	return nil
}

// PodLogs implements Backend.
func (s *Simulator) PodLogs(ctx context.Context, name string, since time.Time) (io.ReadCloser, error) {
	s.mu.Lock()
	pod, ok := s.pods[name]
	s.mu.Unlock()
	if !ok {
		return nil, &APIError{StatusCode: 404, Method: "GET", Path: "/pods/" + name + "/log", Body: "not found"}
	}
	return pod.log.reader(), nil
}

// GetLease implements Backend.
func (s *Simulator) GetLease(ctx context.Context, name string) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lease, ok := s.leases[name]
	if !ok {
		return Lease{}, &APIError{StatusCode: 404, Method: "GET", Path: "/leases/" + name, Body: "not found"}
	}
	return lease, nil
}

// CreateLease implements Backend.
func (s *Simulator) CreateLease(ctx context.Context, lease Lease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.leases[lease.Metadata.Name]; exists {
		return &APIError{StatusCode: 409, Method: "POST", Path: "/leases", Body: "alreadyexists"}
	}
	s.revision++
	lease.Metadata.ResourceVersion = strconv.Itoa(s.revision)
	s.leases[lease.Metadata.Name] = lease
	return nil
}

// UpdateLease implements Backend.
func (s *Simulator) UpdateLease(ctx context.Context, lease Lease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.leases[lease.Metadata.Name]
	if !ok {
		return &APIError{StatusCode: 404, Method: "PUT", Path: "/leases", Body: "not found"}
	}
	if existing.Metadata.ResourceVersion != lease.Metadata.ResourceVersion {
		return &APIError{StatusCode: 409, Method: "PUT", Path: "/leases", Body: "conflict"}
	}
	s.revision++
	lease.Metadata.ResourceVersion = strconv.Itoa(s.revision)
	s.leases[lease.Metadata.Name] = lease
	return nil
}

// spotPreemptMarker is the marker a step uses to request a simulated
// Spot preemption.
const spotPreemptMarker = "SIMULATE_SPOT_PREEMPT"

// spotSimulated reports whether a run already simulated a preemption.
func (s *Simulator) spotSimulated(runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spotRuns[runID]
}

// markSpotSimulated records that a run simulated a preemption.
func (s *Simulator) markSpotSimulated(runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spotRuns == nil {
		s.spotRuns = make(map[string]bool)
	}
	s.spotRuns[runID] = true
}

// simLog is a fan-out log buffer with independent readers.
type simLog struct {
	mu     sync.Mutex
	cond   *sync.Cond
	chunks [][]byte
	closed bool
}

func newSimLog() *simLog {
	log := &simLog{}
	log.cond = sync.NewCond(&log.mu)
	return log
}

// Write appends output.
func (l *simLog) Write(data []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	copied := append([]byte(nil), data...)
	l.chunks = append(l.chunks, copied)
	l.cond.Broadcast()
	return len(data), nil
}

// close marks the log complete.
func (l *simLog) close() {
	l.mu.Lock()
	l.closed = true
	l.cond.Broadcast()
	l.mu.Unlock()
}

// reader returns an independent reader over the log.
func (l *simLog) reader() io.ReadCloser {
	return &simLogReader{log: l}
}

// simLogReader reads the log from the beginning.
type simLogReader struct {
	log    *simLog
	offset int
}

// Read implements io.Reader.
func (r *simLogReader) Read(p []byte) (int, error) {
	r.log.mu.Lock()
	defer r.log.mu.Unlock()
	for r.offset >= len(r.log.chunks) && !r.log.closed {
		r.log.cond.Wait()
	}
	if r.offset >= len(r.log.chunks) {
		return 0, io.EOF
	}
	chunk := r.log.chunks[r.offset]
	r.offset++
	return copy(p, chunk), nil
}

// Close implements io.Closer.
func (r *simLogReader) Close() error { return nil }
