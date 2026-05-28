# Changelog — agent-hub

All notable changes to this project are documented here.

## [0.1.18] — 2026-05-28

Peer-targeted message primitive. Closes the chat-emit ↔ agent-events two-surface gap empirically observed 2026-05-28: Mikey sent suggestions for Donnie via `/chat-emit`, which produced an MM post that didn't lead with `@`, so the MM outgoing-webhook never fired, so Donnie's `mattermost_inbox` never got the row, so Donnie's `/agent-inbox` poll returned empty even though the message existed in MM. The operator had to play man-in-the-middle: tell Donnie "check chat" before Donnie noticed. Pairs with **concept-workflow plugin v0.5.8** which adds the `/message-peer` skill + chat-emit guidance update.

### Added — `agent.peer-message` curated event type

- New entry in `CuratedEventTypes` (`internal/events/events.go`). Payload shape: `{target_agent: "<peer-alias>", intent: "info|question|blocker|status|directive", summary: "...", details: "..."}`. directive intent stays operator-gated by the existing v0.1.10 role check.
- New `formatPeerMessage` in `internal/server/handlers_events_format.go`. Renders the chat-side line as `@<target> <intent-icon> <sender>: <summary>`. The leading `@<target>` is load-bearing: it's what triggers the MM outgoing-webhook (`trigger_when=1`, `trigger_word=@`) that writes the post into the recipient's `mattermost_inbox`. Without it the message would only live in MM, not in the agent-events bus.
- Icon precedence: intent-based (info=💬, question=❓, blocker=🚫, status=📊, directive=⚡); default 💬 for unset intent.
- Edge case: `target_agent` missing → no `@` prefix (caller can still see in MM; just doesn't trigger inbox routing). Verified by test.

### Added — `agentctl message <peer-alias>` subcommand

- New `internal/agentctl/commands/message.go`. Positional first arg is the recipient's MM alias (case-insensitive match per v0.1.8 #45). Required flag `--summary` (≤280 chars, matching improvement emit cap). Optional flags `--intent` (default info), `--details` (uncapped longer body; `@file` syntax supported same as improvement emit).
- Wraps POST `/v1/events` with `event_type=agent.peer-message` and the validated payload. Sanitiser-blocked responses surface matched-pattern detail for operator triage (same pattern as `improvement emit`).
- Registered in `cmd/agentctl/main.go` alongside `NewWorkItemCmd()`.
- Reuses `resolveClaudeSessionID` (the v0.1.17 file fallback path), `runCall`/`callOpts`, audit log path — no new infrastructure.

### Why

Two surfaces for peer comms (MM and agent-events) with no bridge for the "I want to message a specific peer" case. The auto-relay path (v0.1.15) only bridges work-item events; the chat-emit path only writes to MM. `/message-peer` is the missing primitive — one round-trip writes durable Postgres + MM-with-@-mention; v0.5.6's mid-session inbox poll then surfaces the message to the recipient agent within 5 min without operator intervention.

### Tests

- 5 new unit tests in `handlers_events_format_test.go`: speech-balloon default for info intent, question/blocker intent icons, no-target-omits-@-prefix edge case, alias-falls-back-to-name when MM username unset.
- `agentctl message` exercised by the live smoke at deploy time (operator-Mac sends → agent VM polls → message surfaces in inbox + MM channel).

`go build ./...` clean. `go vet ./...` clean. All tests pass.

## [0.1.17] — 2026-05-26

`resolveClaudeSessionID` file fallback. Closes a recurring footgun: Claude Code's Bash tool spawns subshells that don't reliably inherit `$CLAUDE_SESSION_ID`, so the v0.1.11 env fallback misses every bash-invocation of agentctl in practice. Empirical case 2026-05-26: `/checkpoint` skill consistently sees `$CLAUDE_SESSION_ID` empty in its bash subshell and has to fish the id out of the SessionStart payload by hand. Pairs with **concept-workflow plugin v0.5.7** (SessionStart hook writes the id to a cache file; SessionEnd hook removes it).

### Added — file fallback in `resolveClaudeSessionID`

Precedence chain becomes:

1. Explicit `--claude-session-id` flag (unchanged; still wins).
2. `$CLAUDE_SESSION_ID` env var (v0.1.11; unchanged; still wins over file).
3. **NEW** — contents of the cache file at `$CLAUDE_SESSION_ID_FILE` (override) OR `~/.cache/concept-workflow/claude-session-id` (default). Plugin v0.5.7's SessionStart hook writes this file with the session id extracted from the hook's stdin JSON (which Claude Code DOES populate reliably).
4. Empty string (unchanged; caller-decides whether to warn).

Reads via `os.ReadFile` + `strings.TrimSpace`; soft-fails to empty on any I/O error. No new dependencies.

### Updated — flag help text on every emit-style subcommand

`agentctl checkpoint`, `event emit`, `improvement emit`, `work-item claim|finish`, `resume-context` — all `--claude-session-id` help strings now name the full precedence chain (`$CLAUDE_SESSION_ID` → `$CLAUDE_SESSION_ID_FILE` → default path) so operators can see at `--help` time why a flag-less invocation still works.

### Added — test isolation (resolveClaudeSessionID file fallback)

`newFixture` now sets `$CLAUDE_SESSION_ID_FILE` to a non-existent path in the test's temp dir by default, so cached session-id files in a developer's `~/.cache/concept-workflow/` don't leak into tests that expect empty-session-id semantics. Tests that specifically exercise the file fallback override `$CLAUDE_SESSION_ID_FILE` themselves.

Four new unit tests cover the precedence: flag-wins-over-everything, env-wins-over-file, falls-back-to-file (incl. trailing-newline tolerance), empty-when-all-unset.

### Net result

Every agentctl emit-style subcommand now resolves the session id transparently from a bash subshell, no matter how the subshell was spawned. The v0.1.11 helper's original promise — "callers in Claude tool contexts shouldn't need to plumb the flag" — finally holds in the empirically common case.

`go build ./...` clean. `go vet ./...` clean. All tests pass.

## [0.1.16] — 2026-05-25

Smart 422 when `--task-key` is given a concept-workflow work-item key, plus help-text correction on `agentctl event-emit` + `checkpoint`. Closes a real-user-test confusion (2026-05-24): an agent ran agentctl with `--task-key feat-XX-...` and hit bare `HTTP 422 unknown_reference`, then spent cycles hypothesising the work-item.claimed event was session-orphaned and the WI row hadn't materialised. Wrong causal chain — the actual root cause: `--task-key` looks up the legacy `tasks` table, which is a different namespace than concept-workflow work-item keys. Work-items live in event payloads (`payload.wi_key`); the agentctl help text was misleadingly saying *"e.g. feat-01-landing-page"*.

### Fixed — gateway `writeResolveError` namespace-mismatch detection

- New `workItemKeyPattern` regex (`^(feat|bugfix|improvement|hotfix|task)-\d+-`) in `internal/server/handlers_events.go`. When a `task_key` lookup returns `pgx.ErrNoRows` AND the value matches the pattern, the handler returns HTTP 422 `task_key_looks_like_work_item` with a tailored message: *"'<value>' looks like a concept-workflow work-item key; the --task-key flag looks up the tasks table, which does NOT hold work-item keys. Either: omit --task-key (work-item keys live in event payloads), or use agentctl work-item claim/finish/active for work-item lifecycle."* Response details include `correct_surface: "agentctl work-item {claim,finish,active}"` for programmatic introspection.
- Non-matching `task_key` values keep the original `unknown_reference` shape — only the agent-friendly pattern triggers the tailored message.

### Fixed — agentctl `--task-key` help text on event-emit + checkpoint

- Before: `"task key (e.g. feat-01-landing-page)"` (misleading — feat-01-landing-page is a wi-key, not a task_key).
- After: `"legacy `tasks` table key (NOT a concept-workflow work-item key; for those use `agentctl work-item …`)"`.

### Why

The v0.5.4/v0.5.5 work-item lifecycle (claim, finish, active) deliberately does NOT entangle with the legacy `tasks` table — the rationale was "payload-JSON is sufficient and indexable" and a tasks-table materialisation would expand scope. That's still the right call, but it leaves the `--task-key` flag as a footgun: a flag named for work-item-ish concepts that doesn't accept work-item keys. v0.1.16 closes the gap on the discovery side (smart error + better help) rather than on the data side (no schema change).

`go build ./...` clean. `go vet ./...` clean. All tests pass.

## [0.1.15] — 2026-05-23

Proactive peer @-mentions on `agent.work-item.{claimed,finished}` events. Pairs with **plugin v0.5.5** (peer-coordination policy update banning operator-courier laundering). Closes the v0.5.4 real-work-test finding: Mikey claimed task-02-onboarding-stepper-scaffold correctly, but Donnie was concurrently planning a duplicate dispatch because the channel-only claim event didn't reach Donnie's inbox proactively.

### Added — `events.PeerMentionsForProject` helper

- New function in `internal/events/events.go` returning a space-separated `@<alias1> @<alias2>` string of other agents who (a) have had a session in the given project, (b) have a `mattermost_username` set, (c) have `last_seen_at` within the last 24 hours, (d) are not the claiming agent. Returns `""` for single-agent projects or fully stale-peer projects.
- Lead `@` is load-bearing for the existing outgoing-webhook → inbox-webhook routing (`trigger_when=1` requires first-word-@ per the v0.5.0 e2e finding). Callers MUST keep the prefix leading the entire post.

### Updated — `handleEventEmit` prepends mentions for work-item events

- After `formatCuratedMessage` composes the chat line, the handler now calls `PeerMentionsForProject` when `event_type ∈ {agent.work-item.claimed, agent.work-item.finished}` and prepends the result. Resulting outbox lines:
  ```
  @Donnie @Splinter 🔵 Mikey: claimed feat-04-bulk-import (customer-web)
  @Donnie @Splinter ✅ Mikey: finished feat-04-bulk-import (customer-web) — <pr-url>
  ```
- Soft-fail: if the peer-mentions query errors, the handler logs a warning and proceeds without mentions (the durable event still writes; only the proactive notification is lost). Same posture as other non-blocking enrichment steps.
- 24-hour activity window deliberately filters retired/stale VMs out of the notification fanout. Stale-VM safety on the OTHER side (a VM that wakes up and tries to claim already-claimed work) is still handled by the `/start-work-item` Pre-flight 4 active-claims check from v0.5.4.

### Smoke (operator + claude-1 + claude-2, 2026-05-23)

- claude-1/Mikey claimed `feat-test-96-mentions` → outbox row text was `@Donnie @Splinter 🔵 Mikey: claimed feat-test-96-mentions (customer-web)`. ✓
- Outbox worker posted; MM outgoing-webhook fired (because of leading @); inbox-webhook routed to both Donnie's and Splinter's inboxes. ✓
- `agentctl inbox poll` on agent-2 (Donnie) AND agent-operator-mac (Splinter) both returned the message, each with their own `target_agent_id`. ✓
- Symmetric `agent.work-item.finished` event also @-mentions the same peer set. ✓

### Not addressed (deferred)

- TTL / heartbeat-based "stale peer" handling for the 24h activity window — if pain surfaces (e.g. a long-weekend-quiet VM not getting pinged on Monday claims), revisit.
- Cross-project mention spillover — the query is strictly scoped to `s.project_id = $1`, so a peer that has only ever sessioned in project-A would not be pinged for project-B claims even if it currently has activity in project-B. This is the right default but worth flagging as a known shape.

### Tests

- All existing gateway tests still pass. New helper exercised via the live smoke above; a DB-fixture integration test for `PeerMentionsForProject` is a v0.1.16 follow-up (same posture as other peer-introspection helpers in this package).

`go build ./...` clean. `go vet ./...` clean.

## [0.1.14] — 2026-05-23

Work-item peer-coordination event types + read endpoint. Pairs with **plugin v0.5.4** which adds the `/start-work-item` active-claim pre-flight, the `--force-claim` override, and the symmetric `/finish-work-item` finish-emit. Closes Dale's 2026-05-23 peer-awareness gap (two agents on different VMs could silently race the same `<work-item-key>`; only signal was a Mode-3-only Mattermost heads-up gated on `CONCEPT_CHAT_MM_URL`).

### Added — `agent.work-item.{claimed,finished}` curated event types

- **Two new entries in `CuratedEventTypes`** (`internal/events/events.go`). Same auto-MM-relay mechanism as `agent.improvement-note` — the outbox-worker forwards to the project's `mattermost_outbox_channel` (or the operator's default channel). Mode-1 deployments still get peer-visibility on the DB side; Mode-3 deployments get the chat heads-up for free without per-skill `chat-emit` calls.
- **No DB migration.** `events.event_type` is `text NOT NULL` — no whitelist constraint to update. The existing GIN index on `events.payload` covers the `payload->>'wi_key' = $1` filter the new read endpoint uses.
- **Payload shape:** `{wi_key, repo, branch?, force?}` for `claimed`; `{wi_key, repo, pr_url?}` for `finished`. `task_key`/`task_id` linkage deliberately not used — work-items aren't tracked in the `tasks` table today; payload-JSON is sufficient and indexable.

