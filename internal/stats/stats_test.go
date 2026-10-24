package stats

import (
	"context"
	"testing"
	"time"
)

func summary(id string, status string, created time.Time, finished *time.Time, steps map[string]StepSummary) RunSummary {
	return RunSummary{ID: id, Name: id, Status: status, CreatedAt: created, FinishedAt: finished, Steps: steps}
}

func TestSnapshotComputesRates(t *testing.T) {
	now := time.Now().UTC()
	finished := now.Add(time.Minute)

	runs := []RunSummary{
		summary("a", "SUCCESS", now, &finished, map[string]StepSummary{
			"build": {ID: "build", Status: "SUCCESS", DurationMS: 1000},
			"test":  {ID: "test", Status: "CACHED", DurationMS: 0, CacheHit: true},
		}),
		summary("b", "FAILED", now, &finished, map[string]StepSummary{
			"build": {ID: "build", Status: "FAILED", DurationMS: 2000},
		}),
		summary("c", "RUNNING", now, nil, map[string]StepSummary{
			"build": {ID: "build", Status: "RUNNING", DurationMS: 500},
		}),
	}

	manager := NewManager(nil, "barq")
	manager.SetLoader(func(context.Context) ([]RunSummary, error) { return runs, nil })

	snapshot, err := manager.Snapshot(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.TotalRuns != 3 {
		t.Fatalf("expected 3 runs, got %d", snapshot.TotalRuns)
	}
	if snapshot.SuccessRuns != 1 || snapshot.FailedRuns != 1 || snapshot.ActiveRuns != 1 {
		t.Fatalf("unexpected run counts: %+v", snapshot)
	}
	// One of two completed runs succeeded.
	if snapshot.SuccessRate != 50 {
		t.Fatalf("expected a 50%% success rate, got %v", snapshot.SuccessRate)
	}
	// One of four steps was cached.
	if snapshot.CacheHitRate != 25 {
		t.Fatalf("expected a 25%% cache hit rate, got %v", snapshot.CacheHitRate)
	}
	if snapshot.TotalSteps != 4 {
		t.Fatalf("expected 4 steps, got %d", snapshot.TotalSteps)
	}
	if snapshot.CachedSteps != 1 {
		t.Fatalf("expected 1 cached step, got %d", snapshot.CachedSteps)
	}
	if snapshot.FailedSteps != 1 {
		t.Fatalf("expected 1 failed step, got %d", snapshot.FailedSteps)
	}
	// TotalComputeMS is the sum of the completed run durations.
	// Two runs completed, each one minute long.
	if snapshot.TotalComputeMS != 120000 {
		t.Fatalf("expected 120000ms of compute, got %d", snapshot.TotalComputeMS)
	}
}

func TestSnapshotReportsFailureHotspots(t *testing.T) {
	now := time.Now().UTC()
	runs := []RunSummary{
		summary("a", "FAILED", now, nil, map[string]StepSummary{
			"lint":  {ID: "lint", Status: "FAILED"},
			"build": {ID: "build", Status: "FAILED"},
		}),
		summary("b", "FAILED", now, nil, map[string]StepSummary{
			"lint": {ID: "lint", Status: "FAILED"},
		}),
	}
	manager := NewManager(nil, "barq")
	manager.SetLoader(func(context.Context) ([]RunSummary, error) { return runs, nil })

	snapshot, err := manager.Snapshot(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.FailureReasons) != 2 {
		t.Fatalf("expected 2 failure groups, got %+v", snapshot.FailureReasons)
	}
	// The hotspots are ordered by frequency.
	if snapshot.FailureReasons[0].StepID != "lint" || snapshot.FailureReasons[0].Count != 2 {
		t.Fatalf("expected lint to be the top hotspot, got %+v", snapshot.FailureReasons)
	}
}

func TestSnapshotIsCached(t *testing.T) {
	calls := 0
	manager := NewManager(nil, "barq")
	manager.SetLoader(func(context.Context) ([]RunSummary, error) {
		calls++
		return nil, nil
	})
	if _, err := manager.Snapshot(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Snapshot(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("expected the snapshot to be cached, got %d loader calls", calls)
	}
	manager.Invalidate()
	if _, err := manager.Snapshot(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("expected the cache to be invalidated, got %d loader calls", calls)
	}
}

func TestSnapshotExcludesRunsOutsideTheWindow(t *testing.T) {
	old := time.Now().UTC().Add(-48 * time.Hour)
	runs := []RunSummary{
		summary("old", "SUCCESS", old, nil, nil),
		summary("new", "SUCCESS", time.Now().UTC(), nil, nil),
	}
	manager := NewManager(nil, "barq")
	manager.SetLoader(func(context.Context) ([]RunSummary, error) { return runs, nil })

	snapshot, err := manager.Snapshot(context.Background(), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.TotalRuns != 1 {
		t.Fatalf("expected only the recent run to count, got %d", snapshot.TotalRuns)
	}
}
