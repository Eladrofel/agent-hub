package server

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/Eladrofel/agent-hub/gateway/internal/auth"
	"github.com/Eladrofel/agent-hub/gateway/internal/events"
	"github.com/Eladrofel/agent-hub/gateway/internal/sessions"
)

// =============================================================================
// POST /v1/sessions/start
// =============================================================================

type sessionStartRequest struct {
	ClaudeSessionID string         `json:"claude_session_id"`
	ProjectSlug     string         `json:"project_slug,omitempty"`
	VMHostname      string         `json:"vm_hostname,omitempty"`
	CWD             string         `json:"cwd,omitempty"`
	WorktreePath    string         `json:"worktree_path,omitempty"`
	Branch          string         `json:"branch,omitempty"`
	BaseBranch      string         `json:"base_branch,omitempty"`
	GitHeadSHA      string         `json:"git_head_sha,omitempty"`
	StartReason     string         `json:"start_reason,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
}

func (a *App) handleSessionStart(w http.ResponseWriter, r *http.Request) {
	caller := auth.FromContext(r.Context())

	var req sessionStartRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.ClaudeSessionID == "" {
		writeError(w, http.StatusBadRequest, "claude_session_id_required", "field claude_session_id is required")
		return
	}

	projectID, err := events.ResolveProjectID(r.Context(), a.Store.Pool, req.ProjectSlug)
	if err != nil {
		a.writeResolveError(w, err, "project_slug", req.ProjectSlug)
		return
	}

	sess, err := sessions.Start(r.Context(), a.Store.Pool, sessions.StartParams{
		ClaudeSessionID: req.ClaudeSessionID,
		AgentID:         caller.ID,
		ProjectID:       projectID,
		VMHostname:      stringPtr(req.VMHostname),
		CWD:             stringPtr(req.CWD),
		WorktreePath:    stringPtr(req.WorktreePath),
		Branch:          stringPtr(req.Branch),
		BaseBranch:      stringPtr(req.BaseBranch),
		GitHeadSHA:      stringPtr(req.GitHeadSHA),
		StartReason:     stringPtr(req.StartReason),
		Metadata:        req.Metadata,
	})
	if err != nil {
		a.Logger.Error("session start failed", "err", err)
		writeError(w, http.StatusInternalServerError, "start_failed", err.Error())
		return
	}

	// Side-effect: emit session.started. Best-effort — failure logged but
	// doesn't fail the call, since the durable state (agent_sessions row)
	// already landed.
	a.emitLifecycleEvent(r, caller.ID, "session.started", sess.ID, req.ClaudeSessionID,
		"session started for "+caller.Name, map[string]any{
			"start_reason": req.StartReason,
			"branch":       req.Branch,
		})

	writeJSON(w, http.StatusCreated, sess)
}

// =============================================================================
// POST /v1/sessions/checkpoint
// =============================================================================

type sessionCheckpointRequest struct {
	ClaudeSessionID string         `json:"claude_session_id"`
	TaskKey         string         `json:"task_key,omitempty"`
	CheckpointType  string         `json:"checkpoint_type,omitempty"`
	Status          string         `json:"status,omitempty"`
	CurrentGoal     string         `json:"current_goal,omitempty"`
	Summary         string         `json:"summary"`
	NextActions     []any          `json:"next_actions,omitempty"`
	OpenQuestions   []any          `json:"open_questions,omitempty"`
	FilesRelevant   []any          `json:"files_relevant,omitempty"`
	Risks           []any          `json:"risks,omitempty"`
	Payload         map[string]any `json:"payload,omitempty"`
}

func (a *App) handleSessionCheckpoint(w http.ResponseWriter, r *http.Request) {
	caller := auth.FromContext(r.Context())

	var req sessionCheckpointRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.ClaudeSessionID == "" {
		writeError(w, http.StatusBadRequest, "claude_session_id_required", "field claude_session_id is required")
		return
	}
	if req.Summary == "" {
		writeError(w, http.StatusBadRequest, "summary_required", "field summary is required")
		return
	}

	sess, err := sessions.GetByClaudeSessionID(r.Context(), a.Store.Pool, req.ClaudeSessionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeErrorWithDetails(w, http.StatusUnprocessableEntity, "unknown_session",
				"no agent_sessions row for claude_session_id; call /v1/sessions/start first",
				map[string]string{"claude_session_id": req.ClaudeSessionID})
			return
		}
		writeError(w, http.StatusInternalServerError, "lookup_failed", err.Error())
		return
	}

	taskID, err := events.ResolveTaskID(r.Context(), a.Store.Pool, sess.ProjectID, req.TaskKey)
	if err != nil {
		a.writeResolveError(w, err, "task_key", req.TaskKey)
		return
	}

	ckpt, err := sessions.InsertCheckpoint(r.Context(), a.Store.Pool, sessions.CheckpointParams{
		AgentSessionID: sess.ID,
		TaskID:         taskID,
		CheckpointType: req.CheckpointType,
		Status:         req.Status,
		CurrentGoal:    stringPtr(req.CurrentGoal),
		Summary:        req.Summary,
		NextActions:    req.NextActions,
		OpenQuestions:  req.OpenQuestions,
		FilesRelevant:  req.FilesRelevant,
		Risks:          req.Risks,
		Payload:        req.Payload,
	})
	if err != nil {
		a.Logger.Error("checkpoint failed", "err", err)
		writeError(w, http.StatusInternalServerError, "checkpoint_failed", err.Error())
		return
	}

	a.emitLifecycleEvent(r, caller.ID, "session.checkpointed", sess.ID, req.ClaudeSessionID,
		req.Summary, map[string]any{
			"checkpoint_id":   ckpt.ID,
			"checkpoint_type": ckpt.CheckpointType,
			"status":          ckpt.Status,
		})

	writeJSON(w, http.StatusCreated, ckpt)
}

// =============================================================================
// POST /v1/sessions/end
// =============================================================================

type sessionEndRequest struct {
	ClaudeSessionID string `json:"claude_session_id"`
	FinalStatus     string `json:"final_status,omitempty"` // e.g. "task_completed"; surfaces stalls per Mattermost curation
}

func (a *App) handleSessionEnd(w http.ResponseWriter, r *http.Request) {
	caller := auth.FromContext(r.Context())

	var req sessionEndRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.ClaudeSessionID == "" {
		writeError(w, http.StatusBadRequest, "claude_session_id_required", "field claude_session_id is required")
		return
	}

	sess, err := sessions.End(r.Context(), a.Store.Pool, req.ClaudeSessionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeErrorWithDetails(w, http.StatusUnprocessableEntity, "unknown_session",
				"no agent_sessions row for claude_session_id",
				map[string]string{"claude_session_id": req.ClaudeSessionID})
			return
		}
		writeError(w, http.StatusInternalServerError, "end_failed", err.Error())
		return
	}

	a.emitLifecycleEvent(r, caller.ID, "session.ended", sess.ID, req.ClaudeSessionID,
		"session ended for "+caller.Name, map[string]any{
			"final_status": req.FinalStatus,
		})

	writeJSON(w, http.StatusOK, sess)
}

// =============================================================================
// GET /v1/sessions/{claude_session_id}/resume-context
// =============================================================================

func (a *App) handleSessionResumeContext(w http.ResponseWriter, r *http.Request) {
	caller := auth.FromContext(r.Context())
	cid := chi.URLParam(r, "claude_session_id")
	if cid == "" {
		writeError(w, http.StatusBadRequest, "claude_session_id_required", "missing path parameter")
		return
	}

	packet, err := sessions.Resume(r.Context(), a.Store.Pool, cid, 20)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeErrorWithDetails(w, http.StatusNotFound, "unknown_session",
				"no agent_sessions row for claude_session_id",
				map[string]string{"claude_session_id": cid})
			return
		}
		writeError(w, http.StatusInternalServerError, "resume_failed", err.Error())
		return
	}

	// Owner-only access: an agent can read its own session, and operator-role
	// agents can read any. (Permissions.cross_project_read also grants this
	// once the schema is populated; for v0.1.0, role check is sufficient.)
	if packet.Session.AgentID != caller.ID && caller.Role != "operator" {
		writeError(w, http.StatusForbidden, "not_owner",
			"resume-context is only readable by the session's owner or an operator-role agent")
		return
	}

	writeJSON(w, http.StatusOK, packet)
}

// =============================================================================
// shared helpers
// =============================================================================

// emitLifecycleEvent writes one event with the lifecycle-emit posture:
// best-effort; failures logged but don't fail the calling handler.
// Used by session.started / session.checkpointed / session.ended emissions.
// Curated event_types (e.g., session.ended) also enqueue a mattermost_outbox
// row in the same transaction; non-curated types are a single insert.
func (a *App) emitLifecycleEvent(r *http.Request, agentID, eventType, sessionID, claudeSessionID, summary string, payload map[string]any) {
	_, err := events.InsertWithOutbox(r.Context(), a.Store.Pool, events.InsertParams{
		EventType:       eventType,
		AgentID:         agentID,
		AgentSessionID:  &sessionID,
		ClaudeSessionID: stringPtr(claudeSessionID),
		Summary:         stringPtr(summary),
		Payload:         payload,
	}, events.OutboxConfig{
		DefaultChannel: a.MattermostDefaultOutbox,
	}, "")
	if err != nil {
		a.Logger.Warn("lifecycle event emission failed",
			"event_type", eventType, "claude_session_id", claudeSessionID, "err", err)
	}
}
