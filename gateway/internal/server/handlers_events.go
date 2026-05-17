package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/Eladrofel/agent-hub/gateway/internal/auth"
	"github.com/Eladrofel/agent-hub/gateway/internal/events"
)

// eventEmitRequest is the POST /v1/events body. Optional fields are
// represented as zero-valued strings / nil maps so callers can omit them.
// Slug-based scoping (project_slug, task_key, claude_session_id) is
// resolved server-side so agentctl never has to track UUIDs.
type eventEmitRequest struct {
	EventType       string         `json:"event_type"`
	EventVersion    int            `json:"event_version,omitempty"`
	ProjectSlug     string         `json:"project_slug,omitempty"`
	TaskKey         string         `json:"task_key,omitempty"`
	ClaudeSessionID string         `json:"claude_session_id,omitempty"`
	CorrelationID   string         `json:"correlation_id,omitempty"`
	CausationID     string         `json:"causation_id,omitempty"`
	ParentEventID   string         `json:"parent_event_id,omitempty"`
	ActorType       string         `json:"actor_type,omitempty"`
	ActorName       string         `json:"actor_name,omitempty"`
	Branch          string         `json:"branch,omitempty"`
	GitHeadSHA      string         `json:"git_head_sha,omitempty"`
	WorktreePath    string         `json:"worktree_path,omitempty"`
	Summary         string         `json:"summary,omitempty"`
	Payload         map[string]any `json:"payload,omitempty"`
	ArtefactPointer map[string]any `json:"artefact_pointer,omitempty"`
}

type eventEmitResponse struct {
	EventID string `json:"event_id"`
}

type sanitiserBlockedResponse struct {
	Error           string `json:"error"`
	Message         string `json:"message"`
	MatchedPattern  string `json:"matched_pattern"`
	MatchedField    string `json:"matched_field"`
	BlockedEventID  string `json:"blocked_event_id"`
}

func (a *App) handleEventEmit(w http.ResponseWriter, r *http.Request) {
	agent := auth.FromContext(r.Context())

	var req eventEmitRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.EventType == "" {
		writeError(w, http.StatusBadRequest, "event_type_required", "field event_type is required")
		return
	}

	// Sanitiser runs BEFORE any resolution lookups so a leak in summary or
	// payload can never reach the DB query layer.
	if hit, err := a.Sanitiser.Check(req.Summary, req.Payload); err != nil {
		writeError(w, http.StatusInternalServerError, "sanitiser_error", err.Error())
		return
	} else if hit != nil {
		blockedID := a.writeSanitiserBlocked(r.Context(), agent.ID, req.EventType, hit.Pattern, hit.MatchedField)
		writeJSON(w, http.StatusUnprocessableEntity, sanitiserBlockedResponse{
			Error:          "sanitiser_blocked",
			Message:        "event blocked by §2.1 sanitiser pattern; offending content NOT stored",
			MatchedPattern: hit.Pattern,
			MatchedField:   hit.MatchedField,
			BlockedEventID: blockedID,
		})
		return
	}

	projectID, err := events.ResolveProjectID(r.Context(), a.Store.Pool, req.ProjectSlug)
	if err != nil {
		a.writeResolveError(w, err, "project_slug", req.ProjectSlug)
		return
	}
	taskID, err := events.ResolveTaskID(r.Context(), a.Store.Pool, projectID, req.TaskKey)
	if err != nil {
		a.writeResolveError(w, err, "task_key", req.TaskKey)
		return
	}
	sessionID, err := events.ResolveSessionID(r.Context(), a.Store.Pool, req.ClaudeSessionID)
	if err != nil {
		a.writeResolveError(w, err, "claude_session_id", req.ClaudeSessionID)
		return
	}

	params := events.InsertParams{
		EventType:       req.EventType,
		EventVersion:    req.EventVersion,
		AgentID:         agent.ID,
		ProjectID:       projectID,
		TaskID:          taskID,
		AgentSessionID:  sessionID,
		ClaudeSessionID: stringPtr(req.ClaudeSessionID),
		CorrelationID:   stringPtr(req.CorrelationID),
		CausationID:     stringPtr(req.CausationID),
		ParentEventID:   stringPtr(req.ParentEventID),
		ActorType:       req.ActorType,
		ActorName:       stringPtr(req.ActorName),
		Branch:          stringPtr(req.Branch),
		GitHeadSHA:      stringPtr(req.GitHeadSHA),
		WorktreePath:    stringPtr(req.WorktreePath),
		Summary:         stringPtr(req.Summary),
		Payload:         req.Payload,
		ArtefactPointer: req.ArtefactPointer,
	}

	id, err := events.Insert(r.Context(), a.Store.Pool, params)
	if err != nil {
		a.Logger.Error("insert event failed", "err", err)
		writeError(w, http.StatusInternalServerError, "insert_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, eventEmitResponse{EventID: id})
}

// writeSanitiserBlocked writes the metadata-only audit event. Returns the
// new event ID, or "" on failure (the operator-facing 422 still goes out;
// we just couldn't audit it).
func (a *App) writeSanitiserBlocked(ctx context.Context, agentID, blockedType, pattern, field string) string {
	id, err := events.Insert(ctx, a.Store.Pool, events.InsertParams{
		EventType: "sanitiser.blocked",
		AgentID:   agentID,
		Summary:   stringPtr("event blocked by §2.1 sanitiser; original event_type=" + blockedType),
		Payload: map[string]any{
			"matched_pattern": pattern,
			"matched_field":   field,
			"blocked_type":    blockedType,
		},
	})
	if err != nil {
		a.Logger.Error("failed to write sanitiser.blocked audit event", "err", err)
		return ""
	}
	return id
}

func (a *App) writeResolveError(w http.ResponseWriter, err error, field, value string) {
	if errors.Is(err, pgx.ErrNoRows) {
		writeErrorWithDetails(w, http.StatusUnprocessableEntity, "unknown_reference",
			"referenced row not found", map[string]string{"field": field, "value": value})
		return
	}
	a.Logger.Error("resolve failed", "field", field, "value", value, "err", err)
	writeError(w, http.StatusInternalServerError, "resolve_failed", err.Error())
}

func decodeJSON(body io.Reader, into any) error {
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}

func stringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
