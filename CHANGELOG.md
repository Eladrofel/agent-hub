# Changelog — terraform-agent-hub

All notable changes to this project are documented here.

## [0.1.0] — 2026-05-17

Gateway endpoint flesh-out. Unblocks `concept-workflow` plugin v0.2.8 + v0.2.9 lifecycle hooks (Component B) and the operator-side `/setup-agent-events` flow. Outbox-worker + inbox-webhook remain stubbed; those ship in v0.1.1 alongside ROADMAP `#10` Component C.

### Added — `agent-hub serve` HTTP gateway

- **9 endpoints, all backed by real pgx-against-Postgres handlers + integration-tested:**
  - `GET /health` — no-auth liveness probe; pings Postgres, reports loaded sanitiser pattern count. Accepts HEAD too (Docker healthcheck uses `wget --spider`).
  - `POST /v1/events` — the canonical append-only write. Runs the §2.1 sanitiser over `summary` + `payload` BEFORE any DB resolve; on hit, returns 422 with the matched pattern name and writes a payload-free `sanitiser.blocked` audit event.
  - `POST /v1/agents/register` — idempotent update of the authenticated agent's role / host_kind / vm_hostname / capabilities / metadata. Name in body must match the bearer-token identity (no masquerading).
  - `POST /v1/sessions/start` — inserts `agent_sessions` row; auto-emits `session.started` event (best-effort).
  - `POST /v1/sessions/checkpoint` — inserts `session_checkpoints` row; auto-emits `session.checkpointed`.
  - `POST /v1/sessions/end` — marks session ended; auto-emits `session.ended` (Mattermost-curated only if `final_status != task_completed`).
  - `GET /v1/sessions/{claude_session_id}/resume-context` — composes the V2-critical resume packet: session row + latest checkpoint + recent events tail (capped at 20). Owner-only by default; operator-role agents read cross-session. Byte-identical across reads (validated by `TestResumeContext_IdempotentReads`).
  - `GET /v1/inbox?agent_name=…&since=…` — polls `mattermost_inbox` for undelivered messages addressed to the agent; marks delivered in the same transaction. Returns empty pre-Component C (no inbox-webhook writer yet).
  - `POST /v1/admin/agents/{name}/mint-token` — admin-only (gated by `ADMIN_TOKEN` env var). Upserts the agent, generates a fresh 32-byte base64-url token, bcrypts into `token_hash`. Returns plaintext exactly once.
- **Auth middleware** (`internal/auth`): bearer-token verification against bcrypt'd `agents.token_hash`, attaches `*Agent` (id, name, role, permissions) to request context. Best-effort `last_seen_at` update. Separate `RequireAdmin` middleware for `/v1/admin/*` with constant-time compare against `ADMIN_TOKEN`.
- **Sanitiser** (`internal/sanitiser`): loads RE2 patterns from `SANITISER_PATTERNS_FILE` (one regex per non-blank, non-comment line); `Check(summary, payload)` returns the matched pattern name without echoing the matched substring (would defeat the purpose). 5 unit tests + 2 integration tests confirm no leak in the 422 response or the audit event.
- **Per-domain DAOs**: `internal/agents`, `internal/sessions`, `internal/events`, `internal/inbox`. HTTP handlers in `internal/server/handlers_*.go` are thin glue; per-domain packages own SQL + types.

### Added — `agent-hub migrate` subcommand

- **Idempotent embedded migration runner** (`internal/store/migrate.go`): tracks applied versions in `schema_migrations`; each migration applies in its own transaction; safe to re-run on every boot. Migrations embedded via `gateway/db/embed.go` (`//go:embed migrations/*.sql`) so the binary is self-contained — no runtime dependency on a mounted volume.
- Called automatically by `serve` on startup so a fresh cluster comes up green.

### Changed

- **Migrations moved** from `db/migrations/` to `gateway/db/migrations/` so Go's `//go:embed` can reach them (embed can't cross the module root).
- **docker-compose.yml**: dropped the `docker-entrypoint-initdb.d` bind mount on Postgres. Caused a real bug — initdb applied SQL without marking `schema_migrations`, then the gateway's runner re-applied and 42P07'd on `CREATE TABLE`. Single source of truth is now the gateway's `migrate` subcommand (called by `serve` on startup).
- **docker-compose.yml**: `outbox-worker` + `inbox-webhook` services moved behind a `v0.1.1` compose profile so they don't crash-loop on the not-implemented stubs. Bring up only when v0.1.1 lands: `docker compose --profile v0.1.1 up -d`.
- **Dockerfile**: bumped build stage to `golang:1.25-alpine` (dependencies require Go ≥ 1.25).
- **`Bearer` auth + chi router + pgx/v5 + bcrypt** are the committed runtime stack.

### Tests

