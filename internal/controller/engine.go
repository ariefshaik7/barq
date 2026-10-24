// Package controller implements the Barq control plane.
//
// The controller owns scheduling, reconciliation, leader election, the
// REST and SSE API and the embedded web UI. Only the leader replica
// schedules work; followers proxy control plane requests to the leader.
package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"barq/internal/alert"
	"barq/internal/auth"
	"barq/internal/cache"
	"barq/internal/dag"
	"barq/internal/health"
	"barq/internal/k8s"
	"barq/internal/queue"
	"barq/internal/runner"
	"barq/internal/secrets"
	"barq/internal/stats"
	"barq/internal/storage"
	"barq/internal/yaml"
)

// Config configures the engine.
type Config struct {
	// Identity is the stable identity of this controller instance.
	Identity string
	// Namespace is the namespace workers run in.
	Namespace string
	// LeaseName is the Lease used for leader election.
	LeaseName string
	// LeaderURL is the base URL of this controller for follower proxying.
	LeaderURL string
	// ObjectPrefix namespaces all objects.
	ObjectPrefix string
	// MaxAttempts bounds automatic retries of a preempted step.
	MaxAttempts int
	// PollInterval is the reconciliation interval.
	PollInterval time.Duration
	// LeaseDuration is how long a lease is valid without renewal.
	LeaseDuration time.Duration
	// LeaseRenewTime is how often the lease is renewed.
	LeaseRenewTime time.Duration
	// AllowedBranches restricts webhook triggered runs.
	AllowedBranches map[string]struct{}
	// APIToken authenticates the control plane API. It is required in
	// production mode.
	APIToken string
	// GitHubWebhookSecret validates GitHub webhook payloads.
	GitHubWebhookSecret string
	// GitLabWebhookToken validates GitLab webhook payloads.
	GitLabWebhookToken string
	// GitHubAPIToken authenticates the GitHub API calls used to fetch a
	// pipeline definition.
	GitHubAPIToken string
	// GitLabAPIToken authenticates the GitLab API calls.
	GitLabAPIToken string
	// GitHubAPIBase is the GitHub API base URL.
	GitHubAPIBase string
	// GitLabAPIBase is the GitLab API base URL.
	GitLabAPIBase string
	// ForgeTokens maps a provider name to its API token.
	ForgeTokens map[string]string
	// pipelinePath is the path of the pipeline definition inside the
	// repository.
	pipelinePath string
	// Alerts delivers run events to the configured destinations.
	Alerts *alert.Manager
	// Health monitors the controller's dependencies.
	Health *health.Monitor
	// Stats computes the usage statistics.
	Stats *stats.Manager
	// MaxConcurrentRuns is the global limit on the number of runs
	// that may execute at the same time. Zero means unlimited.
	MaxConcurrentRuns int
	// PublicURL is the externally reachable base URL of the
	// controller. It is used to build links in alerts.
	PublicURL string
}

// auth is the authenticator used by the API. It is a field rather
// than part of the configuration because it carries runtime state.
type engineAuth = Authenticator

// runHandle owns a run state and serializes access to it.
//
// The reconciliation loop mutates run state from a background goroutine
// while API handlers read it concurrently, so every access goes through
// the handle's mutex.
type runHandle struct {
	mu    sync.Mutex
	state *RunState
}

// snapshot returns a deep copy of the run state.
func (h *runHandle) snapshot() *RunState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state.Clone()
}

// with locks the run state and invokes the callback.
func (h *runHandle) with(fn func(state *RunState)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn(h.state)
}

// Engine is the Barq control plane.
type Engine struct {
	config   Config
	backend  k8s.Backend
	tiered   *storage.Tiered
	cache    *cache.Manager
	vault    *storage.SecretVault
	logger   *slog.Logger
	compiled dag.Options

	// Auth authenticates users. It is nil when authentication is
	// disabled, which is only acceptable in local and simulator mode.
	Auth Authenticator
	// Tokens issues and verifies programmatic API tokens.
	Tokens *auth.APITokenStore
	// Forges holds the source control providers.
	Forges *ForgeManager
	// Queue admits runs.
	Queue *queue.Queue
	// metrics is the exported metric set.
	metrics *Metrics
	// admitted tracks the runs that the queue has released.
	admitted map[string]bool
	// admittedMu guards the admitted set.
	admittedMu sync.RWMutex
	// secureCookies marks session cookies as Secure.
	secureCookies bool
	// csrf issues and validates CSRF tokens.
	csrf *csrfStore
	// applier reconciles configuration as code.
	applier *configApplier

	mu   sync.Mutex
	runs map[string]*runHandle

	leader       atomic.Bool
	leaderURL    atomic.Value
	leaseHolder  atomic.Value
	revision     atomic.Uint64
	streamMu     sync.Mutex
	streams      map[string]chan struct{}
	triggerMu    sync.Mutex
	triggeredIDs map[string]struct{}
}

// Options carries the runtime services an engine depends on.
type Options struct {
	// Auth authenticates users. It may be nil.
	Auth Authenticator
	// Tokens issues and verifies programmatic API tokens.
	Tokens *auth.APITokenStore
	// Forges holds the source control providers.
	Forges *ForgeManager
	// PipelinePath is the path of the pipeline definition inside the
	// repository.
	PipelinePath string
	// SecureCookies marks session cookies as Secure.
	SecureCookies bool
	// Alerts delivers run events.
	Alerts *alert.Manager
	// Health monitors the controller's dependencies.
	Health *health.Monitor
	// Stats computes the usage statistics.
	Stats *stats.Manager
	// Queue admits runs. A run that is not admitted does not execute
	// any step.
	Queue *queue.Queue
	// Metrics is the exported metric set.
	Metrics *Metrics
	// PublicURL is the externally reachable base URL.
	PublicURL string
}

// New creates an engine.
func New(config Config, backend k8s.Backend, tiered *storage.Tiered, manager *cache.Manager, vault *storage.SecretVault, logger *slog.Logger, compiled dag.Options, options Options) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = 3
	}
	if config.PollInterval <= 0 {
		config.PollInterval = 2 * time.Second
	}
	if config.LeaseDuration <= 0 {
		config.LeaseDuration = 10 * time.Second
	}
	if config.LeaseRenewTime <= 0 {
		config.LeaseRenewTime = 3 * time.Second
	}
	if options.Alerts != nil {
		config.Alerts = options.Alerts
	}
	if options.Health != nil {
		config.Health = options.Health
	}
	if options.Stats != nil {
		config.Stats = options.Stats
	}
	if options.PublicURL != "" {
		config.PublicURL = options.PublicURL
	}
	engine := &Engine{
		config:        config,
		Auth:          options.Auth,
		Tokens:        options.Tokens,
		Forges:        options.Forges,
		Queue:         options.Queue,
		metrics:       options.Metrics,
		admitted:      make(map[string]bool),
		secureCookies: options.SecureCookies,
		csrf:          newCSRFStore(),
		backend:       backend,
		tiered:        tiered,
		cache:         manager,
		vault:         vault,
		logger:        logger,
		compiled:      compiled,
		runs:          make(map[string]*runHandle),
		streams:       make(map[string]chan struct{}),
		triggeredIDs:  make(map[string]struct{}),
	}
	if engine.Forges == nil {
		engine.Forges = NewForgeManager(logger)
	}
	engine.config.pipelinePath = options.PipelinePath
	if engine.config.pipelinePath == "" {
		engine.config.pipelinePath = ".barq.yaml"
	}
	engine.applier = newConfigApplier(options.Auth, vault)
	engine.leaderURL.Store("")
	engine.leaseHolder.Store("")
	return engine
}

