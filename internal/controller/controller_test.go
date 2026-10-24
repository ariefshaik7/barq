package controller

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"barq/internal/auth"
	"barq/internal/cache"
	"barq/internal/dag"
	"barq/internal/forge"
	"barq/internal/k8s"
	"barq/internal/queue"
	"barq/internal/storage"
	"barq/internal/yaml"
)

// newTestEngine builds an engine backed by the simulator.
func newTestEngine(t *testing.T) (*Engine, *k8s.Simulator) {
	t.Helper()
	// The engine writes to the object store from background goroutines,
	// so the directory is created outside the test temp dir and removed
	// explicitly after the engine has stopped.
	root, err := os.MkdirTemp("", "barq-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Give the background goroutines a moment to finish writing.
		time.Sleep(200 * time.Millisecond)
		_ = os.RemoveAll(root)
	})

	store, err := storage.NewLocal(filepath.Join(root, "objects"), "http://example.invalid", []byte("0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	tiered, err := storage.NewTiered(store, "barq", filepath.Join(root, "hot"), storage.NewSecretMasker(nil), storage.TieredOptions{})
	if err != nil {
		t.Fatal(err)
	}
	manager := cache.NewManager(store, "barq")
	backend, err := k8s.NewSimulator(filepath.Join(root, "sim"))
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	queueCtx, queueCancel := context.WithCancel(context.Background())
	t.Cleanup(queueCancel)
	taskQueue := queue.New(queueCtx, queue.Options{
		Extension:       time.Second,
		ProcessInterval: 5 * time.Millisecond,
	})

	engine := New(Config{
		Identity:       "test-identity",
		Namespace:      "default",
		LeaseName:      "barq-leader",
		ObjectPrefix:   "barq",
		MaxAttempts:    3,
		PollInterval:   50 * time.Millisecond,
		LeaseDuration:  5 * time.Second,
		LeaseRenewTime: time.Second,
	}, backend, tiered, manager, nil, logger, dag.Options{
		PluginRuntimeImage: "barq/plugin-runtime:1.0.0",
	}, Options{Queue: taskQueue})
	return engine, backend
}

// startEngine runs the engine in the background for the duration of a
// test and elects it as the leader.
func startEngine(t *testing.T, engine *Engine, backend *k8s.Simulator) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	duration := int32(30)
	if err := backend.CreateLease(ctx, k8s.Lease{
		Metadata: k8s.ObjectMeta{Name: engine.config.LeaseName},
		Spec: k8s.LeaseSpec{
			HolderIdentity:       "other-replica",
			LeaseDurationSeconds: &duration,
			RenewTime:            time.Now().UTC().Add(-time.Hour).Format(k8s.K8sMicroTimeLayout),
		},
	}); err != nil {
		t.Fatal(err)
	}
	engine.campaign(ctx)
	if !engine.IsLeader() {
		t.Fatal("expected the engine to become the leader")
	}
	engine.Run(ctx)
}

func TestCreateRunCompilesAndPersists(t *testing.T) {
	engine, _ := newTestEngine(t)
	ctx := context.Background()

	spec, err := yaml.Parse([]byte(`
name: demo
steps:
  - id: build
    image: alpine
    run: echo build
`))
	if err != nil {
		t.Fatal(err)
	}
	state, err := engine.CreateRun(ctx, spec, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	if state.Name != "demo" {
		t.Fatalf("unexpected run name: %q", state.Name)
	}
	if len(state.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(state.Steps))
	}
	// The state must be durable in the object store.
	var reloaded RunState
	if err := engine.tiered.LoadState(ctx, state.ID, &reloaded); err != nil {
		t.Fatal(err)
	}
	if reloaded.Name != "demo" {
		t.Fatalf("unexpected reloaded state: %+v", reloaded)
	}
}

func TestCreateRunRejectsInvalidPipeline(t *testing.T) {
	engine, _ := newTestEngine(t)
	if _, err := engine.CreateRun(context.Background(), yaml.PipelineSpec{}, "test", ""); err == nil {
		t.Fatal("expected an error for an invalid pipeline")
	}
}

