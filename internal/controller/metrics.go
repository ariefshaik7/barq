package controller

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"barq/internal/metrics"
	"barq/internal/queue"
)

// Metrics is the set of metrics Barq exports.
//
// The names follow the Prometheus conventions: a namespace, a
// subsystem and a unit.
type Metrics struct {
	registry *metrics.Registry

	runsCreated    *metrics.Counter
	runsFinished   *metrics.Counter
	stepsStarted   *metrics.Counter
	stepsFinished  *metrics.Counter
	stepsFailed    *metrics.Counter
	stepsCached    *metrics.Counter
	stepsPreempted *metrics.Counter
	stepDuration   *metrics.Histogram
	runDuration    *metrics.Histogram
	queueDepth     *metrics.Gauge
	queueRunning   *metrics.Gauge
	activeRuns     *metrics.Gauge
	admission      *metrics.Counter
}

// NewMetrics creates the metric set.
func NewMetrics() *Metrics {
	registry := metrics.NewRegistry()
	return &Metrics{
		registry:       registry,
		runsCreated:    registry.NewCounter("barq_runs_created_total", "The number of runs that were created."),
		runsFinished:   registry.NewCounter("barq_runs_finished_total", "The number of runs that reached a terminal state."),
		stepsStarted:   registry.NewCounter("barq_steps_started_total", "The number of steps that started."),
		stepsFinished:  registry.NewCounter("barq_steps_finished_total", "The number of steps that completed."),
		stepsFailed:    registry.NewCounter("barq_steps_failed_total", "The number of steps that failed."),
		stepsCached:    registry.NewCounter("barq_steps_cached_total", "The number of steps that were served from the cache."),
		stepsPreempted: registry.NewCounter("barq_steps_preempted_total", "The number of steps that were rescheduled after a preemption."),
		stepDuration:   registry.NewHistogram("barq_step_duration_seconds", "The duration of a step.", []float64{0.1, 0.5, 1, 5, 15, 60, 300, 900, 3600}),
		runDuration:    registry.NewHistogram("barq_run_duration_seconds", "The duration of a run.", []float64{1, 5, 15, 60, 300, 900, 3600, 14400}),
		queueDepth:     registry.NewGauge("barq_queue_depth", "The number of runs waiting to be admitted."),
		queueRunning:   registry.NewGauge("barq_queue_running", "The number of runs that are executing."),
		activeRuns:     registry.NewGauge("barq_active_runs", "The number of runs that are executing."),
		admission:      registry.NewCounter("barq_admission_total", "The number of runs that were admitted to execution."),
	}
}

// Handler serves the metrics endpoint.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(m.registry.Gather()))
	})
}

// Gather renders the metrics.
func (m *Metrics) Gather() string { return m.registry.Gather() }

// recordRunCreated counts a new run.
func (e *Engine) recordRunCreated() {
	if e.metrics == nil {
		return
	}
	e.metrics.runsCreated.Inc(nil)
}

// recordRunFinished counts a finished run and observes its duration.
func (e *Engine) recordRunFinished(state *RunState) {
	if e.metrics == nil {
		return
	}
	labels := map[string]string{"status": string(state.Status)}
	e.metrics.runsFinished.Inc(labels)
	if state.StartedAt != nil && state.FinishedAt != nil {
		e.metrics.runDuration.Observe(state.FinishedAt.Sub(*state.StartedAt).Seconds(), nil)
	}
}

// recordStepStarted counts a started step.
func (e *Engine) recordStepStarted() {
	if e.metrics == nil {
		return
	}
	e.metrics.stepsStarted.Inc(nil)
}

// recordStepFinished counts a finished step and observes its duration.
func (e *Engine) recordStepFinished(status string, duration time.Duration) {
	if e.metrics == nil {
		return
	}
	labels := map[string]string{"status": status}
	e.metrics.stepsFinished.Inc(labels)
	switch status {
	case string(StepFailed):
		e.metrics.stepsFailed.Inc(nil)
	case string(StepCached):
		e.metrics.stepsCached.Inc(nil)
	case string(StepSpotMigrating):
		e.metrics.stepsPreempted.Inc(nil)
	}
	e.metrics.stepDuration.Observe(duration.Seconds(), nil)
}

// updateQueueMetrics refreshes the queue gauges.
func (e *Engine) updateQueueMetrics() {
	if e.metrics == nil || e.Queue == nil {
		return
	}
	stats := e.Queue.Stats()
	e.metrics.queueDepth.Set(float64(stats.Pending+stats.WaitingOnDeps), nil)
	e.metrics.queueRunning.Set(float64(stats.Running), nil)
	e.metrics.activeRuns.Set(float64(stats.Running), nil)
}

// recordAdmission counts an admitted run.
func (e *Engine) recordAdmission() {
	if e.metrics == nil {
		return
	}
	e.metrics.admission.Inc(nil)
}

// queueStats exposes the queue statistics for the API.
func (e *Engine) queueStats() queue.Stats {
	if e.Queue == nil {
		return queue.Stats{}
	}
	return e.Queue.Stats()
}

// formatCount renders a count for the API.
func formatCount(value int) string { return strconv.Itoa(value) }

// ensure the context import is used by the metrics helpers.
var _ = context.Background