// Backend exposes the execution backend name.
func (e *Engine) Backend() string { return e.backend.Name() }

// IsLeader reports whether this replica is the active leader.
func (e *Engine) IsLeader() bool { return e.leader.Load() }

// LeaderURL returns the URL of the active leader, if known.
func (e *Engine) LeaderURL() string {
	value, _ := e.leaderURL.Load().(string)
	return value
}

// Run starts the election loop, the reconciliation loop and the queue
// admission loop.
func (e *Engine) Run(ctx context.Context) {
	go e.electionLoop(ctx)
	go e.reconcileLoop(ctx)
	go e.admissionLoop(ctx)
	go e.cronLoop(ctx)
}

// admissionLoop admits the runs that the queue releases.
//
// A run is pushed onto the queue when it is created. The queue
// decides when it may execute, based on the global concurrency limit
// and the concurrency group of the pipeline. Until the run is
// admitted, none of its steps start.
func (e *Engine) admissionLoop(ctx context.Context) {
	if e.Queue == nil {
		return
	}
	for {
		task, release, err := e.Queue.Poll(ctx, "barq-admission", func(*queue.Task) (bool, int) {
			return true, 1
		})
		if err != nil {
			return
		}
		e.admitRun(ctx, task, release)
	}
}

// admitRun marks a run as admitted and starts its execution.
//
// The release function is called when the run reaches a terminal
// state, which frees the concurrency slot.
func (e *Engine) admitRun(ctx context.Context, task *queue.Task, release func(queue.Status, error)) {
	e.admittedMu.Lock()
	e.admitted[task.ID] = true
	e.admittedMu.Unlock()

	e.recordAdmission()
	e.logger.Info("run admitted", "run", task.ID, "group", task.ConcurrencyGroup)

	// The run is released when it finishes. The watcher runs in its
	// own goroutine so that the admission loop is not blocked.
	go func() {
		defer func() {
			e.admittedMu.Lock()
			delete(e.admitted, task.ID)
			e.admittedMu.Unlock()
			release(queue.StatusSuccess, nil)
		}()

		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			state, ok := e.GetRun(ctx, task.ID)
			if !ok {
				return
			}
			if state.IsTerminal() {
				return
			}
		}
	}()
}

// isAdmitted reports whether a run may execute.
//
// A controller without a queue admits every run immediately, which is
// the behaviour of a single replica development installation.
func (e *Engine) isAdmitted(runID string) bool {
	if e.Queue == nil {
		return true
	}
	e.admittedMu.RLock()
	defer e.admittedMu.RUnlock()
	return e.admitted[runID]
}

// electionLoop maintains the leader lease.
//
// The lease is updated with optimistic concurrency using the
// resourceVersion returned by the API server. If the update conflicts,
// another replica won the election and this replica stops scheduling.
func (e *Engine) electionLoop(ctx context.Context) {
	ticker := time.NewTicker(e.config.LeaseRenewTime)
	defer ticker.Stop()
	for {
		e.campaign(ctx)
		select {
		case <-ctx.Done():
			e.releaseLease(ctx)
			return
		case <-ticker.C:
		}
	}
}

// campaign attempts to acquire or renew the lease.
func (e *Engine) campaign(ctx context.Context) {
	lease, err := e.backend.GetLease(ctx, e.config.LeaseName)
	switch {
	case errors.Is(err, io.EOF):
		return
	case err != nil && k8s.IsNotFound(err):
		duration := int32(e.config.LeaseDuration.Seconds())
		now := time.Now().UTC()
		candidate := k8s.Lease{
			Metadata: k8s.ObjectMeta{
				Name:        e.config.LeaseName,
				Annotations: map[string]string{"barq.io/leader-url": e.config.LeaderURL},
			},
			Spec: k8s.LeaseSpec{
				HolderIdentity:       e.config.Identity,
				LeaseDurationSeconds: &duration,
				AcquireTime:          now.Format(k8s.K8sMicroTimeLayout),
				RenewTime:            now.Format(k8s.K8sMicroTimeLayout),
			},
		}
		if err := e.backend.CreateLease(ctx, candidate); err != nil {
			e.demote()
			return
		}
		e.promote()
		return
	case err != nil:
		e.logger.Warn("lease read failed", "error", err)
		return
	}

	e.leaseHolder.Store(lease.Spec.HolderIdentity)
	if url, ok := lease.Metadata.Annotations["barq.io/leader-url"]; ok {
		e.leaderURL.Store(url)
	}

	if lease.Spec.HolderIdentity == e.config.Identity {
		duration := int32(e.config.LeaseDuration.Seconds())
		lease.Spec.LeaseDurationSeconds = &duration
		lease.Spec.RenewTime = time.Now().UTC().Format(k8s.K8sMicroTimeLayout)
		lease.Metadata.Annotations = mergeAnnotations(lease.Metadata.Annotations, map[string]string{
			"barq.io/leader-url": e.config.LeaderURL,
		})
		if err := e.backend.UpdateLease(ctx, lease); err != nil {
			if k8s.IsConflict(err) {
				e.logger.Info("lost lease to another replica", "holder", lease.Spec.HolderIdentity)
			} else {
				e.logger.Warn("lease renewal failed", "error", err)
			}
			e.demote()
			return
		}
		e.promote()
		return
	}

	if leaseExpired(lease, e.config.LeaseDuration) {
		duration := int32(e.config.LeaseDuration.Seconds())
		now := time.Now().UTC()
		lease.Spec.HolderIdentity = e.config.Identity
		lease.Spec.LeaseDurationSeconds = &duration
		lease.Spec.AcquireTime = now.Format(k8s.K8sMicroTimeLayout)
		lease.Spec.RenewTime = now.Format(k8s.K8sMicroTimeLayout)
		lease.Spec.LeaseTransitions++
		lease.Metadata.Annotations = mergeAnnotations(lease.Metadata.Annotations, map[string]string{
			"barq.io/leader-url": e.config.LeaderURL,
		})
		if err := e.backend.UpdateLease(ctx, lease); err != nil {
			e.demote()
			return
		}
		e.logger.Info("acquired expired lease", "holder", e.config.Identity)
		e.promote()
		return
	}

	e.demote()
}

// leaseExpired reports whether a lease may be taken over.
func leaseExpired(lease k8s.Lease, duration time.Duration) bool {
	if lease.Spec.RenewTime == "" {
		return true
	}
	renew, err := time.Parse(k8s.K8sMicroTimeLayout, lease.Spec.RenewTime)
	if err != nil {
		return true
	}
	return time.Now().UTC().After(renew.Add(duration))
}

