-- 001_init.sql
-- Initial schema for agent-hub: Postgres-backed event ledger + session/task/handoff state
-- + Mattermost outbox + inbox + agent-lock primitives, supporting the concept-workflow
-- plugin's peer-agent fleet (4 VM agents + operator-on-Mac).
--
-- Design: ADR-001-postgres-as-queryable-index-over-canonical-artefacts.md
-- All tables are append-mostly. The `events` table is strictly append-only;
-- corrections to past events are themselves new events.
--
-- Retention: see ADR-002. `events` is hot for 90 days (configurable);
-- archived to compressed JSONL beyond that. session_checkpoints, handoffs,
-- decisions, tasks are NOT archived — small + load-bearing for resume.

CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
-- pgvector deferred to a future ROADMAP item (cradle-to-grave §Phase 6).

-- =============================================================================
-- projects: one row per consuming workspace (e.g., secureup-concepts)
-- =============================================================================
CREATE TABLE projects (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  slug text NOT NULL UNIQUE,
  name text NOT NULL,
  forge_url text,                       -- primary forge URL (e.g., ssh://...:2222/secureup-concepts/workspace.git)
  default_branch text DEFAULT 'main',
  mattermost_outbox_channel text,
  mattermost_inbox_channel text,
  created_at timestamptz NOT NULL DEFAULT now()
);

-- =============================================================================
-- agents: one row per peer agent (4 VM agents + 1 operator-Mac, today)
-- =============================================================================
CREATE TABLE agents (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL UNIQUE,            -- "agent-1".."agent-4", "agent-operator-mac"
  role text,                            -- "frontend", "backend", "review", "operator"
  host_kind text,                       -- "linux-vm" | "macos" (informs token storage UX)
  vm_hostname text,                     -- "claude-1".."claude-4", or Mac's hostname
  mattermost_username text,
  token_hash text,                      -- bcrypt of the per-host token
  permissions jsonb NOT NULL DEFAULT '{}'::jsonb,
  capabilities jsonb NOT NULL DEFAULT '[]'::jsonb,
  metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  last_seen_at timestamptz
);

CREATE INDEX idx_agents_role ON agents(role);

-- =============================================================================
-- agent_sessions: one row per Claude Code CLI session on a host
-- =============================================================================
CREATE TABLE agent_sessions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  claude_session_id text NOT NULL UNIQUE,
  agent_id uuid NOT NULL REFERENCES agents(id),
  project_id uuid REFERENCES projects(id),
  vm_hostname text,
  cwd text,
  worktree_path text,
  branch text,
  base_branch text,
  git_head_sha text,
  start_reason text,
  status text NOT NULL DEFAULT 'active',
  started_at timestamptz NOT NULL DEFAULT now(),
  ended_at timestamptz,
  metadata jsonb NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX idx_agent_sessions_agent ON agent_sessions(agent_id);
CREATE INDEX idx_agent_sessions_project ON agent_sessions(project_id);
CREATE INDEX idx_agent_sessions_status ON agent_sessions(status);

-- =============================================================================
-- tasks: workspace work items cross-referenced from Forgejo
-- =============================================================================
CREATE TABLE tasks (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id),
  task_key text NOT NULL UNIQUE,        -- e.g., "feat-04-dev-affordances-flag"
  title text NOT NULL,
  description text,
  status text NOT NULL DEFAULT 'open',
  priority text NOT NULL DEFAULT 'normal',
  assigned_agent_id uuid REFERENCES agents(id),
  forge_brief_path text,                -- pointer to canonical brief in Forgejo
  mattermost_root_post_id text,
  metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  completed_at timestamptz
);

CREATE INDEX idx_tasks_project_status ON tasks(project_id, status);
CREATE INDEX idx_tasks_assigned_agent ON tasks(assigned_agent_id);

