package metrics

import (
	"strings"
	"testing"
)

func TestCounterRendersInTheExpositionFormat(t *testing.T) {
	registry := NewRegistry()
	counter := registry.NewCounter("barq_test_total", "A test counter.")
	counter.Inc(nil)
	counter.Add(2, map[string]string{"status": "ok"})

	output := registry.Gather()
	if !strings.Contains(output, "# HELP barq_test_total A test counter.") {
		t.Fatalf("the help line is missing:\n%s", output)
	}
	if !strings.Contains(output, "# TYPE barq_test_total counter") {
		t.Fatalf("the type line is missing:\n%s", output)
	}
	if !strings.Contains(output, "barq_test_total 1") {
		t.Fatalf("the unlabelled value is missing:\n%s", output)
	}
	if !strings.Contains(output, `barq_test_total{status="ok"} 2`) {
		t.Fatalf("the labelled value is missing:\n%s", output)
	}
}

func TestGaugeRendersInTheExpositionFormat(t *testing.T) {
	registry := NewRegistry()
	gauge := registry.NewGauge("barq_test_depth", "A test gauge.")
	gauge.Set(7, nil)
	gauge.Add(-2, nil)

	output := registry.Gather()
	if !strings.Contains(output, "barq_test_depth 5") {
		t.Fatalf("the value is missing:\n%s", output)
	}
}

func TestHistogramRendersBuckets(t *testing.T) {
	registry := NewRegistry()
	histogram := registry.NewHistogram("barq_test_seconds", "A test histogram.", []float64{0.1, 1, 10})
	histogram.Observe(0.05, nil)
	histogram.Observe(5, nil)

	output := registry.Gather()
	if !strings.Contains(output, "# TYPE barq_test_seconds histogram") {
		t.Fatalf("the type line is missing:\n%s", output)
	}
	if !strings.Contains(output, `barq_test_seconds_bucket{le="0.1"} 1`) {
		t.Fatalf("the first bucket is missing:\n%s", output)
	}
	if !strings.Contains(output, `barq_test_seconds_bucket{le="1"} 1`) {
		t.Fatalf("the second bucket is missing:\n%s", output)
	}
	if !strings.Contains(output, `barq_test_seconds_bucket{le="10"} 2`) {
		t.Fatalf("the third bucket is missing:\n%s", output)
	}
	if !strings.Contains(output, `barq_test_seconds_bucket{le="+Inf"} 2`) {
		t.Fatalf("the infinity bucket is missing:\n%s", output)
	}
	if !strings.Contains(output, "barq_test_seconds_count 2") {
		t.Fatalf("the count is missing:\n%s", output)
	}
	if !strings.Contains(output, "barq_test_seconds_sum 5.05") {
		t.Fatalf("the sum is missing:\n%s", output)
	}
}

func TestLabelValuesAreEscaped(t *testing.T) {
	registry := NewRegistry()
	counter := registry.NewCounter("barq_test_total", "A test counter.")
	counter.Inc(map[string]string{"message": `a "quoted" value`})

	output := registry.Gather()
	if !strings.Contains(output, `barq_test_total{message="a \"quoted\" value"} 1`) {
		t.Fatalf("the label was not escaped:\n%s", output)
	}
}

func TestMetricsAreDeterministic(t *testing.T) {
	build := func() string {
		registry := NewRegistry()
		counter := registry.NewCounter("barq_test_total", "A test counter.")
		for _, label := range []string{"c", "a", "b"} {
			counter.Inc(map[string]string{"key": label})
		}
		return registry.Gather()
	}
	if build() != build() {
		t.Fatal("the exposition output must be deterministic")
	}
}
