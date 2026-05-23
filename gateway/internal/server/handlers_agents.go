package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/Eladrofel/agent-hub/gateway/internal/agents"
	"github.com/Eladrofel/agent-hub/gateway/internal/auth"
	"github.com/Eladrofel/agent-hub/gateway/internal/sessions"
)

type agentRegisterRequest struct {
	Name               string         `json:"name"`
	Role               *string        `json:"role,omitempty"`
	HostKind           *string        `json:"host_kind,omitempty"`
	VMHostname         *string        `json:"vm_hostname,omitempty"`
	MattermostUsername *string        `json:"mattermost_username,omitempty"`
	Capabilities       []any          `json:"capabilities,omitempty"`
	Metadata           map[string]any `json:"metadata,omitempty"`
}

// handleAgentRegister updates the authenticated agent's row. Idempotent;
// the agent name in the body MUST match the bearer-token identity (an agent
// can't masquerade as another).
func (a *App) handleAgentRegister(w http.ResponseWriter, r *http.Request) {
	caller := auth.FromContext(r.Context())

	var req agentRegisterRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name_required", "field name is required")
		return
	}
	if req.Name != caller.Name {
		writeError(w, http.StatusForbidden, "name_mismatch",
			"name in body must match the authenticated agent's name")
		return
	}

	out, err := agents.Register(r.Context(), a.Store.Pool, agents.RegisterParams{
		Name:               req.Name,
		Role:               req.Role,
		HostKind:           req.HostKind,
		VMHostname:         req.VMHostname,
		MattermostUsername: req.MattermostUsername,
		Capabilities:       req.Capabilities,
		Metadata:           req.Metadata,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Should be impossible — auth resolved the agent from the same name.
			writeError(w, http.StatusNotFound, "agent_not_found", err.Error())
			return
		}
		a.Logger.Error("register agent failed", "err", err)
		writeError(w, http.StatusInternalServerError, "register_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// =============================================================================
// GET /v1/agents/{name_or_alias}/latest-session
//
// Backs the v0.1.12 `agentctl resume-context` no-flag fallback. The cross-
// /clear flow forces a brand-new $CLAUDE_SESSION_ID inside Claude Code, so
// the operator can no longer query resume-context with their PRIOR session
// id without manually pasting it. This endpoint returns the most-recent
// agent_sessions row for the named agent, with `?exclude=<session_id>` to
// skip the current (new, useless) session and surface the prior one.
//
// Admin-protected — same posture as the other /v1/agents/* reads in
// handlers_query.go. Resolution is case-INSENSITIVE on agents.name first,
// then mattermost_username (alias) — matches the v0.1.8 #45 fix in
// inbox.webhook.resolveAgent so "@Splinter" / "splinter" / "agent-operator-mac"
// all resolve to the same row.
// =============================================================================

type latestSessionItem struct {
	ClaudeSessionID string  `json:"claude_session_id"`
	StartedAt       string  `json:"started_at"`
	EndedAt         *string `json:"ended_at"`
	Status          string  `json:"status"`
	StartReason     *string `json:"start_reason,omitempty"`
}

type latestSessionResponse struct {
	AgentID       string             `json:"agent_id"`
	AgentName     string             `json:"agent_name"`
	Alias         *string            `json:"alias,omitempty"`
	LatestSession *latestSessionItem `json:"latest_session"`
}

func (a *App) handleAgentLatestSession(w http.ResponseWriter, r *http.Request) {
	handle := strings.TrimSpace(chi.URLParam(r, "name_or_alias"))
	if handle == "" {
		writeError(w, http.StatusBadRequest, "name_or_alias_required", "missing path parameter")
		return
	}

	agentID, name, alias, ok := a.resolveAgentByHandle(r.Context(), handle)
	if !ok {
		writeErrorWithDetails(w, http.StatusNotFound, "unknown_agent",
			"no agent with that name or alias",
			map[string]string{"name_or_alias": handle})
		return
	}

	exclude := strings.TrimSpace(r.URL.Query().Get("exclude"))

	sess, err := sessions.LatestForAgent(r.Context(), a.Store.Pool, agentID, exclude)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeErrorWithDetails(w, http.StatusNotFound, "no_sessions",
				"agent has no sessions (or none not matching exclude filter)",
				map[string]string{"name_or_alias": handle, "agent_id": agentID})
			return
		}
		a.Logger.Error("latest session query failed", "agent_id", agentID, "err", err)
		writeError(w, http.StatusInternalServerError, "query_failed", err.Error())
		return
	}

	resp := latestSessionResponse{
		AgentID:   agentID,
		AgentName: name,
		LatestSession: &latestSessionItem{
			ClaudeSessionID: sess.ClaudeSessionID,
			StartedAt:       sess.StartedAt,
			EndedAt:         sess.EndedAt,
			Status:          sess.Status,
			StartReason:     sess.StartReason,
		},
	}
	if alias != "" {
		aliasVal := alias
		resp.Alias = &aliasVal
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleMeLatestSession returns the authenticated caller's most-recent
// session — same payload shape as handleAgentLatestSession but scoped to
// the bearer's own identity (no admin token required). Backs the v0.5.3
// /resume-context skill: post-/clear the operator has no admin context,
// just their per-host bearer, but wants to look up their own prior
// session. Self-lookup only — no path param means no possibility of
// reading another agent's sessions.
//
// Optional ?exclude=<claude_session_id> skips that session id (the
// post-/clear case where the current new-session id is known and the
// caller wants the PRIOR one).
func (a *App) handleMeLatestSession(w http.ResponseWriter, r *http.Request) {
	caller := auth.FromContext(r.Context())
	if caller == nil || caller.ID == "" {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "no caller identity in request context")
		return
	}

	exclude := strings.TrimSpace(r.URL.Query().Get("exclude"))

	sess, err := sessions.LatestForAgent(r.Context(), a.Store.Pool, caller.ID, exclude)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeErrorWithDetails(w, http.StatusNotFound, "no_sessions",
				"agent has no sessions (or none not matching exclude filter)",
				map[string]string{"agent_id": caller.ID, "agent_name": caller.Name})
			return
		}
		a.Logger.Error("me latest session query failed", "agent_id", caller.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "query_failed", err.Error())
		return
	}

	resp := latestSessionResponse{
		AgentID:   caller.ID,
		AgentName: caller.Name,
		LatestSession: &latestSessionItem{
			ClaudeSessionID: sess.ClaudeSessionID,
			StartedAt:       sess.StartedAt,
			EndedAt:         sess.EndedAt,
			Status:          sess.Status,
			StartReason:     sess.StartReason,
		},
	}
	if caller.Alias != "" {
		aliasVal := caller.Alias
		resp.Alias = &aliasVal
	}
	writeJSON(w, http.StatusOK, resp)
}

