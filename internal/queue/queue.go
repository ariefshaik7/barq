// Package queue implements the Barq task queue.
//
// The queue is the admission control point of the control plane. It
// decides which workflows may run at the same time, holds the ones
// that are waiting for their dependencies, and reclaims the ones
// whose worker disappeared.
//
// The design follows the pattern that mature CI systems converge on:
// three collections, a single background process loop, and a single
// mutex guarding all of them. The simplicity is the point. A queue
// that is hard to reason about is a queue that will eventually
// schedule a workflow twice.
package queue

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Errors returned by the queue.
var (
	// ErrNotFound is returned when a task is not in the queue.
	ErrNotFound = errors.New("the task is not in the queue")
	// ErrTaskExpired is returned when a task exceeded its deadline.
	ErrTaskExpired = errors.New("the task exceeded its deadline")
	// ErrWorkerKicked is returned to a worker that was removed.
	ErrWorkerKicked = errors.New("the worker was kicked")
	// ErrPaused is returned when the queue is paused.
	ErrPaused = errors.New("the queue is paused")
)

// Status is the outcome of a task.
type Status string

const (
	// StatusSuccess means the task completed successfully.
	StatusSuccess Status = "success"
	// StatusFailure means the task failed.
	StatusFailure Status = "failure"
	// StatusKilled means the task was canceled.
	StatusKilled Status = "killed"
	// StatusSkipped means the task was skipped because a dependency
	// did not succeed.
	StatusSkipped Status = "skipped"
)

// Task is a unit of work in the queue.
//
// A task is a workflow: a run of the pipeline, which may itself
// contain several stages.
type Task struct {
	// ID is the unique identifier of the task.
	ID string
	// RunID is the pipeline run the task belongs to.
	RunID string
	// Name is a human readable label.
	Name string
	// Dependencies are the task identifiers that must complete
	// before this task may run.
	Dependencies []string
	// DepStatus records the outcome of each dependency.
	DepStatus map[string]Status
	// Labels are used by the worker filters to decide whether a
	// worker can run the task.
	Labels map[string]string
	// ConcurrencyGroup groups tasks that share a concurrency budget.
	// An empty group means the task is unconstrained.
	ConcurrencyGroup string
	// ConcurrencyLimit is the maximum number of tasks that may run
	// at the same time in the concurrency group.
	ConcurrencyLimit int
	// Created is the timestamp used to order tasks within a
	// concurrency group.
	Created time.Time
	// Deadline is the absolute time by which the task must complete.
	Deadline time.Time
	// Priority is the scheduling priority (higher = more urgent).
	// Default is 0. Range is typically 0-100.
	Priority int
	// QuietUntil is the time before which the task must not be scheduled.
	// This implements Jenkins-style quiet periods for debouncing rapid triggers.
	QuietUntil time.Time
}

// Clone returns a copy of the task.
func (t *Task) Clone() *Task {
	if t == nil {
		return nil
	}
	copied := *t
	copied.Dependencies = append([]string(nil), t.Dependencies...)
	copied.DepStatus = make(map[string]Status, len(t.DepStatus))
	for key, value := range t.DepStatus {
		copied.DepStatus[key] = value
	}
	copied.Labels = make(map[string]string, len(t.Labels))
	for key, value := range t.Labels {
		copied.Labels[key] = value
	}
	return &copied
}

// effectivePriority returns the task's priority for scheduling.
// Higher values are scheduled first.
func (t *Task) effectivePriority() int {
	return t.Priority
}

// isQuiet reports whether the task is still in its quiet period.
func (t *Task) isQuiet(now time.Time) bool {
	return !t.QuietUntil.IsZero() && now.Before(t.QuietUntil)
}

// entry is a running task.
type entry struct {
	task     *Task
	done     chan struct{}
	err      error
	deadline time.Time
}

// worker is a consumer of the queue.
type worker struct {
	id      string
	filter  func(*Task) (bool, int)
	channel chan *Task
	stop    context.CancelCauseFunc
}

