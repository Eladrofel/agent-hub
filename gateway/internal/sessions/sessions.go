// Package sessions owns agent_sessions + session_checkpoints. Session start,
// checkpoint, and end are the load-bearing hooks shipped in plugin v0.2.9
// (SessionStart / PreCompact / Stop / SessionEnd). Resume-context composes
// the latest checkpoint + recent events tail into a single JSON packet that
// SessionStart prepends to the agent's first message — the critical AC from
// V2: brief is identical before and after /clear.
package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// =============================================================================
// agent_sessions
// =============================================================================

type Session struct {
	ID              string         `json:"id"`
	ClaudeSessionID string         `json:"claude_session_id"`
	AgentID         string         `json:"agent_id"`
	ProjectID       *string        `json:"project_id,omitempty"`
	VMHostname      *string        `json:"vm_hostname,omitempty"`
	CWD             *string        `json:"cwd,omitempty"`
	WorktreePath    *string        `json:"worktree_path,omitempty"`
	Branch          *string        `json:"branch,omitempty"`
	BaseBranch      *string        `json:"base_branch,omitempty"`
	GitHeadSHA      *string        `json:"git_head_sha,omitempty"`
	StartReason     *string        `json:"start_reason,omitempty"`
	Status          string         `json:"status"`
	StartedAt       string         `json:"started_at"`
	EndedAt         *string        `json:"ended_at,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
}

type StartParams struct {
	ClaudeSessionID string
	AgentID         string
	ProjectID       *string
	VMHostname      *string
	CWD             *string
	WorktreePath    *string
	Branch          *string
	BaseBranch      *string
	GitHeadSHA      *string
	StartReason     *string
	Metadata        map[string]any
}

// Start inserts an agent_sessions row. The claude_session_id is the natural
// key from Claude Code's environment; UNIQUE constraint on it means re-runs
// of the same SessionStart hook (e.g., after a resumed connection) are
// caller-error, not silent overwrite.
func Start(ctx context.Context, pool *pgxpool.Pool, p StartParams) (*Session, error) {
	if p.ClaudeSessionID == "" {
		return nil, errors.New("claude_session_id required")
	}
	if p.AgentID == "" {
		return nil, errors.New("agent_id required")
	}
	metaJSON, err := json.Marshal(coalesceMap(p.Metadata))
	if err != nil {
		return nil, err
	}

	const q = `
		INSERT INTO agent_sessions (
			claude_session_id, agent_id, project_id, vm_hostname,
			cwd, worktree_path, branch, base_branch, git_head_sha,
			start_reason, metadata
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, $8, $9,
			$10, $11
		)
		RETURNING id, claude_session_id, agent_id, project_id, vm_hostname,
		          cwd, worktree_path, branch, base_branch, git_head_sha,
		          start_reason, status, started_at, ended_at, metadata`

	row := pool.QueryRow(ctx, q,
		p.ClaudeSessionID, p.AgentID, p.ProjectID, p.VMHostname,
		p.CWD, p.WorktreePath, p.Branch, p.BaseBranch, p.GitHeadSHA,
		p.StartReason, metaJSON,
	)
	return scanSession(row)
}

// End marks a session as ended. Returns the updated row.
func End(ctx context.Context, pool *pgxpool.Pool, claudeSessionID string) (*Session, error) {
	if claudeSessionID == "" {
		return nil, errors.New("claude_session_id required")
	}
	const q = `
		UPDATE agent_sessions
		   SET status = 'ended', ended_at = now()
		 WHERE claude_session_id = $1
		 RETURNING id, claude_session_id, agent_id, project_id, vm_hostname,
		           cwd, worktree_path, branch, base_branch, git_head_sha,
		           start_reason, status, started_at, ended_at, metadata`
	row := pool.QueryRow(ctx, q, claudeSessionID)
	s, err := scanSession(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("claude_session_id %q: %w", claudeSessionID, pgx.ErrNoRows)
		}
		return nil, err
	}
	return s, nil
}

// LatestForAgent returns the most-recent agent_sessions row for the given
// agent_id, optionally excluding one claude_session_id (used by the post-/clear
// fallback: the operator is in a NEW session and wants the PRIOR one). Returns
// pgx.ErrNoRows when the agent has zero sessions (or the only one matches the
// excludeClaudeSessionID filter).
//
// Added in v0.1.12 for the `agentctl resume-context` no-flag fallback. See
// `gateway/internal/server/handlers_agents.go::handleAgentLatestSession` for
// the response contract.
func LatestForAgent(ctx context.Context, pool *pgxpool.Pool, agentID, excludeClaudeSessionID string) (*Session, error) {
	if agentID == "" {
		return nil, errors.New("agent_id required")
	}
	const qBase = `
		SELECT id, claude_session_id, agent_id, project_id, vm_hostname,
		       cwd, worktree_path, branch, base_branch, git_head_sha,
		       start_reason, status, started_at, ended_at, metadata
		  FROM agent_sessions
		 WHERE agent_id = $1`
	const qOrder = `
		 ORDER BY started_at DESC
		 LIMIT 1`

	if excludeClaudeSessionID != "" {
		row := pool.QueryRow(ctx, qBase+`
		   AND claude_session_id <> $2`+qOrder, agentID, excludeClaudeSessionID)
		return scanSession(row)
	}
	row := pool.QueryRow(ctx, qBase+qOrder, agentID)
	return scanSession(row)
}

// GetByClaudeSessionID is the lookup primitive used by resume-context.
func GetByClaudeSessionID(ctx context.Context, pool *pgxpool.Pool, claudeSessionID string) (*Session, error) {
	const q = `
		SELECT id, claude_session_id, agent_id, project_id, vm_hostname,
		       cwd, worktree_path, branch, base_branch, git_head_sha,
		       start_reason, status, started_at, ended_at, metadata
		  FROM agent_sessions
		 WHERE claude_session_id = $1`
	row := pool.QueryRow(ctx, q, claudeSessionID)
	return scanSession(row)
}

func scanSession(row pgx.Row) (*Session, error) {
	var s Session
	var metaRaw []byte
	var startedAt time.Time
	var endedAt *time.Time
	err := row.Scan(
		&s.ID, &s.ClaudeSessionID, &s.AgentID, &s.ProjectID, &s.VMHostname,
		&s.CWD, &s.WorktreePath, &s.Branch, &s.BaseBranch, &s.GitHeadSHA,
		&s.StartReason, &s.Status, &startedAt, &endedAt, &metaRaw,
	)
	if err != nil {
		return nil, err
	}
	s.StartedAt = startedAt.UTC().Format(time.RFC3339Nano)
	if endedAt != nil {
		formatted := endedAt.UTC().Format(time.RFC3339Nano)
		s.EndedAt = &formatted
	}
	_ = json.Unmarshal(metaRaw, &s.Metadata)
	return &s, nil
}

// =============================================================================
// session_checkpoints
// =============================================================================

type Checkpoint struct {
	ID             string         `json:"id"`
	AgentSessionID string         `json:"agent_session_id"`
	TaskID         *string        `json:"task_id,omitempty"`
	CheckpointType string         `json:"checkpoint_type"`
	Status         string         `json:"status"`
	CurrentGoal    *string        `json:"current_goal,omitempty"`
	Summary        string         `json:"summary"`
	NextActions    []any          `json:"next_actions,omitempty"`
	OpenQuestions  []any          `json:"open_questions,omitempty"`
	FilesRelevant  []any          `json:"files_relevant,omitempty"`
	Risks          []any          `json:"risks,omitempty"`
	Payload        map[string]any `json:"payload,omitempty"`
	CreatedAt      string         `json:"created_at"`
}

type CheckpointParams struct {
	AgentSessionID string
	TaskID         *string
	CheckpointType string
	Status         string
	CurrentGoal    *string
	Summary        string
	NextActions    []any
	OpenQuestions  []any
	FilesRelevant  []any
	Risks          []any
	Payload        map[string]any
}

func InsertCheckpoint(ctx context.Context, pool *pgxpool.Pool, p CheckpointParams) (*Checkpoint, error) {
	if p.AgentSessionID == "" {
		return nil, errors.New("agent_session_id required")
	}
	if p.Summary == "" {
		return nil, errors.New("summary required")
	}
	if p.CheckpointType == "" {
		p.CheckpointType = "manual"
	}
	if p.Status == "" {
		p.Status = "in_progress"
	}

	nextJSON, _ := json.Marshal(coalesceSlice(p.NextActions))
	openJSON, _ := json.Marshal(coalesceSlice(p.OpenQuestions))
	filesJSON, _ := json.Marshal(coalesceSlice(p.FilesRelevant))
	risksJSON, _ := json.Marshal(coalesceSlice(p.Risks))
	payloadJSON, _ := json.Marshal(coalesceMap(p.Payload))

	const q = `
		INSERT INTO session_checkpoints (
			agent_session_id, task_id, checkpoint_type, status,
			current_goal, summary,
			next_actions, open_questions, files_relevant, risks, payload
		) VALUES (
			$1, $2, $3, $4,
			$5, $6,
			$7, $8, $9, $10, $11
		)
		RETURNING id, agent_session_id, task_id, checkpoint_type, status,
		          current_goal, summary,
		          next_actions, open_questions, files_relevant, risks, payload,
		          created_at`

	var (
		c            Checkpoint
		nextRaw      []byte
		openRaw      []byte
		filesRaw     []byte
		risksRaw     []byte
		payloadRaw   []byte
		createdAt    time.Time
	)
	err := pool.QueryRow(ctx, q,
		p.AgentSessionID, p.TaskID, p.CheckpointType, p.Status,
		p.CurrentGoal, p.Summary,
		nextJSON, openJSON, filesJSON, risksJSON, payloadJSON,
	).Scan(
		&c.ID, &c.AgentSessionID, &c.TaskID, &c.CheckpointType, &c.Status,
		&c.CurrentGoal, &c.Summary,
		&nextRaw, &openRaw, &filesRaw, &risksRaw, &payloadRaw,
		&createdAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert checkpoint: %w", err)
	}
	c.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
	_ = json.Unmarshal(nextRaw, &c.NextActions)
	_ = json.Unmarshal(openRaw, &c.OpenQuestions)
	_ = json.Unmarshal(filesRaw, &c.FilesRelevant)
	_ = json.Unmarshal(risksRaw, &c.Risks)
	_ = json.Unmarshal(payloadRaw, &c.Payload)
	return &c, nil
}

// LatestCheckpoint returns the most recent checkpoint for a session, or nil.
func LatestCheckpoint(ctx context.Context, pool *pgxpool.Pool, agentSessionID string) (*Checkpoint, error) {
	const q = `
		SELECT id, agent_session_id, task_id, checkpoint_type, status,
		       current_goal, summary,
		       next_actions, open_questions, files_relevant, risks, payload,
		       created_at
		  FROM session_checkpoints
		 WHERE agent_session_id = $1
		 ORDER BY created_at DESC
		 LIMIT 1`
	var (
		c          Checkpoint
		nextRaw    []byte
		openRaw    []byte
		filesRaw   []byte
		risksRaw   []byte
		payloadRaw []byte
		createdAt  time.Time
	)
	err := pool.QueryRow(ctx, q, agentSessionID).Scan(
		&c.ID, &c.AgentSessionID, &c.TaskID, &c.CheckpointType, &c.Status,
		&c.CurrentGoal, &c.Summary,
		&nextRaw, &openRaw, &filesRaw, &risksRaw, &payloadRaw,
		&createdAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	c.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
	_ = json.Unmarshal(nextRaw, &c.NextActions)
	_ = json.Unmarshal(openRaw, &c.OpenQuestions)
	_ = json.Unmarshal(filesRaw, &c.FilesRelevant)
	_ = json.Unmarshal(risksRaw, &c.Risks)
	_ = json.Unmarshal(payloadRaw, &c.Payload)
	return &c, nil
}

// =============================================================================
// resume-context composer
// =============================================================================
//
// The resume-context endpoint is the source-of-truth for cross-/clear handoff
// per Dale's 2026-05-23 directive: an agent that loses its in-process context
// (via /clear or session-restart) must be able to reconstruct what it was
// doing + what the fleet has learned by GET-ing this packet. The composition
// is intentionally deterministic so the same call returns the same JSON until
// new events land (V2's "identical before and after /clear" AC).
//
// What's IN the packet:
//   - session: the agent_sessions row.
//   - latest_checkpoint: most recent session_checkpoints row, or nil.
//   - recent_events: per-session event tail, capped, most-recent first.
//     Default excludes event_type='tool.used' (noise; 19/20 of an agent's
//     tail can be tool calls). Opt back in via ?include_tool_use=true.
//   - recent_improvements: last N agent.improvement-note events for THIS
//     agent across ALL sessions. Improvement-notes are cross-cutting fleet
//     learnings, not session-scoped state, so per-session filtering would
//     hide them. ?improvements_limit=N (default 10, max 50) tunes the cap.
//
// What's NOT (yet) in the packet — surface in a future release as
// concrete-use-cases land:
//   - open handoffs / claimable work
//   - open decisions / decisions needing review
//   - pending operator messages (currently fanned out via inbox poll)
//   - active locks held by this agent
// These are listed here so future readers don't assume their absence is a
// bug, and so the doc-comment is the authoritative spec for the response
// shape.

// ResumePacket is the V2-critical brief returned by GET
// /v1/sessions/:id/resume-context. Identical-before-and-after-/clear is the
// load-bearing AC; the composition is intentionally deterministic.
type ResumePacket struct {
	Session            *Session          `json:"session"`
	Checkpoint         *Checkpoint       `json:"latest_checkpoint,omitempty"`
	RecentEvents       []EventTail       `json:"recent_events"`
	RecentImprovements []ImprovementTail `json:"recent_improvements"`
}

type EventTail struct {
	ID        string `json:"id"`
	EventType string `json:"event_type"`
	Summary   string `json:"summary,omitempty"`
	CreatedAt string `json:"created_at"`
}

// ImprovementTail is one item in ResumePacket.RecentImprovements. Shape
// mirrors EventTail but adds the improvement-specific payload fields
// (category, intent, propagation_hint, context) decoded out of payload
// JSONB. AgentSessionID is nullable because v0.1.10-and-earlier improvement-
// notes are session-orphaned (the v0.1.11 bug this release fixes); we
// preserve their legibility for resume-context queries even though new
// emissions will always set it.
type ImprovementTail struct {
	ID              string  `json:"id"`
	EventID         string  `json:"event_id"`
	CreatedAt       string  `json:"created_at"`
	Summary         string  `json:"summary,omitempty"`
	Category        string  `json:"category,omitempty"`
	Intent          string  `json:"intent,omitempty"`
	PropagationHint string  `json:"propagation_hint,omitempty"`
	Context         string  `json:"context,omitempty"`
	AgentSessionID  *string `json:"agent_session_id"`
}

// ResumeOpts carries the optional knobs for Resume. Zero-value defaults
// match the historical behaviour (recentEventsLimit=20, improvementsLimit=10,
// tool.used filtered). The handler maps query params onto this struct.
type ResumeOpts struct {
	RecentEventsLimit  int
	ImprovementsLimit  int
	IncludeToolUse     bool
}

const (
	defaultRecentEventsLimit = 20
	defaultImprovementsLimit = 10
	maxImprovementsLimit     = 50
)

// Resume composes the latest session row + latest checkpoint + recent events
// tail (most-recent first, capped) + recent improvement-notes for the agent
// across all sessions. See the package-level doc for the response contract.
func Resume(ctx context.Context, pool *pgxpool.Pool, claudeSessionID string, opts ResumeOpts) (*ResumePacket, error) {
	if opts.RecentEventsLimit <= 0 {
		opts.RecentEventsLimit = defaultRecentEventsLimit
	}
	if opts.ImprovementsLimit <= 0 {
		opts.ImprovementsLimit = defaultImprovementsLimit
	}
	if opts.ImprovementsLimit > maxImprovementsLimit {
		opts.ImprovementsLimit = maxImprovementsLimit
	}

	sess, err := GetByClaudeSessionID(ctx, pool, claudeSessionID)
	if err != nil {
		return nil, err
	}

	ckpt, err := LatestCheckpoint(ctx, pool, sess.ID)
	if err != nil {
		return nil, fmt.Errorf("latest checkpoint: %w", err)
	}

	events, err := recentEvents(ctx, pool, sess.ID, opts.RecentEventsLimit, opts.IncludeToolUse)
	if err != nil {
		return nil, fmt.Errorf("recent events: %w", err)
	}

	improvements, err := recentImprovements(ctx, pool, sess.AgentID, opts.ImprovementsLimit)
	if err != nil {
		return nil, fmt.Errorf("recent improvements: %w", err)
	}

	return &ResumePacket{
		Session:            sess,
		Checkpoint:         ckpt,
		RecentEvents:       events,
		RecentImprovements: improvements,
	}, nil
}

// recentEvents returns the per-session event tail. tool.used is excluded by
// default (it can occupy 19/20 of the tail in active sessions, drowning out
// session.checkpointed / agent.improvement-note / progress.updated). Callers
// who want the unfiltered raw stream pass includeToolUse=true.
func recentEvents(ctx context.Context, pool *pgxpool.Pool, sessionID string, limit int, includeToolUse bool) ([]EventTail, error) {
	q := `
		SELECT id, event_type, COALESCE(summary, ''), created_at
		  FROM events
		 WHERE agent_session_id = $1`
	if !includeToolUse {
		q += `
		   AND event_type <> 'tool.used'`
	}
	q += `
		 ORDER BY created_at DESC
		 LIMIT $2`

	rows, err := pool.Query(ctx, q, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]EventTail, 0, limit)
	for rows.Next() {
		var e EventTail
		var createdAt time.Time
		if err := rows.Scan(&e.ID, &e.EventType, &e.Summary, &createdAt); err != nil {
			return nil, err
		}
		e.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
		out = append(out, e)
	}
	return out, rows.Err()
}

// recentImprovements returns the last N agent.improvement-note events for
// the given agent across ALL sessions. Improvement-notes are cross-cutting
// learnings, not session-scoped state, so per-session filtering would hide
// the fleet's history from a freshly-/clear'd agent. Decoded payload fields
// (category / intent / propagation_hint / context) are surfaced flat to
// avoid forcing the caller to re-decode JSONB.
func recentImprovements(ctx context.Context, pool *pgxpool.Pool, agentID string, limit int) ([]ImprovementTail, error) {
	const q = `
		SELECT id, COALESCE(summary, ''), payload, agent_session_id, created_at
		  FROM events
		 WHERE agent_id = $1
		   AND event_type = 'agent.improvement-note'
		 ORDER BY created_at DESC
		 LIMIT $2`
	rows, err := pool.Query(ctx, q, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ImprovementTail, 0, limit)
	for rows.Next() {
		var (
			it        ImprovementTail
			payload   []byte
			createdAt time.Time
			sessID    *string
		)
		if err := rows.Scan(&it.ID, &it.Summary, &payload, &sessID, &createdAt); err != nil {
			return nil, err
		}
		it.EventID = it.ID // mirror so the shape matches per-event consumers' expectations
		it.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
		it.AgentSessionID = sessID

		// Decode the curated payload fields. Best-effort: a malformed JSONB
		// row shouldn't fail the whole resume — we just surface the bare row.
		var pl map[string]any
		if err := json.Unmarshal(payload, &pl); err == nil {
			if v, ok := pl["category"].(string); ok {
				it.Category = v
			}
			if v, ok := pl["intent"].(string); ok {
				it.Intent = v
			}
			if v, ok := pl["propagation_hint"].(string); ok {
				it.PropagationHint = v
			}
			if v, ok := pl["context"].(string); ok {
				it.Context = v
			}
			// Prefer payload.summary if the event-row summary column was
			// empty (shouldn't happen for v0.1.9+ improvement emits, but
			// keeps legacy rows legible).
			if it.Summary == "" {
				if v, ok := pl["summary"].(string); ok {
					it.Summary = v
				}
			}
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func coalesceMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func coalesceSlice(s []any) []any {
	if s == nil {
		return []any{}
	}
	return s
}
