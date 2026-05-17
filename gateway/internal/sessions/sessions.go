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

// ResumePacket is the V2-critical brief returned by GET
// /v1/sessions/:id/resume-context. Identical-before-and-after-/clear is the
// load-bearing AC; the composition is intentionally deterministic.
type ResumePacket struct {
	Session    *Session     `json:"session"`
	Checkpoint *Checkpoint  `json:"latest_checkpoint,omitempty"`
	RecentEvents []EventTail `json:"recent_events"`
}

type EventTail struct {
	ID        string `json:"id"`
	EventType string `json:"event_type"`
	Summary   string `json:"summary,omitempty"`
	CreatedAt string `json:"created_at"`
}

// Resume composes the latest session row + latest checkpoint + recent events
// tail (most-recent first, capped). recentEventsLimit defaults to 20 if zero.
func Resume(ctx context.Context, pool *pgxpool.Pool, claudeSessionID string, recentEventsLimit int) (*ResumePacket, error) {
	if recentEventsLimit <= 0 {
		recentEventsLimit = 20
	}

	sess, err := GetByClaudeSessionID(ctx, pool, claudeSessionID)
	if err != nil {
		return nil, err
	}

	ckpt, err := LatestCheckpoint(ctx, pool, sess.ID)
	if err != nil {
		return nil, fmt.Errorf("latest checkpoint: %w", err)
	}

	events, err := recentEvents(ctx, pool, sess.ID, recentEventsLimit)
	if err != nil {
		return nil, fmt.Errorf("recent events: %w", err)
	}

	return &ResumePacket{
		Session:      sess,
		Checkpoint:   ckpt,
		RecentEvents: events,
	}, nil
}

func recentEvents(ctx context.Context, pool *pgxpool.Pool, sessionID string, limit int) ([]EventTail, error) {
	const q = `
		SELECT id, event_type, COALESCE(summary, ''), created_at
		  FROM events
		 WHERE agent_session_id = $1
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
