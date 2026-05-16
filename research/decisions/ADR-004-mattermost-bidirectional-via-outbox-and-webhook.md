# ADR-004: Mattermost bidirectional integration via outbox + outgoing webhook

**Status:** Decided
**Date:** 2026-05-16
**Decision owner:** Dale Littleford
**Affects:** `outbox-worker`, `inbox-webhook` subcommands; pairs with `concept-workflow` plugin's `/agent-inbox` slash command.

## Decision (one sentence)

Outbound (peer agent → Mattermost) flows through a Postgres outbox table polled by a worker process (at-least-once + idempotency keys); inbound (Mattermost → peer agent) flows through a Mattermost outgoing webhook that calls a gateway endpoint writing to a Postgres inbox table polled by the target peer's `SessionStart` / `Stop` hooks.

## Context

ADR-008 (plugin) covers the chat layer: peer agents post via direct MCP (Mattermost MCP Server's `create_post`). That's ephemeral signalling — fits its purpose.

agent-events adds **durable bidirectional** flow:
- **Out:** curated event subset (task.claimed / task.blocked / decision.proposed / handoff.created / task.completed / session.ended-with-unfinished-task) → Mattermost channel for operator visibility. Not every event (which would be noise).
- **In:** operator can ping any peer (including the operator-agent on the Mac) by `@-mention` in the Mattermost channel. Peer reads on next SessionStart hook.

Three sub-decisions:

### Sub-decision A: Outbox table + worker (not direct MCP from gateway)

Reasons:
- Outbox decouples gateway request-path from Mattermost API availability. A Mattermost outage queues posts; doesn't fail event-emission.
- Easy retry / dead-letter semantics.
- Single Mattermost auth point (the worker), not every gateway-emit path.

### Sub-decision B: Mattermost outgoing webhook (not polling Mattermost from the gateway)

Reasons:
- No polling load on Mattermost.
- Inbound latency: webhook fires within seconds of operator post; vs polling-on-a-cadence latency.
- Reuses Mattermost's first-class outgoing-webhook feature; well-documented + battle-tested.

### Sub-decision C: At-least-once delivery + idempotency keys

The outbox-worker may retry on transient failure. To prevent duplicate Mattermost posts, every post carries `props.idempotency_key = sha256(event_id + attempt_number)`. Mattermost dedupes server-side on this key.

The inbox direction has natural idempotency: rows have unique IDs; the target peer's poll uses `WHERE delivered_at IS NULL` and updates `delivered_at` after surfacing. A retry of a webhook delivery from Mattermost creates a duplicate row that the peer would surface twice — accepted edge case; operators can dedupe by content if it matters in practice.

## Curated event list (outbound)

```text
task.created
task.claimed
task.blocked
task.unblocked
task.completed
decision.proposed
decision.accepted
decision.rejected
handoff.created
session.ended         (only when final_status != task_completed)
sanitiser.blocked     (operator-visible only; payload-free)
```

NOT posted: `progress.updated`, `session.started`, `session.checkpointed`, `tool.used`, `inbox.message_received`.

## Channel split (mirrors plan §"Mattermost channel strategy")

- `agent-chat` — chat-emit (Mode-3, direct MCP) posts here; ephemeral. Unchanged from ADR-008.
- `agent-events` — outbox-worker posts here; durable, structured. NEW.
- Outgoing webhook configured on `agent-events`. `@agent-N` mentions on this channel route to `mattermost_inbox`.

## Why a separate channel (not consolidate into one)

The two layers serve different audiences:
- `agent-chat` is informal, high-volume, "agent says hi at WI start" signalling.
- `agent-events` is structured, low-volume, "things the operator might need to act on" event stream.

Mixing them dilutes the latter. The Ultraplan critique (concern #8 + #13) flagged the duplication risk if both posted to the same channel; the split resolves it.

## Triggers for revisit

- If Mattermost-MCP gets a webhook receiver feature (eliminating the need for our own inbox-webhook endpoint), revisit Sub-decision B.
- If at-least-once semantics produce visible duplicate posts that hurt operator trust (idempotency-key dedup turns out to be insufficient in practice), revisit Sub-decision C (move to exactly-once via Mattermost server-side dedup hook, if such a thing exists).