-- =============================================================================
-- events: append-only ledger. Hot retention 90 days (see retention policy).
-- =============================================================================
CREATE TABLE events (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid REFERENCES projects(id),
  task_id uuid REFERENCES tasks(id),
  agent_id uuid REFERENCES agents(id),
  agent_session_id uuid REFERENCES agent_sessions(id),

  event_type text NOT NULL,             -- e.g., "session.started", "task.claimed"
  event_version int NOT NULL DEFAULT 1,

  correlation_id uuid,
  causation_id uuid REFERENCES events(id),
  parent_event_id uuid REFERENCES events(id),

  actor_type text NOT NULL DEFAULT 'agent',
  actor_name text,

  branch text,
  git_head_sha text,
  worktree_path text,
  claude_session_id text,

  summary text,
  payload jsonb NOT NULL DEFAULT '{}'::jsonb,
  -- Pointer to canonical artefact in Forgejo (per ADR-001/ADR-009 framing)
  artefact_pointer jsonb,               -- {"repo":"customer-web","commit_sha":"abc","path":"docs/plans/..."}

  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_events_project_created ON events(project_id, created_at DESC);
CREATE INDEX idx_events_task_created ON events(task_id, created_at DESC);
CREATE INDEX idx_events_agent_created ON events(agent_id, created_at DESC);
CREATE INDEX idx_events_session_created ON events(agent_session_id, created_at DESC);
CREATE INDEX idx_events_type_created ON events(event_type, created_at DESC);
CREATE INDEX idx_events_correlation ON events(correlation_id);
CREATE INDEX idx_events_payload_gin ON events USING gin(payload);

-- =============================================================================
-- session_checkpoints: durable resume state across /clear
-- =============================================================================
CREATE TABLE session_checkpoints (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  agent_session_id uuid NOT NULL REFERENCES agent_sessions(id),
  task_id uuid REFERENCES tasks(id),
  checkpoint_type text NOT NULL DEFAULT 'manual',
  status text NOT NULL DEFAULT 'in_progress',

  current_goal text,
  summary text NOT NULL,
  next_actions jsonb NOT NULL DEFAULT '[]'::jsonb,
  open_questions jsonb NOT NULL DEFAULT '[]'::jsonb,
  files_relevant jsonb NOT NULL DEFAULT '[]'::jsonb,
  risks jsonb NOT NULL DEFAULT '[]'::jsonb,
  payload jsonb NOT NULL DEFAULT '{}'::jsonb,

  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_checkpoints_session_created ON session_checkpoints(agent_session_id, created_at DESC);
CREATE INDEX idx_checkpoints_task_created ON session_checkpoints(task_id, created_at DESC);

-- =============================================================================
-- handoffs: structured handoffs between peer agents; pointer to canonical
-- Forgejo handoff document if one exists.
-- =============================================================================
CREATE TABLE handoffs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id),
  task_id uuid REFERENCES tasks(id),

  from_agent_id uuid REFERENCES agents(id),
  to_agent_id uuid REFERENCES agents(id),

  from_session_id uuid REFERENCES agent_sessions(id),
  to_session_id uuid REFERENCES agent_sessions(id),

  status text NOT NULL DEFAULT 'open',

  title text NOT NULL,
  summary text NOT NULL,
  current_state text,
  next_steps jsonb NOT NULL DEFAULT '[]'::jsonb,
  risks jsonb NOT NULL DEFAULT '[]'::jsonb,
  files_changed jsonb NOT NULL DEFAULT '[]'::jsonb,
  files_relevant jsonb NOT NULL DEFAULT '[]'::jsonb,
  branch text,
  git_head_sha text,
  artefact_pointer jsonb,               -- pointer to canonical handoff doc in Forgejo

  payload jsonb NOT NULL DEFAULT '{}'::jsonb,

  created_at timestamptz NOT NULL DEFAULT now(),
  accepted_at timestamptz,
  completed_at timestamptz
);

CREATE INDEX idx_handoffs_project_status ON handoffs(project_id, status);
CREATE INDEX idx_handoffs_task_status ON handoffs(task_id, status);