// Stats describes the state of the queue.
type Stats struct {
	// Workers is the number of registered workers.
	Workers int `json:"workers"`
	// Pending is the number of tasks ready to run.
	Pending int `json:"pending"`
	// WaitingOnDeps is the number of tasks blocked on a dependency.
	WaitingOnDeps int `json:"waiting_on_deps"`
	// Running is the number of tasks that are executing.
	Running int `json:"running"`
	// Paused reports whether the queue is handing out work.
	Paused bool `json:"paused"`
	// PendingTasks lists the tasks that are ready to run.
	PendingTasks []*Task `json:"pending_tasks,omitempty"`
	// RunningTasks lists the tasks that are executing.
	RunningTasks []*Task `json:"running_tasks,omitempty"`
}

// Queue is the task queue.
type Queue struct {
	mu sync.Mutex

	ctx    context.Context
	logger *slog.Logger

	workers map[string]*worker
	running map[string]*entry
	// pending holds the tasks that could run now.
	pending *list.List
	// waitingOnDeps holds the tasks that are blocked.
	waitingOnDeps *list.List
	// extension is how long a running task's deadline is extended by.
	extension time.Duration
	paused    bool
	// clock is the time source. It exists for testing.
	clock func() time.Time
	// processInterval is how often the queue rearranges itself.
	processInterval time.Duration
}

// Options configures a queue.
type Options struct {
	// Extension is how long a running task may take before it is
	// reclaimed. The task owner extends the deadline while it works.
	Extension time.Duration
	// ProcessInterval is how often the queue is rearranged.
	ProcessInterval time.Duration
	// Logger receives the queue's diagnostics.
	Logger *slog.Logger
	// Clock is the time source.
	Clock func() time.Time
}

// New creates a queue and starts its process loop.
func New(ctx context.Context, options Options) *Queue {
	if options.Extension <= 0 {
		options.Extension = 30 * time.Minute
	}
	if options.ProcessInterval <= 0 {
		options.ProcessInterval = 100 * time.Millisecond
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.Clock == nil {
		options.Clock = time.Now
	}
	q := &Queue{
		ctx:             ctx,
		logger:          options.Logger,
		workers:         make(map[string]*worker),
		running:         make(map[string]*entry),
		pending:         list.New(),
		waitingOnDeps:   list.New(),
		extension:       options.Extension,
		clock:           options.Clock,
		processInterval: options.ProcessInterval,
	}
	go q.process()
	return q
}

// Push adds a task to the queue.
func (q *Queue) Push(ctx context.Context, task *Task) error {
	if task == nil {
		return fmt.Errorf("the task is required")
	}
	if task.ID == "" {
		return fmt.Errorf("the task requires an identifier")
	}
	q.mu.Lock()
	if _, running := q.running[task.ID]; running {
		q.mu.Unlock()
		return fmt.Errorf("the task %q is already running", task.ID)
	}
	if task.DepStatus == nil {
		task.DepStatus = make(map[string]Status)
	}
	if task.Created.IsZero() {
		task.Created = q.clock()
	}
	q.pending.PushBack(task)
	q.mu.Unlock()
	return nil
}

// PushAll adds several tasks at once.
//
// The tasks are added under a single lock so that a set of related
// tasks becomes visible to the scheduler atomically.
func (q *Queue) PushAll(ctx context.Context, tasks []*Task) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, task := range tasks {
		if task == nil {
			continue
		}
		if task.ID == "" {
			return fmt.Errorf("a task requires an identifier")
		}
		if task.DepStatus == nil {
			task.DepStatus = make(map[string]Status)
		}
		if task.Created.IsZero() {
			task.Created = q.clock()
		}
		q.pending.PushBack(task)
	}
	return nil
}

