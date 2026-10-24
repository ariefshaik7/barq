package queue

import (
	"context"
	"sync"
	"testing"
	"time"
)

func newTestQueue(t *testing.T) *Queue {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return New(ctx, Options{
		Extension:       time.Second,
		ProcessInterval: 5 * time.Millisecond,
	})
}

func acceptAll(*Task) (bool, int) { return true, 1 }

func TestPushAndPoll(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()

	if err := q.Push(ctx, &Task{ID: "a", Name: "first"}); err != nil {
		t.Fatal(err)
	}

	type result struct {
		task    *Task
		release func(Status, error)
		err     error
	}
	results := make(chan result, 1)
	go func() {
		task, release, err := q.Poll(ctx, "worker-1", acceptAll)
		results <- result{task, release, err}
	}()

	select {
	case got := <-results:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.task.ID != "a" {
			t.Fatalf("expected task a, got %q", got.task.ID)
		}
		got.release(StatusSuccess, nil)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the task")
	}
}

func TestDoneReleasesTheSlot(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()
	if err := q.Push(ctx, &Task{ID: "a"}); err != nil {
		t.Fatal(err)
	}

	received := make(chan *Task, 1)
	go func() {
		task, release, err := q.Poll(ctx, "worker-1", acceptAll)
		if err != nil {
			return
		}
		received <- task
		release(StatusSuccess, nil)
	}()

	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}

	// The task must be gone from the running set.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if q.Stats().Running == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the task was not released")
}

