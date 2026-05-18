package server

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/Eladrofel/agent-hub/gateway/internal/projects"
)

// projectUpsertRequest is the body of POST /v1/projects. Slug + Name are
// required; the remaining fields are optional. On slug conflict, the row is
// updated rather than rejected — /setup-agent-events can run repeatedly.
type projectUpsertRequest struct {
	Slug                    string  `json:"slug"`
	Name                    string  `json:"name"`
	ForgeURL                *string `json:"forge_url,omitempty"`
	DefaultBranch           *string `json:"default_branch,omitempty"`
	MattermostOutboxChannel *string `json:"mattermost_outbox_channel,omitempty"`
	MattermostInboxChannel  *string `json:"mattermost_inbox_channel,omitempty"`
}

type projectListResponse struct {
	Projects []*projects.Project `json:"projects"`
}

// handleProjectUpsert creates or updates a project keyed by slug. Idempotent.
func (a *App) handleProjectUpsert(w http.ResponseWriter, r *http.Request) {
	var req projectUpsertRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.Slug == "" {
		writeError(w, http.StatusBadRequest, "slug_required", "field slug is required")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name_required", "field name is required")
		return
	}

	out, err := projects.Upsert(r.Context(), a.Store.Pool, projects.UpsertParams{
		Slug:                    req.Slug,
		Name:                    req.Name,
		ForgeURL:                req.ForgeURL,
		DefaultBranch:           req.DefaultBranch,
		MattermostOutboxChannel: req.MattermostOutboxChannel,
		MattermostInboxChannel:  req.MattermostInboxChannel,
	})
	if err != nil {
		a.Logger.Error("upsert project failed", "err", err)
		writeError(w, http.StatusInternalServerError, "upsert_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleProjectList returns every project. Used by /agent-events-health.
func (a *App) handleProjectList(w http.ResponseWriter, r *http.Request) {
	out, err := projects.List(r.Context(), a.Store.Pool)
	if err != nil {
		a.Logger.Error("list projects failed", "err", err)
		writeError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, projectListResponse{Projects: out})
}

// handleProjectGet returns a single project by slug. 404 if missing.
func (a *App) handleProjectGet(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		writeError(w, http.StatusBadRequest, "slug_required", "URL param slug is required")
		return
	}
	out, err := projects.GetBySlug(r.Context(), a.Store.Pool, slug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "project_not_found", "no project with slug "+slug)
			return
		}
		a.Logger.Error("get project failed", "err", err)
		writeError(w, http.StatusInternalServerError, "get_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}
