#cloud-config
# -----------------------------------------------------------------------------
# agent-hub cloud-init: Docker + docker-compose + agent-hub stack on first boot
# -----------------------------------------------------------------------------
# Templated by Terraform; literal $${VAR} survives templatefile() expansion.

hostname: ${hostname}
manage_etc_hosts: true

users:
  - name: ${username}
    sudo: ALL=(ALL) NOPASSWD:ALL
    groups: [sudo, docker]
    shell: /bin/bash
    lock_passwd: true
    ssh_authorized_keys:
      - ${ssh_key}

ssh_pwauth: false
disable_root: true

package_update: true
package_upgrade: true
packages:
  - ca-certificates
  - curl
  - git
  - jq
  - postgresql-client-16
  - unattended-upgrades
  - ufw

write_files:
  # docker-compose.yml is templated in at terraform-apply time so the VM has
  # a copy at /opt/agent-hub/docker-compose.yml without needing to clone the repo.
  - path: /opt/agent-hub/docker-compose.yml
    owner: root:root
    permissions: '0644'
    content: |
${indent(6, docker_compose_content)}
  # .env stub — operator must populate post-boot before bringing the stack up.
  - path: /opt/agent-hub/.env
    owner: root:root
    permissions: '0600'
    content: |
      POSTGRES_DB=agent_hub
      POSTGRES_USER=agent_hub
      POSTGRES_PASSWORD=CHANGE_ME_FIRST_BOOT
      ADMIN_TOKEN=CHANGE_ME_FIRST_BOOT
      MATTERMOST_URL=https://mattermost.litts.link
      MATTERMOST_TOKEN=CHANGE_ME_FIRST_BOOT
      MATTERMOST_INBOX_WEBHOOK_SECRET=CHANGE_ME_FIRST_BOOT
  # Docker daemon: json-file log rotation so container logs don't grow unbounded.
  # Written BEFORE runcmd starts docker, so the daemon picks it up on first start.
  - path: /etc/docker/daemon.json
    owner: root:root
    permissions: '0644'
    content: |
      {
        "log-driver": "json-file",
        "log-opts": {
          "max-size": "100m",
          "max-file": "5"
        }
      }
  # Nightly events-table archive: exports rows older than 90 days to gzipped
  # JSONL under /var/lib/agent-hub/archives/, then deletes + vacuums.
  # Replaceable by `agentctl archive-events --older-than 90d` when that
  # subcommand ships in v0.1.x.
  - path: /usr/local/bin/agent-hub-archive-events.sh
    owner: root:root
    permissions: '0755'
    content: |
      #!/usr/bin/env bash
      # agent-hub-archive-events: nightly job that exports events older than 90 days
      # to compressed JSONL archives, then deletes the archived rows from Postgres.
      #
      # Idempotent: re-runs append to the same month-bucketed archive file.
      # Best-effort: errors logged but exit 0 so cron's mail-on-failure stays quiet.
      # Replaceable: when agent-hub agentctl ships `archive-events`, swap this for
      # `agentctl archive-events --older-than 90d` in the crontab.
      set -uo pipefail

      ARCHIVE_DIR="$${AGENT_HUB_ARCHIVE_DIR:-/var/lib/agent-hub/archives}"
      RETENTION_DAYS="$${AGENT_HUB_RETENTION_DAYS:-90}"
      LOG_FILE="$${AGENT_HUB_ARCHIVE_LOG:-/var/log/agent-hub-archive.log}"

      mkdir -p "$ARCHIVE_DIR"
      exec >>"$LOG_FILE" 2>&1
      echo "=== $(date -Iseconds) archive run start (retention=$${RETENTION_DAYS}d) ==="

      # Source POSTGRES_* from /opt/agent-hub/.env so we don't duplicate creds.
      if [ ! -r /opt/agent-hub/.env ]; then
        echo "ERROR: /opt/agent-hub/.env not readable; aborting"
        exit 0
      fi
      set -a
      . /opt/agent-hub/.env
      set +a

      DSN="postgres://$${POSTGRES_USER}:$${POSTGRES_PASSWORD}@127.0.0.1:54329/$${POSTGRES_DB}?sslmode=disable"
      BUCKET="$(date -u +%Y-%m)"
      OUT="$${ARCHIVE_DIR}/events-$${BUCKET}.jsonl.gz"

      # Export + delete in one transaction so we never lose data on partial failure.
      psql "$DSN" <<SQL | gzip >> "$OUT"
      \copy (
        SELECT row_to_json(e) FROM events e
         WHERE created_at < now() - interval '$${RETENTION_DAYS} days'
         ORDER BY created_at
      ) TO STDOUT
      SQL
      COPY_RC=$${PIPESTATUS[0]}

      if [ "$COPY_RC" -ne 0 ]; then
        echo "ERROR: \copy failed (rc=$COPY_RC); skipping delete to preserve rows"
        exit 0
      fi

      # Only delete what was archived — same WHERE clause, same transaction window.
      psql "$DSN" -c "DELETE FROM events WHERE created_at < now() - interval '$${RETENTION_DAYS} days';"

      # Vacuum the now-pruned region so disk space actually reclaims.
      psql "$DSN" -c "VACUUM (ANALYZE) events;"

      echo "=== $(date -Iseconds) archive run end (archive=$${OUT}) ==="
  # Crontab fragment: /etc/cron.d/* is picked up by cron automatically.
  - path: /etc/cron.d/agent-hub-archive
    owner: root:root
    permissions: '0644'
    content: |
      # /etc/cron.d/agent-hub-archive: nightly events-table archive + prune
      SHELL=/bin/bash
      PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
      # Runs at 03:17 UTC daily — off-peak; psql timing is not load-sensitive at this volume
      17 3 * * * root /usr/local/bin/agent-hub-archive-events.sh

runcmd:
  # Install Docker via Docker's official convenience script (matches terraform-mattermost)
  - curl -fsSL https://get.docker.com -o /tmp/get-docker.sh
  - sh /tmp/get-docker.sh
  - usermod -aG docker ${username}
  - systemctl enable --now docker
  # Ensure daemon.json (written above) is picked up even if docker was already running.
  - systemctl restart docker || true

  # Firewall: SSH from anywhere; gateway + inbox-webhook ports from 10.0.0.0/16
  - ufw --force enable
  - ufw allow OpenSSH
  - ufw allow from 10.0.0.0/16 to any port 8787 proto tcp
  - ufw allow from 10.0.0.0/16 to any port 8788 proto tcp

  # NOTE: docker-compose is NOT brought up automatically — the .env file has
  # placeholder values that must be replaced before first start. The post-
  # provision checklist in `terraform output post_provision_checklist` walks
  # the operator through this.
  - echo "agent-hub VM provisioned. Populate /opt/agent-hub/.env then 'docker compose up -d'." > /etc/motd

final_message: |
  agent-hub VM ready. Cloud-init took $UPTIME seconds.
  Next: SSH in, populate /opt/agent-hub/.env, then 'cd /opt/agent-hub && docker compose up -d'.