- **27 tests passing** against real Postgres (5 sanitiser unit + 22 server integration). Tests skip if `AGENT_HUB_TEST_DATABASE_URL` is unset, so `go test ./...` stays green on machines without a DB.
- **End-to-end smoke** via Docker Compose validates the full stack: mint token → register → session start → event emit → resume-context → sanitiser block (422). All paths green; see `TestMintToken_RotatesExistingAgent` for token rotation, `TestResumeContext_IdempotentReads` for the V2 critical AC.

### Known limitations (v0.1.0)

- **`agentctl` subcommands are still stubbed.** Client-side wiring to these endpoints comes in the next release.
- **Outbox-worker + inbox-webhook stubs remain.** Component C is the v0.1.1 release.
- **Tasks / handoffs / decisions / locks endpoints are deferred to v0.1.1.** Not called by plugin v0.2.8 or v0.2.9; first matter at plugin v0.2.13 (Component D-3). See plan §"Release prerequisite matrix".
- **No per-route rate limiting / no bcrypt cache.** Fine at the design's volume (5 peers, ~6500 events/day busy). Revisit if profiling shows it matters.

### Plugin coupling

`concept-workflow` plugin **v0.2.8+** (Component A — operator skills) and **v0.2.9+** (Component B — lifecycle hooks) consume this release. Without `AGENT_HUB_URL` set in `.claude/concept-workflow.local.md` the plugin still behaves byte-for-byte as v0.2.7 (Mode-1 chat-only); this gateway is opt-in per the plugin's release prerequisite matrix.

## [0.1.0-dev] — 2026-05-16

Initial scaffold. Ships ROADMAP `#10` Component A's infra-side scope of the concept-workflow plugin's agent-events system.

### Added

- **Terraform module** (`*.tf` at repo root) for the `agent-hub` VM on the `apollo` Proxmox node. Defaults: 2 vCPU / 4 GB RAM / 40 GB disk / static IP `10.0.5.50/16`. Mirrors the `terraform-mattermost` shape: `provider.tf`, `versions.tf`, `variables.tf`, `main.tf`, `outputs.tf`, `terraform.tfvars.example`, `cloud-init/user-data.yaml.tpl`. SAFETY layers: `prevent_destroy = true`, Proxmox-level `protection = true`, ignore_changes for known bpg/proxmox quirks.
- **Postgres schema** at `gateway/db/migrations/001_init.sql`: 11 tables — `projects`, `agents`, `agent_sessions`, `tasks`, `events` (append-only ledger), `session_checkpoints`, `handoffs`, `decisions`, `agent_locks`, `artifacts`, `mattermost_inbox`, `mattermost_outbox`. Indexes per the cradle-to-grave reference design. `events.artefact_pointer` field carries pointers to canonical Forgejo artefacts (per ADR-001 framing).
- **Docker-compose stack** (`docker-compose.yml`): postgres (PG16) + gateway + outbox-worker + inbox-webhook. Healthchecks on postgres + gateway. Localhost-only Postgres binding by default.
- **Go module skeleton** at `gateway/`: `go.mod`, `Dockerfile` (multi-stage build), two `main.go` entry points (`cmd/agent-hub` server + `cmd/agentctl` CLI), `sanitiser-patterns.txt` (§2.1 leak patterns). Cobra-based subcommand routing; endpoint implementations stubbed (return "not implemented" — flesh out in v0.1.x).
- **ADRs**: ADR-001 (Postgres-as-queryable-index-over-canonical-artefacts), ADR-002 (dedicated-VM topology), ADR-003 (per-host-token + dual-layer sanitiser), ADR-004 (Mattermost bidirectional via outbox + outgoing webhook).
- **Operator docs**: `README.md` (project orientation), `SETUP.md` (step-by-step walkthrough including Proxmox prereqs + Mattermost outgoing-webhook config + `/setup-agent-events` operator-Mac flow).
- **`.gitignore`** covers Terraform state, `.env`, Go build outputs, editor cruft.

### Known limitations (v0.1.0-dev)

- All HTTP endpoint handlers are stubbed (return `not implemented`). Flesh out in v0.1.0 patches before tagging `v0.1.0`.
- Outbox-worker + inbox-webhook subcommands are stubbed.
- `agentctl` subcommands are stubbed.
- No tests yet. v0.1.0 patches will add Go-level tests + a docker-compose-based integration test.
- Migration runner (`agent-hub migrate`) is stubbed; the docker-compose init mounts `db/migrations` into `/docker-entrypoint-initdb.d` so first-boot Postgres applies the schema on its own. Manual schema changes for now: `psql $DATABASE_URL -f gateway/db/migrations/00X_*.sql`.

### Plugin coupling

This project pairs with `concept-workflow` plugin **v0.2.8+** (ROADMAP `#10` Component A). Plugin earlier than that doesn't know about `AGENT_HUB_URL` and won't consume the gateway even when the VM is up.
