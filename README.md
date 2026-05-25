# agent-hub

Terraform module + service code for the **agent-events hub** — a Postgres-backed event ledger + Mattermost outbox/inbox that serves the `concept-workflow` plugin's peer-agent fleet (4 VM agents + the operator's Mac, today).

## What it does

Provisions a single Ubuntu 24.04 VM on the `apollo` Proxmox cluster (IP via DHCP reservation; default expected `10.0.5.38`) and runs a `docker-compose` stack with:

| Service | Port | Purpose |
|---|---|---|
| `postgres` | `5432` (localhost-only) | 11-table schema: events / agent_sessions / tasks / handoffs / decisions / locks / artifacts / mattermost_inbox / mattermost_outbox + indexes. |
| `gateway` | `8787` | HTTP API for peer agents — event-emit, session-lifecycle, checkpoint, resume-context (`/v1/sessions/{id}/resume-context`, `/v1/me/latest-session`), work-item peer-coordination (`/v1/work-items/{wi-key}/active-claims`, v0.1.14+), inbox poll. Per-host bearer auth + `§2.1` sanitiser at write. |
| `outbox-worker` | (no port) | Polls `mattermost_outbox`, posts to Mattermost via service-account PAT. At-least-once + idempotency keys. Auto-relay for curated event types: `agent.improvement-note`, `agent.work-item.claimed` (🔵 + blue), `agent.work-item.finished` (✅ + green), plus the session-lifecycle events. v0.1.15+ prepends `@<peer-alias>` mentions on work-item events so peers get inbox-routed automatically. |
| `inbox-webhook` | `8788` | Receives Mattermost outgoing webhooks → writes to `mattermost_inbox`. Peer agents poll on `SessionStart` + `Stop` hooks, plus a throttled mid-session `PostToolUse` poll (concept-workflow plugin v0.5.6+). |

**Designed for** the agent-events layer of the [concept-workflow plugin](https://github.com/Eladrofel/concept-workflow) — ROADMAP `#10`. The plugin's lifecycle hooks shell out to `agentctl` (a Go binary distributed from this repo) which hits the gateway's HTTP API.

## What this is (and what it isn't)

`agent-hub` is **self-hosted coordination infrastructure** for a small fleet of Claude Code sessions running on the operator's own VMs. It exists so that Claude Code sessions on different physical hosts (e.g., `agent-1`, `agent-2`, the operator's Mac) can share durable state — work-item claims, lifecycle events, peer @-mentions, captured improvement-notes — that Claude Code itself has no cross-host primitive for today.

### What it is NOT

- **NOT a Claude API wrapper, proxy, or replicator.** The gateway never calls the Claude API. It only receives HTTP requests from `agentctl` (the Go CLI the operator runs on their own machines) and stores them in Postgres.
- **NOT an "agentic framework" that drives Claude.** Each Claude Code session is operator-driven; the agent does work when a human prompts it, not in autonomous loops between prompts. The gateway is downstream of those sessions, not driving them.
- **NOT a multi-tenant or shared-account system.** Every agent VM runs Claude Code under its own paid Anthropic account. The gateway has no Anthropic credentials and no path to obtain one.
- **NOT a competitor to Anthropic-hosted features** like routines or scheduled agents. Where first-party features fit (cron-shape automation, cloud-side orchestration), prefer those. This project addresses the multi-host peer-coordination gap, which routines don't solve.

### Topology

```
┌────────────────────────────────────────────────────────────────────┐
│  Anthropic Cloud                                                   │
│  ────────────────                                                  │
│  Claude API — accessed per VM with that VM's OWN paid account.     │
│  Standard Claude Code CLI; no API wrapping or shared-account use.  │
└──────────────┬───────────────┬──────────────┬─────────────────────┘
               │ HTTPS         │ HTTPS        │ HTTPS
               │ per-account   │ per-account  │ per-account
               │               │              │
       ┌───────▼──────┐ ┌──────▼─────┐ ┌─────▼──────┐
       │ operator-Mac │ │ claude-1   │ │ claude-2   │
       │ "Splinter"   │ │ "Mikey"    │ │ "Donnie"   │
       │              │ │            │ │            │
       │ Claude Code  │ │ Claude Code│ │ Claude Code│
       │ + agentctl   │ │ + agentctl │ │ + agentctl │
       └───────┬──────┘ └──────┬─────┘ └─────┬──────┘
               │               │             │
               │  HTTP (private network — no Anthropic involvement)
               └───────────────┼─────────────┘
                               │
                               ▼
       ┌───────────────────────────────────────┐
       │           agent-hub VM                │
       │           (self-hosted)               │
       │                                       │
       │  gateway (Go) ──▶ Postgres            │
       │       │                               │
       │       ├─▶ outbox-worker ──▶ MM        │
       │       │                               │
       │       └◀── inbox-webhook ◀── MM       │
       └───────────────────────────────────────┘
                               │
                               ▼
                  ┌──────────────────┐
                  │  Mattermost      │
                  │  (self-hosted)   │
                  └──────────────────┘
```

The Anthropic API touches only the leftmost column (Claude Code → Claude API, per VM, per paid account). Everything below the operator-Mac/claude-1/claude-2 row is the operator's own infrastructure on the operator's own private network. The gateway has no outbound path to Anthropic; the agentctl CLI talks only to the gateway over private HTTP.

## Repository layout

