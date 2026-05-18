// Package events owns the events table — the append-only ledger that is the
// gateway's most-written endpoint and the foundation everything else builds on.
//
// Caller-facing shape uses human-readable identifiers (project_slug, task_key,
// claude_session_id) rather than UUIDs; the DAO resolves them server-side so
// agentctl callers don't need to track UUIDs across requests.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Eladrofel/agent-hub/gateway/internal/outbox"
)

// CuratedEventTypes is the set of event_type values that get mirrored to
// Mattermost via mattermost_outbox. Membership = "operator wants to see this
// in chat by default". Conditional rendering (e.g., session.ended being
// suppressed when final_status=task_completed) is the outbox-worker's job;
// the gateway writes the outbox row unconditionally for any curated type so
// the worker has the data to make that call.
var CuratedEventTypes = map[string]bool{
	"task.created":      true,
	"task.claimed":      true,
	"task.blocked":      true,
	"task.unblocked":    true,
	"task.completed":    true,
	"decision.proposed": true,
	"decision.accepted": true,
	"decision.rejected": true,
	"handoff.created":   true,
	"session.ended":     true,
	"sanitiser.blocked": true,
}

// IsCurated reports whether an event_type should be mirrored to Mattermost.
func IsCurated(eventType string) bool {
	return CuratedEventTypes[eventType]
}

// InsertParams is the gateway's POST /v1/events request shape, post-resolution.
// Optional fields use pointers so the handler can distinguish "omitted" from
// "zero value" and so the DB receives proper NULLs.
type InsertParams struct {
	// Required
	EventType string
	AgentID   string // resolved from auth context

	// Optional scoping (resolved from slugs server-side)
	ProjectID       *string
	TaskID          *string
	AgentSessionID  *string
	ClaudeSessionID *string

	// Optional metadata
	EventVersion    int
	CorrelationID   *string
	CausationID     *string
	ParentEventID   *string
	ActorType       string
	ActorName       *string
	Branch          *string
	GitHeadSHA      *string
	WorktreePath    *string
	Summary         *string
	Payload         map[string]any
	ArtefactPointer map[string]any
}

// OutboxConfig controls how Insert resolves the Mattermost channel for
// curated events. ProjectChannel (resolved by the caller from the project
// row) wins if non-empty; otherwise DefaultChannel applies. If both are
// empty, no outbox row is written even for curated types — same as if the
// event is non-curated. Caller is responsible for logging that case.
type OutboxConfig struct {
	ProjectChannel string
	DefaultChannel string
}

// Insert writes one event and returns its uuid. Single-statement path.
// Used internally for non-transactional inserts (e.g., the sanitiser audit
// event, which must NOT recursively trigger another outbox write — though
// sanitiser.blocked IS curated, that recursion is broken by the caller
// using InsertWithOutbox for the outbox-bound writes and Insert for the
// audit row).
func Insert(ctx context.Context, pool *pgxpool.Pool, p InsertParams) (string, error) {
	if err := validateInsertParams(&p); err != nil {
		return "", err
	}
	payloadJSON, artefactJSON, err := marshalInsertJSON(p)
	if err != nil {
		return "", err
	}
	var id string
	err = pool.QueryRow(ctx, insertEventSQL,
		p.ProjectID, p.TaskID, p.AgentID, p.AgentSessionID,
		p.EventType, p.EventVersion,
		p.CorrelationID, p.CausationID, p.ParentEventID,
		p.ActorType, p.ActorName,
		p.Branch, p.GitHeadSHA, p.WorktreePath, p.ClaudeSessionID,
		p.Summary, payloadJSON, nullIfEmpty(artefactJSON),
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert event: %w", err)
	}
	return id, nil
}

