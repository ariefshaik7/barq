package controller

import (
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"barq/internal/alert"
	"barq/internal/apierr"
	"barq/internal/auth"
	"barq/internal/dag"
	"barq/internal/storage"
	"barq/internal/yaml"
)

// Handler builds the HTTP handler for the controller.
func (e *Engine) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public endpoints. The health check is intentionally open so that
	// Kubernetes probes work without credentials.
	mux.HandleFunc("GET /api/v1/health", e.handleHealth)
	mux.HandleFunc("GET /healthz", e.handleHealth)
	mux.HandleFunc("GET /api/v1/auth/status", e.handleAuthStatus)
	mux.HandleFunc("POST /api/v1/auth/login", e.handleLogin)
	mux.HandleFunc("POST /api/v1/auth/signup", e.handleSignup)
	mux.HandleFunc("POST /api/v1/auth/logout", e.handleLogout)
	mux.HandleFunc("GET /api/v1/auth/session", e.handleSession)
	mux.HandleFunc("GET /api/v1/auth/oidc", e.handleOIDCStart)
	mux.HandleFunc("GET /api/v1/auth/oidc/callback", e.handleOIDCCallback)

	// Control plane. Read operations need a viewer, mutations need a
	// builder, and administration needs an admin.
	mux.HandleFunc("GET /api/v1/runs", e.requireAuth(auth.RoleViewer, e.handleListRuns))
	mux.HandleFunc("POST /api/v1/runs", e.requireAuth(auth.RoleBuilder, e.handleCreateRun))
	mux.HandleFunc("GET /api/v1/runs/{id}", e.requireAuth(auth.RoleViewer, e.handleGetRun))
	mux.HandleFunc("GET /api/v1/runs/{id}/logs", e.requireAuth(auth.RoleViewer, e.handleRunLogs))
	mux.HandleFunc("POST /api/v1/runs/{id}/stop", e.requireAuth(auth.RoleBuilder, e.handleStopRun))
	mux.HandleFunc("POST /api/v1/runs/{id}/rebuild", e.requireAuth(auth.RoleBuilder, e.handleRebuildRun))
	mux.HandleFunc("GET /api/v1/plugins", e.requireAuth(auth.RoleViewer, e.handlePluginCatalog))
	mux.HandleFunc("GET /api/v1/stats", e.requireAuth(auth.RoleViewer, e.handleStats))
	mux.HandleFunc("GET /api/v1/system/health", e.requireAuth(auth.RoleViewer, e.handleSystemHealth))
	mux.HandleFunc("GET /api/v1/system/queue", e.requireAuth(auth.RoleViewer, e.handleQueueStats))
	mux.HandleFunc("GET /metrics", e.handleMetrics)
	mux.HandleFunc("GET /api/v1/alerts", e.requireAuth(auth.RoleAdmin, e.handleListAlerts))
	mux.HandleFunc("POST /api/v1/alerts", e.requireAuth(auth.RoleAdmin, e.handleAddAlert))
	mux.HandleFunc("DELETE /api/v1/alerts/{id}", e.requireAuth(auth.RoleAdmin, e.handleDeleteAlert))
	mux.HandleFunc("GET /api/v1/crons", e.requireAuth(auth.RoleBuilder, e.handleListCron))
	mux.HandleFunc("POST /api/v1/crons", e.requireAuth(auth.RoleBuilder, e.handleCreateCron))
	mux.HandleFunc("DELETE /api/v1/crons/{id}", e.requireAuth(auth.RoleBuilder, e.handleDeleteCron))
	mux.HandleFunc("GET /api/v1/approvals", e.requireAuth(auth.RoleBuilder, e.handleListApprovals))
	mux.HandleFunc("POST /api/v1/runs/{id}/approve", e.requireAuth(auth.RoleBuilder, e.handleRequestApproval))
	mux.HandleFunc("POST /api/v1/runs/{id}/decision", e.requireAuth(auth.RoleBuilder, e.handleApprovalDecision))
	mux.HandleFunc("POST /api/v1/config/apply", e.requireAuth(auth.RoleAdmin, e.handleConfigApply))
	mux.HandleFunc("POST /api/v1/config/validate", e.requireAuth(auth.RoleAdmin, e.handleConfigValidate))

	// Administration.
	mux.HandleFunc("GET /api/v1/secrets", e.requireAuth(auth.RoleAdmin, e.handleListSecrets))
	mux.HandleFunc("POST /api/v1/secrets", e.requireAuth(auth.RoleAdmin, e.handleSetSecret))
	mux.HandleFunc("DELETE /api/v1/secrets/{name}", e.requireAuth(auth.RoleAdmin, e.handleDeleteSecret))
	mux.HandleFunc("GET /api/v1/users", e.requireAuth(auth.RoleAdmin, e.handleListUsers))
	mux.HandleFunc("PATCH /api/v1/users/{id}", e.requireAuth(auth.RoleAdmin, e.handleUpdateUser))
	mux.HandleFunc("DELETE /api/v1/users/{id}", e.requireAuth(auth.RoleAdmin, e.handleDeleteUser))
	mux.HandleFunc("PATCH /api/v1/profile", e.requireAuth(auth.RoleViewer, e.handleProfile))
	mux.HandleFunc("POST /api/v1/profile/password", e.requireAuth(auth.RoleViewer, e.handleChangePassword))
	mux.HandleFunc("GET /api/v1/tokens", e.requireAuth(auth.RoleViewer, e.handleListTokens))
	mux.HandleFunc("POST /api/v1/tokens", e.requireAuth(auth.RoleViewer, e.handleCreateToken))
	mux.HandleFunc("DELETE /api/v1/tokens/{id}", e.requireAuth(auth.RoleViewer, e.handleRevokeToken))

	// Webhooks authenticate the sender, not the operator.
	mux.HandleFunc("POST /api/v1/webhooks/github", e.handleForgeWebhook("github"))
	mux.HandleFunc("POST /api/v1/webhooks/gitlab", e.handleForgeWebhook("gitlab"))

	// The local object store exposes a signed relay so workers can
	// exchange workspaces without an external S3 bucket. It is
	// registered before the UI catch-all so that it is not shadowed.
	if relay, ok := e.tiered.Store().(storage.RelayProvider); ok {
		mux.Handle("GET /_objects/{key...}", relay.RelayHandler())
		mux.Handle("PUT /_objects/{key...}", relay.RelayHandler())
	}

	mux.HandleFunc("GET /{path...}", e.handleUI)

	// The middleware chain is applied around the whole mux. The order
	// matters: the request identifier is assigned first so that every
	// later step can log it, the security headers are set on every
	// response, and the rate limiter runs last before the handler.
	return Chain(mux,
		requestIDMiddleware,
		securityHeadersMiddleware,
		rateLimitMiddleware(newRateLimiter(20)),
	)
}