// resolveAgentByHandle mirrors inbox.webhook.resolveAgent: case-INSENSITIVE
// match on agents.name first, then mattermost_username. Returns
// (agent_id, name, alias, ok). Alias is the empty string when the matched
// row has no mattermost_username set.
func (a *App) resolveAgentByHandle(ctx context.Context, handle string) (id, name, alias string, ok bool) {
	var aliasPtr *string
	err := a.Store.Pool.QueryRow(ctx,
		`SELECT id, name, mattermost_username
		   FROM agents WHERE LOWER(name) = LOWER($1)`, handle,
	).Scan(&id, &name, &aliasPtr)
	if err == nil {
		if aliasPtr != nil {
			alias = *aliasPtr
		}
		return id, name, alias, true
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		a.Logger.Warn("agent name lookup failed", "handle", handle, "err", err)
		return "", "", "", false
	}
	err = a.Store.Pool.QueryRow(ctx,
		`SELECT id, name, mattermost_username
		   FROM agents WHERE LOWER(mattermost_username) = LOWER($1)`, handle,
	).Scan(&id, &name, &aliasPtr)
	if err == nil {
		if aliasPtr != nil {
			alias = *aliasPtr
		}
		return id, name, alias, true
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		a.Logger.Warn("agent alias lookup failed", "handle", handle, "err", err)
	}
	return "", "", "", false
}