// Register adds a worker to the queue.
//
// The filter decides whether the worker can run a task and returns a
// score. The highest scoring worker wins.
//
// The returned function unregisters the worker.
func (q *Queue) Register(ctx context.Context, id string, filter func(*Task) (bool, int)) func() {
	_, stop := context.WithCancelCause(ctx)
	w := &worker{
		id:      id,
		filter:  filter,
		channel: make(chan *Task, 1),
		stop:    stop,
	}
	q.mu.Lock()
	q.workers[id] = w
	q.mu.Unlock()

	return func() {
		q.mu.Lock()
		if existing, ok := q.workers[id]; ok && existing == w {
			delete(q.workers, id)
		}
		q.mu.Unlock()
		stop(ErrWorkerKicked)
	}
}

// Poll waits for a task that matches the worker's filter.
//
// It returns the task and a function that must be called when the
// task is finished. The function releases the concurrency slot and
// wakes anything waiting on the task.
func (q *Queue) Poll(ctx context.Context, id string, filter func(*Task) (bool, int)) (*Task, func(Status, error), error) {
	_, stop := context.WithCancelCause(ctx)
	w := &worker{
		id:      id,
		filter:  filter,
		channel: make(chan *Task, 1),
		stop:    stop,
	}
	q.mu.Lock()
	q.workers[id] = w
	q.mu.Unlock()

	defer func() {
		q.mu.Lock()
		if existing, ok := q.workers[id]; ok && existing == w {
			delete(q.workers, id)
		}
		q.mu.Unlock()
		stop(ErrWorkerKicked)
	}()

	for {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case task := <-w.channel:
			release := func(status Status, err error) {
				q.finish(task.ID, status, err)
			}
			return task, release, nil
		}
	}
}

// Done marks a task as finished.
func (q *Queue) Done(ctx context.Context, id string, status Status) error {
	q.finish(id, status, nil)
	return nil
}

// Error marks a task as finished with an error.
func (q *Queue) Error(ctx context.Context, id string, err error) error {
	q.finish(id, StatusFailure, err)
	return nil
}

// finish removes a task from the queue and propagates its outcome to
// the tasks that depend on it.
func (q *Queue) finish(id string, status Status, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if running, ok := q.running[id]; ok {
		running.err = err
		close(running.done)
		delete(q.running, id)
	} else {
		q.removeFromPendingAndWaiting(id)
	}

	// The outcome is propagated after the task itself has been
	// removed, because a task may be both a dependency of others and
	// one of the tasks being finished.
	q.updateDependencyStatus(id, status)
}

// Wait blocks until a task has finished.
func (q *Queue) Wait(ctx context.Context, id string) error {
	q.mu.Lock()
	running := q.running[id]
	q.mu.Unlock()
	if running == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-running.done:
		return running.err
	}
}

// Extend pushes back the deadline of a running task.
//
// The owner of a task must extend it periodically. A task whose
// deadline passes is reclaimed and requeued, which is how a workflow
// survives the loss of its controller.
func (q *Queue) Extend(ctx context.Context, id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	running, ok := q.running[id]
	if !ok {
		return ErrNotFound
	}
	running.deadline = q.clock().Add(q.extension)
	return nil
}

// Stats returns the state of the queue.
func (q *Queue) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()

	stats := Stats{
		Workers:       len(q.workers),
		Pending:       q.pending.Len(),
		WaitingOnDeps: q.waitingOnDeps.Len(),
		Running:       len(q.running),
		Paused:        q.paused,
	}
	for element := q.pending.Front(); element != nil; element = element.Next() {
		if task, ok := element.Value.(*Task); ok {
			stats.PendingTasks = append(stats.PendingTasks, task.Clone())
		}
	}
	for _, running := range q.running {
		stats.RunningTasks = append(stats.RunningTasks, running.task.Clone())
	}
	sort.Slice(stats.RunningTasks, func(i, j int) bool {
		return stats.RunningTasks[i].ID < stats.RunningTasks[j].ID
	})
	return stats
}

// Pause stops the queue from handing out work.
func (q *Queue) Pause() {
	q.mu.Lock()
	q.paused = true
	q.mu.Unlock()
}