### Added — `GET /v1/work-items/{wi_key}/active-claims` agent-readable endpoint

- New handler `handleWorkItemActiveClaims` in `internal/server/handlers_workitem.go`. Registered in the non-admin `RequireAgent` route group (sibling to `/v1/me/latest-session`) so any authenticated agent can pre-flight before claiming — operator-only gating would have forced the agent VMs to ask the operator to run the check for them, defeating the peer-coordination purpose.
- **Project scope via `?project_slug=<slug>` query param.** Same resolution path as POST /v1/events; if missing or unresolvable, returns an empty list with HTTP 200 (best-effort posture — the pre-flight skill should treat empty as "no conflicts" without ceremony).
- **Algorithm:** `WITH latest AS (SELECT DISTINCT ON (agent_id) … ORDER BY agent_id, created_at DESC)` returns the most-recent event per agent for the wi-key; the outer filter keeps only those whose latest is `claimed`. Handles `--force-claim` (re-claim after another agent already claimed → both surface as active) and re-claim-after-finish correctly without app-side bookkeeping.
- **Response:** `{wi_key, project_slug, active_claims: [{event_id, agent_id, agent_name, alias?, claimed_at, claude_session_id?, repo?, branch?, force?}], total}`.

### Added — chat-side icon + color polish for work-item events

- **`agent.work-item.claimed` → 🔵 + blue (#0d6efd)**; **`agent.work-item.finished` → ✅ + green (#198754)**. Plain-line fallback renders `🔵 <alias>: claimed <wi-key> (<repo>) [forced]` / `✅ <alias>: finished <wi-key> (<repo>) — <pr-url>` — same icon+alias treatment improvement-notes get. Visual scan-ability matches the existing 💡 (note) / 🟢 (session-start) / 🔴 (session-end) / 📍 (checkpoint) vocabulary.
- Implementation: extended `eventTypeIcon` + `eventTypeColor` in `internal/outbox/adapters.go` and `formatCuratedMessage` in `internal/server/handlers_events_format.go`, plus 4 new tests in `handlers_events_format_test.go` covering icon presence, alias-fallback, and `[forced]` suffix pass-through.

### Added — `agentctl work-item {claim,finish,active}` subcommand group

- New `gateway/internal/agentctl/commands/work_item.go` mirroring the multi-verb pattern in `improvement.go`. Three verbs:
  - `agentctl work-item claim --wi-key X --repo Y [--branch Z] [--force]` → POST `/v1/events` (event_type=`agent.work-item.claimed`).
  - `agentctl work-item finish --wi-key X --repo Y [--pr-url U]` → POST `/v1/events` (event_type=`agent.work-item.finished`).
  - `agentctl work-item active --wi-key X [--pretty]` → GET `/v1/work-items/{X}/active-claims?project_slug=<cfg.ProjectSlug>`.
- Reuses `resolveClaudeSessionID` (v0.1.11 helper) for env-parity with `improvement emit`, `event emit`, `checkpoint` (v0.1.12+), and now `work-item claim/finish`.
- Reuses `runCall` + `callOpts` POST pattern (per `checkpoint.go`) and GET pattern (per `resume_context.go`). Audit log entries written via existing `audit.Append()`.
- Registered in `cmd/agentctl/main.go` alongside `NewImprovementCmd()`.

### Why (Dale's 2026-05-23 peer-awareness audit)

The question was "when agents pick up work, especially concept work, will they advise the peers so that they know not to pick the same work?" Investigation found: (1) `/start-work-item` Phase 1.2 emitted only a `chat-emit` heads-up — Mode-3-only, not durable, not queryable. (2) No `agent.work-item.*` event type existed in the store. (3) No skill queried the event store before claiming. (4) `references/peer-coordination-policy.md` explicitly named "auto-claiming a work item another peer was assigned" as a forbidden anti-pattern, but the system relied on operator-side out-of-band coordination to prevent it. v0.1.14 + v0.5.4 closes the gap with Postgres-authoritative claim/finish events and a cheap agent-readable read endpoint backing the pre-flight check. Mattermost remains the human-visibility relay (now driven by the curated event-type outbox, not by a separate `chat-emit` call).

### Smoke results (operator + claude-1 + claude-2, 2026-05-23)

- **Happy path:** operator claim feat-test-99-dummy → `active`=1 → finish → `active`=0. ✓
- **Cross-host visibility:** claude-1/Mikey claims feat-test-98-race → operator Mac sees the claim; claude-2/Donnie sees the claim via `agentctl work-item active`. ✓
- **Force-claim race:** claude-2/Donnie passes `--force` → second claim row written → operator sees both (newer Donnie + older Mikey) with `force: true` on Donnie's row. ✓
- **MM auto-relay:** all 6 outbox rows for the smoke events `status=sent, attempts=0`. ✓
- **Mode-1 (no chat config)** and **cross-project isolation** scenarios architecturally guaranteed by construction (`CONCEPT_CHAT_MM_URL` gates only `chat-emit`, not the durable event path; project_id filter is mandatory on the read query); live smoke deferred to v0.1.15 if pain surfaces.

### Tests

- All existing gateway test packages continue to pass (`go test ./...` clean).
- New handler is exercised by the smoke run above; an integration test fixture for `handleWorkItemActiveClaims` is a v0.1.15 follow-up (same posture as `handleMeLatestSession` shipped in v0.1.13).

`go build ./...` clean. `go vet ./...` clean.

## [0.1.13] — 2026-05-23

Hotfix: v0.1.12's `resume-context` no-flag fallback called `/v1/agents/{name}/latest-session` which is admin-token-protected → non-operator peers (and operator's per-host bearer) hit HTTP 401 `invalid_admin_token`. Discovered empirically during the v0.5.3 `/resume-context` skill smoke test 2026-05-23.

