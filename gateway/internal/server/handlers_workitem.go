package server

// Work-item peer-coordination endpoints (v0.1.14).
//
// /start-work-item runs as a Pre-flight check: "is anyone on this work-item
// right now?". The plugin queries this endpoint before branching and halts on
// any active claim from another agent. --force-claim lets the operator
// override; the second claim is still written, so audit shows both.
//
// Authoritative coordination state lives in Postgres (agent.work-item.claimed
// + agent.work-item.finished events). Mattermost auto-relay (via CuratedEventTypes)
// is the human-visibility layer, not the source of truth.

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/Eladrofel/agent-hub/gateway/internal/events"
)

type activeClaim struct {
	EventID         string    `json:"event_id"`
	AgentID         string    `json:"agent_id"`
	AgentName       string    `json:"agent_name"`
	Alias           string    `json:"alias,omitempty"`
	ClaimedAt       time.Time `json:"claimed_at"`
	ClaudeSessionID string    `json:"claude_session_id,omitempty"`
	Repo            string    `json:"repo,omitempty"`
	Branch          string    `json:"branch,omitempty"`
	Force           bool      `json:"force,omitempty"`
}

type activeClaimsResponse struct {
	WIKey        string        `json:"wi_key"`
	ProjectSlug  string        `json:"project_slug,omitempty"`
	ActiveClaims []activeClaim `json:"active_claims"`
	Total        int           `json:"total"`
}

// handleWorkItemActiveClaims returns the set of agents whose most-recent
// agent.work-item.{claimed|finished} event for the given wi-key is "claimed"
// (i.e. they have an open claim with no matching later finish).
//
// Project scope: ?project_slug=<slug> query param, resolved server-side via
// the same path as POST /v1/events. Required — the wi-key namespace is per-
// project, so an unscoped read would conflate unrelated projects' claims. If
// the slug is omitted OR doesn't resolve, returns an empty list with a
// 200 response (best-effort posture matching emit-side behaviour; the
// pre-flight skill can treat empty as "no conflicts" without ceremony).
//
// DISTINCT ON (agent_id) + ORDER BY agent_id, created_at DESC gives "latest
// event per agent" in one pass; the outer filter then keeps only the ones
// whose latest is claimed. Handles --force-claim (re-claim after another
// agent already claimed) and re-claim-after-finish correctly without any
// app-side bookkeeping.
func (a *App) handleWorkItemActiveClaims(w http.ResponseWriter, r *http.Request) {
	wiKey := strings.TrimSpace(chi.URLParam(r, "wi_key"))
	if wiKey == "" {
		writeError(w, http.StatusBadRequest, "wi_key_required", "missing path parameter wi_key")
		return
	}

	projectSlug := strings.TrimSpace(r.URL.Query().Get("project_slug"))
	if projectSlug == "" {
		// Empty slug → empty result. Same posture as `agentctl event emit`
		// without a project: events still write (project_id null), reads
		// just see nothing useful. Keeps the pre-flight skill cheap.
		writeJSON(w, http.StatusOK, activeClaimsResponse{
			WIKey:        wiKey,
			ActiveClaims: []activeClaim{},
			Total:        0,
		})
		return
	}

	projectID, err := events.ResolveProjectID(r.Context(), a.Store.Pool, projectSlug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Unknown project — return empty, not 422. Pre-flight should not
			// be a project-existence checker; that's setup's job.
			writeJSON(w, http.StatusOK, activeClaimsResponse{
				WIKey:        wiKey,
				ProjectSlug:  projectSlug,
				ActiveClaims: []activeClaim{},
				Total:        0,
			})
			return
		}
		a.Logger.Error("resolve project slug failed", "slug", projectSlug, "err", err)
		writeError(w, http.StatusInternalServerError, "resolve_failed", err.Error())
		return
	}
	if projectID == nil {
		writeJSON(w, http.StatusOK, activeClaimsResponse{
			WIKey:        wiKey,
			ProjectSlug:  projectSlug,
			ActiveClaims: []activeClaim{},
			Total:        0,
		})
		return
	}

	const q = `
		WITH latest AS (
		  SELECT DISTINCT ON (agent_id)
		         id, agent_id, event_type, created_at, claude_session_id, payload
		    FROM events
		   WHERE project_id = $1
		     AND payload->>'wi_key' = $2
		     AND event_type IN ('agent.work-item.claimed', 'agent.work-item.finished')
		   ORDER BY agent_id, created_at DESC
		)
		SELECT l.id, l.agent_id, l.created_at, l.claude_session_id, l.payload,
		       a.name, a.mattermost_username
		  FROM latest l
		  LEFT JOIN agents a ON a.id = l.agent_id
		 WHERE l.event_type = 'agent.work-item.claimed'
		 ORDER BY l.created_at DESC`

	rows, err := a.Store.Pool.Query(r.Context(), q, *projectID, wiKey)
	if err != nil {
		a.Logger.Error("active-claims query failed", "wi_key", wiKey, "err", err)
		writeError(w, http.StatusInternalServerError, "query_failed", err.Error())
		return
	}
	defer rows.Close()

	out := []activeClaim{}
	for rows.Next() {
		var (
			claim       activeClaim
			csid        *string
			alias       *string
			payloadJSON map[string]any
		)
		if err := rows.Scan(&claim.EventID, &claim.AgentID, &claim.ClaimedAt,
			&csid, &payloadJSON, &claim.AgentName, &alias); err != nil {
			a.Logger.Error("active-claims scan failed", "err", err)
			writeError(w, http.StatusInternalServerError, "scan_failed", err.Error())
			return
		}
		if csid != nil {
			claim.ClaudeSessionID = *csid
		}
		if alias != nil {
			claim.Alias = *alias
		}
		if v, ok := payloadJSON["repo"].(string); ok {
			claim.Repo = v
		}
		if v, ok := payloadJSON["branch"].(string); ok {
			claim.Branch = v
		}
		if v, ok := payloadJSON["force"].(bool); ok {
			claim.Force = v
		}
		out = append(out, claim)
	}
	if err := rows.Err(); err != nil {
		a.Logger.Error("active-claims rows.Err", "err", err)
		writeError(w, http.StatusInternalServerError, "rows_err", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, activeClaimsResponse{
		WIKey:        wiKey,
		ProjectSlug:  projectSlug,
		ActiveClaims: out,
		Total:        len(out),
	})
}
