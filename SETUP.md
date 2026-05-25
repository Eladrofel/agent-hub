# SETUP — terraform-agent-hub

Operator-facing setup walkthrough for a fresh agent-hub deployment. Pairs with `README.md`'s overview.

**Currency:** Step 1 → Step 3 (VM provision + bring-up) match the v0.1.16 gateway. Step 5 (per-peer agent provisioning) uses the v0.3.0+ split flow (`/bootstrap-agent-events` + `/join-agent-events`); the legacy single `/setup-agent-events` skill referenced in older docs is a deprecated stub. End-to-end recipe across operator + N VMs lives in the plugin's `references/portable-setup-guide.md`; this doc covers the agent-hub side only.

## Prerequisites

| Item | Where it comes from |
|---|---|
| **Proxmox API token** scoped to the `apollo` node | Proxmox UI → Datacenter → Permissions → API Tokens. Set as env var `PROXMOX_VE_API_TOKEN` (see `terraform-mattermost/README.md` § "Prerequisites" for the direnv + Keychain pattern). |
| **Apollo node reachable** at `10.0.1.4` (default) | Tailscale or LAN connection to the apollo subnet. |
| **DHCP reservation** for the chosen MAC + IP | Reserve `BC:24:11:10:05:38` → `10.0.5.38` (defaults) in your DHCP server BEFORE first apply. The VM otherwise won't get the static IP. |
| **DNS record** for `agent-hub.litts.link` → `10.0.5.38` | Add to your local DNS or `/etc/hosts` for operator convenience. Optional — IP-only access works. |
| **`terraform-apollo` applied** (or Ubuntu image manually present) | The Ubuntu 24.04 cloud image must exist on the apollo node as `local:iso/ubuntu-24.04-server-cloudimg-amd64.img`. See `terraform-mattermost/main.tf` comment header for the manual download recipe. |
| **Mattermost reachable + bot account provisioned** | `agent-hub`'s outbox-worker + inbox-webhook reuse the chat-emit service account from `setup-agent-comms`. If chat-emit isn't yet set up, do that first. |
| **Terraform 1.6+ installed** locally | `brew install terraform`. |
| **Go 1.23+ installed locally** (if you'll build agentctl from source) | `brew install go`. The Dockerfile builds the gateway binary inside the build container, so Go is only needed locally for cross-compiling agentctl for darwin-arm64 + linux-amd64. |

## Step 1 — Configure Terraform

```bash
cd ~/projects/terraform-agent-hub
cp terraform.tfvars.example terraform.tfvars
# Edit terraform.tfvars if your IP / MAC / hostname differs from defaults.

# Authenticate to Proxmox (recommended pattern — direnv + macOS Keychain;
# see terraform-mattermost/.envrc.example for the full one-time setup).
export PROXMOX_VE_ENDPOINT="https://apollo.litts.link:8006"
export PROXMOX_VE_API_TOKEN="terraform@pve!provider=<token-value>"
export PROXMOX_VE_SSH_USERNAME="root"

terraform init
terraform plan
terraform apply
```

After `apply` succeeds, `terraform output post_provision_checklist` prints the next-step recipe; the rest of this doc is that recipe in expanded form.

## Step 2 — Populate `.env` on the VM

Cloud-init wrote `/opt/agent-hub/.env` with placeholder `CHANGE_ME_FIRST_BOOT` values. Replace them before bringing the stack up:

```bash
ssh dale@10.0.5.38
sudo nano /opt/agent-hub/.env
```

Required values:

- `POSTGRES_PASSWORD` — generate fresh: `openssl rand -base64 32`
- `ADMIN_TOKEN` — generate fresh; this is what `/setup-agent-events` will use on the operator Mac to mint per-VM tokens. `openssl rand -base64 32`
- `MATTERMOST_TOKEN` — service-account PAT (reuse from chat-emit)
- `MATTERMOST_INBOX_WEBHOOK_SECRET` — generate fresh; you'll paste this same value into Mattermost's outgoing-webhook config in Step 4

## Step 3 — Bring the stack up

```bash
cd /opt/agent-hub
sudo docker compose up -d
sudo docker compose ps
```

Expected on current (v0.1.16): four services healthy — `agent-hub-postgres`, `agent-hub-gateway`, `agent-hub-outbox`, `agent-hub-inbox-webhook`. The v0.1.1 compose-profile gating shipped in v0.1.2 has been retired; all services run by default.

Smoke test:

```bash
curl -fsSL http://10.0.5.38:8787/health
# Expected (current v0.1.16): {"sanitiser_patterns":<N>,"status":"ok"}
```

## Step 4 — Configure the Mattermost outgoing webhook

In Mattermost → Integrations → Outgoing Webhooks → Add:

- **Content type:** `application/json`
- **Trigger when:** `1` (first word matches) — load-bearing for the peer @-mention routing path (see v0.5.0 empirical finding in plugin CHANGELOG); see "Trigger words" below.
- **Trigger words:** `@` (literal @-sign). Combined with `trigger_when=1`, this fires the webhook on any post whose first word starts with `@`. The gateway's outbox-worker (v0.1.15+) prepends `@<peer-alias>` to work-item events for exactly this reason — peer mentions become inbox-routed automatically.
- **Channel:** `agent-events` (create the channel first if needed).
- **Callback URL:** `http://10.0.5.38:8788/v1/inbox/webhook`
- **Token:** paste the value you set for `MATTERMOST_INBOX_WEBHOOK_SECRET` in Step 2.

## Step 5 — Per-peer agent provisioning (split flow, v0.3.0+)

The legacy single `/setup-agent-events` skill was split in plugin v0.3.0 into operator-side bootstrap + per-VM join. The new flow is:

### 5a. Operator-Mac config (v0.2.11+ split — operator-wide, fleet settings)

Operator-wide settings live at `~/.config/concept-workflow/config.yaml` (paste-ready template at `references/agent-events-operator-config.template.yaml` in the plugin repo). Required keys:

```yaml
agent-events:
  gateway-url: http://10.0.5.38:8787
  per-vm-token-file: ~/.config/concept-workflow/agent-hub-token
  default-mattermost-outbox-channel: agent-events
  mattermost-inbox-webhook-secret: <same value as Step 2 / Step 4>
  alias-map:
    agent-operator-mac: Splinter
    agent-1: Mikey
    agent-2: Donnie
    # add more as VMs come up
  ssh-map:                              # optional; only needed for --prepare-vm fan-out
    agent-1: claude-1
    agent-2: claude-2
```

Per-workspace settings (project slug, role map, MM channel override) live in each `<workspace>/.claude/concept-workflow.local.md` frontmatter:

```yaml
agent-events:
  project-slug: secureup
  role-map: {agent-1: frontend, agent-2: backend, agent-operator-mac: operator}
```

### 5b. From the operator Mac, run `/bootstrap-agent-events`

```bash
export AGENT_HUB_ADMIN_TOKEN=<the ADMIN_TOKEN from Step 2>
# From a Claude Code session on the Mac:
/bootstrap-agent-events
# or, to also fan out the agentctl binary + per-VM tokens via SSH:
/bootstrap-agent-events --prepare-vm claude-1 --prepare-vm claude-2
```

This will:

1. Upsert the project on the gateway (using `project-slug` from the workspace).
2. Register the operator Mac as the first peer (`agent-operator-mac`, alias `Splinter`).
3. Mint a per-host token for the Mac, write to `~/.config/concept-workflow/agent-hub-token` (chmod 600).
4. (With `--prepare-vm`) scp the `agentctl` binary + a one-time admin-scoped token to each VM listed under `ssh-map`, so the VM-side `/join-agent-events` has what it needs.
5. (Alternative) `/bootstrap-agent-events --issue-join-code <agent-name>` mints a signed, single-use, TTL-bounded join-code for agents whose VMs the operator doesn't have SSH access to (federated path, v0.4.0+).

### 5c. From each agent VM's own Claude Code session, run `/join-agent-events`

This runs **on the VM itself**, NOT from the operator's Mac. Each VM owns its own bot identity.

```bash
# In the VM's Claude Code session:
/join-agent-events
# or, if joining via federated code:
/join-agent-events --code <code-issued-by-operator>
```

The skill provisions: this agent's identity against the gateway, registers `last_seen_at`, smoke-tests an event emit + inbox round-trip, and runs `/join-agent-comms` for the Mattermost bot identity if `--with-comms` is passed.

End-to-end recipe with the full operator + N-VM choreography lives at `references/portable-setup-guide.md` in the plugin repo.

## Step 6 — Verify

```bash
# On the Mac, after /bootstrap-agent-events + per-VM /join-agent-events:
agentctl health
# Expected: gateway reachable, token valid, last_seen_at updates.

# Verify all peers registered (use the /agent-events-health skill from the
# operator Mac for a richer report including outbox-worker, inbox-webhook,
# MM reachability, and per-peer recent-event activity):
/agent-events-health
```

Direct Postgres query (port 54329 is the localhost-only bind on the agent-hub VM, exposed for operator diagnostics over SSH tunnel):

```bash
ssh dale@10.0.5.38 \
  sudo docker exec agent-hub-postgres psql -U agent_hub -d agent_hub \
  -c "select name, role, host_kind, last_seen_at from agents order by name;"
```

### Verify the work-item peer-coordination path (v0.1.14+)

```bash
# From the operator Mac, source the agent-events env then exercise the
# work-item lifecycle against a throwaway wi-key:
source ~/.config/concept-workflow/agent-events.env
agentctl work-item claim --wi-key feat-test-99-smoke --repo customer-web --branch smoke
agentctl work-item active --wi-key feat-test-99-smoke --pretty   # expect total=1
agentctl work-item finish --wi-key feat-test-99-smoke --repo customer-web
agentctl work-item active --wi-key feat-test-99-smoke            # expect total=0
```

You should also see the corresponding posts in the `agent-events` Mattermost channel: 🔵 for the claim, ✅ for the finish, both leading with `@<peer-alias>` mentions (v0.1.15+) that route through the inbox-webhook back into each peer's inbox.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `terraform apply` hangs at `proxmox_virtual_environment_vm.agent_hub_vm: Still creating...` | VM is booting + cloud-init is running. Wait ~3 min. | `ssh dale@10.0.5.38 'cloud-init status --wait'` to watch progress. |
| Gateway returns `connection refused` after `docker compose up -d` | `.env` placeholder values still present | `cat /opt/agent-hub/.env` — make sure every `CHANGE_ME_FIRST_BOOT` is replaced. `docker compose down && docker compose up -d`. |
| Outbox-worker logs `mattermost: unauthorized` | `MATTERMOST_TOKEN` invalid | Verify with `curl -fsSL -H "Authorization: Bearer $MATTERMOST_TOKEN" $MATTERMOST_URL/api/v4/users/me`. |
| Inbox-webhook logs `invalid token` on incoming Mattermost posts | Webhook secret mismatch between Mattermost UI and `.env` | Re-paste the value in both places; they must match exactly. |
| `/bootstrap-agent-events --prepare-vm` fails at "VM unreachable" | SSH alias in operator config is stale | Check `~/.config/concept-workflow/config.yaml` `agent-events.ssh-map` block; verify `ssh <alias> echo ok` works. |
| `agentctl work-item active` returns HTTP 401 `invalid_token` | Per-host token file unreadable or hash drifted | `chmod 600 ~/.config/concept-workflow/agent-hub-token`; if persistent, re-run `/bootstrap-agent-events --register-mac-only` (operator Mac) or `/join-agent-events --rotate-token` (VM). |
| `agentctl event emit --task-key feat-04-...` returns HTTP 422 `task_key_looks_like_work_item` | The `--task-key` flag is the legacy `tasks` table key, NOT a concept-workflow work-item key (v0.1.16 smart 422). | Omit `--task-key`; use `agentctl work-item {claim,finish,active}` for work-item lifecycle. |

## Maintenance

- **Backups:** Postgres data volume is at `agent_hub_pg` (Docker named volume). Snapshot via `pg_dump` weekly; archive to off-VM storage. Terraform doesn't manage backups; configure separately.
- **Updates to agent-hub itself:** on the agent-hub VM, `git pull && sudo docker compose build gateway && sudo docker compose up -d gateway` (the other services don't change as often; rebuild them individually if their Dockerfile shifts). Migrations are idempotent; the gateway's embedded migration runner applies new SQL files automatically on boot.
- **Updating `agentctl` on operator Mac + every VM:** the agentctl binary is built locally via `make agentctl-all` and distributed manually. After bumping the gateway version, also bump `agentctl` so the client + server commit hashes match (otherwise `agentctl --version` reports an older commit). Recipe:
  ```bash
  # On the operator Mac, after pulling agent-hub master:
  make agentctl-all
  cp bin/agentctl-darwin-arm64 ~/.local/bin/agentctl
  # Push to each VM (loop over ssh-map):
  for vm in claude-1 claude-2; do
    scp bin/agentctl-linux-amd64 "$vm":/tmp/a
    ssh "$vm" 'sudo install -m 0755 -o root -g root /tmp/a /usr/local/bin/agentctl && rm /tmp/a'
  done
  ```
  Verify with `agentctl --version` on each host.
- **Moving the agent-hub to a new VM:** see plugin's `/move-agent-hub` skill.
- **Periodic check:** `/agent-events-health` from the operator Mac surfaces gateway / Postgres / outbox-worker / inbox-webhook / MM / per-peer event-activity in one report; useful at the start of any session where the agent-events layer feels off.
