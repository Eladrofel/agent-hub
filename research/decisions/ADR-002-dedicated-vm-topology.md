# ADR-002: Dedicated VM topology (vs operator-Mac localhost / per-workspace docker stack)

**Status:** Decided
**Date:** 2026-05-16
**Decision owner:** Dale Littleford

## Decision (one sentence)

agent-hub runs on a **dedicated VM** in the agent-VM subnet (default IP `10.0.5.50`), provisioned via this Terraform module. The hostname/IP is configurable per consumer; relocation is supported via the plugin's `/move-agent-hub` skill.

## Context

Three topologies were considered:

1. **Operator's Mac, localhost Postgres** — simplest setup. Single backup surface. But VMs need network reachability when the operator is away; ties availability to a single laptop.
2. **Dedicated VM in 10.0.5.x subnet** — always-on, agent VMs reach via LAN, matches the existing `infra/agent-vm-tooling` fleet pattern. One more VM to maintain.
3. **Per-consuming-workspace docker-compose stack** — self-contained per workspace. Higher cost for multi-workspace operators (multiple postgres instances + tokens to manage).

## Decision

**Option 2.** Dedicated VM on the 10.0.5.x subnet provisioned by this Terraform module. Sized small (2 vCPU / 4 GB RAM / 40 GB disk — Postgres + 3 small Go binaries fit comfortably).

## Why

- Agent VMs already live on 10.0.5.40-43; the hub at 10.0.5.50 is the natural neighbour. Same subnet → low-latency event emission.
- Always-on independence from the operator's Mac. The operator can put their Mac to sleep and overnight workflows on agent VMs still emit events.
- Matches the existing `terraform-mattermost` pattern (operator-tier infra, separate VM, Terraform-managed).
- Single backup surface for the canonical runtime data — one VM's `pg_dump` per night, not five.

## Configurability + relocation

`agent-events.gateway-url` in `<consuming-workspace>/.claude/concept-workflow.local.md` captures the URL. The plugin's `/move-agent-hub --new-url <url>` rotates tokens + updates per-VM env across all peers; `--migrate-data` chains a `pg_dump → restore` operator-block.

## Alternatives' merits acknowledged

- **Option 1** would be right if the workspace had only a single user with one workstation. Doesn't scale to "agent VMs run overnight while the operator's Mac is closed."
- **Option 3** would be right if multiple isolated consumer organisations needed separate Postgres state. Today the operator-cohort is one person; one shared agent-hub instance is enough.

## Triggers for revisit

- Multi-tenant scenario (multiple isolated organisations of users) → revisit Option 3 or add schema-per-tenant in Option 2.
- Hub VM becomes a meaningful bottleneck for a busy fleet (>10 VM agents emitting >50k events/day) → consider read replicas or hub-per-cluster.
