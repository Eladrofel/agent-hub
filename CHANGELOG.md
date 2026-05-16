# Changelog — terraform-agent-hub

All notable changes to this project are documented here.

## [0.1.0-dev] — 2026-05-16

Initial scaffold. Ships ROADMAP `#10` Component A's infra-side scope of the concept-workflow plugin's agent-events system.

### Added

- **Terraform module** (`*.tf` at repo root) for the `agent-hub` VM on the `apollo` Proxmox node. Defaults: 2 vCPU / 4 GB RAM / 40 GB disk / static IP `10.0.5.50/16`. Mirrors the `terraform-mattermost` shape: `provider.tf`, `versions.tf`, `variables.tf`, `main.tf`, `outputs.tf`, `terraform.tfvars.example`, `cloud-init/user-data.yaml.tpl`. SAFETY layers: `prevent_destroy = true`, Proxmox-level `protection = true`, ignore_changes for known bpg/proxmox quirks.
- **Postgres schema** at `db/migrations/001_init.sql`: 11 tables — `projects`, `agents`, `agent_sessions`, `tasks`, `events` (append-only ledger), `session_checkpoints`, `handoffs`, `decisions`, `agent_locks`, `artifacts`, `mattermost_inbox`, `mattermost_outbox`. Indexes per the cradle-to-grave reference design. `events.artefact_pointer` field carries pointers to canonical Forgejo artefacts (per ADR-001 framing).
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
- Migration runner (`agent-hub migrate`) is stubbed; the docker-compose init mounts `db/migrations` into `/docker-entrypoint-initdb.d` so first-boot Postgres applies the schema on its own. Manual schema changes for now: `psql $DATABASE_URL -f db/migrations/00X_*.sql`.

### Plugin coupling

This project pairs with `concept-workflow` plugin **v0.2.8+** (ROADMAP `#10` Component A). Plugin earlier than that doesn't know about `AGENT_HUB_URL` and won't consume the gateway even when the VM is up.