```
terraform-agent-hub/
├── README.md                   ← you are here
├── SETUP.md                    ← operator walkthrough
├── CHANGELOG.md
├── docker-compose.yml          ← deploys on the agent-hub VM via cloud-init
├── .env.example                ← copy to .env and populate
├── *.tf                        ← Terraform module (provider/versions/main/outputs/variables)
├── terraform.tfvars.example    ← copy to terraform.tfvars; gitignored
├── cloud-init/
│   └── user-data.yaml.tpl      ← VM bootstrap
├── gateway/
│   ├── Dockerfile
│   ├── go.mod
│   ├── cmd/agent-hub/main.go   ← server binary (serve / outbox-worker / inbox-webhook subcommands)
│   ├── cmd/agentctl/main.go    ← CLI binary, distributed to agent VMs + Mac
│   ├── internal/               ← package skeletons (auth / events / sessions / outbox / inbox / sanitiser / store)
│   ├── db/migrations/001_init.sql ← 11-table schema; embedded into the binary and applied by the gateway's migrate runner on boot
│   └── sanitiser-patterns.txt  ← §2.1 leak patterns applied at write time
├── cli/install.sh              ← SSH-installable on agent VMs (cp binary + chmod)
├── scripts/                    ← helpers
└── research/decisions/         ← ADR-001..ADR-004 (this project's architecture decisions)
```

## VM specs (defaults; override via `terraform.tfvars`)

| Resource | Value |
|---|---|
| Hostname | `agent-hub` |
| Expected IP | `10.0.5.38/16` (assigned via DHCP reservation against `vm_mac_address`) |
| Static MAC | `BC:24:11:10:05:38` (last byte matches IP for memorability; **reserve in DHCP BEFORE first apply**) |
| DNS name | `agent-hub.litts.link` (resolved on the apollo network) |
| vCPU / RAM | 2 / 4 GB |
| Disk | 60 GB |
| OS | Ubuntu 24.04 |
| SSH user | `dale` (passwordless sudo, SSH key-only) |

## Quick start

See [SETUP.md](SETUP.md) for the full walkthrough. Short version:

1. Populate `terraform.tfvars` (copy from `terraform.tfvars.example`).
2. `terraform init && terraform apply`.
3. SSH to the new VM, populate `/opt/agent-hub/.env` (placeholder values are intentional; you must set them yourself), then `cd /opt/agent-hub && docker compose up -d`.
4. From your operator Mac, edit `<consuming-workspace>/.claude/concept-workflow.local.md` and add an `agent-events:` block pointing at the VM.
5. Run `/setup-agent-events` from a Claude Code session on the Mac.

## Versioning + commit identity

- **Project version:** lives in `Makefile` (`VERSION ?=` line, baked into binaries via ldflags) + `CHANGELOG.md`. Currently `0.1.16`.
- **Plugin compatibility:** `concept-workflow` plugin v0.2.8+ (ROADMAP `#10` Component A) is the first plugin release that consumes this project; the work-item peer-coordination pair (v0.1.14 gateway + v0.5.4 plugin) is the current floor for full feature parity, with v0.1.15 (peer @-mentions) + v0.1.16 (smart 422) + plugin v0.5.5 (operator-courier policy) + v0.5.6 (mid-session inbox poll) on top.
- **Commit identity:** this project lives outside `~/projects/secureup/` so it uses the default `~/.gitconfig` identity (typically `Eladrofel`). This is the same identity that publishes the `concept-workflow` plugin to the GitHub marketplace.

## Related

- **`concept-workflow` plugin** — consumer of agent-hub; lives at `~/projects/dev-environment/Claude/plugins/concept-workflow/`. ROADMAP `#10` is the canonical entry for the cross-project effort.
- **`terraform-mattermost`** — sibling Terraform project for the chat layer. Mattermost is reused by agent-hub's outbox-worker + inbox-webhook (no separate Mattermost provisioning here).
- **`terraform-apollo`** — owns the shared Ubuntu cloud image referenced from `main.tf`. Apply that first if it's not already present on the Proxmox node.

## Status

**`v0.1.16` — in active use.** Gateway endpoints + agentctl subcommands fleshed out and serving live traffic from the operator's fleet (operator-Mac + claude-1 + claude-2). Cross-/clear session handoff (`v0.1.11`–`v0.1.13`), work-item peer coordination (`v0.1.14`), proactive peer @-mentions (`v0.1.15`), and smart-422 namespace-mismatch detection (`v0.1.16`) all shipped and verified end-to-end.

Schema is settled (no migrations since `001_init.sql` + small additive follow-ups in 002/003); the event-type column is `text` so future curated event types are additive-only with no DB changes. See `CHANGELOG.md` for the release narrative.

### Capability surface (current)

- **Session lifecycle:** start, checkpoint, end, resume-context (with `--prior` and no-flag auto-walk to latest prior session per `v0.1.12`/`v0.1.13`).
- **Improvement-notes:** durable + optionally MM-relayed captured-learning events, with `--intent` (info / directive / question / blocker / status), category enum, sanitiser-gated summary + details.
- **Work-item peer coordination:** `agentctl work-item claim/finish/active`; the gateway's `agent.work-item.{claimed,finished}` curated events auto-relay to MM with peer @-mentions; `GET /v1/work-items/{wi-key}/active-claims` is the agent-readable pre-flight backing the plugin's `/start-work-item` Pre-flight 4 conflict check.
- **Cross-/clear handoff:** post-`/clear` agent runs `agentctl resume-context` (no flags) and the gateway auto-walks to the agent's most-recent prior session, returns the full resume packet.
- **Inbox routing:** Mattermost outgoing-webhook → inbox-webhook → per-agent `mattermost_inbox` rows, polled on `SessionStart` / `Stop` / throttled `PostToolUse`.
- **§2.1 sanitiser** at write time; agent-side hook also pre-filters tool.used payloads.
