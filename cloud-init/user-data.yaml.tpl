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

runcmd:
  # Install Docker via Docker's official convenience script (matches terraform-mattermost)
  - curl -fsSL https://get.docker.com -o /tmp/get-docker.sh
  - sh /tmp/get-docker.sh
  - usermod -aG docker ${username}
  - systemctl enable --now docker

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
