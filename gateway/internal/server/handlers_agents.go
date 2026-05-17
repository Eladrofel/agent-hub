package server

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/Eladrofel/agent-hub/gateway/internal/agents"
	"github.com/Eladrofel/agent-hub/gateway/internal/auth"
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