// promote marks this replica as the leader and recovers state.
func (e *Engine) promote() {
	if e.leader.Swap(true) {
		e.logger.Info("became leader", "identity", e.config.Identity)
		go e.recover()
	}
}

// demote stops this replica from scheduling.
func (e *Engine) demote() {
	if e.leader.Swap(false) {
		e.logger.Info("stepped down as leader", "identity", e.config.Identity)
	}
}

// releaseLease gives up the lease on shutdown.
func (e *Engine) releaseLease(ctx context.Context) {
	if !e.leader.Load() {
		return
	}
	lease, err := e.backend.GetLease(ctx, e.config.LeaseName)
	if err != nil {
		return
	}
	if lease.Spec.HolderIdentity != e.config.Identity {
		return
	}
	lease.Spec.RenewTime = time.Now().UTC().Add(-2 * e.config.LeaseDuration).Format(k8s.K8sMicroTimeLayout)
	_ = e.backend.UpdateLease(ctx, lease)
	e.demote()
}

// recover reloads active runs from the durable object store.
func (e *Engine) recover() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	identifiers, err := e.tiered.ListRuns(ctx)
	if err != nil {
		e.logger.Warn("recover runs failed", "error", err)
		return
	}
	for _, id := range identifiers {
		var state RunState
		if err := e.tiered.LoadState(ctx, id, &state); err != nil {
			if !errors.Is(err, storage.ErrNotFound) {
				e.logger.Warn("load run state failed", "run", id, "error", err)
			}
			continue
		}
		if state.Steps == nil {
			state.Steps = map[string]*StepState{}
		}
		e.mu.Lock()
		if _, exists := e.runs[id]; !exists {
			e.runs[id] = &runHandle{state: &state}
		}
		e.mu.Unlock()

		// A recovered run is re-queued, because the run that held its
		// concurrency slot died with the previous leader.
		if e.Queue != nil && !state.IsTerminal() {
			group := state.Pipeline.Group
			if group == "" {
				group = "pipeline:" + state.Pipeline.Name
			}
			limit := state.Pipeline.MaxConcurrent
			if limit <= 0 {
				limit = e.config.MaxConcurrentRuns
			}
			if err := e.Queue.Push(ctx, &queue.Task{
				ID:               id,
				RunID:            id,
				Name:             state.Name,
				ConcurrencyGroup: group,
				ConcurrencyLimit: limit,
				Created:          state.CreatedAt,
			}); err != nil {
				e.logger.Warn("the recovered run could not be queued", "run", id, "error", err)
			}
		}
		e.logger.Info("recovered run", "run", id, "status", state.Status)
	}
}

// reconcileLoop drives the scheduler.
func (e *Engine) reconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(e.config.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !e.leader.Load() {
				continue
			}
			e.reconcile(ctx)
		}
	}
}

// reconcile advances every active run.
func (e *Engine) reconcile(ctx context.Context) {
	e.mu.Lock()
	handles := make([]*runHandle, 0, len(e.runs))
	for _, handle := range e.runs {
		handles = append(handles, handle)
	}
	e.mu.Unlock()

	for _, handle := range handles {
		state := handle.snapshot()
		if state.IsTerminal() {
			continue
		}
		e.reconcileRun(ctx, handle)
	}
}

// reconcileRun advances a single run: adopt pods, dispatch ready steps,
// harvest results and finalize.
func (e *Engine) reconcileRun(ctx context.Context, handle *runHandle) {
	state := handle.snapshot()
	pods, err := e.backend.ListRunPods(ctx, state.ID)
	if err != nil {
		e.logger.Warn("list pods failed", "run", state.ID, "error", err)
		return
	}

	for _, pod := range pods {
		e.harvestPod(ctx, handle, pod)
	}

	e.dispatch(ctx, handle)
	e.finalize(ctx, handle)
}

// harvestPod maps a pod back to its step and applies the result.
func (e *Engine) harvestPod(ctx context.Context, handle *runHandle, pod k8s.Pod) {
	stepID := pod.Metadata.Labels["barq.io/step-id"]
	attempt, _ := strconv.Atoi(pod.Metadata.Labels["barq.io/attempt"])

	var (
		terminal    bool
		exitCode    int
		stepIDFound bool
		superseded  bool
		cacheRecord *cache.Record
		failedStep  *StepState
		preempted   bool
	)

	handle.with(func(state *RunState) {
		step, ok := state.Steps[stepID]
		if !ok {
			return
		}
		stepIDFound = true
		// A pod is only harvested once. The step records the pod that
		// is currently authoritative, and a pod whose attempt is older
		// than the recorded attempt has already been processed.
		if attempt < step.Attempt {
			superseded = true
			return
		}
		if step.PodName != "" && step.PodName != pod.Metadata.Name {
			superseded = true
			return
		}
		if !pod.Terminated() {
			if step.Status == StepQueued {
				step.Status = StepRunning
				step.PodName = pod.Metadata.Name
				step.Message = "worker running"
			}
			return
		}

		terminal = true
		exitCode, _ = pod.ExitCode()
		step.ExitCode = &exitCode
		step.PodName = pod.Metadata.Name
		step.Message = pod.Status.Message
		if step.StartedAt == nil {
			now := time.Now().UTC()
			step.StartedAt = &now
		}
		now := time.Now().UTC()
		step.FinishedAt = &now
		if step.StartedAt != nil {
			step.DurationMS = now.Sub(*step.StartedAt).Milliseconds()
		}

		switch exitCode {
		case 75:
			// Exit code 75 is EX_TEMPFAIL and is the signal that the
			// worker was preempted. The step is rescheduled from its
			// checkpoint instead of failing the pipeline.
			e.handlePreemption(state, step)
			preempted = true
		case 0:
			step.Status = StepSuccess
			step.Message = "completed successfully"
			if step.CacheHit {
				e.publishAlert(ctx, alert.EventCacheHit, alert.SeverityInfo, state, step, "served from the Merkle cache")
			}
			if definition, ok := state.Compiled.ByID[step.ID]; ok && definition.Cacheable {
				cacheRecord = &cache.Record{
					Fingerprint:  step.Merkle,
					StepID:       step.ID,
					WorkspaceKey: step.WorkspaceKey,
					DurationMS:   step.DurationMS,
				}
			}
		default:
			step.Status = StepFailed
			step.Message = fmt.Sprintf("failed with exit code %d", exitCode)
			e.cancelDependents(state, step.ID)
			failedStep = step
		}
	})

	if !stepIDFound {
		return
	}
	if superseded {
		_ = e.backend.DeletePod(ctx, pod.Metadata.Name)
		return
	}
	if !terminal {
		return
	}

	// The pod is only removed once the log stream has reached EOF so
	// that no output is lost.
	e.waitForStream(ctx, handle.snapshot().ID, pod.Metadata.Name)
	_ = e.backend.DeletePod(ctx, pod.Metadata.Name)
	e.persist(ctx, handle)

	if cacheRecord != nil {
		if err := e.cache.Store(ctx, *cacheRecord); err != nil {
			e.logger.Warn("cache store failed", "run", handle.snapshot().ID, "step", cacheRecord.StepID, "error", err)
		}
	}
	if failedStep != nil {
		e.publishAlert(ctx, alert.EventStepFailed, alert.SeverityError, handle.snapshot(), failedStep, failedStep.Message)
	}
	if preempted {
		e.publishAlert(ctx, alert.EventStepPreempted, alert.SeverityWarning, handle.snapshot(), nil, "worker was preempted and rescheduled")
	}
}

