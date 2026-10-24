package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"barq/internal/apierr"
	"barq/internal/forge"
	"barq/internal/yaml"
)

// ForgeManager holds the source control providers.
//
// The manager is deliberately tiny. Barq supports a small number of
// providers, and each one is configured statically. A cache with a
// short TTL would be premature.
type ForgeManager struct {
	mu     sync.RWMutex
	forges map[string]forge.Forge
	logger *slog.Logger
}

// NewForgeManager creates a manager.
func NewForgeManager(logger *slog.Logger) *ForgeManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &ForgeManager{
		forges: make(map[string]forge.Forge),
		logger: logger,
	}
}

// Register adds a provider.
func (m *ForgeManager) Register(provider forge.Forge) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.forges[provider.Name()] = provider
}

// Get returns a provider by name.
func (m *ForgeManager) Get(name string) (forge.Forge, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	provider, ok := m.forges[name]
	return provider, ok
}

// Names returns the registered provider names.
func (m *ForgeManager) Names() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.forges))
	for name := range m.forges {
		names = append(names, name)
	}
	return names
}

// handleForgeWebhook validates a webhook and starts a pipeline.
//
// The flow follows the pattern that mature CI systems converge on:
//
//  1. Verify the signature. A bad signature is a 401 and nothing
//     else happens.
//  2. Parse the payload. A valid payload that describes an
//     uninteresting event is acknowledged with a 200 and no run.
//  3. Check the branch allow list.
//  4. Fetch the pipeline definition from the pushed commit, so that
//     the definition that runs is the one in the commit.
//  5. Check the delivery identifier for a replay.
//  6. Start the run.
//
// The response is sent as soon as the run has been created. The
// pipeline does not block the webhook.
func (e *Engine) handleForgeWebhook(providerName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		provider, ok := e.Forges.Get(providerName)
		if !ok {
			writeAPIError(w, apierr.NotFound("the provider is not configured"))
			return
		}

		hook, err := provider.Hook(r.Context(), r)
		if err != nil {
			if errors.Is(err, forge.ErrIgnoreEvent) {
				writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
				return
			}
			writeAPIError(w, apierr.Unauthorized("the webhook could not be verified"))
			return
		}

		if !e.branchAllowed(hook.Branch) {
			writeJSON(w, http.StatusOK, map[string]string{"status": "branch not allowed"})
			return
		}

		delivery := deliveryID(r)
		if e.alreadyTriggered(delivery) {
			writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
			return
		}

		definition, err := provider.Pipeline(r.Context(), e.forgeToken(providerName),
			hook.Repository, e.config.pipelinePath, hook.Commit)
		if err != nil {
			if errors.Is(err, forge.ErrNotFound) {
				writeAPIError(w, apierr.NotFound("the pipeline definition does not exist in that commit"))
				return
			}
			writeAPIError(w, apierr.Unavailable("the pipeline definition could not be fetched"))
			return
		}

		spec, err := yaml.Parse(definition)
		if err != nil {
			writeAPIError(w, apierr.Unprocessable("the pipeline definition is invalid: "+err.Error()))
			return
		}

		state, err := e.CreateRun(r.Context(), spec, "webhook:"+providerName, delivery)
		if err != nil {
			writeAPIError(w, apierr.Unprocessable(err.Error()))
			return
		}
		e.markTriggered(delivery)

		// The run is reported to the forge as a commit status so that
		// the pull request shows the result.
		go e.reportStatus(provider, hook, state)

		writeJSON(w, http.StatusAccepted, map[string]string{
			"run_id": state.ID,
			"commit": hook.Commit,
		})
	}
}

// reportStatus publishes the run outcome to the forge.
//
// The status is published when the run starts and again when it
// finishes. It runs in the background because a forge that is slow or
// down must never stall the pipeline.
func (e *Engine) reportStatus(provider forge.Forge, hook *forge.HookRequest, state *RunState) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	token := e.forgeToken(provider.Name())
	if err := provider.Status(ctx, token, hook.Repository, hook.Commit, forge.Status{
		State:       "in_progress",
		Target:      e.runURL(state.ID),
		Description: "Barq pipeline " + state.Name + " is running",
		Context:     "barq/" + state.Name,
	}); err != nil {
		e.logger.Warn("publish the initial commit status failed", "error", err)
	}

	// Wait for the run to reach a terminal state, then report it.
	deadline := time.Now().Add(2 * time.Hour)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)
		current, ok := e.GetRun(ctx, state.ID)
		if !ok || !current.IsTerminal() {
			continue
		}
		status := forge.Status{
			State:       "success",
			Target:      e.runURL(current.ID),
			Description: "Barq pipeline " + current.Name + " succeeded",
			Context:     "barq/" + current.Name,
		}
		switch current.Status {
		case RunFailed:
			status.State = "failure"
			status.Description = "Barq pipeline " + current.Name + " failed"
		case RunCanceled:
			status.State = "error"
			status.Description = "Barq pipeline " + current.Name + " was canceled"
		}
		if err := provider.Status(ctx, token, hook.Repository, hook.Commit, status); err != nil {
			e.logger.Warn("publish the commit status failed", "error", err)
		}
		return
	}
}

// branchAllowed reports whether a branch may trigger a run.
func (e *Engine) branchAllowed(branch string) bool {
	if len(e.config.AllowedBranches) == 0 {
		return true
	}
	ref := "refs/heads/" + branch
	if _, ok := e.config.AllowedBranches[ref]; ok {
		return true
	}
	_, ok := e.config.AllowedBranches[branch]
	return ok
}

// alreadyTriggered reports whether a delivery was processed before.
func (e *Engine) alreadyTriggered(delivery string) bool {
	if delivery == "" {
		return false
	}
	e.triggerMu.Lock()
	defer e.triggerMu.Unlock()
	_, ok := e.triggeredIDs[delivery]
	return ok
}

// markTriggered records a processed delivery.
func (e *Engine) markTriggered(delivery string) {
	if delivery == "" {
		return
	}
	e.triggerMu.Lock()
	defer e.triggerMu.Unlock()
	e.triggeredIDs[delivery] = struct{}{}
}

// runURL builds the URL of a run.
func (e *Engine) runURL(runID string) string {
	if e.config.PublicURL == "" {
		return ""
	}
	return strings.TrimRight(e.config.PublicURL, "/") + "/?run=" + runID
}

// forgeToken returns the token to use for a provider.
func (e *Engine) forgeToken(providerName string) string {
	return e.config.ForgeTokens[providerName]
}

// deliveryID extracts the delivery identifier of a webhook.
//
// Each provider uses a different header. The identifiers are
// provider specific, so they are prefixed to avoid a collision
// between two providers.
func deliveryID(r *http.Request) string {
	for _, header := range []string{
		"X-GitHub-Delivery",
		"X-Gitlab-Event-UUID",
		"X-Gitlab-Event",
	} {
		if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
			return value
		}
	}
	return ""
}

// webhookError renders a webhook failure.
func webhookError(w http.ResponseWriter, err error) {
	writeAPIError(w, apierr.From(fmt.Errorf("the webhook failed: %w", err)))
}
