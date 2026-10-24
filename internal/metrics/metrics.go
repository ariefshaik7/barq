// Package metrics implements a small Prometheus compatible metrics
// registry.
//
// The registry is deliberately minimal. It supports counters, gauges
// and histograms with labels, and it renders the text exposition
// format. There is no dependency on a client library, which keeps the
// binary small and the behaviour predictable.
//
// The exposition format is the one Prometheus scrapes. A scrape
// endpoint is what an operator needs, and it costs a few kilobytes of
// code.
package metrics

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Registry holds a set of metrics.
type Registry struct {
	mu       sync.RWMutex
	counters map[string]*counter
	gauges   map[string]*gauge
	histos   map[string]*histogram
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		counters: make(map[string]*counter),
		gauges:   make(map[string]*gauge),
		histos:   make(map[string]*histogram),
	}
}

// counter is a monotonically increasing value.
type counter struct {
	name   string
	help   string
	mu     sync.Mutex
	values map[string]float64
	order  []string
}

// gauge is a value that can go up and down.
type gauge struct {
	name   string
	help   string
	mu     sync.Mutex
	values map[string]float64
	order  []string
}

// histogram records a distribution of observations.
type histogram struct {
	name    string
	help    string
	buckets []float64
	mu      sync.Mutex
	counts  map[string][]uint64
	sums    map[string]float64
	totals  map[string]uint64
	order   []string
}

// NewCounter registers a counter.
func (r *Registry) NewCounter(name, help string) *Counter {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.counters[name]; !exists {
		r.counters[name] = &counter{
			name:   name,
			help:   help,
			values: make(map[string]float64),
		}
	}
	return &Counter{registry: r, name: name}
}

// NewGauge registers a gauge.
func (r *Registry) NewGauge(name, help string) *Gauge {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.gauges[name]; !exists {
		r.gauges[name] = &gauge{
			name:   name,
			help:   help,
			values: make(map[string]float64),
		}
	}
	return &Gauge{registry: r, name: name}
}

// NewHistogram registers a histogram.
func (r *Registry) NewHistogram(name, help string, buckets []float64) *Histogram {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.histos[name]; !exists {
		sorted := append([]float64(nil), buckets...)
		sort.Float64s(sorted)
		r.histos[name] = &histogram{
			name:    name,
			help:    help,
			buckets: sorted,
			counts:  make(map[string][]uint64),
			sums:    make(map[string]float64),
			totals:  make(map[string]uint64),
		}
	}
	return &Histogram{registry: r, name: name}
}

// Counter is a handle to a registered counter.
type Counter struct {
	registry *Registry
	name     string
}

// Inc increments the counter by one.
func (c *Counter) Inc(labels map[string]string) { c.Add(1, labels) }

// Add increments the counter.
func (c *Counter) Add(value float64, labels map[string]string) {
	c.registry.mu.RLock()
	metric, ok := c.registry.counters[c.name]
	c.registry.mu.RUnlock()
	if !ok {
		return
	}
	key := encodeLabels(labels)
	metric.mu.Lock()
	defer metric.mu.Unlock()
	if _, exists := metric.values[key]; !exists {
		metric.order = append(metric.order, key)
	}
	metric.values[key] += value
}

// Gauge is a handle to a registered gauge.
type Gauge struct {
	registry *Registry
	name     string
}

// Set replaces the value of the gauge.
func (g *Gauge) Set(value float64, labels map[string]string) {
	g.registry.mu.RLock()
	metric, ok := g.registry.gauges[g.name]
	g.registry.mu.RUnlock()
	if !ok {
		return
	}
	key := encodeLabels(labels)
	metric.mu.Lock()
	defer metric.mu.Unlock()
	if _, exists := metric.values[key]; !exists {
		metric.order = append(metric.order, key)
	}
	metric.values[key] = value
}

// Add adds to the value of the gauge.
func (g *Gauge) Add(value float64, labels map[string]string) {
	g.registry.mu.RLock()
	metric, ok := g.registry.gauges[g.name]
	g.registry.mu.RUnlock()
	if !ok {
		return
	}
	key := encodeLabels(labels)
	metric.mu.Lock()
	defer metric.mu.Unlock()
	if _, exists := metric.values[key]; !exists {
		metric.order = append(metric.order, key)
	}
	metric.values[key] += value
}

// Histogram is a handle to a registered histogram.
type Histogram struct {
	registry *Registry
	name     string
}

