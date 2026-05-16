# terraform-agent-hub

Terraform module + service code for the **agent-events hub** — a Postgres-backed event ledger + Mattermost outbox/inbox that serves the `concept-workflow` plugin's peer-agent fleet (4 VM agents + the operator's Mac, today).

## What it does

Provisions a single Ubuntu 24.04 VM on the `apollo` Proxmox cluster (default IP `10.0.5.50`) and runs a `docker-compose` stack with:

| Service | Port | Purpose |
|---|---|---|
| `postgres` | `5432` (localhost-only) | 11-table schema: events / agent_sessions / tasks / handoffs / decisions / locks / artifacts / mattermost_inbox / mattermost_outbox + indexes. |
| `gateway` | `8787` | HTTP API for peer agents — event-emit, session-lifecycle, resume-context, task-claim, handoff-create, decision-propose. Token-auth + `§2.1` sanitiser at write. |
| `outbox-worker` | (no port) | Polls `mattermost_outbox`, posts to Mattermost via service-account PAT. At-least-once + idempotency keys. |
| `inbox-webhook` | `8788` | Receives Mattermost outgoing webhooks → writes to `mattermost_inbox`. Peer agents poll on `SessionStart` + `Stop` hooks. |

**Designed for** the agent-events layer of the [concept-workflow plugin](https://github.com/Eladrofel/concept-workflow) — ROADMAP `#10`. The plugin's lifecycle hooks shell out to `agentctl` (a Go binary distributed from this repo) which hits the gateway's HTTP API.

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
├── db/
│   └── migrations/001_init.sql ← 11-table schema
├── gateway/
│   ├── Dockerfile
│   ├── go.mod
│   ├── cmd/agent-hub/main.go   ← server binary (serve / outbox-worker / inbox-webhook subcommands)
│   ├── cmd/agentctl/main.go    ← CLI binary, distributed to agent VMs + Mac
│   ├── internal/               ← package skeletons (auth / events / sessions / outbox / inbox / sanitiser / store)
│   └── sanitiser-patterns.txt  ← §2.1 leak patterns applied at write time
├── cli/install.sh              ← SSH-installable on agent VMs (cp binary + chmod)
├── scripts/                    ← helpers
└── research/decisions/         ← ADR-001..ADR-004 (this project's architecture decisions)
```

## VM specs (defaults; override via `terraform.tfvars`)

| Resource | Value |
|---|---|
| Hostname | `agent-hub` |
| Static IP | `10.0.5.50/16` (next free slot above the `agent-1..4` fleet at `10.0.5.40..43`) |
| Static MAC | `BC:24:11:10:05:50` (DHCP-reserved separately) |
| DNS name | `agent-hub.litts.link` (resolved on the apollo network) |
| vCPU / RAM | 2 / 4 GB |
| Disk | 40 GB |
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

- **Project version:** lives in `gateway/go.mod` + `CHANGELOG.md`. Currently `0.1.0-dev`.
- **Plugin compatibility:** `concept-workflow` plugin `v0.2.8+` (ROADMAP `#10` Component A) is the first plugin release that consumes this project.
- **Commit identity:** this project lives outside `~/projects/secureup/` so it uses the default `~/.gitconfig` identity (typically `Eladrofel`). This is the same identity that publishes the `concept-workflow` plugin to the GitHub marketplace.

## Related

- **`concept-workflow` plugin** — consumer of agent-hub; lives at `~/projects/dev-environment/Claude/plugins/concept-workflow/`. ROADMAP `#10` is the canonical entry for the cross-project effort.
- **`terraform-mattermost`** — sibling Terraform project for the chat layer. Mattermost is reused by agent-hub's outbox-worker + inbox-webhook (no separate Mattermost provisioning here).
- **`terraform-apollo`** — owns the shared Ubuntu cloud image referenced from `main.tf`. Apply that first if it's not already present on the Proxmox node.

## Status

`v0.1.0-dev` — scaffold. Schema + docker-compose + Terraform skeleton in place; gateway endpoints + agentctl subcommands are stubbed (return "not implemented"). Implementation flesh-out tracked in the `concept-workflow` plugin's ROADMAP `#10` Component A.
