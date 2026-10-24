package alert

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// testLogger records log calls.
type testLogger struct {
	mu    sync.Mutex
	warns []string
}

func (l *testLogger) Warn(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg)
}

func (l *testLogger) Info(msg string, args ...any) {}

// captureServer records the payloads it receives.
type captureServer struct {
	mu       sync.Mutex
	payloads []Payload
	servers  []string
	status   int
	server   *httptest.Server
}

func newCaptureServer(t *testing.T) *captureServer {
	capture := &captureServer{status: http.StatusOK}
	capture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload Payload
		_ = json.Unmarshal(body, &payload)
		capture.mu.Lock()
		capture.payloads = append(capture.payloads, payload)
		capture.servers = append(capture.servers, r.Header.Get("X-Barq-Signature"))
		capture.mu.Unlock()
		w.WriteHeader(capture.status)
	}))
	t.Cleanup(capture.server.Close)
	return capture
}

func (c *captureServer) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.payloads)
}

func (c *captureServer) first() Payload {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.payloads) == 0 {
		return Payload{}
	}
	return c.payloads[0]
}

func TestPublishDeliversToAMatchingDestination(t *testing.T) {
	capture := newCaptureServer(t)
	logger := &testLogger{}
	manager := NewManager(logger)
	manager.Add(Destination{ID: "a", Name: "a", URL: capture.server.URL, Enabled: true})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.Start(ctx, 1)

	manager.Publish(ctx, Payload{Event: EventRunFailed, Severity: SeverityError, RunID: "r1"})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && capture.count() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if capture.count() != 1 {
		t.Fatalf("expected 1 delivery, got %d", capture.count())
	}
	if capture.first().Event != EventRunFailed {
		t.Fatalf("unexpected payload: %+v", capture.first())
	}
}

func TestPublishRespectsTheEventFilter(t *testing.T) {
	capture := newCaptureServer(t)
	manager := NewManager(&testLogger{})
	manager.Add(Destination{
		ID: "a", Name: "a", URL: capture.server.URL, Enabled: true,
		Events: []Event{EventRunSucceeded},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.Start(ctx, 1)

	manager.Publish(ctx, Payload{Event: EventRunFailed, Severity: SeverityError})
	time.Sleep(300 * time.Millisecond)
	if capture.count() != 0 {
		t.Fatalf("expected no delivery, got %d", capture.count())
	}

	manager.Publish(ctx, Payload{Event: EventRunSucceeded, Severity: SeverityInfo})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && capture.count() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if capture.count() != 1 {
		t.Fatalf("expected 1 delivery, got %d", capture.count())
	}
}

func TestPublishRespectsTheSeverityFilter(t *testing.T) {
	capture := newCaptureServer(t)
	manager := NewManager(&testLogger{})
	manager.Add(Destination{
		ID: "a", Name: "a", URL: capture.server.URL, Enabled: true,
		Severities: []Severity{SeverityError},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.Start(ctx, 1)

	manager.Publish(ctx, Payload{Event: EventRunSucceeded, Severity: SeverityInfo})
	time.Sleep(300 * time.Millisecond)
	if capture.count() != 0 {
		t.Fatalf("expected no delivery, got %d", capture.count())
	}
}

func TestDisabledDestinationReceivesNothing(t *testing.T) {
	capture := newCaptureServer(t)
	manager := NewManager(&testLogger{})
	manager.Add(Destination{ID: "a", Name: "a", URL: capture.server.URL, Enabled: false})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.Start(ctx, 1)

	manager.Publish(ctx, Payload{Event: EventRunFailed, Severity: SeverityError})
	time.Sleep(300 * time.Millisecond)
	if capture.count() != 0 {
		t.Fatalf("expected no delivery, got %d", capture.count())
	}
}

func TestDeliveryIsSigned(t *testing.T) {
	capture := newCaptureServer(t)
	manager := NewManager(&testLogger{})
	manager.Add(Destination{
		ID: "a", Name: "a", URL: capture.server.URL, Enabled: true, Secret: "shared-secret",
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.Start(ctx, 1)

	manager.Publish(ctx, Payload{Event: EventRunFailed, Severity: SeverityError})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && capture.count() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	capture.mu.Lock()
	signature := ""
	if len(capture.servers) > 0 {
		signature = capture.servers[0]
	}
	capture.mu.Unlock()
	if !strings.HasPrefix(signature, "sha256=") {
		t.Fatalf("expected a signed payload, got %q", signature)
	}
}

func TestDeliveryRetriesOnFailure(t *testing.T) {
	attempts := 0
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	manager := NewManager(&testLogger{})
	manager.Add(Destination{ID: "a", Name: "a", URL: server.URL, Enabled: true, Timeout: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.Start(ctx, 1)

	manager.Publish(ctx, Payload{Event: EventRunFailed, Severity: SeverityError})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := attempts
		mu.Unlock()
		if count > 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("expected the delivery to be retried")
}

func TestListAndRemove(t *testing.T) {
	manager := NewManager(&testLogger{})
	manager.Add(Destination{ID: "a", Name: "Alpha", URL: "https://example.com", Enabled: true})
	manager.Add(Destination{ID: "b", Name: "Beta", URL: "https://example.org", Enabled: true})

	destinations := manager.List()
	if len(destinations) != 2 {
		t.Fatalf("expected 2 destinations, got %d", len(destinations))
	}
	if destinations[0].Name != "Alpha" {
		t.Fatalf("expected the destinations to be sorted, got %+v", destinations)
	}
	manager.Remove("a")
	if len(manager.List()) != 1 {
		t.Fatal("expected the destination to be removed")
	}
}