func TestRunExecutesStepsAndFinalizes(t *testing.T) {
	engine, backend := newTestEngine(t)
	startEngine(t, engine, backend)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	spec, err := yaml.Parse([]byte(`
name: demo
steps:
  - id: build
    image: alpine
    run: echo building
  - id: test
    image: alpine
    depends_on: [build]
    run: echo testing
`))
	if err != nil {
		t.Fatal(err)
	}
	state, err := engine.CreateRun(ctx, spec, "test", "")
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		current, ok := engine.GetRun(ctx, state.ID)
		if ok && current.IsTerminal() {
			if current.Status != RunSuccess {
				t.Fatalf("expected success, got %s: %s", current.Status, current.Message)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the run did not reach a terminal state")
}

func TestFailedStepFailsTheRun(t *testing.T) {
	engine, backend := newTestEngine(t)
	startEngine(t, engine, backend)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	spec, err := yaml.Parse([]byte(`
name: demo
steps:
  - id: build
    image: alpine
    run: exit 3
`))
	if err != nil {
		t.Fatal(err)
	}
	state, err := engine.CreateRun(ctx, spec, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		current, ok := engine.GetRun(ctx, state.ID)
		if ok && current.IsTerminal() {
			if current.Status != RunFailed {
				t.Fatalf("expected failure, got %s", current.Status)
			}
			if current.Steps["build"].Status != StepFailed {
				t.Fatalf("expected the step to fail, got %s", current.Steps["build"].Status)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the run did not reach a terminal state")
}

func TestSpotPreemptionReschedulesTheStep(t *testing.T) {
	engine, backend := newTestEngine(t)
	startEngine(t, engine, backend)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	spec, err := yaml.Parse([]byte(`
name: demo
steps:
  - id: build
    image: alpine
    run: SIMULATE_SPOT_PREEMPT
`))
	if err != nil {
		t.Fatal(err)
	}
	state, err := engine.CreateRun(ctx, spec, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		current, ok := engine.GetRun(ctx, state.ID)
		if ok && current.IsTerminal() {
			// The first attempt is preempted, the second succeeds.
			if current.Status != RunSuccess {
				t.Fatalf("expected the retry to succeed, got %s: %s", current.Status, current.Message)
			}
			if current.Steps["build"].Attempt < 1 {
				t.Fatalf("expected at least one retry, got attempt %d", current.Steps["build"].Attempt)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the run did not reach a terminal state")
}

func TestStopRunCancelsTheRun(t *testing.T) {
	engine, backend := newTestEngine(t)
	startEngine(t, engine, backend)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	spec, err := yaml.Parse([]byte(`
name: demo
steps:
  - id: build
    image: alpine
    run: sleep 60
`))
	if err != nil {
		t.Fatal(err)
	}
	state, err := engine.CreateRun(ctx, spec, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := engine.StopRun(ctx, state.ID); err != nil {
		t.Fatal(err)
	}
	current, _ := engine.GetRun(ctx, state.ID)
	if current.Status != RunCanceled {
		t.Fatalf("expected the run to be canceled, got %s", current.Status)
	}
}

func TestRebuildFailedKeepsSuccessfulUpstream(t *testing.T) {
	engine, backend := newTestEngine(t)
	startEngine(t, engine, backend)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	spec, err := yaml.Parse([]byte(`
name: demo
steps:
  - id: build
    image: alpine
    run: echo build
  - id: test
    image: alpine
    depends_on: [build]
    run: exit 4
`))
	if err != nil {
		t.Fatal(err)
	}
	state, err := engine.CreateRun(ctx, spec, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		current, ok := engine.GetRun(ctx, state.ID)
		if ok && current.IsTerminal() {
			rebuilt, err := engine.Rebuild(ctx, state.ID, RebuildRequest{Mode: RebuildFailed})
			if err != nil {
				t.Fatal(err)
			}
			if rebuilt.Steps["build"].Status != StepSuccess {
				t.Fatalf("expected the upstream step to be preserved, got %s", rebuilt.Steps["build"].Status)
			}
			if rebuilt.Steps["test"].Status != StepQueued {
				t.Fatalf("expected the failed step to be requeued, got %s", rebuilt.Steps["test"].Status)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the run did not reach a terminal state")
}

func TestGitHubSignatureVerification(t *testing.T) {
	// The signature check lives in the forge layer, because that is
	// where the provider contract is defined.
	body := []byte(`{"ref":"refs/heads/main"}`)
	secret := "webhook-secret"
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	header := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	provider := forge.NewGitHub(forge.Config{WebhookSecret: secret})
	if !provider.VerifySignature(body, header) {
		t.Fatal("expected the signature to verify")
	}
	other := forge.NewGitHub(forge.Config{WebhookSecret: "other-secret"})
	if other.VerifySignature(body, header) {
		t.Fatal("expected a wrong secret to fail")
	}
	if provider.VerifySignature([]byte("tampered"), header) {
		t.Fatal("expected a tampered body to fail")
	}
	if provider.VerifySignature(body, "sha256=nothex") {
		t.Fatal("expected a malformed signature to fail")
	}
}

func TestRebuildStepPreservesTheUpstreamSteps(t *testing.T) {
	// A targeted rebuild re-executes the selected step and its
	// descendants. The upstream steps keep their result, so the work
	// that already succeeded is not repeated.
	engine, backend := newTestEngine(t)
	startEngine(t, engine, backend)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	spec, err := yaml.Parse([]byte(`
name: demo
steps:
  - id: build
    image: alpine
    run: echo build
  - id: test
    image: alpine
    depends_on: [build]
    run: echo test
  - id: report
    image: alpine
    depends_on: [test]
    run: echo report
`))
	if err != nil {
		t.Fatal(err)
	}
	state, err := engine.CreateRun(ctx, spec, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		current, ok := engine.GetRun(ctx, state.ID)
		if ok && current.IsTerminal() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	rebuilt, err := engine.Rebuild(ctx, state.ID, RebuildRequest{
		Mode:   RebuildStep,
		StepID: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The upstream step is preserved.
	if rebuilt.Steps["build"].Status != StepSuccess {
		t.Fatalf("expected the upstream step to be preserved, got %q", rebuilt.Steps["build"].Status)
	}
	// The selected step and its descendant re-execute.
	if rebuilt.Steps["test"].Status != StepQueued {
		t.Fatalf("expected the selected step to be requeued, got %q", rebuilt.Steps["test"].Status)
	}
	if rebuilt.Steps["report"].Status != StepQueued {
		t.Fatalf("expected the descendant to be requeued, got %q", rebuilt.Steps["report"].Status)
	}
}

func TestRebuildWavePreservesTheEarlierWaves(t *testing.T) {
	engine, backend := newTestEngine(t)
	startEngine(t, engine, backend)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	spec, err := yaml.Parse([]byte(`
name: demo
steps:
  - id: build
    image: alpine
    run: echo build
  - id: test
    image: alpine
    depends_on: [build]
    run: echo test
`))
	if err != nil {
		t.Fatal(err)
	}
	state, err := engine.CreateRun(ctx, spec, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		current, ok := engine.GetRun(ctx, state.ID)
		if ok && current.IsTerminal() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	rebuilt, err := engine.Rebuild(ctx, state.ID, RebuildRequest{
		Mode: RebuildWave,
		Wave: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Steps["build"].Status != StepSuccess {
		t.Fatalf("expected wave 0 to be preserved, got %q", rebuilt.Steps["build"].Status)
	}
	if rebuilt.Steps["test"].Status != StepQueued {
		t.Fatalf("expected wave 1 to be requeued, got %q", rebuilt.Steps["test"].Status)
	}
}

func TestHandlerRequiresAuthentication(t *testing.T) {
	engine, backend := newTestEngine(t)
	startEngine(t, engine, backend)

	// With no authenticator configured the API is open, which is the
	// local and simulator behaviour.
	handler := engine.Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200 without an authenticator, got %d", recorder.Code)
	}

	// With an authenticator the API requires a session.
	provider, err := auth.New(auth.Config{
		Store:       engine.tiered.Store(),
		Prefix:      "barq",
		AllowSignup: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	engine.Auth = provider
	handler = engine.Handler()

	request = httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a session, got %d", recorder.Code)
	}

	// A valid session is accepted. The session is presented as a
	// cookie, because a bearer token is a different credential type
	// with its own scope model.
	if _, err := provider.Signup(context.Background(), "test@example.com", "correct horse battery staple", "Tester"); err != nil {
		t.Fatal(err)
	}
	_, session, err := provider.Signin(context.Background(), "test@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
	request.AddCookie(&http.Cookie{Name: "barq_session", Value: session.ID})
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200 with a valid session, got %d: %s", recorder.Code, recorder.Body.String())
	}

	// A bearer token is verified against the token store, not the
	// session table.
	engine.Tokens = auth.NewAPITokenStore([]byte("test"), func(string) (*auth.User, error) {
		return nil, auth.ErrUserNotFound
	})
	request = httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
	request.Header.Set("Authorization", "Bearer not-a-real-token")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an invalid bearer token, got %d", recorder.Code)
	}
}

func TestHandlerServesHealth(t *testing.T) {
	engine, _ := newTestEngine(t)
	handler := engine.Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	var health Health
	if err := json.Unmarshal(recorder.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if !health.Simulator {
		t.Fatal("expected the simulator flag to be set")
	}
}

func TestHandlerCreatesAndFetchesRuns(t *testing.T) {
	engine, backend := newTestEngine(t)
	startEngine(t, engine, backend)
	handler := engine.Handler()

	pipeline := "name: demo\nsteps:\n  - id: build\n    image: alpine\n    run: echo hi\n"
	request := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(pipeline))
	request.Header.Set("Content-Type", "application/yaml")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var created RunState
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Name != "demo" {
		t.Fatalf("unexpected run: %+v", created)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+created.ID, nil)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
}

func TestHandlerRejectsInvalidPipeline(t *testing.T) {
	engine, backend := newTestEngine(t)
	startEngine(t, engine, backend)
	handler := engine.Handler()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader("name: demo\n"))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}

func TestHandlerServesTheUI(t *testing.T) {
	engine, _ := newTestEngine(t)
	handler := engine.Handler()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "Barq") {
		t.Fatal("expected the dashboard markup")
	}
}

func TestLeaderElectionUsesOptimisticConcurrency(t *testing.T) {
	engine, backend := newTestEngine(t)
	ctx := context.Background()

	// Another replica holds the lease.
	duration := int32(30)
	if err := backend.CreateLease(ctx, k8s.Lease{
		Metadata: k8s.ObjectMeta{Name: engine.config.LeaseName},
		Spec: k8s.LeaseSpec{
			HolderIdentity:       "other-replica",
			LeaseDurationSeconds: &duration,
			RenewTime:            time.Now().UTC().Format(k8s.K8sMicroTimeLayout),
		},
	}); err != nil {
		t.Fatal(err)
	}
	engine.campaign(ctx)
	if engine.IsLeader() {
		t.Fatal("must not become leader while another replica holds a fresh lease")
	}
}

func TestLeaderElectionAcquiresAnExpiredLease(t *testing.T) {
	engine, backend := newTestEngine(t)
	ctx := context.Background()

	duration := int32(1)
	if err := backend.CreateLease(ctx, k8s.Lease{
		Metadata: k8s.ObjectMeta{Name: engine.config.LeaseName},
		Spec: k8s.LeaseSpec{
			HolderIdentity:       "other-replica",
			LeaseDurationSeconds: &duration,
			RenewTime:            time.Now().UTC().Add(-time.Hour).Format(k8s.K8sMicroTimeLayout),
		},
	}); err != nil {
		t.Fatal(err)
	}
	engine.campaign(ctx)
	if !engine.IsLeader() {
		t.Fatal("expected to take over an expired lease")
	}
}
