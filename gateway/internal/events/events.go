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
)

// InsertParams is the gateway's POST /v1/events request shape, post-resolution.
// Optional fields use pointers so the handler can distinguish "omitted" from
// "zero value" and so the DB receives proper NULLs.
type InsertParams struct {
	// Required
	EventType string
	AgentID   string // resolved from auth context

	// Optional scoping (resolved from slugs server-side)
	ProjectID        *string
	TaskID           *string
	AgentSessionID   *string
	ClaudeSessionID  *string

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

// Insert writes one event and returns its uuid.
func Insert(ctx context.Context, pool *pgxpool.Pool, p InsertParams) (string, error) {
	if p.EventType == "" {
		return "", errors.New("event_type required")
	}
	if p.AgentID == "" {
		return "", errors.New("agent_id required (derived from bearer token)")
	}
	if p.EventVersion == 0 {
		p.EventVersion = 1
	}
	if p.ActorType == "" {
		p.ActorType = "agent"
	}

	payloadJSON, err := json.Marshal(coalesceMap(p.Payload))
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}
	var artefactJSON []byte
	if p.ArtefactPointer != nil {
		artefactJSON, err = json.Marshal(p.ArtefactPointer)
		if err != nil {
			return "", fmt.Errorf("marshal artefact_pointer: %w", err)
		}
	}

	const q = `
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

	var id string
	err = pool.QueryRow(ctx, q,
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