### Added

- **`GET /v1/me/latest-session`** — self-scoped latest-session lookup. Per-host bearer only (no admin required); resolves the agent identity from the bearer via `auth.FromContext`. Optional `?exclude=<claude_session_id>` for the `--prior` post-/clear case. Returns the same payload shape as the admin endpoint. Self-lookup only — no path param means no possibility of reading another agent's sessions.

### Fixed

- **`agentctl resume-context` fallback path** — now calls `/v1/me/latest-session` instead of `/v1/agents/{name}/latest-session`. Works for any per-host bearer including non-operator peers. The admin endpoint stays as the operator's cross-agent lookup tool; the new endpoint is the self-scoped UX the v0.5.3 `/resume-context` skill actually needs.

### Tests

- Updated v0.1.12 commands_test.go cases (no-flag + --prior) to expect `/v1/me/latest-session`.
- Admin endpoint `/v1/agents/{name_or_alias}/latest-session` unchanged + existing tests still pass.
- Manual smoke confirmed: operator-Mac per-host bearer + `agentctl resume-context` (no flag) returns Splinter's most-recent session correctly.

## [0.1.12] — 2026-05-23

Cross-/clear handoff UX fix. v0.1.11 closed the data-plane gap (improvement-notes get tagged with `agent_session_id`, `tool.used` filtered from `recent_events`, `recent_improvements` surfaces cross-cutting learnings) but Dale's same-day empirical test surfaced the next bottleneck: post-/clear, Claude Code mints a brand-new `$CLAUDE_SESSION_ID`, so an in-shell `agentctl resume-context` queries the new (empty) session instead of the prior one. The operator had to manually paste the old id. v0.1.12 removes that friction with a no-flag auto-fallback that asks the gateway "what was this agent's most recent session?" and threads the answer through. Also brings `agentctl checkpoint` into parity with v0.1.11's env-var fallback (the only emit-style subcommand still requiring an explicit flag).

### Added — `GET /v1/agents/{name_or_alias}/latest-session` endpoint

- New admin-token-protected endpoint that returns the most-recent `agent_sessions` row for the named agent. Resolution is case-INSENSITIVE on `agents.name` first, then `mattermost_username` (alias) — same lookup contract as v0.1.8's `inbox.webhook.resolveAgent` #45 fix, so `/v1/agents/Splinter/latest-session`, `/v1/agents/splinter/latest-session`, and `/v1/agents/agent-operator-mac/latest-session` all resolve to the same row.
- Response shape: `{agent_id, agent_name, alias?, latest_session: {claude_session_id, started_at, ended_at, status, start_reason?}}`. 404 with `error="unknown_agent"` when the name/alias doesn't resolve; 404 with `error="no_sessions"` when the agent exists but has zero sessions (or none pass the exclude filter).
- Optional `?exclude=<claude_session_id>` query param — when set, the named session is skipped and the next-most-recent returned. This is the load-bearing query for the post-/clear case: the operator's new shell already knows its own `$CLAUDE_SESSION_ID` and wants the SESSION-BEFORE-THIS-ONE, not its own brand-new (empty) shell.
- Implementation: new `sessions.LatestForAgent(ctx, pool, agentID, excludeClaudeSessionID)` helper in `internal/sessions/sessions.go`, new `handleAgentLatestSession` + shared `resolveAgentByHandle` (case-insensitive name-then-alias resolver) in `internal/server/handlers_agents.go`, single route registration in `router.go` under the existing admin-token middleware group.
- No DB schema change. Reads against the existing `agent_sessions` + `agents` tables.

### Added — `agentctl resume-context` no-flag fallback + `--prior` flag

- **No-flag fallback** — when invoked without `--claude-session-id` AND with `$CLAUDE_SESSION_ID` env unset, `agentctl resume-context` now auto-discovers this agent's most-recent session by calling `GET /v1/agents/{$AGENT_NAME}/latest-session` and threading the discovered id into the existing `/v1/sessions/{id}/resume-context` query. Pre-v0.1.12 the same invocation halted with `"--claude-session-id is required"` even though the gateway had the answer one query away.
- **`--prior` flag** — when set, the fallback path runs EVEN IF `$CLAUDE_SESSION_ID` is set, and passes the current env value through as `?exclude=…`. This is the canonical post-/clear invocation: a freshly /clear'd Claude Code session has a useless new `$CLAUDE_SESSION_ID`, the operator wants the prior one. Picked `--prior` over `--exclude-current-session-id` for terse readability; the flag's help text spells out the semantics.
- Explicit `--claude-session-id flag` still wins outright — bypasses the fallback path entirely, preserves v0.1.11 behavior for callers that already have the id in hand.
- `$AGENT_NAME` is now required for the fallback path (already required by `loadAuthedConfig` for every authed subcommand, but the fallback re-checks + surfaces a user-actionable error if the env is somehow empty at the call site).
- Precedence summary: `--claude-session-id flag` > `$CLAUDE_SESSION_ID env` > **NEW** auto-fallback via `latest-session` endpoint. The `--prior` flag flips the second-and-third rules into "always run the fallback, pass env through as exclude".

### Fixed — `agentctl checkpoint` reads `$CLAUDE_SESSION_ID` env (consistency with v0.1.11)

- `agentctl checkpoint` now uses the shared `resolveClaudeSessionID` helper (v0.1.11) so flag-empty invocations transparently pick up `$CLAUDE_SESSION_ID` from the Claude Code tool context, matching the behavior already shipped on `improvement emit` + `event emit`. Pre-v0.1.12 the checkpoint subcommand was the only emit-style path still requiring an explicit `--claude-session-id` flag — operators had to remember to plumb it through manually inside Claude tool calls or hit `"--claude-session-id is required"`. Help text updated to reflect the env fallback.
- No behavior change for callers that pass the explicit flag — flag still wins over env.

### Why (Dale's 2026-05-23 cross-/clear test)

Dale tested the full cross-/clear handoff path on 2026-05-23 and confirmed the v0.1.11 fixes work end-to-end: a checkpoint emitted in session A is recoverable from session B's resume-context query, with rich structure (latest_checkpoint preserved, recent_events filtered, recent_improvements populated). BUT the operator UX still required pasting the prior session id by hand because Claude Code mints a fresh `$CLAUDE_SESSION_ID` on /clear and the in-shell `agentctl resume-context` invocation has no way to know which prior session to ask about. Dale's exact request: "I need a skill like `/resume-context` that can go pick up the previous session-id" — and the agentctl side has to support no-flag invocation for that. v0.1.12 supplies the agentctl side; the plugin's `/resume-context` skill can now just invoke `agentctl resume-context --prior` and the gateway figures out the rest.

### Tests

- 3 new agentctl tests in `commands_test.go`: `TestResumeContext_NoFlag_FallsBackToLatestSession` (no flag + no env → two-call sequence, first to latest-session, second to resume-context with the discovered id, no `?exclude=`); `TestResumeContext_PriorFlag_PassesExcludeFromEnv` (--prior + env set → first call has `?exclude=<env>`, second call uses the gateway-discovered prior id); `TestResumeContext_ExplicitFlagSkipsFallback` (explicit flag bypasses fallback even with empty env → single call directly to resume-context).
- 2 new agentctl tests for `checkpoint` env-fallback parity: `TestCheckpoint_FallsBackToCLAUDESESSIONIDEnv`, `TestCheckpoint_FlagWinsOverEnv`.
- 6 new integration tests in `handlers_agents_test.go` (DB-gated like the rest of `internal/server`): returns-most-recent ordering, exclude filtering, 404 on unknown agent, 404 on agent-with-zero-sessions, case-insensitive alias resolution + alias field in response, 401 on missing admin auth.
- All existing tests continue to pass without modification — the resume-context flag/env path is unchanged for callers that supplied either; the new endpoint is JSON-additive admin-only.

`go build ./...` clean. `go vet ./...` clean. All packages pass `go test ./...`.

## [0.1.11] — 2026-05-23

Cross-/clear handoff bugfix. End-of-day empirical test by Dale on 2026-05-23 found that `agentctl improvement emit` did NOT tag events with `agent_session_id`, so improvement-notes landed session-orphaned and were invisible to `resume-context` queries. Combined with `recent_events` being dominated by `tool.used` noise (19/20 events), cross-/clear lost all captured learnings — including the load-bearing peer-coordination principle. v0.1.11 fixes the emit-side tagging bug, filters tool.used from the default resume-context tail, and adds a new `recent_improvements` field that surfaces cross-cutting fleet learnings regardless of which session they were emitted from.

### Fixed — `agent_session_id` tagging bug on improvement-note emission