-- =============================================================================
-- decisions: operator/agent decisions surfaced via Mattermost
-- =============================================================================
CREATE TABLE decisions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id),
  task_id uuid REFERENCES tasks(id),
  proposed_by_agent_id uuid REFERENCES agents(id),

  status text NOT NULL DEFAULT 'proposed',
  question text NOT NULL,
  options jsonb NOT NULL DEFAULT '[]'::jsonb,
  recommended_option text,
  selected_option text,
  rationale text,
  decided_by text,

  mattermost_post_id text,

  created_at timestamptz NOT NULL DEFAULT now(),
  decided_at timestamptz
);

CREATE INDEX idx_decisions_project_status ON decisions(project_id, status);
CREATE INDEX idx_decisions_task_status ON decisions(task_id, status);

-- =============================================================================
-- agent_locks: advisory locks for collision-prone work
-- =============================================================================
CREATE TABLE agent_locks (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id),
  task_id uuid REFERENCES tasks(id),
  agent_id uuid NOT NULL REFERENCES agents(id),

  lock_key text NOT NULL,
  lock_type text NOT NULL DEFAULT 'advisory',
  reason text,
  expires_at timestamptz NOT NULL,

  created_at timestamptz NOT NULL DEFAULT now(),
  released_at timestamptz,

  UNIQUE(project_id, lock_key, released_at)
);

CREATE INDEX idx_agent_locks_active
ON agent_locks(project_id, lock_key)
WHERE released_at IS NULL;

-- =============================================================================
-- artifacts: pointers to per-run artefacts (screenshots, logs, etc.) outside
-- source control. URI may be a filesystem path or s3:// / r2:// / minio:// .
-- =============================================================================
CREATE TABLE artifacts (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid REFERENCES projects(id),
  task_id uuid REFERENCES tasks(id),
  event_id uuid REFERENCES events(id),
  agent_session_id uuid REFERENCES agent_sessions(id),

  artifact_type text NOT NULL,
  uri text NOT NULL,
  description text,
  content_type text,
  size_bytes bigint,
  sha256 text,

  metadata jsonb NOT NULL DEFAULT '{}'::jsonb,

  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_artifacts_task_created ON artifacts(task_id, created_at DESC);

-- =============================================================================
-- mattermost_outbox: gateway emits events → outbox-worker posts to Mattermost.
-- At-least-once delivery; idempotency via props.idempotency_key on each post.
-- Rows deleted after successful post (kept on permanent failure for debugging).
-- =============================================================================
CREATE TABLE mattermost_outbox (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid REFERENCES projects(id),
  task_id uuid REFERENCES tasks(id),
  event_id uuid REFERENCES events(id),

  channel_id text NOT NULL,
  root_post_id text,
  message text NOT NULL,
  props jsonb NOT NULL DEFAULT '{}'::jsonb,

  status text NOT NULL DEFAULT 'pending',
  attempts int NOT NULL DEFAULT 0,
  last_error text,

  created_at timestamptz NOT NULL DEFAULT now(),
  sent_at timestamptz
);

CREATE INDEX idx_mattermost_outbox_pending
ON mattermost_outbox(status, created_at)
WHERE status = 'pending';

-- =============================================================================
-- mattermost_inbox: outgoing-webhook receiver writes here; target agent reads
-- on SessionStart hook + optional Stop poll. Rows deleted after first poll
-- by the addressed agent.
-- =============================================================================
CREATE TABLE mattermost_inbox (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid REFERENCES projects(id),
  target_agent_id uuid NOT NULL REFERENCES agents(id),

  source_username text,                 -- the Mattermost user who posted
  source_channel_id text,
  source_post_id text,
  message text NOT NULL,
  props jsonb NOT NULL DEFAULT '{}'::jsonb,

  delivered_at timestamptz,             -- when target_agent first polled and saw this row
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_mattermost_inbox_pending
ON mattermost_inbox(target_agent_id, created_at)
WHERE delivered_at IS NULL;