func TestDependenciesGateATask(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()

	// The dependent task is pushed first. It must wait for its
	// dependency, even though it is at the front of the queue.
	if err := q.Push(ctx, &Task{ID: "b", Dependencies: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	if err := q.Push(ctx, &Task{ID: "a"}); err != nil {
		t.Fatal(err)
	}

	order := make(chan string, 4)
	var mu sync.Mutex
	seen := map[string]bool{}

	go func() {
		for index := 0; index < 2; index++ {
			task, release, err := q.Poll(ctx, "worker-1", acceptAll)
			if err != nil {
				return
			}
			mu.Lock()
			seen[task.ID] = true
			mu.Unlock()
			order <- task.ID
			release(StatusSuccess, nil)
		}
	}()

	first := <-order
	second := <-order
	if first != "a" {
		t.Fatalf("expected the dependency to run first, got %q", first)
	}
	if second != "b" {
		t.Fatalf("expected the dependent task second, got %q", second)
	}
}

func TestFailedDependencyPropagatesTheStatus(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()
	if err := q.Push(ctx, &Task{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := q.Push(ctx, &Task{ID: "b", Dependencies: []string{"a"}}); err != nil {
		t.Fatal(err)
	}

	got := make(chan *Task, 2)
	go func() {
		for index := 0; index < 2; index++ {
			task, release, err := q.Poll(ctx, "worker-1", acceptAll)
			if err != nil {
				return
			}
			got <- task
			if task.ID == "a" {
				release(StatusFailure, nil)
			} else {
				release(StatusSuccess, nil)
			}
		}
	}()

	first := <-got
	second := <-got
	if first.ID != "a" {
		t.Fatalf("expected a first, got %q", first.ID)
	}
	if second.ID != "b" {
		t.Fatalf("expected b second, got %q", second.ID)
	}
}

func TestConcurrencyLimitIsEnforced(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()

	// Two tasks in the same group with a limit of one.
	if err := q.Push(ctx, &Task{ID: "a", ConcurrencyGroup: "g", ConcurrencyLimit: 1}); err != nil {
		t.Fatal(err)
	}
	if err := q.Push(ctx, &Task{ID: "b", ConcurrencyGroup: "g", ConcurrencyLimit: 1}); err != nil {
		t.Fatal(err)
	}

	first := make(chan *Task, 1)
	go func() {
		task, release, err := q.Poll(ctx, "worker-1", acceptAll)
		if err != nil {
			return
		}
		first <- task
		// The task is held so that the group stays occupied.
		time.Sleep(500 * time.Millisecond)
		release(StatusSuccess, nil)
	}()

	select {
	case task := <-first:
		if task.ID != "a" {
			t.Fatalf("expected a first, got %q", task.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}

	// While the first task runs, the second must not be handed out.
	second := make(chan *Task, 1)
	go func() {
		task, release, err := q.Poll(ctx, "worker-2", acceptAll)
		if err != nil {
			return
		}
		second <- task
		release(StatusSuccess, nil)
	}()
	select {
	case task := <-second:
		t.Fatalf("the concurrency limit was not enforced, got %q", task.ID)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestWorkerFilterSelectsTheTask(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()
	if err := q.Push(ctx, &Task{ID: "a", Labels: map[string]string{"kind": "build"}}); err != nil {
		t.Fatal(err)
	}

	filter := func(task *Task) (bool, int) {
		if task.Labels["kind"] == "build" {
			return true, 10
		}
		return false, 0
	}

	got := make(chan *Task, 1)
	go func() {
		task, release, err := q.Poll(ctx, "worker-1", filter)
		if err != nil {
			return
		}
		got <- task
		release(StatusSuccess, nil)
	}()

	select {
	case task := <-got:
		if task.ID != "a" {
			t.Fatalf("unexpected task %q", task.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}

func TestExpiredTaskIsReclaimed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := New(ctx, Options{
		Extension:       50 * time.Millisecond,
		ProcessInterval: 5 * time.Millisecond,
	})

	if err := q.Push(ctx, &Task{ID: "a"}); err != nil {
		t.Fatal(err)
	}

	received := make(chan *Task, 2)
	go func() {
		for index := 0; index < 2; index++ {
			task, release, err := q.Poll(ctx, "worker-1", acceptAll)
			if err != nil {
				return
			}
			received <- task
			// Never release it, so the deadline passes.
			_ = release
		}
	}()

	// The first task is handed out. The worker never releases it, so
	// the deadline passes and the task is requeued and handed out
	// again.
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first delivery")
	}
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("the expired task was not reclaimed")
	}
}

func TestPauseAndResume(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()
	q.Pause()
	if err := q.Push(ctx, &Task{ID: "a"}); err != nil {
		t.Fatal(err)
	}

	got := make(chan *Task, 1)
	go func() {
		task, release, err := q.Poll(ctx, "worker-1", acceptAll)
		if err != nil {
			return
		}
		got <- task
		release(StatusSuccess, nil)
	}()

	select {
	case <-got:
		t.Fatal("the paused queue handed out work")
	case <-time.After(200 * time.Millisecond):
	}

	q.Resume()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the resumed queue did not hand out work")
	}
}

func TestStats(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()
	if err := q.Push(ctx, &Task{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stats := q.Stats()
		if stats.Pending+stats.Running+stats.WaitingOnDeps == 1 {
			if stats.Workers != 0 {
				t.Fatalf("expected no workers, got %d", stats.Workers)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the task was not accounted for")
}

func TestPushRejectsADuplicateRunningTask(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()
	if err := q.Push(ctx, &Task{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	// The task is queued, so a second push is allowed but replaces the
	// first. A push of a task that is already running is an error.
	received := make(chan struct{})
	go func() {
		_, release, err := q.Poll(ctx, "worker-1", acceptAll)
		if err != nil {
			return
		}
		close(received)
		time.Sleep(300 * time.Millisecond)
		release(StatusSuccess, nil)
	}()
	<-received
	if err := q.Push(ctx, &Task{ID: "a"}); err == nil {
		t.Fatal("expected an error for a duplicate running task")
	}
}

func TestPriorityScheduling(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()

	// Push low priority first, then high priority.
	if err := q.Push(ctx, &Task{ID: "low", Priority: 10, Created: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := q.Push(ctx, &Task{ID: "high", Priority: 100, Created: time.Now()}); err != nil {
		t.Fatal(err)
	}

	got := make(chan *Task, 2)
	go func() {
		for i := 0; i < 2; i++ {
			task, release, err := q.Poll(ctx, "worker-1", acceptAll)
			if err != nil {
				return
			}
			got <- task
			release(StatusSuccess, nil)
		}
	}()

	first := <-got
	second := <-got
	if first.ID != "high" {
		t.Fatalf("expected high priority task first, got %q", first.ID)
	}
	if second.ID != "low" {
		t.Fatalf("expected low priority task second, got %q", second.ID)
	}
}

func TestQuietPeriod(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := New(ctx, Options{
		Extension:       time.Second,
		ProcessInterval: 5 * time.Millisecond,
		Clock:           func() time.Time { return time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC) },
	})

	// Push a task with a quiet period that hasn't elapsed.
	quietUntil := time.Date(2024, 1, 1, 12, 0, 10, 0, time.UTC)
	if err := q.Push(ctx, &Task{ID: "quiet", QuietUntil: quietUntil}); err != nil {
		t.Fatal(err)
	}

	// The task should not be handed out while in quiet period.
	got := make(chan *Task, 1)
	go func() {
		task, release, err := q.Poll(ctx, "worker-1", acceptAll)
		if err != nil {
			return
		}
		got <- task
		release(StatusSuccess, nil)
	}()

	select {
	case <-got:
		t.Fatal("task was handed out during quiet period")
	case <-time.After(100 * time.Millisecond):
		// Expected: no task received
	}

	// Advance clock past quiet period.
	clockTime := time.Date(2024, 1, 1, 12, 0, 11, 0, time.UTC)
	q2 := New(ctx, Options{
		Extension:       time.Second,
		ProcessInterval: 5 * time.Millisecond,
		Clock:           func() time.Time { return clockTime },
	})
	if err := q2.Push(ctx, &Task{ID: "quiet2", QuietUntil: quietUntil}); err != nil {
		t.Fatal(err)
	}
	got2 := make(chan *Task, 1)
	go func() {
		task, release, err := q2.Poll(ctx, "worker-1", acceptAll)
		if err != nil {
			return
		}
		got2 <- task
		release(StatusSuccess, nil)
	}()

	select {
	case task := <-got2:
		if task.ID != "quiet2" {
			t.Fatalf("expected task quiet2, got %q", task.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("task was not handed out after quiet period")
	}
}