- **`agentctl improvement emit`** now reads `--claude-session-id` (with `$CLAUDE_SESSION_ID` env fallback) and threads it into the POST `/v1/events` body so the gateway's existing `ResolveSessionID` path populates `agent_session_id`. Pre-v0.1.11 the flag didn't exist on this subcommand and there was no env fallback, so every emission landed with `agent_session_id IS NULL`. Empty-resolution is best-effort: stderr warns `"improvement emit: no CLAUDE_SESSION_ID — event will be session-orphaned (cross-/clear handoff will not surface it)"` but the call proceeds — one-off operator scripts can legitimately emit notes without a session context.
- **`agentctl event emit`** gets the same env fallback (the flag itself existed pre-v0.1.11 but only respected when set explicitly, so unflagged emits in tool contexts had the same orphaning bug). Identical warning + best-effort posture.
- **Shared helper** `resolveClaudeSessionID` + `warnMissingSessionID` in `gateway/internal/agentctl/commands/session_id.go` so any future emit-style subcommand (e.g. an `agentctl handoff create` of v0.2.x) gets the same posture for free. The helper centralises the `CLAUDE_SESSION_ID` env-var name to keep spelling consistent with the pre-existing `agentctl resume-context` path.
- No DB schema change. The gateway's per-event-insert already supports `agent_session_id` resolution from `claude_session_id` (added in v0.1.0); the bug was purely client-side under-emission.

### Added — `resume-context` filters `tool.used` + new `recent_improvements`

- **`recent_events` default-excludes `event_type='tool.used'`**. Tool calls can occupy 19/20 of an active session's event tail, drowning out the interesting signal (session.checkpointed, agent.improvement-note, progress.updated). New optional query param **`?include_tool_use=true`** (or `=1`) returns the raw unfiltered stream for debugging / detailed audit.
- **`recent_improvements`** — new top-level field on the resume-context response alongside `session` / `latest_checkpoint` / `recent_events`. Returns the last N `agent.improvement-note` events for **this agent_id** across ALL sessions (NOT just the requested session — improvement-notes are cross-cutting fleet learnings, not session-scoped state). Default N=10, max 50. Tune via **`?improvements_limit=N`**. Per-item shape: `{id, event_id, created_at, summary, category, intent, propagation_hint, context, agent_session_id (nullable)}`. The nullable `agent_session_id` preserves legibility of pre-v0.1.11 orphaned notes — they still surface in resume-context queries even though new emissions will always carry it.
- **Handler + package doc** (`handlers_sessions.go` / `sessions.go`) now spell out the resume-context contract: what's IN the packet (session / latest_checkpoint / recent_events / recent_improvements), what's NOT (no aspirational open handoffs / decisions / pending operator messages / active locks — future work, tracked separately), the default filter behaviour + opt-out params, and a note that this is the source-of-truth for cross-/clear handoff per Dale's 2026-05-23 directive.

### Why

Without these two fixes, the cross-/clear handoff path documented in v0.1.10's resume-context contract was operationally broken: a `/clear`'d agent saw `recent_events` full of `Bash: ls` lines and an empty `recent_improvements` slot, missing every captured learning the fleet had logged in the prior session. The checkpoint round-trip itself was working (verified end-to-end pre-fix); only the events-side path was broken.

### Tests