// Resume lets the queue hand out work again.
func (q *Queue) Resume() {
	q.mu.Lock()
	q.paused = false
	q.mu.Unlock()
}

// Kick removes a worker from the queue.
func (q *Queue) Kick(id string) {
	q.mu.Lock()
	worker, ok := q.workers[id]
	if ok {
		delete(q.workers, id)
	}
	q.mu.Unlock()
	if ok {
		worker.stop(ErrWorkerKicked)
	}
}

// process is the queue's background loop.
//
// It runs in its own goroutine for the lifetime of the queue. Every
// rearrangement happens under the single mutex, so the collections
// are never observed in an inconsistent state.
func (q *Queue) process() {
	ticker := time.NewTicker(q.processInterval)
	defer ticker.Stop()
	for {
		select {
		case <-q.ctx.Done():
			return
		case <-ticker.C:
		}
		q.rearrange()
	}
}

// rearrange performs one pass of the queue.
func (q *Queue) rearrange() {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.paused {
		return
	}

	q.reclaimExpired()
	q.promoteWaiting()

	for {
		element, w := q.assign()
		if element == nil || w == nil {
			return
		}
		task, _ := element.Value.(*Task)
		q.pending.Remove(element)
		delete(q.workers, w.id)
		q.running[task.ID] = &entry{
			task:     task,
			done:     make(chan struct{}),
			deadline: q.clock().Add(q.extension),
		}
		select {
		case w.channel <- task:
		default:
			// The worker is gone. Put the task back.
			q.pending.PushFront(element)
			q.running[task.ID] = &entry{
				task:     task,
				done:     make(chan struct{}),
				deadline: q.clock().Add(q.extension),
			}
			return
		}
		q.logger.Debug("queue: assigned task", "task", task.ID, "worker", w.id)
	}
}

// reclaimExpired returns the tasks whose deadline has passed to the
// front of the pending list.
//
// A task is reclaimed rather than failed, because the loss of a
// controller must not fail the build.
func (q *Queue) reclaimExpired() {
	now := q.clock()
	for id, running := range q.running {
		if now.After(running.deadline) {
			q.logger.Info("queue: reclaiming expired task", "task", id)
			running.err = ErrTaskExpired
			close(running.done)
			delete(q.running, id)
			q.pending.PushFront(running.task)
		}
	}
}

// promoteWaiting moves the tasks whose dependencies are satisfied
// back into the pending list.
//
// The pending identifiers are snapshotted before any task is removed,
// because a waiting task may itself be a dependency of another
// waiting task.
func (q *Queue) promoteWaiting() {
	for element := q.waitingOnDeps.Front(); element != nil; element = element.Next() {
		task, _ := element.Value.(*Task)
		q.pending.PushBack(task)
	}
	q.waitingOnDeps = list.New()

	pendingIDs := make(map[string]struct{}, q.pending.Len())
	for element := q.pending.Front(); element != nil; element = element.Next() {
		task, _ := element.Value.(*Task)
		pendingIDs[task.ID] = struct{}{}
	}

	var blocked []*list.Element
	for element := q.pending.Front(); element != nil; element = element.Next() {
		task, _ := element.Value.(*Task)
		if len(task.Dependencies) == 0 {
			continue
		}
		if q.dependencyQueued(task, pendingIDs) {
			q.waitingOnDeps.PushBack(task)
			blocked = append(blocked, element)
		}
	}
	for _, element := range blocked {
		q.pending.Remove(element)
	}
}

// dependencyQueued reports whether a dependency of the task is still
// in the queue.
func (q *Queue) dependencyQueued(task *Task, pendingIDs map[string]struct{}) bool {
	for _, dependency := range task.Dependencies {
		if _, queued := pendingIDs[dependency]; queued {
			return true
		}
		if _, running := q.running[dependency]; running {
			return true
		}
	}
	return false
}

