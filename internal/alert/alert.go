// Package alert delivers notifications about run events.
//
// Alerts are delivered to configured destinations. A destination is a
// generic webhook that receives a JSON payload, which is enough to
// integrate with Slack, Discord, Teams, PagerDuty, Opsgenie and any
// service that accepts a POST. GitHub is integrated natively so that
// a run can be reported as a check on a commit.
//
// Delivery is asynchronous, retried with exponential backoff and never
// blocks the controller.
package alert

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Event is the kind of occurrence that triggered an alert.
type Event string

const (
	// EventRunStarted is emitted when a run begins.
	EventRunStarted Event = "run.started"
	// EventRunSucceeded is emitted when a run succeeds.
	EventRunSucceeded Event = "run.succeeded"
	// EventRunFailed is emitted when a run fails.
	EventRunFailed Event = "run.failed"
	// EventRunCanceled is emitted when a run is canceled.
	EventRunCanceled Event = "run.canceled"
	// EventStepFailed is emitted when an individual step fails.
	EventStepFailed Event = "step.failed"
	// EventStepPreempted is emitted when a worker is preempted.
	EventStepPreempted Event = "step.preempted"
	// EventCacheHit is emitted when a step is served from the cache.
	EventCacheHit Event = "step.cached"
	// EventBuildFailed is the GitHub check conclusion for a failure.
	EventBuildFailed Event = "check.failed"
)

// Severity classifies an alert.
type Severity string

const (
	// SeverityInfo is informational.
	SeverityInfo Severity = "info"
	// SeverityWarning is a recoverable problem.
	SeverityWarning Severity = "warning"
	// SeverityError is a failure.
	SeverityError Severity = "error"
)

// Payload is the JSON body delivered to a webhook.
type Payload struct {
	Event      Event             `json:"event"`
	Severity   Severity          `json:"severity"`
	Timestamp  time.Time         `json:"timestamp"`
	RunID      string            `json:"run_id"`
	RunName    string            `json:"run_name"`
	StepID     string            `json:"step_id,omitempty"`
	Status     string            `json:"status,omitempty"`
	DurationMS int64             `json:"duration_ms,omitempty"`
	Source     string            `json:"source,omitempty"`
	Commit     string            `json:"commit,omitempty"`
	URL        string            `json:"url,omitempty"`
	Message    string            `json:"message,omitempty"`
	Details    map[string]string `json:"details,omitempty"`
}

// Destination is a webhook that receives alerts.
type Destination struct {
	// ID identifies the destination.
	ID string `json:"id"`
	// Name is a human readable label.
	Name string `json:"name"`
	// URL receives the POST.
	URL string `json:"url"`
	// Secret signs the payload. The signature is sent in the
	// X-Barq-Signature header.
	Secret string `json:"secret,omitempty"`
	// Events restricts which events this destination receives. An
	// empty list receives everything.
	Events []Event `json:"events,omitempty"`
	// Severity restricts which severities this destination receives.
	Severities []Severity `json:"severities,omitempty"`
	// Enabled turns the destination on or off.
	Enabled bool `json:"enabled"`
	// Timeout bounds a single delivery.
	Timeout time.Duration `json:"timeout,omitempty"`
}

// GitHubConfig configures the native GitHub integration.
type GitHubConfig struct {
	// Enabled turns the integration on.
	Enabled bool
	// Token authenticates with the GitHub API.
	Token string
	// APIBase is the GitHub API base URL.
	APIBase string
	// Owner and Repository identify the repository.
	Owner      string
	Repository string
}

// Manager delivers alerts.
type Manager struct {
	mu           sync.RWMutex
	destinations map[string]*Destination
	github       GitHubConfig
	http         *http.Client
	logger       Logger
	queue        chan job
	stop         chan struct{}
	once         sync.Once
	delivered    int64
	failed       int64
}

// Logger is the minimal logging interface the manager needs.
type Logger interface {
	Warn(msg string, args ...any)
	Info(msg string, args ...any)
}

// job is a queued delivery.
type job struct {
	destination *Destination
	payload     Payload
	attempt     int
}

// NewManager creates an alert manager.
func NewManager(logger Logger) *Manager {
	manager := &Manager{
		destinations: make(map[string]*Destination),
		http:         &http.Client{Timeout: 15 * time.Second},
		logger:       logger,
		queue:        make(chan job, 1024),
		stop:         make(chan struct{}),
	}
	return manager
}

// SetGitHub configures the native GitHub integration.
func (m *Manager) SetGitHub(config GitHubConfig) {
	if config.APIBase == "" {
		config.APIBase = "https://api.github.com"
	}
	m.mu.Lock()
	m.github = config
	m.mu.Unlock()
}

// Add registers a destination.
func (m *Manager) Add(destination Destination) {
	if destination.ID == "" {
		destination.ID = destination.Name
	}
	if destination.Timeout <= 0 {
		destination.Timeout = 15 * time.Second
	}
	m.mu.Lock()
	m.destinations[destination.ID] = &destination
	m.mu.Unlock()
}