// guard applies authentication and leader forwarding to a handler.
//
// It is retained for the endpoints that are reachable without a
// session in local mode.
func (e *Engine) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !e.IsLeader() {
			if e.forwardToLeader(w, r) {
				return
			}
		}
		next(w, r)
	}
}

// authorize validates the API token using a constant time comparison.
func authorize(token string, r *http.Request) bool {
	if token == "" {
		return true
	}
	provided := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(provided), "bearer ") {
		provided = strings.TrimSpace(provided[len("bearer "):])
	} else if header := r.Header.Get("X-Barq-Token"); header != "" {
		provided = strings.TrimSpace(header)
	}
	return hmac.Equal([]byte(provided), []byte(token))
}

// forwardToLeader proxies a request to the active leader.
//
// Followers never mutate state. Every control plane request is routed
// to the leader so that a Service load balancer cannot split state
// across replicas.
func (e *Engine) forwardToLeader(w http.ResponseWriter, r *http.Request) bool {
	target := e.LeaderURL()
	if target == "" {
		writeError(w, http.StatusServiceUnavailable, "no leader is currently elected")
		return true
	}
	parsed, err := url.Parse(target)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "leader URL is invalid")
		return true
	}
	if parsed.Host == "" {
		writeError(w, http.StatusServiceUnavailable, "leader URL is not routable")
		return true
	}
	proxy := httputil.NewSingleHostReverseProxy(parsed)
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		writeError(w, http.StatusBadGateway, "leader is unreachable")
	}
	proxy.ServeHTTP(w, r)
	return true
}

