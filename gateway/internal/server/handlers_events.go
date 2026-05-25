package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"

	"github.com/jackc/pgx/v5"

	"github.com/Eladrofel/agent-hub/gateway/internal/auth"
	"github.com/Eladrofel/agent-hub/gateway/internal/events"
	"github.com/Eladrofel/agent-hub/gateway/internal/outbox"
)

// workItemKeyPattern matches concept-workflow work-item keys
// ((feat|bugfix|improvement|hotfix|task)-NN-<name>). Used by writeResolveError
// to detect when --task-key was given a wi-key shape value and return a
// tailored error message pointing the caller at the correct surface
// (event payload's wi_key field, not the legacy tasks table). v0.1.16.
var workItemKeyPattern = regexp.MustCompile(`^(feat|bugfix|improvement|hotfix|task)-\d+-`)

// validIntents is the locked v0.1.10 enum for payload.intent. Mirrored on
// the agentctl side (commands.ValidIntents) so both halves of the wire
// validate the same set. Absent / empty payload.intent is treated as "info".
var validIntents = map[string]bool{
	"info":      true,
	"directive": true,
	"question":  true,
	"blocker":   true,
	"status":    true,
}

// directiveAuthorizedRole is the role required to emit intent=directive.
// v0.1.10 single-tier check: only operators direct; peer agents collaborate
// via info / question / blocker / status. This is the programmatic
// enforcement of the v0.5.0 peer-coordination policy (previously norm-only).
const directiveAuthorizedRole = "operator"

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

	// v0.1.10: validate payload.intent and enforce the directive-only-from-
	// operator policy BEFORE any other work. Doing this ahead of sanitiser /
	// resolution / insert keeps the unauthorized-directive path cheap and
	// makes the audit log unambiguous (no half-written rows on rejection).
	intent := extractIntent(req.Payload)
	if intent != "" && !validIntents[intent] {
		writeErrorWithDetails(w, http.StatusBadRequest, "invalid_intent",
			fmt.Sprintf("payload.intent=%q invalid; must be one of info|directive|question|blocker|status (or omit for default 'info')", intent),
			map[string]string{"intent": intent})
		return
	}
	if intent == "directive" && agent.Role != directiveAuthorizedRole {
		// 403 — the caller authenticated successfully but is not authorised
		// to use this intent. Body includes the caller's actual role so the
		// operator can spot mis-tagged agents, and a relative docs path
		// pointing into the concept-workflow plugin's published policy.
		writeErrorWithDetails(w, http.StatusForbidden, "directive_not_authorized",
			fmt.Sprintf("only operators can emit intent=directive events; this caller is role=%s", agent.Role),
			map[string]string{
				"role": agent.Role,
				"docs": "references/peer-coordination-policy.md",
			})
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

	// Resolve the per-project Mattermost outbox channel + slug (may be
	// empty — caller falls back to the operator-level default channel,
	// and an empty slug just means no project label in the attachment
	// title).
	projectChannel := ""
	projectSlug := ""
	if projectID != nil {
		pc, perr := events.ResolveProjectChannel(r.Context(), a.Store.Pool, projectID)
		if perr != nil {
			a.Logger.Warn("resolve project outbox channel failed; falling back to default",
				"project_id", *projectID, "err", perr)
		} else {
			projectChannel = pc
		}
		ps, serr := events.ResolveProjectSlug(r.Context(), a.Store.Pool, projectID)
		if serr != nil {
			a.Logger.Warn("resolve project slug failed; attachment title omits @project",
				"project_id", *projectID, "err", serr)
		} else {
			projectSlug = ps
		}
	}

	// Per-event-type curated formatter — line-based fallback for clients
	// that don't render attachments + as the outbox row's `message` text.
	outboxMessage := formatCuratedMessage(req.EventType, agent, req.Summary, req.Payload)

	// v0.1.15 — prepend peer @-mentions for work-item events so other active
	// agents in the project receive inbox notification (via the existing
	// outgoing-webhook → inbox-webhook → agent-inbox routing). Without this,
	// claim events sit in the channel until each peer manually polls.
	if req.EventType == "agent.work-item.claimed" || req.EventType == "agent.work-item.finished" {
		mentions, merr := events.PeerMentionsForProject(r.Context(), a.Store.Pool, projectID, agent.ID)
		if merr != nil {
			// Soft-fail: the event itself still writes; we just lose the
			// proactive peer notification. Worth a warn-level log so the
			// drift is visible in gateway logs.
			a.Logger.Warn("peer mentions lookup failed; emitting without mentions",
				"event_type", req.EventType, "err", merr)
		} else if mentions != "" {
			// MM outgoing-webhook trigger_when=1 needs first-word-@. Prepend
			// the mentions to the existing line (formatCuratedMessage already
			// leads with the icon, but @-mentions take precedence for routing).
			if outboxMessage != "" {
				outboxMessage = mentions + " " + outboxMessage
			} else {
				outboxMessage = mentions
			}
		}
	}

	// v0.1.10 — build the rich attachment via the MM adapter. The
	// outbox-worker forwards props.attachments to Mattermost natively
	// (Slack/Discord adapters live in the same package for future
	// activation but the runtime backend is hard-coded to MM today).
	// `intent` is already validated + extracted earlier in this handler.
	alias := agent.Alias
	if alias == "" {
		alias = agent.Name
	}
	summaryText := req.Summary
	adapter := outbox.AdapterFor("mattermost")
	attachmentProps, ferr := adapter.FormatEvent(outbox.FormatterInputs{
		EventType:   req.EventType,
		Alias:       alias,
		ProjectSlug: projectSlug,
		Summary:     summaryText,
		Intent:      intent,
		Payload:     req.Payload,
	})
	var attachments []map[string]any
	if ferr != nil {
		a.Logger.Warn("adapter FormatEvent failed; falling back to line-only outbox row",
			"event_type", req.EventType, "err", ferr)
	} else if raw, ok := attachmentProps["attachments"].([]map[string]any); ok {
		attachments = raw
	}

	id, err := events.InsertWithOutbox(r.Context(), a.Store.Pool, params,
		events.OutboxConfig{
			ProjectChannel: projectChannel,
			DefaultChannel: a.MattermostDefaultOutbox,
			Attachments:    attachments,
		},
		outboxMessage,
	)
	if err != nil {
		a.Logger.Error("insert event failed", "err", err)
		writeError(w, http.StatusInternalServerError, "insert_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, eventEmitResponse{EventID: id})
}

// writeSanitiserBlocked writes the metadata-only audit event. Returns the
// new event ID, or "" on failure (the operator-facing 422 still goes out;
// we just couldn't audit it). sanitiser.blocked is curated → flows to the
// outbox so the operator sees the §2.1 hit in Mattermost too.
func (a *App) writeSanitiserBlocked(ctx context.Context, agentID, blockedType, pattern, field string) string {
	id, err := events.InsertWithOutbox(ctx, a.Store.Pool, events.InsertParams{
		EventType: "sanitiser.blocked",
		AgentID:   agentID,
		Summary:   stringPtr("event blocked by §2.1 sanitiser; original event_type=" + blockedType),
		Payload: map[string]any{
			"matched_pattern": pattern,
			"matched_field":   field,
			"blocked_type":    blockedType,
		},
	}, events.OutboxConfig{
		DefaultChannel: a.MattermostDefaultOutbox,
	}, "sanitiser blocked event (type="+blockedType+", pattern="+pattern+", field="+field+")")
	if err != nil {
		a.Logger.Error("failed to write sanitiser.blocked audit event", "err", err)
		return ""
	}
	return id
}

func (a *App) writeResolveError(w http.ResponseWriter, err error, field, value string) {
	if errors.Is(err, pgx.ErrNoRows) {
		// v0.1.16 — detect work-item key shape on --task-key lookups and
		// return a tailored message pointing the caller at the right
		// surface. The misleading CLI help text on agentctl event-emit /
		// checkpoint ("e.g. feat-01-landing-page") sends agents here, but
		// concept-workflow work-items are never inserted into the tasks
		// table — wi-keys live in event payloads (payload.wi_key) for the
		// v0.1.14+ agent.work-item.{claimed,finished} pair. Plain 422
		// "unknown_reference" sent agents on extended hypothesis-chasing
		// detours; this guides them straight to the correct surface.
		if field == "task_key" && workItemKeyPattern.MatchString(value) {
			writeErrorWithDetails(w, http.StatusUnprocessableEntity, "task_key_looks_like_work_item",
				fmt.Sprintf("'%s' looks like a concept-workflow work-item key; the --task-key flag looks up the tasks table, which does NOT hold work-item keys. Either: omit --task-key (work-item keys live in event payloads), or use agentctl work-item claim/finish/active for work-item lifecycle.", value),
				map[string]string{
					"field":           field,
					"value":           value,
					"correct_surface": "agentctl work-item {claim,finish,active}",
					"docs":            "references/work-item-coordination.md",
				})
			return
		}
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

// extractIntent reads payload.intent without erroring on a non-string value
// (we still want the request to flow if the caller mis-typed; the strict
// enum check in handleEventEmit will catch unknown strings). A non-string
// payload.intent is treated as "" → default "info" semantics.
func extractIntent(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	if v, ok := payload["intent"].(string); ok {
		return v
	}
	return ""
}