// InsertWithOutbox writes an events row and, if the event_type is curated,
// also writes a mattermost_outbox row in the same transaction. Returns the
// new event id. Outbox writes are skipped (without erroring) when the
// resolved channel is empty — that's a config issue surfaced via a warning
// log in the caller, not a hard failure for the event write.
//
// This is the path POST /v1/events uses. The single-statement Insert above
// is retained for non-handler callers (audit events, tests) where the
// transactional semantics aren't needed and the recursive-outbox-for-the-
// audit-event question is sidestepped by skipping the outbox entirely.
func InsertWithOutbox(ctx context.Context, pool *pgxpool.Pool, p InsertParams, oc OutboxConfig, message string) (string, error) {
	if err := validateInsertParams(&p); err != nil {
		return "", err
	}
	payloadJSON, artefactJSON, err := marshalInsertJSON(p)
	if err != nil {
		return "", err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var id string
	err = tx.QueryRow(ctx, insertEventSQL,
		p.ProjectID, p.TaskID, p.AgentID, p.AgentSessionID,
		p.EventType, p.EventVersion,
		p.CorrelationID, p.CausationID, p.ParentEventID,
		p.ActorType, p.ActorName,
		p.Branch, p.GitHeadSHA, p.WorktreePath, p.ClaudeSessionID,
		p.Summary, payloadJSON, nullIfEmpty(artefactJSON),
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert event: %w", err)
	}

	if IsCurated(p.EventType) {
		channel := oc.ProjectChannel
		if channel == "" {
			channel = oc.DefaultChannel
		}
		if channel != "" {
			// Outbox message body. The outbox-worker renders the final
			// Mattermost markdown; we hand it the event id + a short
			// summary so it has enough to compose without a re-read.
			outMsg := message
			if outMsg == "" {
				outMsg = "[" + p.EventType + "]"
				if p.Summary != nil && *p.Summary != "" {
					outMsg = outMsg + " " + *p.Summary
				}
			}
			props := map[string]any{
				"event_type":      p.EventType,
				"event_id":        id,
				"idempotency_key": id + "_0",
			}
			if _, err := outbox.InsertPending(ctx, tx, outbox.InsertParams{
				ProjectID: p.ProjectID,
				TaskID:    p.TaskID,
				EventID:   id,
				ChannelID: channel,
				Message:   outMsg,
				Props:     props,
			}); err != nil {
				return "", fmt.Errorf("insert outbox row: %w", err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit tx: %w", err)
	}
	return id, nil
}

const insertEventSQL = `
	INSERT INTO events (
		project_id, task_id, agent_id, agent_session_id,
		event_type, event_version,
		correlation_id, causation_id, parent_event_id,
		actor_type, actor_name,
		branch, git_head_sha, worktree_path, claude_session_id,
		summary, payload, artefact_pointer
	) VALUES (
		$1, $2, $3, $4,
		$5, $6,
		$7, $8, $9,
		$10, $11,
		$12, $13, $14, $15,
		$16, $17, $18
	)
	RETURNING id`

func validateInsertParams(p *InsertParams) error {
	if p.EventType == "" {
		return errors.New("event_type required")
	}
	if p.AgentID == "" {
		return errors.New("agent_id required (derived from bearer token)")
	}
	if p.EventVersion == 0 {
		p.EventVersion = 1
	}
	if p.ActorType == "" {
		p.ActorType = "agent"
	}
	return nil
}

func marshalInsertJSON(p InsertParams) ([]byte, []byte, error) {
	payloadJSON, err := json.Marshal(coalesceMap(p.Payload))
	if err != nil {
		return nil, nil, fmt.Errorf("marshal payload: %w", err)
	}
	var artefactJSON []byte
	if p.ArtefactPointer != nil {
		artefactJSON, err = json.Marshal(p.ArtefactPointer)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal artefact_pointer: %w", err)
		}
	}
	return payloadJSON, artefactJSON, nil
}

// ResolveProjectID looks up a project by slug. Returns nil pointer (not an
// error) if slug is empty; returns an error wrapping pgx.ErrNoRows if the
// slug doesn't exist — caller chooses whether that's a 404 or 422.
func ResolveProjectID(ctx context.Context, pool *pgxpool.Pool, slug string) (*string, error) {
	if slug == "" {
		return nil, nil
	}
	var id string
	err := pool.QueryRow(ctx, `SELECT id FROM projects WHERE slug = $1`, slug).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("project slug %q: %w", slug, pgx.ErrNoRows)
		}
		return nil, err
	}
	return &id, nil
}

// ResolveProjectChannel returns the project's mattermost_outbox_channel
// (may be empty). Used by the events handler to decide which channel to
// write the outbox row against without pulling the whole projects row.
func ResolveProjectChannel(ctx context.Context, pool *pgxpool.Pool, projectID *string) (string, error) {
	if projectID == nil {
		return "", nil
	}
	var ch *string
	err := pool.QueryRow(ctx,
		`SELECT mattermost_outbox_channel FROM projects WHERE id = $1`,
		*projectID).Scan(&ch)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	if ch == nil {
		return "", nil
	}
	return *ch, nil
}

// ResolveTaskID looks up a task by key, optionally scoped to a project.
func ResolveTaskID(ctx context.Context, pool *pgxpool.Pool, projectID *string, taskKey string) (*string, error) {
	if taskKey == "" {
		return nil, nil
	}
	var (
		id  string
		err error
	)
	if projectID != nil {
		err = pool.QueryRow(ctx,
			`SELECT id FROM tasks WHERE task_key = $1 AND project_id = $2`,
			taskKey, *projectID).Scan(&id)
	} else {
		err = pool.QueryRow(ctx,
			`SELECT id FROM tasks WHERE task_key = $1`, taskKey).Scan(&id)
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("task_key %q: %w", taskKey, pgx.ErrNoRows)
		}
		return nil, err
	}
	return &id, nil
}

// ResolveSessionID looks up an active agent_sessions row by claude_session_id.
func ResolveSessionID(ctx context.Context, pool *pgxpool.Pool, claudeSessionID string) (*string, error) {
	if claudeSessionID == "" {
		return nil, nil
	}
	var id string
	err := pool.QueryRow(ctx,
		`SELECT id FROM agent_sessions WHERE claude_session_id = $1`,
		claudeSessionID).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("claude_session_id %q: %w", claudeSessionID, pgx.ErrNoRows)
		}
		return nil, err
	}
	return &id, nil
}

func coalesceMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func nullIfEmpty(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