// assign finds the best task and worker pair.
//
// Priority is the primary sort key (higher priority wins).
// Within the same priority, earliest creation time wins (FIFO).
// Tasks in their quiet period are skipped.
func (q *Queue) assign() (*list.Element, *worker) {
	var bestElement *list.Element
	var bestWorker *worker
	bestScore := 0
	bestPriority := -1
	now := q.clock()

	for element := q.pending.Front(); element != nil; element = element.Next() {
		task, _ := element.Value.(*Task)
		if !q.canRunConcurrent(task) {
			continue
		}
		// Skip tasks in their quiet period.
		if task.isQuiet(now) {
			continue
		}
		for _, w := range q.workers {
			matched, score := w.filter(task)
			if !matched {
				continue
			}
			priority := task.effectivePriority()
			if bestWorker == nil ||
				priority > bestPriority ||
				(priority == bestPriority && score > bestScore) ||
				(priority == bestPriority && score == bestScore && bestElement != nil && task.Created.Before(taskCreated(bestElement))) {
				bestElement = element
				bestWorker = w
				bestScore = score
				bestPriority = priority
			}
		}
	}
	return bestElement, bestWorker
}

// taskCreated returns the creation time of a queued task.
func taskCreated(element *list.Element) time.Time {
	if task, ok := element.Value.(*Task); ok {
		return task.Created
	}
	return time.Time{}
}

// canRunConcurrent reports whether the task may start without
// violating its concurrency group limit.
//
// The slots within a group are granted in creation order, so an
// earlier pipeline keeps its place even when a later one becomes
// ready first. The reservation only applies across pipelines,
// because dependencies never cross pipelines and reserving a slot
// across them could deadlock.
func (q *Queue) canRunConcurrent(task *Task) bool {
	if task.ConcurrencyLimit <= 0 || task.ConcurrencyGroup == "" {
		return true
	}

	running := 0
	for _, entry := range q.running {
		if entry.task.ConcurrencyGroup == task.ConcurrencyGroup {
			running++
		}
	}
	if running >= task.ConcurrencyLimit {
		return false
	}

	ahead := 0
	countAhead := func(other *Task) {
		if other.ConcurrencyGroup != task.ConcurrencyGroup || other.ID == task.ID {
			return
		}
		if other.RunID == task.RunID {
			return
		}
		if other.Created.Before(task.Created) {
			ahead++
		}
	}
	for element := q.pending.Front(); element != nil; element = element.Next() {
		if other, ok := element.Value.(*Task); ok {
			countAhead(other)
		}
	}
	for element := q.waitingOnDeps.Front(); element != nil; element = element.Next() {
		if other, ok := element.Value.(*Task); ok {
			countAhead(other)
		}
	}

	return running+ahead < task.ConcurrencyLimit
}

// updateDependencyStatus records the outcome of a task in the
// dependency status of the tasks that depend on it.
func (q *Queue) updateDependencyStatus(id string, status Status) {
	for element := q.pending.Front(); element != nil; element = element.Next() {
		task, _ := element.Value.(*Task)
		for _, dependency := range task.Dependencies {
			if dependency == id {
				task.DepStatus[dependency] = status
			}
		}
	}
	for _, running := range q.running {
		for _, dependency := range running.task.Dependencies {
			if dependency == id {
				running.task.DepStatus[dependency] = status
			}
		}
	}
	for element := q.waitingOnDeps.Front(); element != nil; element = element.Next() {
		task, _ := element.Value.(*Task)
		for _, dependency := range task.Dependencies {
			if dependency == id {
				task.DepStatus[dependency] = status
			}
		}
	}
}

// removeFromPendingAndWaiting removes a task that never ran.
func (q *Queue) removeFromPendingAndWaiting(id string) error {
	for element := q.pending.Front(); element != nil; element = element.Next() {
		task, _ := element.Value.(*Task)
		if task.ID == id {
			q.pending.Remove(element)
			return nil
		}
	}
	for element := q.waitingOnDeps.Front(); element != nil; element = element.Next() {
		task, _ := element.Value.(*Task)
		if task.ID == id {
			q.waitingOnDeps.Remove(element)
			return nil
		}
	}
	return ErrNotFound
}
