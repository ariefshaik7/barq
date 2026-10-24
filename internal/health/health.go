// Package health tracks the health of the Barq controller and of the
// systems it depends on.
//
// The checks are cheap and run in the background so that the dashboard
// can render them without blocking a request. The result is a snapshot
// of every dependency with its state, its latency and, when it is
// failing, the reason.
package health

import (
	"context"
	"sort"
	"sync"
	"time"
)

// State is the state of a dependency.
type State string

const (
	// StateHealthy means the dependency is working normally.
	StateHealthy State = "healthy"
	// StateDegraded means the dependency is slow or partially working.
	StateDegraded State = "degraded"
	// StateUnhealthy means the dependency is failing.
	StateUnhealthy State = "unhealthy"
	// StateUnknown means the check has not run yet.
	StateUnknown State = "unknown"
)

// Check is a single dependency check.
type Check struct {
	// Name identifies the check.
	Name string `json:"name"`
	// Description explains what the check covers.
	Description string `json:"description"`
	// Critical marks a dependency whose failure stops Barq from
	// working at all.
	Critical bool `json:"critical"`
	// State is the current state.
	State State `json:"state"`
	// Message explains a failure or a degradation.
	Message string `json:"message,omitempty"`
	// LatencyMS is the duration of the last successful check.
	LatencyMS int64 `json:"latency_ms"`
	// LastChecked is when the check last ran.
	LastChecked time.Time `json:"last_checked"`
	// Failures is the number of consecutive failures.
	Failures int `json:"failures"`
}

// Report is the complete health picture.
type Report struct {
	GeneratedAt time.Time     `json:"generated_at"`
	State       State         `json:"state"`
	Checks      []Check       `json:"checks"`
	Uptime      time.Duration `json:"uptime"`
	Version     string        `json:"version,omitempty"`
	Leader      bool          `json:"leader"`
	Backend     string        `json:"backend"`
	Simulator   bool          `json:"simulator"`
	Healthy     int           `json:"healthy"`
	Degraded    int           `json:"degraded"`
	Unhealthy   int           `json:"unhealthy"`
}

// Probe is a function that performs one check.
type Probe struct {
	Name        string
	Description string
	Critical    bool
	// Timeout bounds the probe.
	Timeout time.Duration
	// Check performs the probe. A nil error means healthy.
	Check func(context.Context) error
}

// Monitor runs probes in the background and caches the results.
type Monitor struct {
	mu        sync.RWMutex
	probes    []Probe
	results   map[string]Check
	started   time.Time
	version   string
	leader    bool
	backend   string
	simulator bool

	stop chan struct{}
	once sync.Once
}

// New creates a monitor.
func New(version string) *Monitor {
	return &Monitor{
		results: make(map[string]Check),
		started: time.Now(),
		version: version,
		stop:    make(chan struct{}),
	}
}

// Register adds a probe.
func (m *Monitor) Register(probe Probe) {
	if probe.Timeout <= 0 {
		probe.Timeout = 5 * time.Second
	}
	m.mu.Lock()
	m.probes = append(m.probes, probe)
	m.mu.Unlock()
}

// SetRuntime records the runtime identity of the controller.
func (m *Monitor) SetRuntime(leader bool, backend string, simulator bool) {
	m.mu.Lock()
	m.leader = leader
	m.backend = backend
	m.simulator = simulator
	m.mu.Unlock()
}

// Start runs the probes on an interval.
func (m *Monitor) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	go func() {
		m.runOnce(ctx)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-m.stop:
				return
			case <-ticker.C:
				m.runOnce(ctx)
			}
		}
	}()
}

// Stop halts the monitor.
func (m *Monitor) Stop() {
	m.once.Do(func() { close(m.stop) })
}

// runOnce executes every probe.
func (m *Monitor) runOnce(ctx context.Context) {
	m.mu.RLock()
	probes := make([]Probe, len(m.probes))
	copy(probes, m.probes)
	m.mu.RUnlock()

	for _, probe := range probes {
		result := m.execute(ctx, probe)
		m.mu.Lock()
		m.results[probe.Name] = result
		m.mu.Unlock()
	}
}

// execute runs a single probe with a timeout.
func (m *Monitor) execute(ctx context.Context, probe Probe) Check {
	result := Check{
		Name:        probe.Name,
		Description: probe.Description,
		Critical:    probe.Critical,
		State:       StateUnknown,
	}
	probeCtx, cancel := context.WithTimeout(ctx, probe.Timeout)
	defer cancel()

	start := time.Now()
	err := probe.Check(probeCtx)
	elapsed := time.Since(start)
	result.LastChecked = time.Now().UTC()
	result.LatencyMS = elapsed.Milliseconds()

	if err == nil {
		result.State = StateHealthy
		result.Message = ""
		result.Failures = 0
		return result
	}
	m.mu.RLock()
	previous := m.results[probe.Name]
	m.mu.RUnlock()
	result.Failures = previous.Failures + 1
	result.Message = err.Error()
	if result.Failures >= 3 {
		result.State = StateUnhealthy
	} else {
		result.State = StateDegraded
	}
	if result.Failures < 3 {
		result.LatencyMS = previous.LatencyMS
	}
	return result
}

// Report returns the current health report.
func (m *Monitor) Report() Report {
	m.mu.RLock()
	defer m.mu.RUnlock()

	report := Report{
		GeneratedAt: time.Now().UTC(),
		State:       StateHealthy,
		Uptime:      time.Since(m.started),
		Version:     m.version,
		Leader:      m.leader,
		Backend:     m.backend,
		Simulator:   m.simulator,
		Checks:      make([]Check, 0, len(m.results)),
	}
	for _, check := range m.results {
		report.Checks = append(report.Checks, check)
		switch check.State {
		case StateHealthy:
			report.Healthy++
		case StateDegraded:
			report.Degraded++
			if check.Critical {
				report.State = StateDegraded
			}
		case StateUnhealthy:
			report.Unhealthy++
			if check.Critical {
				report.State = StateUnhealthy
			}
		}
	}
	sort.Slice(report.Checks, func(i, j int) bool { return report.Checks[i].Name < report.Checks[j].Name })
	return report
}

// State returns the overall state.
func (m *Monitor) State() State { return m.Report().State }
