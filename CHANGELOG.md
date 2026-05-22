# Changelog — agent-hub

All notable changes to this project are documented here.

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
