# SETUP — terraform-agent-hub

Operator-facing setup walkthrough. Pairs with `README.md`'s overview.

## Prerequisites

| Item | Where it comes from |
|---|---|
| **Proxmox API token** scoped to the `apollo` node | Proxmox UI → Datacenter → Permissions → API Tokens. Set as env var `PROXMOX_VE_API_TOKEN` (see `terraform-mattermost/README.md` § "Prerequisites" for the direnv + Keychain pattern). |
| **Apollo node reachable** at `10.0.1.4` (default) | Tailscale or LAN connection to the apollo subnet. |
| **DHCP reservation** for the chosen MAC + IP | Reserve `BC:24:11:10:05:50` → `10.0.5.50` (defaults) in your DHCP server BEFORE first apply. The VM otherwise won't get the static IP. |
| **DNS record** for `agent-hub.litts.link` → `10.0.5.50` | Add to your local DNS or `/etc/hosts` for operator convenience. Optional — IP-only access works. |
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
ssh dale@10.0.5.50
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

Expected: 4 services healthy (`agent-hub-postgres`, `agent-hub-gateway`, `agent-hub-outbox`, `agent-hub-inbox-webhook`).

Smoke test:

```bash
curl -fsSL http://10.0.5.50:8787/health
# Expected: {"status":"ok","postgres":"ok"}
```

## Step 4 — Configure the Mattermost outgoing webhook

In Mattermost → Integrations → Outgoing Webhooks → Add:

- **Content type:** `application/json`
- **Trigger words:** leave empty (we trigger on channel membership, not word-match)
- **Channel:** `agent-events` (create the channel first if needed)
- **Callback URL:** `http://10.0.5.50:8788/v1/inbox/webhook`
- **Token:** paste the value you set for `MATTERMOST_INBOX_WEBHOOK_SECRET` in Step 2

## Step 5 — From the operator Mac, run `/setup-agent-events`

In your consuming workspace (e.g., `~/projects/secureup/`), add to `.claude/concept-workflow.local.md` frontmatter:

```yaml
agent-events:
  gateway-url: http://10.0.5.50:8787
  vm-host: agent-hub
  per-vm-token-file: ~/.config/concept-workflow/agent-hub-token
  mattermost-outbox-channel: agent-events
  mattermost-inbox-webhook-secret: <same value as Step 2 / Step 4>
```

Then from a Claude Code session on your Mac, run `/setup-agent-events`. The skill will:

1. Validate gateway reachability.
2. Mint a token for the Mac itself, write it to `~/.config/concept-workflow/agent-hub-token` (chmod 600), register the Mac as `agent-operator-mac`.
3. SSH-dispatch to each agent VM (per the workspace's SSH map) to install `agentctl` + provision per-VM tokens + register the agent.
4. Run an end-to-end smoke test (emit a test event from each peer; verify it lands in Postgres).

## Step 6 — Verify

```bash
# On the Mac, after /setup-agent-events:
agentctl health
# Expected: gateway reachable, token valid, last_seen_at updates.

# Verify all 5 peers registered:
psql "postgres://agent_hub:<password>@10.0.5.50:54329/agent_hub" \
  -c "select name, role, host_kind, last_seen_at from agents order by name;"
```

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `terraform apply` hangs at `proxmox_virtual_environment_vm.agent_hub_vm: Still creating...` | VM is booting + cloud-init is running. Wait ~3 min. | `ssh dale@10.0.5.50 'cloud-init status --wait'` to watch progress. |
| Gateway returns `connection refused` after `docker compose up -d` | `.env` placeholder values still present | `cat /opt/agent-hub/.env` — make sure every `CHANGE_ME_FIRST_BOOT` is replaced. `docker compose down && docker compose up -d`. |
| Outbox-worker logs `mattermost: unauthorized` | `MATTERMOST_TOKEN` invalid | Verify with `curl -fsSL -H "Authorization: Bearer $MATTERMOST_TOKEN" $MATTERMOST_URL/api/v4/users/me`. |
| Inbox-webhook logs `invalid token` on incoming Mattermost posts | Webhook secret mismatch between Mattermost UI and `.env` | Re-paste the value in both places; they must match exactly. |
| `/setup-agent-events` fails at "VM agent-1 unreachable" | SSH map in `concept-workflow.local.md` is stale | Check `concept-workflow.local.md` `ssh-map` block; verify `ssh agent-1@claude-1 echo ok` works. |

## Maintenance

- **Backups:** Postgres data volume is at `agent_hub_pg` (Docker named volume). Snapshot via `pg_dump` weekly; archive to off-VM storage. Terraform doesn't manage backups; configure separately.
- **Updates to agent-hub itself:** `git pull && docker compose build && docker compose up -d`. Migrations idempotent; the gateway's `migrate` subcommand applies new SQL files when added.
- **Moving the agent-hub to a new VM:** see plugin's `/move-agent-hub` skill (ROADMAP `#10` Component A).
