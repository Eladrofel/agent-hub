# ADR-003: Per-host token + dual-layer §2.1 sanitiser

**Status:** Decided
**Date:** 2026-05-16
**Decision owner:** Dale Littleford
**Affects:** All authenticated paths; pairs with `concept-workflow` plugin ADR-008 sub-decisions (chat-layer security posture).

## Decision (one sentence)

Each peer agent gets **one token** (per-host, scoped to one project), stored in `systemd-creds` on Linux VMs and a `chmod 600` file on macOS. §2.1 leak-pattern sanitisation runs at **two layers** (agent-side hook before `agentctl` invocation + gateway-side at write), both fail-closed.

## Context

ADR-008's chat-layer security posture is the highest bar in this codebase: per-agent ed25519 signatures, dispatcher-only-token, dual-layer fail-closed sanitiser. The Ultraplan critique (concern #2) initially read the v1 plan as relaxing this on two axes (per-VM token instead of per-agent; single-layer sanitiser at gateway only).

Closer inspection of the actual topology resolves this: each host runs exactly one peer agent (agent-N user on claude-N host; agent-operator-mac on the Mac; operator never co-located with VMs). "Per-host" and "per-agent" are the same set, with one identity per host.

The dual-layer sanitiser concern was real and is fixed here.

## Decision

### Token model

- One token per peer agent (1 token per host).
- Scoped to one `agents.project_id`; the gateway rejects writes whose `payload` references a different project than the token's scope.
- Operator-agent token additionally has `permissions.cross_project_read = true` for `/agent-events-health` + operator-side resume-context queries (write scope stays project-local).
- Storage:
  - Linux VMs: `systemd-creds`-encrypted file (TPM2-bound where available), path via `AGENT_HUB_TOKEN_FILE` env.
  - macOS (operator): `chmod 600` file, path via same env var. macOS Keychain is a future option.
- Token rotation: `/move-agent-hub --rotate-tokens` (or `/setup-agent-events --rotate-tokens`) per the plugin.

### Sanitiser layers

Both layers read the same pattern file (`gateway/sanitiser-patterns.txt` on gateway side; the plugin's `references/sanitiser-patterns.example.txt` on agent side; the two are kept in sync via the plugin's `setup-agent-events` skill which writes the agent-side file).

1. **Agent-side (hook layer):** Each plugin hook (`session-start.sh`, `post-tool-use.sh`, etc.) runs the pattern set on the prospective event's `summary` + `payload` BEFORE invoking `agentctl`. On match: refuse to invoke; write an audit row to `$CONCEPT_CHAT_AUDIT_LOG`; halt the hook with non-zero exit code.
2. **Gateway-side (write boundary):** `POST /v1/events` runs the pattern set server-side. On match: HTTP 422 with a payload identifying the matched pattern (operator-readable, not the matched content). Writes a `sanitiser.blocked` metadata-only audit event (payload-free so it's safe to persist).

Either layer alone is incomplete:
- Agent-side alone: a compromised gateway could accept unsanitised content.
- Gateway-side alone: a hook-script bypass (or a malicious agentctl caller) could submit unsanitised content.

Together: belt-and-braces.

## Fail-closed posture

If the pattern file is missing or unreadable on either side, the consuming code refuses to operate (gateway returns HTTP 503 on writes; hook scripts exit non-zero). `/agent-events-health` surfaces this state.

## Why per-host (not per-agent) tokens is fine

Each host hosts exactly one agent identity. The "multiple identities sharing a token on one host" attack vector doesn't apply. If a future host runs multiple agent identities (currently no such case), revisit.

## Triggers for revisit

- If a host ever runs multiple agent identities (e.g., a single VM hosts both `agent-frontend` and `agent-backend` for resource-sharing reasons), revisit to use per-agent tokens.
- If the §2.1 pattern table grows to a size where one of the two layers becomes a meaningful performance cost (>5% of hook/request time), revisit the dual-layer choice (probably still right; but worth benchmarking).