- 3 new agentctl tests covering `improvement emit` claude-session-id resolution (env, flag-overrides-env, empty-warns-but-proceeds).
- 2 new agentctl tests covering `event emit` env fallback + empty warn.
- 4 new integration tests covering resume-context tool.used filtering (default + opt-in), recent_improvements cross-session query + per-item payload decode, `?improvements_limit` cap, and orphaned-note legibility.
- All existing tests continue to pass without modification — recent_events still surfaces non-tool.used events at the historical default of 20, and the response packet remains JSON-additive (clients that don't read `recent_improvements` are unaffected).

`go build ./...` clean. `go vet ./...` clean. All packages pass `go test ./...`.

## [0.1.10] — 2026-05-23

Visual + programmatic enforcement of the v0.5.0 peer-coordination policy plus rich Mattermost surface for the team-awareness event stream. Pairs with plugin **v0.5.1** (which threads `--intent` through `/note-improvement` + corrects the v0.5.0 §7.11.2 doc).

### Added — `intent` field on events (programmatic enforcement)

- **`--intent <info|directive|question|blocker|status>`** flag on `agentctl event emit` + `agentctl improvement emit`. Parse-time enum validation with clear error listing valid values. Default omitted → server treats as `info`. Stored in event payload as `payload.intent` (no DB schema change — JSONB).
- **Gateway enforcement** on `POST /v1/events`: when `payload.intent == "directive"` AND the caller's `agents.role != "operator"`, reject with `403 Forbidden` body `{"error":"directive_not_authorized","message":"...","docs":"references/peer-coordination-policy.md"}`. Other intents (`info|question|blocker|status`) accept from any role — they're context/severity hints, not authority claims. This is the programmatic backbone of the peer-coord policy: peers collaborate via info, only operators direct.
- 10 new tests across agentctl + gateway: operator directive 201, non-operator directive 403, non-operator info 201, absent intent 201, invalid intent value 400, agentctl-side enum validation cases.

### Added — pluggable message adapter framework (`gateway/internal/outbox/adapters.go`, 327 LOC)

- **`MessageAdapter` interface** with `FormatEvent(FormatterInputs) (map[string]any, error)` + `Backend() string`. Three concrete impls:
  - **`MattermostAdapter`** — produces `props.attachments` matching MM's message-attachment API (colored sidebar + icon-in-title + fallback text + extensible fields).
  - **`SlackAdapter`** — legacy attachments API, near-identical shape.
  - **`DiscordAdapter`** — embeds (color hex→decimal conversion, `description`/`title` field renaming).
- **Color resolution** (precedence): intent → event_type → default gray. Intent map: `info=#0d6efd` blue, `directive=#fd7e14` orange (operator authority), `question=#ffc107` yellow, `blocker=#dc3545` red, `status=#6c757d` gray. Event-type fallback: `session.started=#20c997` teal, `session.ended/checkpointed=#6f42c1` purple, `improvement-note=#6f42c1` purple.
- **Icon resolution** (precedence): `improvement-note→💡` (preserves v0.1.9), else intent icon (`info=ℹ️`, `directive=⚡`, `question=❓`, `blocker=🚫`, `status=📊`), else event-type fallback (`session.started=🟢`, `session.ended=🔴`, `checkpointed=📍`).
- **`AdapterFor(backend)`** selector — defaults to MM. Slack + Discord adapters are inert at runtime until the worker is taught to pick between them (v0.1.11+); their tests assert correct output shape against a fixture event.

### Added — project visibility in MM attachments

- **`events.ResolveProjectSlug`** helper (mirrors existing `ResolveProjectChannel`) — single-row lookup of `projects.slug` by `project_id`. Empty when project_id is nil or row missing.
- **MM attachment title** format: `<icon> <alias> @ <project_slug>` when slug is non-empty; falls back to `<icon> <alias>` when not. Multi-project fleets now see at-a-glance which project each curated event belongs to: e.g., `💡 Splinter @ secureup: Discovered ...` vs the bare `Splinter: Discovered ...` of v0.1.9.
- Alias resolution prefers `agent.alias` (Mikey / Donnie / Splinter); falls back to canonical name.

### Wired through — `POST /v1/events` calls the MM adapter at enqueue time

- `handlers_events.go` resolves project channel + slug, calls `outbox.AdapterFor("mattermost").FormatEvent(...)`, extracts the `attachments` array, passes via the new `events.OutboxConfig.Attachments` field. The outbox-worker forwards `props.attachments` to Mattermost natively — no worker-side changes needed (the v0.1.4 props pass-through path handles it).
- Line-based `formatCuratedMessage` from v0.1.9 still runs — its output becomes the outbox row's `message` text (the fallback for attachment-unaware clients + the sanitiser-blocked placeholder path).
- On adapter `FormatEvent` failure: log a warning, fall back to plain-text-only outbox row (no attachments). Never blocks the event-write.

### Backwards compatibility

- Existing wire formats unchanged for callers that don't set `--intent`.
- Sanitiser still applies to the full attachment JSON; `sanitiser.blocked` placeholder path unchanged.
- v0.1.9 `agent.improvement-note` 💡 icon preserved via the icon-resolution precedence rule.

### Tests

- 209 LOC of intent-enforcement integration tests (`handlers_events_intent_test.go`).
- 187 LOC of agentctl-side intent enum tests (`intent_test.go`).
- Adapter unit tests cover all three backends' output shape against fixture events.

`go build ./...` clean. `go vet ./...` clean. All packages pass `go test ./...`.

## [0.1.9] — 2026-05-23

First-class support for **captured learnings** as fleet data. Dale's
2026-05-23 directive: what an agent figures out, gets surprised by, or thinks
should change is durable + queryable + optionally chat-visible, and
explicitly **outside source control**. v0.1.9 implements the agent-hub side
(new curated event_type + agentctl subcommand + outbox formatter); the
`concept-workflow` plugin ships the matching `/note-improvement` skill in
its own release.

### Gateway — new curated event_type

- **`agent.improvement-note`** added to `events.CuratedEventTypes`. The
  events table is JSONB-payload so no migration is required; the new type
  flows through the existing `InsertWithOutbox` path and lands a
  `mattermost_outbox` row alongside the durable `events` row, same as
  `task.created` / `session.ended` / etc. Payload shape:
  ```json
  {
    "category":         "process",   // architectural|process|tooling|domain|other
    "summary":          "...",       // ≤ 280 chars (CLI-enforced)
    "context":          "feat-04-x", // optional, e.g. work-item key
    "propagation_hint": "mm",        // none|mm|fleet  (fleet treated as mm in v0.1.9)
    "details":          "..."        // optional longer body (no length cap)
  }
  ```
  `propagation_hint=none` still emits a durable event row (every curated type
  always does) but signals the outbox-worker / future filters that the
  operator does NOT want it surfaced in chat. The hint is honoured at the
  filter layer, not the enqueue layer, so the per-event-type message is
  always materialised on the row for queryability.

### Gateway — per-event-type outbox formatter

- New `handlers_events_format.go::formatCuratedMessage` hook on the
  `POST /v1/events` enqueue path. Returns `""` for every event_type EXCEPT
  `agent.improvement-note` — for which it composes
  `💡 <alias>: <summary> _(<context>)_`. The empty-string contract lets
  `events.InsertWithOutbox` keep its default `"[event_type] summary"`
  composition for every other curated type (no regression). Alias falls
  back to the canonical agent name when unset, matching the v0.1.8
  lifecycle-summary rule. Sanitiser runs upstream of the formatter — no
  bypass.

### agentctl — new subcommand

- **`agentctl improvement emit`** wraps `POST /v1/events` with
  `event_type=agent.improvement-note`. Flags lock the cross-track contract
  with the plugin's `/note-improvement` skill:
  ```
  agentctl improvement emit \
    --category <architectural|process|tooling|domain|other> \
    --summary <text>                  # required, ≤ 280 chars
    --context <wi-key|free-text>      # optional
    --propagation <none|mm|fleet>     # default: none
    --details <text-or-@file>         # optional; @file reads from path
    [--strict]
  ```
  Constructs the payload + routes through the existing `runCall` / auditor
  / strict-mode plumbing — no duplicated wire/auth. Stdout success line:
  `improvement-note emitted (category=<c>, propagation=<p>, event_id=<uuid>)`.
  Sanitiser-blocked responses surface `matched_pattern` + `matched_field` +
  `blocked_event_id` the same way `event emit` does.

### Version

- `agent-hub` binary version: `0.1.8` → `0.1.9`.
- `agentctl` binary version: `0.1.6` → `0.1.9` (aligning the CLI with the
  gateway release line; both binaries ship from the same module).

### Tests

- 12 new pure-unit tests on the `agentctl improvement emit` subcommand:
  payload shape, default propagation, category enum (missing + invalid),
  propagation enum, empty summary, oversized summary (best-effort +
  strict), `@file` details (happy + missing path), 5xx best-effort +
  strict, sanitiser-blocked pattern surfacing.
- 8 new pure-unit tests on `formatCuratedMessage`: non-improvement returns
  empty (regression guard for the existing curated types), alias preferred
  over name, alias fallback when unset, context italicised, whitespace-only
  context omitted, details NOT inlined (v0.1.9 contract), nil-agent
  defensive fallback, summary whitespace trimmed.
- 1 new server integration test: improvement-note routes through to the
  outbox with the exact formatted message (`💡 Splinter: ... _(ctx)_`).

`go build ./...`, `go vet ./...`, `go test ./...` all clean.

## [0.1.8] — 2026-05-22

Three targeted bug fixes from the live-fleet smoke run that landed immediately
after v0.1.7 ship. No new endpoints, no schema changes, no `agentctl` changes
— gateway-only and surgical. Total scope ~70 LOC + tests.

### Gateway — bug fixes

- **`print-tokens` MINT_AUTHORITY_TOKEN was binary garbage.** v0.1.7 persisted
  the mint-authority secret as 32 raw random bytes in `kv_store` and printed
  the bytes verbatim, so the operator's terminal showed unprintable chars and
  the value couldn't be copy-pasted into a `--mint-authority-token-file`. The
  HMAC key already used hex; the mint-authority did not. **v0.1.8** moves the
  mint-authority canonical wire/storage format to a 64-char hex string
  (matches `JOIN_CODE_HMAC_KEY`'s encoding). An idempotent migration runs on
  boot: any pre-v0.1.8 `kv_store` row whose value isn't valid 64-char hex
  gets hex-encoded and rewritten. Env-var override (`MINT_AUTHORITY_TOKEN`
  set externally) is accepted as-is and *not* persisted, so the operator can
  still paste a raw value if they want — env wins on next boot anyway.
  `print-tokens` also detects a pre-migration row at print time and
  hex-encodes on the fly for display safety.
- **`/dist/*` HEAD probes returned 405.** chi doesn't auto-derive HEAD from
  GET, so caching proxies / link-checkers that probe before downloading
  failed. Registered explicit `r.Head` alongside `r.Get` for both
  `/dist/agentctl-linux-amd64` and `/dist/agentctl-darwin-arm64`. The handler
  uses `http.ServeContent` which strips the body on HEAD natively, so no
  handler change was needed.
- **Lifecycle event summaries were bare.** `session.started` /
  `session.checkpointed` / `session.ended` events surfaced in Mattermost as
  `"session ended for agent-operator-mac"` — no session id, no
  `final_status`, no project. Operator couldn't correlate the chat ping with
  the underlying session. Summaries now read like
  `[end] Splinter — session 12345678, user_exit` and
  `[start] Splinter — session 12345678, project=demo`. Alias (e.g.
  "Splinter") is preferred over canonical name when set; the agent alias is
  loaded from `agents.mattermost_username` into the auth context. Event
  payloads are unchanged — only the operator-facing summary string is
  enriched.

### Tests

- 4 new integration tests on the mint-authority migration (legacy-raw →
  hex, already-hex no-op, env-override not persisted, `PrintTokens` output
  is pure printable ASCII).
- 1 new unit test on `/dist/*` HEAD probes (200 + ETag + zero-length body).
- 8 new pure-unit tests on the lifecycle summary formatters (alias
  preference, canonical-name fallback, project-slug toggle, `final_status`
  variants, checkpoint summary fallback to status, short-session-id safety
  on strings < 8 chars).

`go vet ./...` clean. `go build ./...` clean. All unit tests pass.

## [0.1.7] — 2026-05-22

Federated trust path for agent enrolment + 9 follow-up polish items from the
v0.3.x fleet smoke. Pairs with `concept-workflow` plugin **v0.4.0**, which
documents the federated path operator-side and consumes the new signed-join-code
endpoints from `agentctl`.

The headline change is **signed join-codes**: an operator with elevated dual
auth can mint a short-lived signed credential, hand it out-of-band to a
third-party agent-VM owner, and that owner runs `agentctl join --code <code>`
on their own machine — no SSH or admin-token sharing required. The code is
HMAC-SHA256 signed, single-use, TTL-bounded (5min..7d), and atomically
redeemed.

### Gateway — new endpoints

- **`POST /v1/admin/join-codes`** — mint a signed code. Dual auth: the existing
  `Authorization: Bearer <admin-token>` AND `X-Mint-Authority: Bearer
  <mint-authority-token>`. The mint-authority token lives only on the gateway
  host's filesystem (persisted in the new `kv_store` table) and prevents an
  attacker who pops the admin token from minting infinite codes. Response codes
  split: **401** for admin-token failure, **403** for mint-authority failure
  (operator can disambiguate which credential is wrong without log access).
- **`POST /v1/join-codes/redeem`** — public; the code itself is the auth.
  Verifies HMAC sig, looks up jti in the `join_codes` table, checks expiry, then
  atomically `UPDATE … WHERE redeemed_at IS NULL RETURNING` to make 409
  detection race-free. On success, mints the agent's per-host bearer via the
  existing `agents.MintToken` helper (same path as
  `POST /v1/admin/agents/{name}/mint-token`). Returns 410/409/401/404 with
  human-readable error bodies the CLI parses into operator-facing messages.
- **`GET /v1/agents`** — operator-only fleet listing with canonical name,
  alias, role, joined_at, last_seen, and channel memberships derived from
  emitted-event history.
- **`GET /v1/events`** — operator-only paginated event history. Supports
  `since`, `agent`, `type`, `limit` (default 100, max 500), and opaque
  `cursor` token over `(created_at, id)` for stable pagination as new events
  arrive.
- **`GET /v1/health/full`** — extended health: gateway uptime + version,
  database connectivity + lag estimate, outbox-worker last-tick + queue
  depth, inbox-webhook last-received-at per agent, Mattermost reachability,
  count of failed-emit cache entries per peer.
- **`GET /dist/agentctl-linux-amd64`** and **`/dist/agentctl-darwin-arm64`**
  — public binary downloads served from `AGENT_HUB_DIST_DIR` (default
  `/opt/agent-hub/dist`). Federated agents `curl` these directly without
  SSH from the operator. `Content-Type: application/octet-stream`,
  `Content-Disposition: attachment`, ETag from mtime+size, 404 if missing,
  503 if `DistDir` unconfigured.

### Gateway — new subcommand

- **`agent-hub print-tokens`** — reads the persisted `JOIN_CODE_HMAC_KEY` +
  `MINT_AUTHORITY_TOKEN` from `kv_store` and prints to stdout. Intended for
  `docker exec agent-hub agent-hub print-tokens` so the operator can retrieve
  the auto-generated secrets after first boot. Refuses to run if `TERM` is
  unset (cheap "not redirected to a remote pipe" heuristic); pass `--force`
  to override.

### Gateway — bug fixes

- **`#45`** — inbox-webhook @-mention routing is now **case-insensitive**
  (`strings.EqualFold`) for both canonical names and aliases. Closes the gap
  where `@SPLINTER` would not route to `@Splinter`'s inbox. Original mention
  spelling preserved on the stored event for forensics.
- **`#46`** — `sanitiser.CheckMattermost` adds a **structural-field exemption**
  for known-safe Mattermost identifiers (`post_id`, `user_id`, `team_id`,
  `channel_id`, `file_ids`, `root_id`, `parent_id`, `trigger_id`). Free-form
  fields (`text`, `message`) still get full pattern scrutiny — the exemption
  is field-name-based, not content-shape-based. Generic `Check()` is
  unchanged; MM-aware path is opt-in via `CheckMattermost()`.
- **`#48`** — top-of-file design memo on `outbox/worker.go` documents the
  intentional dual-plane `sanitiser.blocked` relay (events plane = durable
  record, MM = curated surface with redacted placeholder). Prevents future
  "fix" PRs that drop sanitiser.blocked outbox rows.

### Gateway — schema

- **Migration `003_join_codes_kv.sql`** adds the `join_codes` table
  (jti UUID PK, agent_canonical, alias, role, expires_at, redeemed_at,
  redeemed_by_hostname) + the generic `kv_store` table for HMAC-key /
  mint-authority-token persistence across restarts.

### `agentctl` — new subcommands

- **`agentctl join-code mint`** — operator-side join-code minting. Flags:
  `--agent`, `--alias`, `--ttl <duration>` (default 24h), `--role`,
  `--gateway-url`, `--admin-token-file`, `--mint-authority-token-file`.
  Calls `POST /v1/admin/join-codes` with dual auth and prints the code +
  a paste-ready hand-off block (redemption command, gateway URL, TTL).
- **`agentctl join --code <code>`** — third-party VM redeem path. Calls
  `POST /v1/join-codes/redeem`, writes `mint_token` + `agent-events.env`
  using the same on-disk layout as the existing `--bootstrap-token` branch,
  then runs the same `register-agent` (+ optional smoke) flow. Friendly
  error messages for HTTP 410 / 409 / 401 / 404. Gateway-supplied
  `agent_canonical` / `alias` / `role` override CLI flags — the signed code
  is the source of truth.

### `agentctl` — bug fixes

- **`#40`** — `agentctl join --gateway-url <url>` flag. Precedence: flag >
  `AGENT_HUB_URL` env > config file. Flag-set value is exported via
  `os.Setenv` BEFORE `config.Load` so all downstream calls (mint, env-file
  write, register-agent, smoke) see the same URL. Persisted into
  `agent-events.env` on success so future invocations don't need the flag.
- **`#49`** — `Mattermost.EnsureTeamMember` adds bot-to-team add before any
  channel-add. Idempotent: HTTP 400 with MM's
  `api.team.add_user.to.team.failed.error` body is treated as success
  (already-member). Other 4xx propagate with HTTP status surfaced. Closes
  the channel-add `403: user not in team` gap on fresh MM teams.
- **`#50`** — `Mattermost.resolveTeamName` auto-derives
  `MATTERMOST_TEAM_NAME` from `GET /api/v4/teams` when the env var is
  unset. Single-team admins get zero-config; zero/multi-team admins get a
  clear error listing the visible teams and asking to set the env var
  explicitly. Existing explicit `MATTERMOST_TEAM_NAME` path is preserved.

### Sign / verify details

- HMAC-SHA256 over a base64url-encoded JSON payload `{jti, agt, exp, rol}`.
  Code format: `AGNT-<payload-b64url>.<sig-b64url>`.
- Key resolution at boot: `JOIN_CODE_HMAC_KEY` env (hex or base64url, 16+
  bytes after decode) → `kv_store` row → generate 32 random bytes + persist.
  Logged once on first-boot generation (`slog.Info "generated join-code
  HMAC key (one-time persistence)"`).
- Same pattern for `MINT_AUTHORITY_TOKEN`.
- `crypto/rand` for both key + jti generation. UUID inline (no `google/uuid`
  dep added).

### Tests

- 5 new unit tests on sign/verify (round-trip, tampered sig, wrong key,
  malformed shapes, jti uniqueness over 100 generations).
- 11 new integration tests on mint+redeem (gated by
  `AGENT_HUB_TEST_DATABASE_URL`; cover dual-auth failure modes, mint TTL
  validation, full happy-path, double-redeem 409, expired 410, tampered
  sig 401, bogus jti 404, missing-body 400).
- 4 new tests on `/dist` serving (200 + Content-Type, 404, 503-when-disabled,
  ETag).
- 4 tests on `#46` sanitiser exemption (structural-field bypass, free-form
  scrutiny, MM-id-shape-inside-text still scrutinised, generic `Check()`
  does NOT bypass).
- 3 tests on `#45` case-insensitive routing.
- 4 tests on `#50` auto-derive.
- 3 tests on `#49` EnsureTeamMember.

`go vet ./...` clean. `go build ./...` clean. All unit tests pass.

## [0.1.6] — 2026-05-22

Two new `agentctl` subcommands that close the operator-push / agent-pull split
introduced by `concept-workflow` plugin **v0.3.0** (the 4-skill bootstrap/join
redesign). No gateway changes, no schema migration — purely client-side
additions that reuse the existing `POST /v1/admin/agents/{name}/mint-token`,
`POST /v1/agents/register`, and lifecycle endpoints.

### Added — `agentctl join`

- **New subcommand** wrapping the events-side bootstrap-then-register-then-smoke
  flow that previously lived as a ~30-line bash sequence inside the operator-Mac
  `/setup-agent-events` skill. Single command on each agent VM provisions itself
  end-to-end:
  1. Resolves `--bootstrap-token <path>|env:VARNAME` (chmod-600-enforced for
     file paths; never logs the plaintext).
  2. Calls `POST /v1/admin/agents/{name}/mint-token` to mint the per-host
     bearer; writes it to `~/.config/concept-workflow/agent-hub-token` chmod
     600 via an atomic rename.
  3. Writes `~/.config/concept-workflow/agent-events.env` with the four env
     vars (`AGENT_HUB_URL`, `AGENT_HUB_TOKEN_FILE`, `AGENT_NAME`,
     `AGENT_PROJECT_SLUG`); shape mirrors v0.2.13's operator-Mac file.
  4. Invokes the existing `register-agent` flow in-process (no shell-out) with
     `--mattermost-username <alias>` so agent aliases (Splinter / Mikey /
     Donnie) reach the gateway.
  5. With `--smoke`, runs the full session-start → event emit (test.smoke) →
     resume-context → session-end round-trip against the gateway to validate
     the freshly-minted token works end-to-end.
- **Idempotent.** Re-running with the same args is a no-op when the per-host
  token file already exists chmod 600; print `token already present, using
  existing`. `--rotate` forces a fresh mint.
- **`--name` defaults** to `agent-operator-mac` on Darwin (the operator-Mac
  case); required on every other GOOS.
- **`--alias` prompt** falls back to interactive stdin when absent so the
  agent VM can pick its own display name without operator dictation.
- **Closes plugin-side bug `#30` constructively** by stamping the smoke
  session-id with `setup-smoke-<name>-<unix-nanos>` so two consecutive joins
  cannot collide on the `agent_sessions` row.

### Added — `agentctl comms-join`

- **New subcommand** that mirrors the join shape for the comms layer
  (Mattermost in v0.1.6; Slack/Discord stubbed for v0.4). Per-VM bot user
  provisioning + PAT mint + chmod-600 storage + env-file write, all
  idempotent:
  1. Resolves `--bootstrap-pat <path>|env:VARNAME` (operator-supplied
     admin PAT; used once, never stored).
  2. `Backend.Validate` confirms the admin PAT against `GET
     /api/v4/users/me`.
  3. `Backend.EnsureBotUser` looks up `<bot-name>` and POSTs to
     `/api/v4/bots` if absent.
  4. `Backend.AddBotToChannel` resolves the team + channel and POSTs the bot
     to `/api/v4/channels/{id}/members`; treats "already member" responses
     as success.
  5. `Backend.MintPAT` calls `/api/v4/users/{id}/tokens` and writes the
     plaintext to `~/.config/concept-workflow/mattermost-bot-pat` chmod 600.
  6. Writes `~/.config/concept-workflow/concept-chat.env` with
     `CONCEPT_CHAT_MM_URL`, `CONCEPT_CHAT_MM_PAT_FILE`,
     `CONCEPT_CHAT_MM_CHANNEL`.
- **`--backend mattermost|none|slack|discord`** — only `mattermost` is
  implemented in v0.1.6. `none` is a no-op (events-only fleets). `slack` /
  `discord` print "not yet implemented (v0.4 stub)" and exit 1.
- **`--bot-name` defaults** to `<AGENT_NAME>-bot` so the per-VM bot user
  matches the agent identity.
- **`--channel` defaults** to `agent-comms`.
- **`--rotate`** forces a fresh PAT mint even if
  `mattermost-bot-pat` is already present chmod 600.
- **systemd-creds dropped** for the comms PAT in v0.1.6+ (per plan
  §"Token-storage consistency"). Single chmod-600 file pattern for both the
  events token and the comms PAT; one audit surface, one recovery story.
  Migration recipe in `concept-workflow` v0.3.0's `/join-agent-comms` skill
  notes the cleanup step for hosts that previously held `systemd-creds`
  blobs.

### Added — pluggable backend interface

- **`internal/agentctl/commands/comms_backends.Backend`** — minimal four-method
  contract (`Validate`, `EnsureBotUser`, `AddBotToChannel`, `MintPAT`) so
  future Slack/Discord backends slot in without touching the `comms-join`
  subcommand wiring.
- **`mattermost.go`** — concrete implementation against the v4 REST API.
  Honours `MATTERMOST_TLS_SKIP_VERIFY` (same knob as the outbox-worker; see
  `[0.1.4]`) so the homelab self-signed-cert deployment shape Just Works.

### Changed

- **Binary version:** `agentctl` `0.1.5` → `0.1.6`. `Makefile` `VERSION ?=
  0.1.5` → `0.1.6`. (No gateway-binary bump this release — the gateway is
  unchanged.)

### Tests

- **15 new unit tests** in `internal/agentctl/commands/`:
  - `join_test.go` (8): flag validation, idempotent re-run skipping mint,
    `--rotate` forcing fresh mint, env-file shape + chmod, smoke round-trip
    call counts, register-agent body carries the alias, insecure bootstrap
    token file rejected, smoke session-id is unique across consecutive runs
    (bug `#30` regression test).
  - `comms_join_test.go` (10): backend validation (`--backend` required,
    slack/discord stub, none no-op), happy-path call counts + chmod-600 PAT
    + env-file shape, default bot-name from `$AGENT_NAME`, default channel,
    idempotent re-run skipping mint, `--rotate` forcing fresh mint,
    validate-error best-effort vs strict.
- **1 new integration test** (`TestIntegration_Join`) in
  `internal/agentctl/commands/integration_test.go` — drives `agentctl join
  --smoke` end-to-end against the live chi router + Postgres, verifies the
  token file lands chmod 600, the agent row has the alias, the smoke
  session + `test.smoke` event landed, and the idempotent re-run path
  prints the expected message without re-minting.

### Tracked follow-ups

- **`#27`** — Integration-test isolation (server + agentctl/commands packages
  share Postgres in parallel). New join integration test runs cleanly when
  the suite is invoked with `go test -p 1 ./...`; parallel-run failures
  remain a pre-existing condition tracked separately.

### Plugin coupling

`concept-workflow` plugin **v0.3.0+** ships the four matching skills
(`/bootstrap-agent-events`, `/join-agent-events`, `/bootstrap-agent-comms`,
`/join-agent-comms`) that consume these subcommands. Plugin v0.2.x continues
to work with the v0.1.5 binary — v0.1.6 is strictly additive.

## [0.1.5] — 2026-05-18

Hotfix: inbox-webhook payload-parse failure on Mattermost's outgoing webhook.

### Fixed

- **`webhookPayload.Timestamp` field type** changed from `string` to `int64`. Mattermost sends `timestamp` as a JSON number (Unix milliseconds), not a string. v0.1.3's struct definition assumed string → `json.Unmarshal` errored with `cannot unmarshal number into Go struct field webhookPayload.timestamp of type string` → every inbound @-mention dropped. Form-encoded path now uses `parseInt64` helper for the same field (best-effort numeric parse; 0 if missing/unparseable).

### How this surfaced

Component C bidi smoke from `agent-events` Mattermost channel hit two earlier blockers first (Mattermost SSRF policy blocking the 10.0.5.38 callback; before that, leaf-only TLS cert chain) — once both were fixed Mattermost DID fire the webhook and our parsing bug surfaced as `webhook parse failed` warnings in the inbox-webhook log. v0.1.5 is the actual fix.

## [0.1.4] — 2026-05-18

Hotfix: outbox-worker TLS-cert validation for homelab Mattermost.

### Fixed

- **`MATTERMOST_TLS_SKIP_VERIFY` env var** (default `false`) lets the outbox-worker bypass TLS certificate validation when posting to Mattermost. Required for homelab deployments where Mattermost runs behind a self-signed or internal-CA-signed cert. Without this, v0.1.3's outbox-worker failed every Mattermost POST with `tls: failed to verify certificate: x509: certificate signed by unknown authority` (all outbox rows stuck `status=failed` after one attempt). Symmetric to the `curl -sk` pattern the operator already uses against the same Mattermost instance.
- `docker-compose.yml` exposes the new env to outbox-worker with default `false` so proper-cert deployments are unaffected.

### Operator action required when upgrading from v0.1.3

If your Mattermost cert isn't trusted by the container's default CA bundle:
1. Add `MATTERMOST_TLS_SKIP_VERIFY=true` to `/opt/agent-hub/.env`.
2. Reset any already-failed outbox rows back to pending so they retry: `UPDATE mattermost_outbox SET status='pending', attempts=0, last_error=NULL WHERE status='failed' AND last_error LIKE '%certificate%';`
3. `docker compose up -d --build outbox-worker` to pick up the new env.

## [0.1.3] — 2026-05-18

ROADMAP `#10` Component C — Mattermost bidirectional. Curated agent events now flow to a Mattermost channel via the outbox-worker, and operator @-mentions in that channel flow back to per-agent inboxes via the inbox-webhook. Also fixes task `#29` (sanitiser self-block on the operator's own gateway URL).

### Added — curated event types + transactional outbox-write (gateway)

- **`events.CuratedEventTypes`** — explicit set of 11 event types that mirror to Mattermost: `task.{created,claimed,blocked,unblocked,completed}`, `decision.{proposed,accepted,rejected}`, `handoff.created`, `session.ended`, `sanitiser.blocked`. Non-curated events skip the outbox (no chat noise).
- **`events.InsertWithOutbox(ctx, pool, params, OutboxConfig, message)`** — single transaction inserts the events row and, when curated, the mattermost_outbox row. Outbox-write failure rolls back the whole event — no orphan ledger entries, no orphan outbox rows. Used by `POST /v1/events`, the sanitiser audit event, and lifecycle session emissions.
- **Channel resolution priority**: per-project `projects.mattermost_outbox_channel` (when set) → `MATTERMOST_DEFAULT_OUTBOX_CHANNEL` env (default `agent-events`). Empty resolved channel = no outbox row (logged warn, event still writes).
- **`internal/outbox/outbox.go`** new package with `InsertPending(ctx, tx, params)` so the events handler and the worker share one source of truth for the outbox row shape.

### Added — `agent-hub outbox-worker` (real impl)

- **Drains `mattermost_outbox`** on a ticker (`POLL_INTERVAL_SECONDS`, default 5s). For each pending row: resolves channel-name → channel-id (cached in-process, hits `/api/v4/teams/name/{team}/channels/name/{channel}`), then POSTs `/api/v4/posts` with `Authorization: Bearer <PAT>` and a `props.idempotency_key = "{event_id}_{attempt}"`.
- **Per-row HTTP-class handling**: 2xx → `status='sent', sent_at=now()`; 4xx → `status='failed'` (non-retryable); 5xx/network → `attempts++`, row stays pending; at `attempts >= MaxAttempts` (5) the row is forced to `failed` so the queue can't grow behind a poisoned message.
- **Graceful shutdown** via `signal.NotifyContext` — in-flight POST completes before exit; the loop respects `ctx.Done()`.
- **New env vars**: `MATTERMOST_TEAM_NAME` (required, for channel-name resolution), `MATTERMOST_DEFAULT_OUTBOX_CHANNEL` (used by gateway, not worker).

### Added — `agent-hub inbox-webhook` (real impl)

- **HTTP server** on `:8788` (`LISTEN_ADDR`). Exposes `GET /health` (no-auth, `{"status":"ok"}`) and `POST /v1/inbox/webhook` (Mattermost outgoing-webhook receiver).
- **Token validation**: constant-time compare of the inbound `token` field against `WEBHOOK_SECRET`. Mismatch → 401 with empty body (no echo).
- **Content-type adaptive**: handles both `application/x-www-form-urlencoded` (Mattermost default) and `application/json` (when the operator configures the webhook for JSON).
- **@-mention parsing**: regex `@([A-Za-z0-9][A-Za-z0-9._-]*)` over the message body. For each handle: try `agents.name` exact match; fall back to `agents.mattermost_username` so `@Splinter` resolves to `agent-operator-mac`. Unresolved handles are logged-warn + skipped silently.
- **Idempotent insert**: one `mattermost_inbox` row per resolved agent, `ON CONFLICT (source_post_id, target_agent_id) DO NOTHING` so Mattermost's at-least-once outgoing-webhook delivery doesn't double-insert. Migration `002_mattermost_inbox_unique.sql` creates the supporting partial unique index.

### Added — schema migration

- **`002_mattermost_inbox_unique.sql`** — partial `UNIQUE (source_post_id, target_agent_id) WHERE source_post_id IS NOT NULL` for inbox-webhook idempotency. Applied by the embedded migration runner on boot.

### Fixed — sanitiser self-block (task `#29`)

- **`SANITISER_EXEMPT_HOSTS`** comma-separated env var (default `10.0.5.38`). When a §2.1 regex matches a substring containing any exempt host, the match is suppressed and scanning continues. Prevents the gateway's permissive `\b10\.\d+\.\d+\.\d+\b` private-range rule from blocking its own URL when it legitimately appears in event payloads (e.g., `AGENT_HUB_URL` in session-start metadata).
- API change: **`sanitiser.Load(path, exemptHosts []string)`** — the exempt list is supplied at load time so per-Check call sites stay unchanged. Empty/whitespace exempt entries are ignored so an unset env var doesn't accidentally exempt everything.

### Changed

- **docker-compose.yml**: `outbox-worker` + `inbox-webhook` services no longer behind the `v0.1.1` compose profile — they now ship real implementations and come up by default with the rest of the stack. `MATTERMOST_TEAM_NAME` added to the outbox-worker env; `SANITISER_EXEMPT_HOSTS` + `MATTERMOST_DEFAULT_OUTBOX_CHANNEL` added to the gateway env.
- **`.env.example`**: documents the three new vars (`MATTERMOST_TEAM_NAME`, `MATTERMOST_DEFAULT_OUTBOX_CHANNEL`, `SANITISER_EXEMPT_HOSTS`).
- **Binary versions**: `agent-hub` `0.1.2` → `0.1.3`; `agentctl` `0.1.2` → `0.1.3`. `Makefile` `VERSION ?= 0.1.2` → `0.1.3`.

### Tests

- **8 new sanitiser unit tests** in `internal/sanitiser/sanitiser_test.go` (exempt-host suppression, mixed-exempt-and-non-exempt, empty-entries-ignored, plus the existing five updated to the new `Load(path, exempt)` signature).
- **8 new outbox-worker integration tests** in `internal/outbox/worker_test.go` — happy path (post body shape + idempotency_key), 4xx → failed, 5xx → attempts-bump → failed at ceiling, channel-lookup failure → failed, empty-queue noop, channel-cache, id-passthrough, config validation. All use httptest-mocked Mattermost, no live MM dependency.
- **8 new inbox-webhook integration tests** in `internal/inbox/webhook_test.go` — bad token 401, non-POST 405, /health, form-post happy path, JSON post path, mattermost_username resolution (@Splinter), unknown-mention skip, multiple mentions, redelivery idempotency.

### Plugin coupling

`concept-workflow` plugin **v0.2.12+** ships the matching `/agent-inbox` operator skill and the Phase 4 Mattermost setup (outgoing-webhook provisioning + agent-aliases populated). Plugin v0.2.11 and earlier continue to work; this release is strictly additive — curated events queue regardless of whether the worker is up, and the worker drains as soon as it starts.

## [0.1.2] — 2026-05-18

Projects API + agent-alias plumbing. Unblocks `concept-workflow` plugin **v0.2.11**'s multi-project event scoping and named agents (Splinter / Mikey / Donnie) posting under per-agent Mattermost handles. Without these, `/setup-agent-events` had to provision the `projects` row via direct SQL, and the plugin couldn't surface agent aliases in Mattermost.

### Added — gateway endpoints

- **`POST /v1/projects`** — idempotent upsert keyed by slug. Body: `{slug, name, forge_url?, default_branch?, mattermost_outbox_channel?, mattermost_inbox_channel?}`. On slug conflict, updates the name + optional fields (via COALESCE so nil values don't clobber existing rows) and returns the existing id. Auth: per-host agent token (`RequireAgent`).
- **`GET /v1/projects`** — list every project ordered by slug. Returns `{"projects": [...]}`. Used by the plugin's `/agent-events-health` diagnostic.
- **`GET /v1/projects/{slug}`** — single fetch; 404 with `project_not_found` if missing.

### Added — `agentctl` subcommands + flags

- **`agentctl project register --slug <slug> --name <name> [--forge-url …] [--default-branch …] [--mattermost-outbox-channel …] [--mattermost-inbox-channel …]`** — wraps `POST /v1/projects`. Best-effort posture per repo convention (audit + stderr + exit 0 on failure; `--strict` for exit 1). `--json` emits the full response body on stdout; default emits a one-line summary on stderr (`project register: project <slug> registered (id=<short>)`).
- **`agentctl register-agent --mattermost-username <handle>`** — flag was already wired through to the request body but lacked test coverage and CLI-side documentation. Test `TestRegisterAgent_SerializesMattermostUsername` now pins the serialisation behaviour so agent aliases (Splinter / Mikey / Donnie) reach the gateway.

### Changed

- **Binary versions:** `agent-hub` bumped `0.1.0` → `0.1.2`; `agentctl` bumped `0.1.0-dev` → `0.1.2` (the two binaries now ship in lockstep). `Makefile` `VERSION ?= 0.1.0-dev` → `0.1.2`.

### Tests

- **8 new unit tests** in `internal/server/handlers_projects_test.go` (project upsert / list / get happy-paths + idempotent-upsert + missing-auth/slug/name 4xxs + 404 on missing).
- **6 new unit tests** in `internal/agentctl/commands/commands_test.go` for `project register` (correct request shape, required-fields validation, best-effort vs strict posture, --json output, network-error best-effort).
- **1 new test** pinning `register-agent --mattermost-username` serialisation.
- **1 new integration test** (`TestIntegration_ProjectRegister`) exercising the upsert path end-to-end against the live gateway + verifying idempotency.

### Plugin coupling

`concept-workflow` plugin **v0.2.11+** consumes the projects API for `/setup-agent-events` provisioning and the `mattermost_username` plumbing for the Splinter / Mikey / Donnie agent aliases. Plugin v0.2.10 and earlier continue to work; this release is strictly additive.

## [0.1.1] — 2026-05-17

`agentctl` client implementation. Closes the v0.1.0 known-limitation "agentctl subcommands are still stubbed" — Component B's plugin hooks (session-start, post-tool-use, pre-compact, stop, session-end on plugin v0.2.9) now reach real handlers via this binary.

### Added — `agentctl` CLI

- **8 subcommands wrapping the v0.1.0 gateway endpoints:** `register-agent`, `session-start`, `session-end`, `event emit`, `checkpoint`, `resume-context`, `inbox poll`, `health`. Distributed as a single static Go binary cross-compiled for `darwin-arm64` (operator Mac) and `linux-amd64` (agent VMs).
- **Shared infrastructure** (`gateway/internal/agentctl/`):
  - `config/` — env-var loader (`AGENT_HUB_URL`, `AGENT_HUB_TOKEN_FILE`, `AGENT_NAME`, `AGENT_PROJECT_SLUG`, `AGENT_HUB_AUDIT_LOG`). **Refuses to load** token files with group/world bits set (mode & 0o077 != 0); emits the paste-ready `chmod 600 <file>` hint.
  - `client/` — HTTP client with bearer-auth, 5s connect / 30s overall timeouts, JSON error-envelope decode into a sentinel-error set (`ErrSanitiserBlocked`, `ErrUnauthorized`, `ErrNotFound`, `ErrServerUnavailable`, `ErrBadRequest`) that satisfies `errors.Is`.
  - `audit/` — append-only JSONL writer at `$AGENT_HUB_AUDIT_LOG` (default `$HOME/.local/state/agent-events/audit.log`); itself best-effort so audit failures never block the caller.
- **Best-effort posture by default** per design plan §"Fail-closed semantics": on any error → audit entry + stderr line + exit 0. The `--strict` persistent flag overrides to exit 1 for hard-fail callers (e.g., the orchestrator's `review-posted` step). The `health` subcommand always exits 1 on failure regardless of `--strict` (a best-effort health check is nonsense).
- **Output shapes:** read commands (`resume-context`, `inbox poll`, `health`) emit JSON on stdout (`--pretty` for indented); mutating commands emit a one-line success summary on stderr (`--json` for the full response body on stdout). Clean for pipe-into-jq usage.
- **Sanitiser-blocked errors surface `matched_pattern`, `matched_field`, and `blocked_event_id` to stderr** so the operator sees exactly which §2.1 pattern fired without the offending content being echoed back.

### Added — build tooling

- **Makefile** at repo root with `agentctl-darwin-arm64`, `agentctl-linux-amd64`, `agentctl-all`, `clean` targets. CGO disabled, `-ldflags="-s -w"` strip, version + commit baked in via ldflags. Output: `bin/agentctl-*` (gitignored).

### Tests

- **40 new tests passing** (6 audit + 8 client + 7 config + 19 commands unit) + 7 integration tests that skip without `AGENT_HUB_TEST_DATABASE_URL`. Cumulative test count: 67 passing.
- **End-to-end live smoke** validated against the running gateway: 8 subcommands happy-path; sanitiser-block path surfaces pattern/field; arg-validation fails best-effort with stderr + exit 0 / `--strict` with exit 1; chmod-644 token file refused at config-load with actionable hint.

### Reviewer findings landed

Code-review-specialist flagged 1 BLOCKER + 2 HIGH + 1 paired MEDIUM; all fixed before merge (commit `2180676` on top of the implementation commit `6a2f612`).

### Known limitations (v0.1.1)

- **Outbox-worker + inbox-webhook stubs remain.** Component C is the v0.1.2 release (bumped from v0.1.1 since agentctl took that slot).
- **Tasks / handoffs / decisions / locks endpoints are deferred to v0.1.2.** Not called by plugin v0.2.8 / v0.2.9 hooks; first matter at plugin v0.2.13 (Component D-3).
- **agentctl operator-role cross-agent reads not yet exposed.** The gateway supports them; agentctl's `inbox poll` only polls self. Add `--agent-name` flag in v0.1.x when use-case demands it.
- **Polish items from the v0.1.1 review:** cmd-name in chmod-perms stderr says "emit" not "event emit" (cobra's `cmd.Name()` quirk); chmod-perms stderr line missing the `continuing (best-effort)` posture marker the other errors carry. Both LOW; fix in v0.1.x.

### Plugin coupling

`concept-workflow` plugin **v0.2.8+** (Component A — operator skills) and **v0.2.9+** (Component B — lifecycle hooks) consume this release. Plugin v0.2.9 hooks were **functionally broken** in v0.1.0 (calling stubbed agentctl); v0.1.1 closes that gap.

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
