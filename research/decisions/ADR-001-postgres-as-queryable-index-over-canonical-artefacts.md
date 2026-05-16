# ADR-001: Postgres as queryable index over canonical Forgejo artefacts

**Status:** Decided
**Date:** 2026-05-16
**Decision owner:** Dale Littleford
**Affects:** All event-emitting paths in this project; pairs with `concept-workflow` plugin's ADR-009.

## Decision (one sentence)

Postgres is the canonical store for **runtime state that has no git equivalent** (session lifecycle, task claims, decisions, operator inbox messages, agent-to-agent coordination). For state that DOES have a canonical Forgejo artefact (briefs, specs, handoff documents, audit JSONLs), Postgres stores **pointers** (commit SHA + repo + path) — not duplicates. The Forgejo artefact remains canonical.

## Context

ADR-008 (in the `concept-workflow` plugin) locked: "artefacts in Forgejo are canonical; chat is ephemeral signalling." A naïve agent-events design that promoted Postgres to "source of truth for all agent runtime state" would flip that invariant silently — handoffs already exist as markdown in Forgejo; checkpoints can live in audit JSONLs.

The Ultraplan critique of the original v1 plan (`breezy-seeking-marble-ultraplan.md`, concern #1) surfaced this as the most material design risk. The chosen framing aligns the new layer with ADR-008 rather than contradicting it.

## Decision

**Two-tier model:**

1. **Postgres-canonical** (data that wouldn't make sense in git):
   - `events` — append-only lifecycle ledger
   - `agent_sessions` — Claude Code session state
   - `session_checkpoints` — durable resume state across `/clear`
   - `mattermost_inbox` / `mattermost_outbox` — message queues
   - `agent_locks` — advisory cross-agent locks
   - `decisions` — decision-proposed state until resolved
   - `tasks` — task-claim/complete state (the task's `forge_brief_path` points at the canonical brief)

2. **Forgejo-canonical, Postgres-pointer** (data with git equivalents):
   - `tasks.forge_brief_path` — pointer to canonical brief
   - `events.artefact_pointer` — `{repo, commit_sha, path}` for the canonical artefact this event references
   - `handoffs.artefact_pointer` — pointer to canonical handoff document (if one exists in Forgejo)

## Why this matters

- **Cold-restart audit:** A reviewer reading ADR-008 and the new ADR-009 (plugin-side) + this ADR-001 sees a coherent story — Forgejo remains canonical where it always was; Postgres adds a NEW source of truth for state that was previously transient.
- **Resume flows:** `agentctl resume-context` composes pointer-dereferenced Forgejo content + Postgres event tail into a single brief. The reader doesn't have to know which sub-state lives where.
- **Loss-safety:** If Postgres dies, no canonical Forgejo state is lost. Only runtime coordination state (which by definition is recoverable by re-running the lifecycle from current Forgejo HEAD).

## Alternatives considered

- **Postgres-as-source-of-truth (the rejected original framing):** Would have flipped ADR-008's invariant. Higher loss-blast-radius — a Postgres outage would block all runtime coordination.
- **No Postgres; richer JSONL audit files:** Considered. Doesn't solve cross-VM querying or the operator-inbox flow. Lower complexity but doesn't address the problem.
- **Postgres + write-through to Forgejo:** Considered. Adds a coupled-write failure mode without obvious benefit (Forgejo isn't designed as a high-frequency event sink).

## Triggers for revisit

- If Postgres becomes a load-bearing canonical store for content that grows to rival the Forgejo artefact tier in volume, revisit whether the model still cleanly separates "runtime" from "canonical."
- If a future ROADMAP item proposes querying Postgres without dereferencing the pointer into Forgejo (e.g., for performance reasons), that's a tier-violation worth a fresh ADR.

## Migration path

None — this is a green-field ADR for a new project. The plugin-side complement is ADR-009.
