// Package stats computes the usage and reliability statistics that the
// Barq dashboard displays.
//
// The statistics are derived from the persisted run history. They are
// computed on demand and cached for a short interval so that the
// dashboard can poll frequently without re-scanning the object store.
package stats

import (
	"context"
	"sort"
	"sync"
	"time"

	"barq/internal/storage"
)

// RunSummary is the minimal run record the statistics need.
type RunSummary struct {
	ID         string
	Name       string
	Status     string
	CreatedAt  time.Time
	FinishedAt *time.Time
	Steps      map[string]StepSummary
}

// StepSummary is the minimal step record the statistics need.
type StepSummary struct {
	ID         string
	Wave       int
	Status     string
	DurationMS int64
	CacheHit   bool
	Attempt    int
}

// Snapshot is a point-in-time view of the installation's health and
// usage.
type Snapshot struct {
	GeneratedAt time.Time `json:"generated_at"`
	Window      string    `json:"window"`

	TotalRuns    int `json:"total_runs"`
	ActiveRuns   int `json:"active_runs"`
	SuccessRuns  int `json:"success_runs"`
	FailedRuns   int `json:"failed_runs"`
	CanceledRuns int `json:"canceled_runs"`

	TotalSteps     int `json:"total_steps"`
	CachedSteps    int `json:"cached_steps"`
	FailedSteps    int `json:"failed_steps"`
	PreemptedSteps int `json:"preempted_steps"`

	// SuccessRate is the percentage of completed runs that succeeded.
	SuccessRate float64 `json:"success_rate"`
	// CacheHitRate is the percentage of steps served from the cache.
	CacheHitRate float64 `json:"cache_hit_rate"`
	// AverageDuration is the mean wall clock duration of completed
	// runs.
	AverageDurationMS int64 `json:"average_duration_ms"`
	// P95Duration is the 95th percentile run duration.
	P95DurationMS int64 `json:"p95_duration_ms"`
	// TotalComputeMS is the sum of all step durations. It is a proxy
	// for consumed compute.
	TotalComputeMS int64 `json:"total_compute_ms"`
	// SavedComputeMS is the compute that the cache avoided.
	SavedComputeMS int64 `json:"saved_compute_ms"`

	// Daily is a time series of run counts per day.
	Daily []DailyPoint `json:"daily"`
	// TopSteps are the slowest steps across the window.
	TopSteps []StepTiming `json:"top_steps"`
	// FailureReasons groups failures by step.
	FailureReasons []FailureGroup `json:"failure_reasons"`
	// StepStatuses counts steps by status.
	StepStatuses map[string]int `json:"step_statuses"`
}

// DailyPoint is one day of the run time series.
type DailyPoint struct {
	Date    string `json:"date"`
	Runs    int    `json:"runs"`
	Success int    `json:"success"`
	Failed  int    `json:"failed"`
	// AverageMS is the mean run duration for that day.
	AverageMS int64 `json:"average_ms"`
}

// StepTiming is the timing of one step.
type StepTiming struct {
	StepID    string `json:"step_id"`
	Count     int    `json:"count"`
	AverageMS int64  `json:"average_ms"`
	MaxMS     int64  `json:"max_ms"`
	TotalMS   int64  `json:"total_ms"`
}

// FailureGroup groups failures by step.
type FailureGroup struct {
	StepID string `json:"step_id"`
	Count  int    `json:"count"`
}

// Manager computes and caches statistics.
type Manager struct {
	store  storage.ObjectStore
	prefix string

	mu       sync.RWMutex
	cached   *Snapshot
	cachedAt time.Time

	// load returns the run summaries. It is injected so that the
	// controller can supply in-memory state without a second scan.
	load func(context.Context) ([]RunSummary, error)
}

// NewManager creates a statistics manager.
func NewManager(store storage.ObjectStore, prefix string) *Manager {
	return &Manager{store: store, prefix: prefix}
}

// SetLoader installs the function that produces run summaries.
func (m *Manager) SetLoader(loader func(context.Context) ([]RunSummary, error)) {
	m.mu.Lock()
	m.load = loader
	m.mu.Unlock()
}

// Snapshot returns the current statistics, recomputing them if the
// cache is older than the TTL.
func (m *Manager) Snapshot(ctx context.Context, window time.Duration) (*Snapshot, error) {
	m.mu.RLock()
	if m.cached != nil && time.Since(m.cachedAt) < 15*time.Second {
		cached := m.cached
		m.mu.RUnlock()
		return cached, nil
	}
	loader := m.load
	m.mu.RUnlock()

	if loader == nil {
		return &Snapshot{GeneratedAt: time.Now().UTC(), StepStatuses: map[string]int{}}, nil
	}
	runs, err := loader(ctx)
	if err != nil {
		return nil, err
	}
	snapshot := compute(runs, window)

	m.mu.Lock()
	m.cached = snapshot
	m.cachedAt = time.Now()
	m.mu.Unlock()
	return snapshot, nil
}

// Invalidate clears the cache.
func (m *Manager) Invalidate() {
	m.mu.Lock()
	m.cached = nil
	m.mu.Unlock()
}