// Remove deletes a destination.
func (m *Manager) Remove(id string) {
	m.mu.Lock()
	delete(m.destinations, id)
	m.mu.Unlock()
}

// List returns the configured destinations.
func (m *Manager) List() []Destination {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]Destination, 0, len(m.destinations))
	for _, destination := range m.destinations {
		result = append(result, *destination)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

// Start begins the delivery workers.
func (m *Manager) Start(ctx context.Context, workers int) {
	if workers <= 0 {
		workers = 4
	}
	for index := 0; index < workers; index++ {
		go m.worker(ctx)
	}
}

// Stop halts the manager.
func (m *Manager) Stop() {
	m.once.Do(func() { close(m.stop) })
}

// worker processes the delivery queue.
func (m *Manager) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stop:
			return
		case item := <-m.queue:
			m.deliver(ctx, item)
		}
	}
}

// Publish queues an alert for every matching destination.
func (m *Manager) Publish(ctx context.Context, payload Payload) {
	if payload.Timestamp.IsZero() {
		payload.Timestamp = time.Now().UTC()
	}
	m.mu.RLock()
	targets := make([]*Destination, 0, len(m.destinations))
	for _, destination := range m.destinations {
		if !destination.Enabled {
			continue
		}
		if !matches(destination, payload) {
			continue
		}
		targets = append(targets, destination)
	}
	m.mu.RUnlock()

	for _, destination := range targets {
		select {
		case m.queue <- job{destination: destination, payload: payload}:
		default:
			// The queue is full. Dropping the alert is better than
			// blocking the controller.
			if m.logger != nil {
				m.logger.Warn("alert queue is full, dropping alert", "destination", destination.Name, "event", payload.Event)
			}
		}
	}
}

// matches reports whether a destination wants an event.
func matches(destination *Destination, payload Payload) bool {
	if len(destination.Events) > 0 {
		found := false
		for _, event := range destination.Events {
			if event == payload.Event {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(destination.Severities) > 0 {
		found := false
		for _, severity := range destination.Severities {
			if severity == payload.Severity {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// deliver performs a single delivery with retries.
func (m *Manager) deliver(ctx context.Context, item job) {
	const maxAttempts = 4
	for attempt := item.attempt; attempt < maxAttempts; attempt++ {
		err := m.post(ctx, item.destination, item.payload)
		if err == nil {
			m.mu.Lock()
			m.delivered++
			m.mu.Unlock()
			return
		}
		if m.logger != nil {
			m.logger.Warn("alert delivery failed", "destination", item.destination.Name, "attempt", attempt+1, "error", err)
		}
		// Exponential backoff: 1s, 2s, 4s.
		backoff := time.Duration(1<<attempt) * time.Second
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
	m.mu.Lock()
	m.failed++
	m.mu.Unlock()
}

// post sends the payload to a destination.
func (m *Manager) post(ctx context.Context, destination *Destination, payload Payload) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	requestCtx, cancel := context.WithTimeout(ctx, destination.Timeout)
	defer cancel()

	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, destination.URL, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "barq-alert/1")
	if destination.Secret != "" {
		mac := hmac.New(sha256.New, []byte(destination.Secret))
		mac.Write(encoded)
		request.Header.Set("X-Barq-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	response, err := m.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("the destination returned status %d", response.StatusCode)
	}
	return nil
}

// ReportGitHubCheck publishes a commit status to GitHub.
//
// This is the native GitHub integration. A run that knows its commit
// reports itself as a check on that commit, so the commit shows the
// pipeline result directly in the pull request.
func (m *Manager) ReportGitHubCheck(ctx context.Context, commit string, payload Payload) error {
	m.mu.RLock()
	config := m.github
	m.mu.RUnlock()
	if !config.Enabled || config.Token == "" || commit == "" {
		return nil
	}

	state := "pending"
	var description string
	switch payload.Event {
	case EventRunSucceeded:
		state = "success"
		description = payload.Message
	case EventRunFailed:
		state = "failure"
		description = payload.Message
	case EventRunCanceled:
		state = "error"
		description = payload.Message
	default:
		state = "in_progress"
		description = payload.Message
	}
	if description == "" {
		description = string(payload.Event)
	}
	if len(description) > 140 {
		description = description[:140]
	}

	body := map[string]any{
		"state":       state,
		"description": description,
		"context":     "barq/" + payload.RunName,
		"target_url":  payload.URL,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/statuses/%s",
		strings.TrimRight(config.APIBase, "/"), config.Owner, config.Repository, commit)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+config.Token)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	response, err := m.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		content, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("github returned status %d: %s", response.StatusCode, strings.TrimSpace(string(content)))
	}
	return nil
}

// Stats returns the delivery counters.
func (m *Manager) Stats() (delivered, failed int64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.delivered, m.failed
}