// handleHealth reports controller health.
func (e *Engine) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, e.Health())
}

// handleListRuns returns every run.
func (e *Engine) handleListRuns(w http.ResponseWriter, r *http.Request) {
	runs := e.ListRuns(r.Context())
	page := parsePagination(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"runs":     slice(runs, page),
		"page":     page.Page,
		"per_page": page.PerPage,
		"total":    len(runs),
	})
}

// handleGetRun returns a single run.
func (e *Engine) handleGetRun(w http.ResponseWriter, r *http.Request) {
	state, ok := e.GetRun(r.Context(), r.PathValue("id"))
	if !ok {
		writeAPIError(w, apierr.NotFound("the run does not exist"))
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// handleCreateRun accepts a pipeline definition.
func (e *Engine) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeAPIError(w, apierr.BadRequest("unable to read the request body"))
		return
	}
	spec, err := yaml.Parse(body)
	if err != nil {
		writeAPIError(w, apierr.BadRequest(err.Error()))
		return
	}
	state, err := e.CreateRun(r.Context(), spec, "api", "")
	if err != nil {
		writeAPIError(w, apierr.BadRequest(err.Error()))
		return
	}
	writeJSON(w, http.StatusCreated, state)
}

// handleStopRun cancels a run.
func (e *Engine) handleStopRun(w http.ResponseWriter, r *http.Request) {
	if err := e.StopRun(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "canceled"})
}

// handleRebuildRun creates a derived run.
func (e *Engine) handleRebuildRun(w http.ResponseWriter, r *http.Request) {
	request := RebuildRequest{Mode: RebuildFull}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if len(body) > 0 {
		var payload struct {
			Mode   string `json:"mode"`
			StepID string `json:"step_id"`
			Wave   *int   `json:"wave"`
		}
		if err := json.Unmarshal(body, &payload); err == nil {
			if payload.Mode != "" {
				request.Mode = RebuildMode(payload.Mode)
			}
			request.StepID = payload.StepID
			if payload.Wave != nil {
				request.Wave = *payload.Wave
			}
		}
	}
	switch request.Mode {
	case RebuildFull, RebuildFailed, RebuildStep, RebuildWave:
	default:
		writeAPIError(w, apierr.BadRequest("mode must be full, failed, step or wave"))
		return
	}
	if request.Mode == RebuildStep && request.StepID == "" {
		writeAPIError(w, apierr.BadRequest("step_id is required for a step rebuild"))
		return
	}
	if request.Mode == RebuildWave && request.Wave < 0 {
		writeAPIError(w, apierr.BadRequest("wave is required for a wave rebuild"))
		return
	}
	state, err := e.Rebuild(r.Context(), r.PathValue("id"), request)
	if err != nil {
		// A rebuild that does not make sense in the current state is
		// unprocessable rather than malformed. The request was
		// understood; the state does not permit it.
		if strings.Contains(err.Error(), "not found") {
			writeAPIError(w, apierr.NotFound(err.Error()))
			return
		}
		writeAPIError(w, apierr.Unprocessable(err.Error()))
		return
	}
	writeJSON(w, http.StatusCreated, state)
}

// handleRunLogs streams run logs over Server-Sent Events.
func (e *Engine) handleRunLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	if history, err := e.ReadLog(r.Context(), id); err == nil && len(history) > 0 {
		fmt.Fprintf(w, "event: log\ndata: %s\n\n", jsonString(string(history)))
		flusher.Flush()
	}

	updates, cancel := e.Subscribe(id)
	defer cancel()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case chunk, ok := <-updates:
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: log\ndata: %s\n\n", jsonString(string(chunk)))
			flusher.Flush()
		}
	}
}

// handleStats returns the usage and reliability statistics.
func (e *Engine) handleStats(w http.ResponseWriter, r *http.Request) {
	if e.config.Stats == nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	window := 30 * 24 * time.Hour
	if raw := r.URL.Query().Get("window"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "the window must be a duration such as 720h")
			return
		}
		window = parsed
	}
	snapshot, err := e.config.Stats.Snapshot(r.Context(), window)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