// compute derives the statistics from the run summaries.
func compute(runs []RunSummary, window time.Duration) *Snapshot {
	now := time.Now().UTC()
	cutoff := now.Add(-window)

	snapshot := &Snapshot{
		GeneratedAt:  now,
		Window:       window.String(),
		Daily:        make([]DailyPoint, 0, 30),
		StepStatuses: make(map[string]int),
	}

	durations := make([]int64, 0, len(runs))
	daily := map[string]*DailyPoint{}
	timings := map[string]*StepTiming{}
	failures := map[string]int{}

	for _, run := range runs {
		if !run.CreatedAt.After(cutoff) && !run.CreatedAt.IsZero() {
			// The run is outside the window. It still counts towards
			// the totals, but not towards the windowed statistics.
			continue
		}
		snapshot.TotalRuns++
		switch run.Status {
		case "RUNNING", "QUEUED":
			snapshot.ActiveRuns++
		case "SUCCESS":
			snapshot.SuccessRuns++
		case "FAILED":
			snapshot.FailedRuns++
		case "CANCELED":
			snapshot.CanceledRuns++
		}

		if !run.CreatedAt.IsZero() {
			key := run.CreatedAt.Format("2006-01-02")
			point, ok := daily[key]
			if !ok {
				point = &DailyPoint{Date: key}
				daily[key] = point
			}
			point.Runs++
			if run.Status == "SUCCESS" {
				point.Success++
			}
			if run.Status == "FAILED" {
				point.Failed++
			}
		}

		if run.FinishedAt != nil && !run.CreatedAt.IsZero() {
			duration := run.FinishedAt.Sub(run.CreatedAt).Milliseconds()
			if duration >= 0 {
				durations = append(durations, duration)
				snapshot.TotalComputeMS += duration
				if key := run.CreatedAt.Format("2006-01-02"); daily[key] != nil {
					daily[key].AverageMS += duration
				}
			}
		}

		for _, step := range run.Steps {
			snapshot.TotalSteps++
			snapshot.StepStatuses[step.Status]++
			if step.CacheHit {
				snapshot.CachedSteps++
				snapshot.SavedComputeMS += step.DurationMS
			}
			switch step.Status {
			case "FAILED", "CANCELED":
				snapshot.FailedSteps++
				failures[step.ID]++
			case "SPOT_MIGRATING":
				snapshot.PreemptedSteps++
			}
			timing, ok := timings[step.ID]
			if !ok {
				timing = &StepTiming{StepID: step.ID}
				timings[step.ID] = timing
			}
			timing.Count++
			timing.TotalMS += step.DurationMS
			if step.DurationMS > timing.MaxMS {
				timing.MaxMS = step.DurationMS
			}
		}
	}

	completed := snapshot.SuccessRuns + snapshot.FailedRuns
	if completed > 0 {
		snapshot.SuccessRate = float64(snapshot.SuccessRuns) / float64(completed) * 100
	}
	if snapshot.TotalSteps > 0 {
		snapshot.CacheHitRate = float64(snapshot.CachedSteps) / float64(snapshot.TotalSteps) * 100
	}
	if len(durations) > 0 {
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		var total int64
		for _, duration := range durations {
			total += duration
		}
		snapshot.AverageDurationMS = total / int64(len(durations))
		index := int(float64(len(durations)) * 0.95)
		if index >= len(durations) {
			index = len(durations) - 1
		}
		snapshot.P95DurationMS = durations[index]
	}

	for _, point := range daily {
		if point.Runs > 0 {
			point.AverageMS /= int64(point.Runs)
		}
		snapshot.Daily = append(snapshot.Daily, *point)
	}
	sort.Slice(snapshot.Daily, func(i, j int) bool { return snapshot.Daily[i].Date < snapshot.Daily[j].Date })

	stepTimings := make([]StepTiming, 0, len(timings))
	for _, timing := range timings {
		if timing.Count > 0 {
			timing.AverageMS = timing.TotalMS / int64(timing.Count)
		}
		stepTimings = append(stepTimings, *timing)
	}
	sort.Slice(stepTimings, func(i, j int) bool {
		if stepTimings[i].TotalMS == stepTimings[j].TotalMS {
			return stepTimings[i].StepID < stepTimings[j].StepID
		}
		return stepTimings[i].TotalMS > stepTimings[j].TotalMS
	})
	if len(stepTimings) > 10 {
		stepTimings = stepTimings[:10]
	}
	snapshot.TopSteps = stepTimings

	for stepID, count := range failures {
		snapshot.FailureReasons = append(snapshot.FailureReasons, FailureGroup{StepID: stepID, Count: count})
	}
	sort.Slice(snapshot.FailureReasons, func(i, j int) bool {
		if snapshot.FailureReasons[i].Count == snapshot.FailureReasons[j].Count {
			return snapshot.FailureReasons[i].StepID < snapshot.FailureReasons[j].StepID
		}
		return snapshot.FailureReasons[i].Count > snapshot.FailureReasons[j].Count
	})
	if len(snapshot.FailureReasons) > 10 {
		snapshot.FailureReasons = snapshot.FailureReasons[:10]
	}

	return snapshot
}
