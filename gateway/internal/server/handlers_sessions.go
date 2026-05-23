package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

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
		formatStartSummary(caller, req.ClaudeSessionID, req.ProjectSlug), map[string]any{
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
		formatCheckpointSummary(caller, req.ClaudeSessionID, req.Summary, ckpt.Status),
		map[string]any{
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
		formatEndSummary(caller, req.ClaudeSessionID, req.FinalStatus), map[string]any{
			"final_status": req.FinalStatus,
		})

	writeJSON(w, http.StatusOK, sess)
}

// =============================================================================
// GET /v1/sessions/{claude_session_id}/resume-context
//
// Source-of-truth for cross-/clear handoff (Dale's 2026-05-23 directive). The
// response packet shape + filter contract is documented on sessions.Resume /
// sessions.ResumePacket; the short version is:
//   - session: the agent_sessions row.
//   - latest_checkpoint: most recent session_checkpoints row, or nil.
//   - recent_events: per-session event tail (default 20, most-recent first).
//     Excludes event_type='tool.used' by default — tool calls drown the
//     interesting events (session.checkpointed, agent.improvement-note,
//     progress.updated). Pass ?include_tool_use=true to get the raw stream.
//   - recent_improvements: last N agent.improvement-note events for this
//     agent across ALL sessions (improvement-notes are cross-cutting fleet
//     learnings, not session-scoped state). Tune via ?improvements_limit=N
//     (default 10, max 50).
//
// What's NOT (yet) in the response: open handoffs, open decisions, pending
// operator messages, active locks — those are aspirational and tracked
// separately. Don't grep this handler expecting to find them.
// =============================================================================

func (a *App) handleSessionResumeContext(w http.ResponseWriter, r *http.Request) {
	caller := auth.FromContext(r.Context())
	cid := chi.URLParam(r, "claude_session_id")
	if cid == "" {
		writeError(w, http.StatusBadRequest, "claude_session_id_required", "missing path parameter")
		return
	}

	// Query-param parsing. Both knobs are optional and silently fall back
	// to defaults on parse failure — a malformed `?improvements_limit=foo`
	// shouldn't 400-bomb the cross-/clear handoff path.
	opts := sessions.ResumeOpts{}
	if v := r.URL.Query().Get("include_tool_use"); v == "1" || v == "true" {
		opts.IncludeToolUse = true
	}
	if v := r.URL.Query().Get("improvements_limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			opts.ImprovementsLimit = n
		}
	}

	packet, err := sessions.Resume(r.Context(), a.Store.Pool, cid, opts)
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

// =============================================================================
// Lifecycle summary formatters (v0.1.8)
//
// Build operator-facing summary strings for the curated lifecycle events
// (session.started / session.checkpointed / session.ended). The summary
// is what Mattermost surfaces via the outbox worker — making it richer
// helps the operator correlate the chat ping with the right session/
// project/agent without re-querying.
//
// The EVENT PAYLOAD is unchanged; we only enrich the display surface.
// =============================================================================

// callerDisplayName returns the agent's alias (e.g. "Splinter") when set,
// else falls back to the canonical agent name ("agent-operator-mac").
func callerDisplayName(c *auth.Agent) string {
	if c.Alias != "" {
		return c.Alias
	}
	return c.Name
}

// shortSessionID returns the first 8 chars of the claude-session-id for
// terse display, or the whole string if it's shorter.
func shortSessionID(sid string) string {
	if len(sid) >= 8 {
		return sid[:8]
	}
	return sid
}

func formatStartSummary(c *auth.Agent, claudeSessionID, projectSlug string) string {
	base := fmt.Sprintf("[start] %s — session %s",
		callerDisplayName(c), shortSessionID(claudeSessionID))
	if projectSlug != "" {
		return base + ", project=" + projectSlug
	}
	return base
}

func formatCheckpointSummary(c *auth.Agent, claudeSessionID, userSummary, status string) string {
	base := fmt.Sprintf("[checkpoint] %s — session %s",
		callerDisplayName(c), shortSessionID(claudeSessionID))
	// Prefer the user-supplied summary for chat context; otherwise fall
	// back to the checkpoint status (e.g. "in_progress", "blocked").
	switch {
	case userSummary != "":
		return base + " — " + userSummary
	case status != "":
		return base + ", status=" + status
	default:
		return base
	}
}

func formatEndSummary(c *auth.Agent, claudeSessionID, finalStatus string) string {
	base := fmt.Sprintf("[end] %s — session %s",
		callerDisplayName(c), shortSessionID(claudeSessionID))
	if finalStatus != "" {
		return base + ", " + finalStatus
	}
	return base
}