// handleSystemHealth returns the dependency health report.
func (e *Engine) handleSystemHealth(w http.ResponseWriter, r *http.Request) {
	if e.config.Health == nil {
		writeJSON(w, http.StatusOK, map[string]any{"state": "unknown"})
		return
	}
	writeJSON(w, http.StatusOK, e.config.Health.Report())
}

// handleQueueStats returns the state of the task queue.
func (e *Engine) handleQueueStats(w http.ResponseWriter, r *http.Request) {
	e.updateQueueMetrics()
	writeJSON(w, http.StatusOK, map[string]any{"queue": e.queueStats()})
}

// handleMetrics serves the Prometheus exposition endpoint.
//
// The endpoint is deliberately unauthenticated, because a scraper
// cannot present a session. It exposes counts and timings, never
// identifiers that could leak information about a pipeline.
func (e *Engine) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if e.metrics == nil {
		writeError(w, http.StatusNotFound, "metrics are not enabled")
		return
	}
	e.updateQueueMetrics()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(e.metrics.Gather()))
}

// handleListAlerts returns the alert destinations.
func (e *Engine) handleListAlerts(w http.ResponseWriter, r *http.Request) {
	if e.config.Alerts == nil {
		writeJSON(w, http.StatusOK, map[string]any{"destinations": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"destinations": e.config.Alerts.List()})
}

// handleAddAlert registers an alert destination.
func (e *Engine) handleAddAlert(w http.ResponseWriter, r *http.Request) {
	if e.config.Alerts == nil {
		writeError(w, http.StatusServiceUnavailable, "alerting is not configured")
		return
	}
	var destination alert.Destination
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&destination); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if destination.URL == "" {
		writeError(w, http.StatusBadRequest, "a destination URL is required")
		return
	}
	parsed, err := url.Parse(destination.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		writeError(w, http.StatusBadRequest, "the destination URL must be an http or https URL")
		return
	}
	if destination.Name == "" {
		destination.Name = destination.ID
	}
	destination.Enabled = true
	e.config.Alerts.Add(destination)
	writeJSON(w, http.StatusCreated, destination)
}

// handleDeleteAlert removes an alert destination.
func (e *Engine) handleDeleteAlert(w http.ResponseWriter, r *http.Request) {
	if e.config.Alerts == nil {
		writeError(w, http.StatusServiceUnavailable, "alerting is not configured")
		return
	}
	e.config.Alerts.Remove(r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handlePluginCatalog lists the built-in plugins.
func (e *Engine) handlePluginCatalog(w http.ResponseWriter, r *http.Request) {
	catalog := make([]map[string]string, 0, len(dag.BuiltinPlugins))
	for name, command := range dag.BuiltinPlugins {
		catalog = append(catalog, map[string]string{"name": name, "entrypoint": command})
	}
	writeJSON(w, http.StatusOK, map[string]any{"plugins": catalog})
}

// handleListSecrets returns secret metadata only.
func (e *Engine) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"secrets": e.SecretMetadata()})
}

// handleSetSecret registers a secret value.
func (e *Engine) handleSetSecret(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	metadata, err := e.SetSecret(r.Context(), request.Name, request.Value)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, metadata)
}

// handleDeleteSecret removes a secret.
func (e *Engine) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	if err := e.DeleteSecret(r.Context(), r.PathValue("name")); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, status int, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

// writeError writes a JSON error response.
//
// It is kept for the call sites that only have a message. The typed
// apierr values are preferred because they carry a stable code.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// writeAPIError writes a typed API error.
//
// The response carries both a stable code and the message, so that a
// client can branch on the code and a human can read the message.
func writeAPIError(w http.ResponseWriter, err error) {
	apiError := apierr.From(err)
	if apiError == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	writeJSON(w, apiError.Status, map[string]any{
		"error": apiError.Message,
		"code":  apiError.Code,
	})
}

// jsonString encodes a string as a JSON scalar for SSE payloads.
func jsonString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(encoded)
}