// Observe records an observation.
func (h *Histogram) Observe(value float64, labels map[string]string) {
	h.registry.mu.RLock()
	metric, ok := h.registry.histos[h.name]
	h.registry.mu.RUnlock()
	if !ok {
		return
	}
	key := encodeLabels(labels)
	metric.mu.Lock()
	defer metric.mu.Unlock()
	if _, exists := metric.counts[key]; !exists {
		metric.counts[key] = make([]uint64, len(metric.buckets))
		metric.order = append(metric.order, key)
	}
	for index, bound := range metric.buckets {
		if value <= bound {
			metric.counts[key][index]++
		}
	}
	metric.sums[key] += value
	metric.totals[key]++
}

// Gather renders the registry in the Prometheus text exposition
// format.
func (r *Registry) Gather() string {
	r.mu.RLock()
	counters := make([]*counter, 0, len(r.counters))
	for _, metric := range r.counters {
		counters = append(counters, metric)
	}
	gauges := make([]*gauge, 0, len(r.gauges))
	for _, metric := range r.gauges {
		gauges = append(gauges, metric)
	}
	histograms := make([]*histogram, 0, len(r.histos))
	for _, metric := range r.histos {
		histograms = append(histograms, metric)
	}
	r.mu.RUnlock()

	sort.Slice(counters, func(i, j int) bool { return counters[i].name < counters[j].name })
	sort.Slice(gauges, func(i, j int) bool { return gauges[i].name < gauges[j].name })
	sort.Slice(histograms, func(i, j int) bool { return histograms[i].name < histograms[j].name })

	var builder strings.Builder
	for _, metric := range counters {
		metric.mu.Lock()
		fmt.Fprintf(&builder, "# HELP %s %s\n# TYPE %s counter\n", metric.name, metric.help, metric.name)
		for _, key := range metric.order {
			fmt.Fprintf(&builder, "%s%s %s\n", metric.name, renderLabels(key), formatValue(metric.values[key]))
		}
		metric.mu.Unlock()
	}
	for _, metric := range gauges {
		metric.mu.Lock()
		fmt.Fprintf(&builder, "# HELP %s %s\n# TYPE %s gauge\n", metric.name, metric.help, metric.name)
		for _, key := range metric.order {
			fmt.Fprintf(&builder, "%s%s %s\n", metric.name, renderLabels(key), formatValue(metric.values[key]))
		}
		metric.mu.Unlock()
	}
	for _, metric := range histograms {
		metric.mu.Lock()
		fmt.Fprintf(&builder, "# HELP %s %s\n# TYPE %s histogram\n", metric.name, metric.help, metric.name)
		for _, key := range metric.order {
			counts := metric.counts[key]
			for index, bound := range metric.buckets {
				labels := decodeLabels(key)
				labels["le"] = formatValue(bound)
				fmt.Fprintf(&builder, "%s_bucket%s %d\n", metric.name, renderLabels(encodeLabels(labels)), counts[index])
			}
			labels := decodeLabels(key)
			labels["le"] = "+Inf"
			fmt.Fprintf(&builder, "%s_bucket%s %d\n", metric.name, renderLabels(encodeLabels(labels)), metric.totals[key])
			fmt.Fprintf(&builder, "%s_sum%s %s\n", metric.name, renderLabels(key), formatValue(metric.sums[key]))
			fmt.Fprintf(&builder, "%s_count%s %d\n", metric.name, renderLabels(key), metric.totals[key])
		}
		metric.mu.Unlock()
	}
	return builder.String()
}

// encodeLabels renders a label set into a stable key.
func encodeLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var builder strings.Builder
	for _, key := range keys {
		builder.WriteString(key)
		builder.WriteByte('=')
		builder.WriteString(labels[key])
		builder.WriteByte(0)
	}
	return builder.String()
}

// decodeLabels parses a label key back into a map.
func decodeLabels(key string) map[string]string {
	labels := make(map[string]string)
	if key == "" {
		return labels
	}
	for _, part := range strings.Split(key, "\x00") {
		if part == "" {
			continue
		}
		index := strings.Index(part, "=")
		if index < 0 {
			continue
		}
		labels[part[:index]] = part[index+1:]
	}
	return labels
}

// renderLabels renders a label key as Prometheus label text.
func renderLabels(key string) string {
	labels := decodeLabels(key)
	if len(labels) == 0 {
		return ""
	}
	names := make([]string, 0, len(labels))
	for name := range labels {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		// The %q verb performs the escaping that the exposition
		// format requires: quotes, backslashes and control
		// characters.
		parts = append(parts, fmt.Sprintf("%s=%q", name, labels[name]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// formatValue renders a float in the exposition format.
func formatValue(value float64) string {
	if math.IsInf(value, 1) {
		return "+Inf"
	}
	if math.IsInf(value, -1) {
		return "-Inf"
	}
	if value == math.Trunc(value) && math.Abs(value) < 1e15 {
		return strconv.FormatInt(int64(value), 10)
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}