// handlePreemption reschedules a preempted step.
//
// The caller must hold the run lock.
func (e *Engine) handlePreemption(state *RunState, step *StepState) {
	if step.Attempt+1 >= e.config.MaxAttempts {
		step.Status = StepFailed
		step.Message = "exceeded retry budget after repeated preemption"
		e.cancelDependents(state, step.ID)
		return
	}
	step.Status = StepSpotMigrating
	step.Message = "worker preempted, rescheduling on another node"
	step.CheckpointKey = step.WorkspaceKey
	step.PodName = ""
	step.FinishedAt = nil
	// The attempt counter advances so that the replacement worker is
	// distinguishable from the preempted one and so that the retry
	// budget is enforced.
	step.Attempt++
}

// cancelDependents marks downstream steps as canceled.
func (e *Engine) cancelDependents(state *RunState, failedID string) {
	blocked := map[string]struct{}{failedID: {}}
	changed := true
	for changed {
		changed = false
		for id, step := range state.Steps {
			if step.Status != StepQueued {
				continue
			}
			for _, dependency := range state.Compiled.ByID[id].DependsOn {
				if _, isBlocked := blocked[dependency]; isBlocked {
					blocked[id] = struct{}{}
					step.Status = StepCanceled
					step.Message = "upstream step failed"
					changed = true
					break
				}
			}
		}
	}
}

// dispatch starts every step whose dependencies are satisfied.
//
// A run that the queue has not admitted does not dispatch anything.
// That is what makes the concurrency limits effective.
func (e *Engine) dispatch(ctx context.Context, handle *runHandle) {
	state := handle.snapshot()
	if !e.isAdmitted(state.ID) {
		return
	}
	type candidate struct {
		step       *StepState
		definition dag.Step
	}
	var ready []candidate

	handle.with(func(s *RunState) {
		for _, step := range s.AllSteps() {
			// A preempted step is re-queued so that it is rescheduled
			// with its next attempt number.
			if step.Status == StepSpotMigrating {
				step.Status = StepQueued
			}
			if step.Status != StepQueued {
				continue
			}
			definition, ok := s.Compiled.ByID[step.ID]
			if !ok {
				continue
			}
			blocked := false
			// skipped records that the step's gate does not match
			// the outcome of its upstream steps. Such a step is
			// skipped rather than left waiting forever.
			skipped := false
			for _, dependency := range definition.DependsOn {
				parent, ok := s.Steps[dependency]
				if !ok {
					blocked = true
					break
				}
				upstreamFailed := parent.Status == StepFailed || parent.Status == StepCanceled
				upstreamDone := parent.Status == StepSuccess || parent.Status == StepCached ||
					upstreamFailed
				if !upstreamDone {
					// The upstream step is still running. The step
					// waits for it.
					blocked = true
					break
				}
				switch {
				case definition.OnFailure:
					// The step only runs on failure.
					if !upstreamFailed {
						skipped = true
					}
				case definition.OnSuccess:
					// The step only runs on success.
					if upstreamFailed {
						skipped = true
					}
				default:
					// The default is to run when the upstream step
					// succeeded.
					if upstreamFailed {
						skipped = true
					}
				}
				if skipped {
					break
				}
			}
			if skipped {
				// The step is skipped. It is recorded as such so
				// that the pipeline can finish.
				step.Status = StepSkipped
				step.Message = "skipped: the upstream steps did not match the condition of this step"
				continue
			}
			if blocked {
				continue
			}
			// The step is marked as launching while the lock is held so
			// that a concurrent reconciliation cannot start it twice.
			step.Status = StepRunning
			step.Message = "starting worker"
			ready = append(ready, candidate{step: step.Clone(), definition: definition})
		}
	})
	if len(ready) == 0 {
		return
	}
	e.persist(ctx, handle)

	for _, item := range ready {
		e.startStep(ctx, handle, item.step, item.definition)
	}
}

