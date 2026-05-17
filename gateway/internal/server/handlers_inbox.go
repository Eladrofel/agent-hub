package server

import (
	"net/http"
	"time"

	"github.com/Eladrofel/terraform-agent-hub/gateway/internal/auth"
	"github.com/Eladrofel/terraform-agent-hub/gateway/internal/inbox"
)

// handleInboxPoll returns the named agent's undelivered messages. Pre-v0.1.1
// (no inbox-webhook running yet) this always returns []; the endpoint exists
// because Component B hooks shipped expecting it.
func (a *App) handleInboxPoll(w http.ResponseWriter, r *http.Request) {
	caller := auth.FromContext(r.Context())

	agentName := r.URL.Query().Get("agent_name")
	if agentName == "" {
		writeError(w, http.StatusBadRequest, "agent_name_required",
			"query param agent_name is required")
		return
	}
	if agentName != caller.Name && caller.Role != "operator" {
		writeError(w, http.StatusForbidden, "not_owner",
			"agent_name must match the authenticated agent's name (or caller must be operator-role)")
		return
	}

	var since time.Time
	if raw := r.URL.Query().Get("since"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeErrorWithDetails(w, http.StatusBadRequest, "invalid_since",
				"since must be RFC3339",
				map[string]string{"got": raw})
			return
		}
		since = t
	}

	agentID := caller.ID // optimisation: caller already auth'd → use their id
	if agentName != caller.Name {
		// operator polling on behalf of another agent
		var err error
		agentID, err = a.resolveAgentIDByName(r, agentName)
		if err != nil {
			writeErrorWithDetails(w, http.StatusUnprocessableEntity, "unknown_agent",
				"no agent with that name", map[string]string{"agent_name": agentName})
			return
		}
	}

	msgs, err := inbox.Poll(r.Context(), a.Store.Pool, agentID, since)
	if err != nil {
		a.Logger.Error("inbox poll failed", "agent", agentName, "err", err)
		writeError(w, http.StatusInternalServerError, "poll_failed", err.Error())
		return
	}
	if msgs == nil {
		msgs = []inbox.Message{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agent_name": agentName,
		"messages":   msgs,
	})
}

func (a *App) resolveAgentIDByName(r *http.Request, name string) (string, error) {
	var id string
	err := a.Store.Pool.QueryRow(r.Context(),
		`SELECT id FROM agents WHERE name = $1`, name).Scan(&id)
	return id, err
}
