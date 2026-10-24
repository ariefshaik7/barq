package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"barq/internal/apierr"
	"barq/internal/auth"
	"barq/internal/cron"
	"barq/internal/yaml"
)

// crons is the store of the scheduled pipelines.
var crons = cron.NewStore()

// handleListCron returns the scheduled pipelines.
func (e *Engine) handleListCron(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"crons": crons.List()})
}

// handleCreateCron registers a scheduled pipeline.
func (e *Engine) handleCreateCron(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name       string `json:"name"`
		Pipeline   string `json:"pipeline"`
		Schedule   string `json:"schedule"`
		Branch     string `json:"branch"`
		Repository string `json:"repository"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&request); err != nil {
		writeAPIError(w, apierr.BadRequest("invalid request body"))
		return
	}
	if request.Name == "" {
		writeAPIError(w, apierr.BadRequest("a name is required"))
		return
	}
	if request.Pipeline == "" {
		writeAPIError(w, apierr.BadRequest("a pipeline is required"))
		return
	}
	entry := cron.Entry{
		Name:       request.Name,
		Pipeline:   request.Pipeline,
		Schedule:   request.Schedule,
		Branch:     request.Branch,
		Repository: request.Repository,
		Enabled:    true,
	}
	if err := crons.Put(entry); err != nil {
		writeAPIError(w, apierr.BadRequest(err.Error()))
		return
	}
	stored, _ := crons.Get(request.Name)
	writeJSON(w, http.StatusCreated, stored)
}

// handleDeleteCron removes a scheduled pipeline.
func (e *Engine) handleDeleteCron(w http.ResponseWriter, r *http.Request) {
	crons.Delete(r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// cronLoop fires the scheduled pipelines.
func (e *Engine) cronLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			e.fireDueCrons(ctx, now)
		}
	}
}

// fireDueCrons starts the pipelines that are due.
func (e *Engine) fireDueCrons(ctx context.Context, now time.Time) {
	for _, entry := range crons.Due(now) {
		e.logger.Info("firing the scheduled pipeline", "cron", entry.Name, "pipeline", entry.Pipeline)
		// The pipeline definition is fetched from the forge when the
		// entry names a repository. An entry without a repository is
		// a placeholder that the operator wires up through the API.
		if entry.Repository == "" {
			continue
		}
		provider, ok := e.Forges.Get("github")
		if !ok {
			continue
		}
		definition, err := provider.Pipeline(ctx, e.forgeToken("github"),
			entry.Repository, e.config.pipelinePath, entry.Branch)
		if err != nil {
			e.logger.Warn("the scheduled pipeline could not be fetched",
				"cron", entry.Name, "error", err)
			continue
		}
		spec, err := parsePipeline(definition)
		if err != nil {
			e.logger.Warn("the scheduled pipeline is invalid", "cron", entry.Name, "error", err)
			continue
		}
		if _, err := e.CreateRun(ctx, spec, "cron:"+entry.Name, ""); err != nil {
			e.logger.Warn("the scheduled run could not be created",
				"cron", entry.Name, "error", err)
		}
	}
}

// approvals holds the runs that are waiting for a human decision.
type approvals struct {
	mu    sync.Mutex
	items map[string]*approval
}

// approval is a run that is waiting for a decision.
type approval struct {
	RunID     string    `json:"run_id"`
	Requested time.Time `json:"requested"`
	By        string    `json:"by,omitempty"`
	Approved  bool      `json:"approved"`
	Declined  bool      `json:"declined"`
	Comment   string    `json:"comment,omitempty"`
}

// runApprovals is the process wide approval registry.
var runApprovals = &approvals{items: make(map[string]*approval)}

// RequestApproval puts a run into the blocked state.
func (e *Engine) RequestApproval(ctx context.Context, runID, by, comment string) error {
	if _, ok := e.GetRun(ctx, runID); !ok {
		return fmt.Errorf("the run %q does not exist", runID)
	}
	runApprovals.mu.Lock()
	runApprovals.items[runID] = &approval{
		RunID:     runID,
		Requested: time.Now().UTC(),
		By:        by,
		Approved:  false,
		Comment:   comment,
	}
	runApprovals.mu.Unlock()

	e.mu.Lock()
	handle, exists := e.runs[runID]
	e.mu.Unlock()
	if exists {
		handle.with(func(s *RunState) {
			s.Status = StatusBlocked
			s.Message = "waiting for an approval"
		})
		e.persist(ctx, handle)
	}
	return nil
}

// DecideApproval approves or declines a blocked run.
//
// An approved run is released into the queue, where it waits for a
// concurrency slot like any other run. A declined run is canceled.
func (e *Engine) DecideApproval(ctx context.Context, runID string, approve bool, comment, by string) error {
	runApprovals.mu.Lock()
	item, ok := runApprovals.items[runID]
	if !ok {
		runApprovals.mu.Unlock()
		return fmt.Errorf("the run %q is not waiting for an approval", runID)
	}
	if approve {
		item.Approved = true
	} else {
		item.Declined = true
	}
	item.Comment = comment
	item.By = by
	runApprovals.mu.Unlock()

	e.mu.Lock()
	handle, exists := e.runs[runID]
	e.mu.Unlock()
	if !exists {
		return fmt.Errorf("the run %q does not exist", runID)
	}
	if approve {
		handle.with(func(s *RunState) {
			s.Status = RunRunning
			s.Message = "approved"
		})
		e.persist(ctx, handle)
		return nil
	}
	return e.StopRun(ctx, runID)
}

// handleListApprovals returns the runs that are waiting for a decision.
func (e *Engine) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	runApprovals.mu.Lock()
	defer runApprovals.mu.Unlock()
	items := make([]*approval, 0, len(runApprovals.items))
	for _, item := range runApprovals.items {
		if !item.Approved && !item.Declined {
			copied := *item
			items = append(items, &copied)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": items})
}

// handleApprovalDecision approves or declines a blocked run.
func (e *Engine) handleApprovalDecision(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Approve bool   `json:"approve"`
		Comment string `json:"comment"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&request); err != nil {
		writeAPIError(w, apierr.BadRequest("invalid request body"))
		return
	}
	user, _ := r.Context().Value(userContextKey).(*auth.User)
	by := ""
	if user != nil {
		by = user.Email
	}
	if err := e.DecideApproval(r.Context(), r.PathValue("id"), request.Approve, request.Comment, by); err != nil {
		writeAPIError(w, apierr.NotFound(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}

// handleRequestApproval puts a run into the blocked state.
func (e *Engine) handleRequestApproval(w http.ResponseWriter, r *http.Request) {
	user, _ := r.Context().Value(userContextKey).(*auth.User)
	by := ""
	if user != nil {
		by = user.Email
	}
	var request struct {
		Comment string `json:"comment"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&request)
	if err := e.RequestApproval(r.Context(), r.PathValue("id"), by, request.Comment); err != nil {
		writeAPIError(w, apierr.NotFound(err.Error()))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "waiting for approval"})
}

// parsePipeline parses a pipeline definition.
func parsePipeline(definition []byte) (yaml.PipelineSpec, error) {
	return yaml.Parse(definition)
}