// startStep performs the cache lookup and launches the worker.
//
// The step is passed as a detached copy. Every mutation is applied back
// to the run under the handle lock so that concurrent reconciliations
// and API reads never observe a partially updated step.
func (e *Engine) startStep(ctx context.Context, handle *runHandle, step *StepState, definition dag.Step) {
	state := handle.snapshot()
	environment := map[string]string{}
	for key, value := range definition.Env {
		environment[key] = value
	}

	fail := func(message string) {
		handle.with(func(s *RunState) {
			target, ok := s.Steps[step.ID]
			if !ok {
				return
			}
			target.Status = StepFailed
			target.Message = message
		})
		e.persist(ctx, handle)
	}

	secretVersions := ""
	injected := secrets.NewMapping()
	for _, name := range definition.Secrets {
		if e.vault == nil {
			fail("secret store is not configured")
			return
		}
		value, metadata, err := e.vault.Value(ctx, name)
		if err != nil {
			fail(fmt.Sprintf("secret %q is unavailable: %v", name, err))
			return
		}
		environment[name] = value
		// The mapping records that this environment variable holds a
		// secret, so that the log masker redacts exactly this value
		// and the runtime can tell it apart from an ordinary value.
		injected.Set(name, value)
		// The value is registered with the log masker immediately, so
		// that a secret cannot leak through a log line that is written
		// between the injection and the next state change.
		e.tiered.AddSecret(value)
		secretVersions += fmt.Sprintf("%s=%d;", name, metadata.Version)
	}

	upstream := make([]string, 0, len(definition.DependsOn))
	for _, dependency := range definition.DependsOn {
		if parent, ok := state.Steps[dependency]; ok && parent.Merkle != "" {
			upstream = append(upstream, parent.Merkle)
		}
	}

	fingerprint := cache.Fingerprint(cache.FingerprintInput{
		Image:          definition.Image,
		Commands:       definition.Commands,
		Env:            environment,
		GitCommitTree:  state.Compiled.GitCommitTree,
		UpstreamHashes: upstream,
		SecretVersions: secretVersions,
	})

	if definition.Cacheable && !step.BypassCache {
		record, hit, err := e.cache.Lookup(ctx, fingerprint)
		switch {
		case err != nil:
			e.logger.Warn("cache lookup failed", "run", state.ID, "step", step.ID, "error", err)
		case hit:
			now := time.Now().UTC()
			handle.with(func(s *RunState) {
				target, ok := s.Steps[step.ID]
				if !ok {
					return
				}
				target.Status = StepCached
				target.CacheHit = true
				target.Merkle = fingerprint
				target.WorkspaceKey = record.WorkspaceKey
				target.Message = "served from the Merkle cache"
				target.StartedAt = &now
				target.FinishedAt = &now
			})
			e.persist(ctx, handle)
			return
		}
	}

	attempt := step.Attempt
	workspaceKey := fmt.Sprintf("%s/runs/%s/workspaces/%s-a%d.tar.gz",
		e.config.ObjectPrefix, state.ID, definition.ID, attempt)

	downloads := make([]string, 0, len(definition.DependsOn))
	seen := map[string]struct{}{}
	for _, dependency := range definition.DependsOn {
		parent, ok := state.Steps[dependency]
		if !ok {
			continue
		}
		key := parent.WorkspaceKey
		if key == "" {
			key = parent.CheckpointKey
		}
		if key == "" {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		url, err := e.tiered.Store().Presign(ctx, "GET", key, 30*time.Minute)
		if err != nil {
			fail(fmt.Sprintf("presign workspace download failed: %v", err))
			return
		}
		downloads = append(downloads, url)
	}

	upload := ""
	if !definition.NoWorkspace {
		url, err := e.tiered.Store().Presign(ctx, "PUT", workspaceKey, 30*time.Minute)
		if err != nil {
			fail(fmt.Sprintf("presign workspace upload failed: %v", err))
			return
		}
		upload = url
	}

	podName := e.podName(state.ID, definition.ID, attempt)

	// The worker environment carries the workspace capabilities, so
	// that the backend does not need to know how they are transported.
	environment["BARQ_WORKSPACE_DOWNLOADS"] = strings.Join(downloads, "\n")
	if upload != "" {
		environment["BARQ_WORKSPACE_UPLOAD"] = upload
	}
	if definition.NoWorkspace {
		environment["BARQ_NO_WORKSPACE"] = "1"
	}

	now := time.Now().UTC()
	handle.with(func(s *RunState) {
		target, ok := s.Steps[step.ID]
		if !ok {
			return
		}
		target.Attempt = attempt
		target.Status = StepRunning
		target.PodName = podName
		target.Merkle = fingerprint
		target.WorkspaceKey = workspaceKey
		target.Message = "worker running"
		if target.StartedAt == nil {
			target.StartedAt = &now
		}
	})
	e.persist(ctx, handle)
	e.recordStepStarted()

	// The step runs in its own goroutine. The runner owns the
	// lifecycle: it starts the worker, drains the log stream, waits
	// for the exit code and cleans the worker up.
	e.launchStep(ctx, handle, definition, podName, attempt, workspaceKey, fingerprint)
}

// launchStep runs a step through the runner.
//
// The goroutine is the unit of execution. It outlives the reconcile
// tick that started it, so a slow step does not block the scheduler.
func (e *Engine) launchStep(ctx context.Context, handle *runHandle, definition dag.Step, podName string, attempt int, workspaceKey, fingerprint string) {
	step := k8s.Step{
		ID:             definition.ID,
		Name:           definition.Name,
		Image:          definition.Image,
		Commands:       definition.Commands,
		Env:            e.stepEnvironment(ctx, handle, definition),
		WorkingDir:     definition.WorkingDir,
		TimeoutSeconds: definition.TimeoutSeconds,
		PodName:        podName,
		RunID:          handle.snapshot().ID,
		Attempt:        attempt,
		Services:       convertServices(definition.Services),
	}

	stepRunner := runner.New(runner.Options{
		Backend:    e.backend,
		Logger:     e.logger,
		WorkflowID: handle.snapshot().ID,
		Failure:    runner.FailureMode(definition.Failure),
		OnLog: func(stepID string, line []byte) {
			e.AppendLog(ctx, handle.snapshot().ID, string(line))
		},
		OnState: func(stepID string, state runner.State) {
			e.recordStepOutcome(ctx, handle, stepID, attempt, state)
		},
	})

	go func() {
		// The cleanup context is derived from the background context,
		// not from the request context, so that a canceled request
		// does not leave the worker behind.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, err := stepRunner.Run(ctx, cleanupCtx, step)
		if err != nil && !errors.Is(err, runner.ErrCanceled) {
			e.logger.Warn("the step did not complete cleanly", "step", step.ID, "error", err)
		}
	}()
}

// stepEnvironment builds the environment of a worker.
//
// The secrets are resolved here rather than in the caller, so that the
// value is only present in the environment of the worker and never in
// the persisted state.
func (e *Engine) stepEnvironment(ctx context.Context, handle *runHandle, definition dag.Step) map[string]string {
	environment := make(map[string]string, len(definition.Env)+4)
	for key, value := range definition.Env {
		environment[key] = value
	}
	for _, name := range definition.Secrets {
		if e.vault == nil {
			continue
		}
		value, _, err := e.vault.Value(ctx, name)
		if err != nil {
			e.logger.Warn("a secret could not be resolved", "secret", name, "error", err)
			continue
		}
		environment[name] = value
		e.tiered.AddSecret(value)
	}
	environment["BARQ_RUN_ID"] = handle.snapshot().ID
	environment["BARQ_STEP_ID"] = definition.ID
	environment["BARQ_WAVE"] = strconv.Itoa(definition.Wave)
	environment["BARQ_NODE_INDEX"] = strconv.Itoa(definition.ShardIndex)
	environment["BARQ_NODE_TOTAL"] = strconv.Itoa(definition.ShardTotal)
	return environment
}

// recordCache stores a cache entry for a successful step.
//
// The cache record keeps the workspace key, so that a downstream step
// receives the artifacts and the exported variables of the cached step
// even though no worker ran.
func (e *Engine) recordCache(ctx context.Context, state *RunState, step *StepState) {
	definition, ok := state.Compiled.ByID[step.ID]
	if !ok || !definition.Cacheable {
		return
	}
	record := cache.Record{
		Fingerprint:  step.Merkle,
		StepID:       step.ID,
		WorkspaceKey: step.WorkspaceKey,
		DurationMS:   step.DurationMS,
	}
	if err := e.cache.Store(ctx, record); err != nil {
		e.logger.Warn("cache store failed", "run", state.ID, "step", step.ID, "error", err)
	}
}

// recordStepOutcome stores the result of a finished step.
func (e *Engine) recordStepOutcome(ctx context.Context, handle *runHandle, stepID string, attempt int, state runner.State) {
	handle.with(func(s *RunState) {
		step, ok := s.Steps[stepID]
		if !ok {
			return
		}
		if step.Attempt != attempt {
			// A newer attempt has already replaced this one.
			return
		}
		if step.Status.IsTerminal() {
			return
		}
		exitCode := state.ExitCode
		step.ExitCode = &exitCode
		now := time.Now().UTC()
		step.FinishedAt = &now
		step.DurationMS = now.Sub(*step.StartedAt).Milliseconds()

		switch {
		case state.OOMKilled:
			step.Status = StepFailed
			step.Message = "the worker was killed for exceeding its memory limit"
		case state.ExitCode == 75:
			// Exit code 75 is EX_TEMPFAIL. The worker was preempted
			// and the step is rescheduled from its checkpoint.
			e.handlePreemption(s, step)
		case state.ExitCode == 0:
			step.Status = StepSuccess
			step.Message = "completed successfully"
		default:
			step.Status = StepFailed
			step.Message = fmt.Sprintf("failed with exit code %d", exitCode)
			// The failure mode decides what happens next. A step that
			// ignores its failure lets the pipeline continue. A step
			// that cancels its failure cancels the whole pipeline.
			definition, ok := s.Compiled.ByID[stepID]
			switch {
			case ok && definition.Failure == "ignore":
				step.Status = StepSuccess
				step.Message = fmt.Sprintf("failed with exit code %d, ignored", exitCode)
			case ok && definition.Failure == "cancel":
				step.Status = StepFailed
				step.Message = fmt.Sprintf("failed with exit code %d, cancelling the pipeline", exitCode)
				e.cancelDependents(s, stepID)
			default:
				e.cancelDependents(s, stepID)
			}
		}
	})
	e.persist(ctx, handle)

	current := handle.snapshot()
	step := current.Steps[stepID]
	if step == nil {
		return
	}
	e.recordStepFinished(string(step.Status), time.Duration(step.DurationMS)*time.Millisecond)
	switch step.Status {
	case StepFailed:
		e.publishAlert(ctx, alert.EventStepFailed, alert.SeverityError, current, step, step.Message)
	case StepSpotMigrating:
		e.publishAlert(ctx, alert.EventStepPreempted, alert.SeverityWarning, current, step, "worker was preempted and rescheduled")
	case StepSuccess:
		if step.CacheHit {
			e.publishAlert(ctx, alert.EventCacheHit, alert.SeverityInfo, current, step, "served from the Merkle cache")
		}
		e.recordCache(ctx, current, step)
	}
}

// convertServices converts the sidecar declarations of a step.
func convertServices(services []dag.Service) []k8s.Sidecar {
	if len(services) == 0 {
		return nil
	}
	result := make([]k8s.Sidecar, 0, len(services))
	for _, service := range services {
		result = append(result, k8s.Sidecar{
			ID:       service.ID,
			Image:    service.Image,
			Commands: append([]string(nil), service.Commands...),
			Env:      service.Env,
			Ports:    append([]string(nil), service.Ports...),
		})
	}
	return result
}

// podName builds a deterministic, DNS compatible pod name.
func (e *Engine) podName(runID, stepID string, attempt int) string {
	name := fmt.Sprintf("barq-%s-%s-a%d", runID, stepID, attempt)
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.Trim(name, "-")
}

// waitForStream blocks until the log stream for a worker completes.
//
// It is used when a worker was adopted rather than started by this
// replica, for example after a leader failover. The stream is
// attached by the adoption path, so the wait is bounded.
func (e *Engine) waitForStream(ctx context.Context, runID, podName string) {
	key := runID + "/" + podName
	e.streamMu.Lock()
	done, ok := e.streams[key]
	e.streamMu.Unlock()
	if !ok {
		return
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	case <-ctx.Done():
	}
}

// AppendLog writes a redacted log line to the hot tier and broadcasts it.
func (e *Engine) AppendLog(ctx context.Context, runID, line string) {
	if line == "" {
		return
	}
	payload := []byte(line + "\n")
	if err := e.tiered.AppendLog(ctx, runID, payload); err != nil {
		e.logger.Warn("append log failed", "run", runID, "error", err)
	}
}

// publishAlert sends a run event to the configured destinations.
func (e *Engine) publishAlert(ctx context.Context, event alert.Event, severity alert.Severity, state *RunState, step *StepState, message string) {
	if e.config.Alerts == nil {
		return
	}
	payload := alert.Payload{
		Event:     event,
		Severity:  severity,
		Timestamp: time.Now().UTC(),
		RunID:     state.ID,
		RunName:   state.Name,
		Status:    string(state.Status),
		Source:    state.Source,
		Commit:    state.Compiled.GitCommitTree,
		Message:   message,
	}
	if step != nil {
		payload.StepID = step.ID
		payload.DurationMS = step.DurationMS
	}
	if e.config.PublicURL != "" {
		payload.URL = fmt.Sprintf("%s/runs/%s", strings.TrimRight(e.config.PublicURL, "/"), state.ID)
	}
	e.config.Alerts.Publish(ctx, payload)

	// The native GitHub integration reports the run as a commit
	// status, so a pull request shows the result directly.
	if payload.Commit != "" {
		if err := e.config.Alerts.ReportGitHubCheck(ctx, payload.Commit, payload); err != nil {
			e.logger.Warn("github check report failed", "run", state.ID, "error", err)
		}
	}
}

// finalize completes a run when no further work is possible.
func (e *Engine) finalize(ctx context.Context, handle *runHandle) {
	var (
		completed     bool
		failed        bool
		runID         string
		finalEvent    alert.Event
		finalSeverity alert.Severity
	)

	handle.with(func(state *RunState) {
		active := 0
		allSucceeded := true
		for _, step := range state.Steps {
			switch step.Status {
			case StepQueued, StepRunning, StepSpotMigrating:
				active++
				allSucceeded = false
			case StepFailed, StepCanceled:
				failed = true
				allSucceeded = false
			}
		}
		runID = state.ID
		if active > 0 {
			if state.Status == RunQueued {
				state.Status = RunRunning
				now := time.Now().UTC()
				state.StartedAt = &now
			}
			return
		}
		if !allSucceeded && !failed {
			return
		}
		if state.IsTerminal() {
			return
		}
		now := time.Now().UTC()
		state.FinishedAt = &now
		if failed {
			state.Status = RunFailed
			state.Message = "one or more steps failed"
			finalEvent = alert.EventRunFailed
			finalSeverity = alert.SeverityError
		} else {
			state.Status = RunSuccess
			state.Message = "all steps succeeded"
			finalEvent = alert.EventRunSucceeded
			finalSeverity = alert.SeverityInfo
		}
		for _, step := range state.Steps {
			step.StorageTier = "COLD"
		}
		state.StorageTier = "COLD"
		completed = true
	})
	if runID == "" {
		return
	}
	e.persist(ctx, handle)
	if !completed {
		return
	}

	if err := e.tiered.ArchiveRun(ctx, runID); err != nil {
		e.logger.Warn("archive run failed", "run", runID, "error", err)
		return
	}
	handle.with(func(state *RunState) {
		state.Archived = true
	})

	if finalEvent != "" {
		e.publishAlert(ctx, finalEvent, finalSeverity, handle.snapshot(), nil, handle.snapshot().Message)
	}
	e.recordRunFinished(handle.snapshot())
}

// persist writes the run state to the hot tier and the object store.
func (e *Engine) persist(ctx context.Context, handle *runHandle) {
	snapshot := handle.snapshot()
	snapshot.Revision = e.revision.Add(1)
	handle.with(func(state *RunState) {
		state.Revision = snapshot.Revision
	})
	if err := e.tiered.SaveState(ctx, snapshot.ID, snapshot); err != nil {
		e.logger.Warn("persist run state failed", "run", snapshot.ID, "error", err)
	}
}

// CreateRun compiles and accepts a new pipeline run.
func (e *Engine) CreateRun(ctx context.Context, spec yaml.PipelineSpec, source, triggerID string) (*RunState, error) {
	compiled, err := dag.Compile(spec, e.compiled)
	if err != nil {
		return nil, err
	}
	id, err := newIdentifier()
	if err != nil {
		return nil, err
	}
	state := NewRunState(id, spec, compiled)
	state.Source = source
	state.TriggerID = triggerID
	state.Status = RunRunning
	now := time.Now().UTC()
	state.StartedAt = &now

	e.mu.Lock()
	handle := &runHandle{state: state}
	e.runs[id] = handle
	e.mu.Unlock()

	// The run is pushed onto the queue. It does not execute until the
	// queue admits it, which is how the concurrency limits are
	// enforced.
	if e.Queue != nil {
		group := spec.Group
		if group == "" {
			group = "pipeline:" + spec.Name
		}
		limit := spec.MaxConcurrent
		if limit <= 0 {
			limit = e.config.MaxConcurrentRuns
		}
		if err := e.Queue.Push(ctx, &queue.Task{
			ID:               id,
			RunID:            id,
			Name:             spec.Name,
			ConcurrencyGroup: group,
			ConcurrencyLimit: limit,
			Created:          time.Now().UTC(),
		}); err != nil {
			e.logger.Warn("the run could not be queued", "run", id, "error", err)
		}
	}

	e.persist(ctx, handle)
	e.recordRunCreated()
	e.logger.Info("run created", "run", id, "name", spec.Name, "steps", len(compiled.Steps))
	return handle.snapshot(), nil
}

// GetRun returns a run by identifier.
func (e *Engine) GetRun(ctx context.Context, id string) (*RunState, bool) {
	e.mu.Lock()
	handle, ok := e.runs[id]
	e.mu.Unlock()
	if ok {
		return handle.snapshot(), true
	}
	var loaded RunState
	if err := e.tiered.LoadState(ctx, id, &loaded); err != nil {
		return nil, false
	}
	if loaded.Steps == nil {
		loaded.Steps = map[string]*StepState{}
	}
	return &loaded, true
}

// ListRuns returns every known run, newest first.
func (e *Engine) ListRuns(ctx context.Context) []*RunState {
	e.mu.Lock()
	handles := make([]*runHandle, 0, len(e.runs))
	for _, handle := range e.runs {
		handles = append(handles, handle)
	}
	e.mu.Unlock()

	states := make([]*RunState, 0, len(handles))
	known := make(map[string]struct{}, len(handles))
	for _, handle := range handles {
		snapshot := handle.snapshot()
		states = append(states, snapshot)
		known[snapshot.ID] = struct{}{}
	}

	identifiers, err := e.tiered.ListRuns(ctx)
	if err == nil {
		for _, id := range identifiers {
			if _, ok := known[id]; ok {
				continue
			}
			var loaded RunState
			if err := e.tiered.LoadState(ctx, id, &loaded); err != nil {
				continue
			}
			if loaded.Steps == nil {
				loaded.Steps = map[string]*StepState{}
			}
			states = append(states, &loaded)
		}
	}

	sortRuns(states)
	return states
}

// sortRuns orders runs by creation time descending.
func sortRuns(states []*RunState) {
	for i := 1; i < len(states); i++ {
		for j := i; j > 0; j-- {
			if !states[j].CreatedAt.After(states[j-1].CreatedAt) {
				break
			}
			states[j-1], states[j] = states[j], states[j-1]
		}
	}
}

// StopRun cancels a run and deletes its workers.
func (e *Engine) StopRun(ctx context.Context, id string) error {
	e.mu.Lock()
	handle, ok := e.runs[id]
	e.mu.Unlock()
	if !ok {
		if _, found := e.GetRun(ctx, id); !found {
			return fmt.Errorf("run %q not found", id)
		}
		return nil
	}

	canceled := false
	handle.with(func(state *RunState) {
		if state.IsTerminal() {
			return
		}
		state.Status = RunCanceled
		state.Message = "canceled by operator"
		now := time.Now().UTC()
		state.FinishedAt = &now
		for _, step := range state.Steps {
			switch step.Status {
			case StepQueued, StepRunning, StepSpotMigrating:
				step.Status = StepCanceled
				step.Message = "canceled by operator"
			}
		}
		canceled = true
	})
	if !canceled {
		return nil
	}
	e.persist(ctx, handle)

	pods, err := e.backend.ListRunPods(ctx, id)
	if err == nil {
		for _, pod := range pods {
			_ = e.backend.DeletePod(ctx, pod.Metadata.Name)
		}
	}
	return nil
}

// RebuildMode selects which steps a rebuild re-executes.
type RebuildMode string

const (
	// RebuildFull re-executes every step and bypasses the cache.
	RebuildFull RebuildMode = "full"
	// RebuildFailed re-executes the failed wave and everything after it.
	RebuildFailed RebuildMode = "failed"
	// RebuildStep re-executes one step and everything downstream of it.
	RebuildStep RebuildMode = "step"
	// RebuildWave re-executes one wave and everything downstream of it.
	RebuildWave RebuildMode = "wave"
)

// RebuildRequest describes a rebuild.
type RebuildRequest struct {
	Mode RebuildMode
	// StepID selects the step for RebuildStep.
	StepID string
	// Wave selects the wave for RebuildWave.
	Wave int
}

// Rebuild creates a new run derived from an existing one.
//
// The rebuild always produces a new run. The original run is left
// untouched so that the history remains auditable.
func (e *Engine) Rebuild(ctx context.Context, id string, request RebuildRequest) (*RunState, error) {
	previous, ok := e.GetRun(ctx, id)
	if !ok {
		return nil, fmt.Errorf("run %q not found", id)
	}
	compiled, err := dag.Compile(previous.Pipeline, e.compiled)
	if err != nil {
		return nil, err
	}
	newID, err := newIdentifier()
	if err != nil {
		return nil, err
	}
	state := NewRunState(newID, previous.Pipeline, compiled)
	state.Source = "rebuild:" + previous.ID
	state.TriggerID = previous.TriggerID
	state.Status = RunRunning
	now := time.Now().UTC()
	state.StartedAt = &now

	// The rebuild is expressed as a set of steps to re-execute. Every
	// other step keeps the result of the previous run, so a targeted
	// rebuild does not redo work that already succeeded.
	requeue := make(map[string]struct{})

	switch request.Mode {
	case RebuildFull:
		for stepID := range state.Steps {
			requeue[stepID] = struct{}{}
		}
	case RebuildFailed:
		failedWave := -1
		for _, step := range previous.Steps {
			if step.Status == StepFailed || step.Status == StepCanceled {
				if failedWave < 0 || step.Wave < failedWave {
					failedWave = step.Wave
				}
			}
		}
		if failedWave < 0 {
			return nil, fmt.Errorf("run %q has no failed wave to rebuild", id)
		}
		for stepID, step := range state.Steps {
			if step.Wave >= failedWave {
				requeue[stepID] = struct{}{}
			}
		}
	case RebuildStep:
		if _, ok := previous.Steps[request.StepID]; !ok {
			return nil, fmt.Errorf("step %q not found in run %q", request.StepID, id)
		}
		// The selected step and everything that transitively depends
		// on it re-executes. Its siblings and its ancestors are kept.
		for _, stepID := range descendantSteps(compiled, request.StepID) {
			requeue[stepID] = struct{}{}
		}
	case RebuildWave:
		for stepID, step := range state.Steps {
			if step.Wave >= request.Wave {
				requeue[stepID] = struct{}{}
			}
		}
	default:
		return nil, fmt.Errorf("unknown rebuild mode %q", request.Mode)
	}

	// Apply the two sets to the new run.
	for stepID, step := range state.Steps {
		if _, rerun := requeue[stepID]; rerun {
			step.Status = StepQueued
			step.PodName = ""
			step.Message = "requeued by rebuild"
			step.FinishedAt = nil
			step.StartedAt = nil
			step.ExitCode = nil
			step.CacheHit = false
			// The point of a rebuild is to actually run the code, so
			// the cache must not short-circuit it.
			step.BypassCache = true
			continue
		}
		// The step is not re-executed, so the previous result is
		// carried over.
		previousStep, ok := previous.Steps[stepID]
		if !ok {
			continue
		}
		preserved := *previousStep
		preserved.PodName = ""
		preserved.BypassCache = true
		state.Steps[stepID] = &preserved
	}

	e.mu.Lock()
	handle := &runHandle{state: state}
	e.runs[newID] = handle
	e.mu.Unlock()
	e.persist(ctx, handle)
	e.logger.Info("rebuild created", "source", id, "run", newID, "mode", request.Mode, "step", request.StepID, "wave", request.Wave)
	return handle.snapshot(), nil
}

// descendantSteps returns a step and every step that transitively
// depends on it.
func descendantSteps(compiled dag.Compiled, stepID string) []string {
	result := []string{stepID}
	changed := true
	for changed {
		changed = false
		for _, step := range compiled.Steps {
			if containsString(result, step.ID) {
				continue
			}
			for _, dependency := range step.DependsOn {
				if containsString(result, dependency) {
					result = append(result, step.ID)
					changed = true
					break
				}
			}
		}
	}
	return result
}

// containsString reports whether a slice contains a value.
func containsString(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

// Summaries returns the minimal run records that the statistics
// manager needs.
//
// It is deliberately cheap. It works from the in-memory run table
// and never touches the object store, so the dashboard can poll it
// frequently.
func (e *Engine) Summaries(ctx context.Context) []stats.RunSummary {
	e.mu.Lock()
	handles := make([]*runHandle, 0, len(e.runs))
	for _, handle := range e.runs {
		handles = append(handles, handle)
	}
	e.mu.Unlock()

	result := make([]stats.RunSummary, 0, len(handles))
	for _, handle := range handles {
		state := handle.snapshot()
		summary := stats.RunSummary{
			ID:         state.ID,
			Name:       state.Name,
			Status:     string(state.Status),
			CreatedAt:  state.CreatedAt,
			FinishedAt: state.FinishedAt,
			Steps:      make(map[string]stats.StepSummary, len(state.Steps)),
		}
		for id, step := range state.Steps {
			summary.Steps[id] = stats.StepSummary{
				ID:         step.ID,
				Wave:       step.Wave,
				Status:     string(step.Status),
				DurationMS: step.DurationMS,
				CacheHit:   step.CacheHit,
				Attempt:    step.Attempt,
			}
		}
		result = append(result, summary)
	}
	return result
}

// ReadLog returns the current log contents of a run.
//
// A finished run has its hot log removed when it is archived, so the
// durable archive is used as the fallback. The UI must be able to show
// the log of a run that finished minutes ago.
func (e *Engine) ReadLog(ctx context.Context, id string) ([]byte, error) {
	contents, err := e.tiered.ReadLog(ctx, id)
	if err == nil && len(contents) > 0 {
		return contents, nil
	}
	archived, archiveErr := e.tiered.LoadArchivedLog(ctx, id)
	if archiveErr == nil {
		return archived, nil
	}
	if err != nil {
		return nil, err
	}
	return nil, archiveErr
}

// Subscribe returns a live log channel for a run.
func (e *Engine) Subscribe(id string) (<-chan []byte, func()) {
	return e.tiered.Subscribe(id)
}

// SecretMetadata exposes the secret vault metadata.
func (e *Engine) SecretMetadata() []storage.SecretMetadata {
	if e.vault == nil {
		return nil
	}
	metadata, err := e.vault.List(context.Background())
	if err != nil {
		return nil
	}
	return metadata
}

// SetSecret registers a secret value.
func (e *Engine) SetSecret(ctx context.Context, name, value string) (storage.SecretMetadata, error) {
	if e.vault == nil {
		return storage.SecretMetadata{}, fmt.Errorf("secret store is not configured")
	}
	return e.vault.Set(ctx, name, value)
}

// DeleteSecret removes a secret.
func (e *Engine) DeleteSecret(ctx context.Context, name string) error {
	if e.vault == nil {
		return fmt.Errorf("secret store is not configured")
	}
	return e.vault.Delete(ctx, name)
}

// Health describes the controller health for the UI and probes.
type Health struct {
	Healthy       bool   `json:"healthy"`
	Leader        bool   `json:"leader"`
	Identity      string `json:"identity"`
	LeaderURL     string `json:"leader_url,omitempty"`
	LeaseHolder   string `json:"lease_holder,omitempty"`
	Backend       string `json:"backend"`
	ObjectBackend string `json:"object_backend"`
	Simulator     bool   `json:"simulator"`
}

// Health returns the current controller health.
func (e *Engine) Health() Health {
	holder, _ := e.leaseHolder.Load().(string)
	return Health{
		Healthy:       true,
		Leader:        e.leader.Load(),
		Identity:      e.config.Identity,
		LeaderURL:     e.LeaderURL(),
		LeaseHolder:   holder,
		Backend:       e.backend.Name(),
		ObjectBackend: e.tiered.Store().Backend(),
		Simulator:     e.backend.Name() == "simulator",
	}
}

// newIdentifier produces a random run identifier.
func newIdentifier() (string, error) {
	buffer := make([]byte, 12)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

// mergeAnnotations merges annotation maps.
func mergeAnnotations(existing, updates map[string]string) map[string]string {
	if existing == nil {
		existing = map[string]string{}
	}
	for key, value := range updates {
		existing[key] = value
	}
	return existing
}

// marshalJSON is a small helper used by the API layer.
func marshalJSON(value any) ([]byte, error) {
	return json.Marshal(value)
}
