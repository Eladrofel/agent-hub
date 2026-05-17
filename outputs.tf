output "vm_id" {
  description = "Proxmox VM ID"
  value       = proxmox_virtual_environment_vm.agent_hub_vm.vm_id
}

output "vm_name" {
  description = "VM hostname"
  value       = proxmox_virtual_environment_vm.agent_hub_vm.name
}

output "vm_ip_address" {
  description = "Expected IP (from DHCP reservation mapped to vm_mac_address). Verify post-boot via `qm guest cmd <vmid> network-get-interfaces` on the Proxmox host or `ssh dale@<expected-ip> hostname`."
  value       = var.vm_ip_address
}

output "gateway_url" {
  description = "Use this in <consuming-workspace>/.claude/concept-workflow.local.md under agent-events.gateway-url"
  value       = "http://${split("/", var.vm_ip_address)[0]}:8787"
}

output "ssh_command" {
  description = "SSH connection command"
  value       = "ssh ${var.vm_user}@${split("/", var.vm_ip_address)[0]}"
}

output "post_provision_checklist" {
  description = "Manual steps to run after terraform apply succeeds"
  value = <<-EOT

    ────────────────────────────────────────────────────────────────────────────
    agent-hub VM provisioned. Post-provision steps:
    ────────────────────────────────────────────────────────────────────────────

    1. Wait ~3 minutes for cloud-init (Docker install + first compose up).
       Check: ssh ${var.vm_user}@${split("/", var.vm_ip_address)[0]} 'cloud-init status --wait'

    2. Verify the stack:
       ssh ${var.vm_user}@${split("/", var.vm_ip_address)[0]} 'sudo docker ps'
       Expected (v0.1.1): agent-hub-postgres + agent-hub-gateway (both 'healthy').
       agent-hub-outbox + agent-hub-inbox-webhook are behind the 'v0.1.1' compose
       profile and won't start by default — they ship in agent-hub v0.1.2
       (Component C). To bring them up later: `docker compose --profile v0.1.1 up -d`.

    3. Verify Postgres schema (applied by the gateway's embedded migrate runner
       on boot — see internal/store/migrate.go):
       ssh ${var.vm_user}@${split("/", var.vm_ip_address)[0]} \\
         'sudo docker exec -i agent-hub-postgres psql -U agent_hub agent_hub \\
          -c "\\dt"'
       Expected: 11 tables (projects, agents, agent_sessions, tasks, events,
                 session_checkpoints, handoffs, decisions, agent_locks,
                 artifacts, mattermost_outbox, mattermost_inbox) plus
                 schema_migrations tracking table = 12 rows.

    4. From operator Mac, point /setup-agent-events at this VM:
       Edit <consuming-workspace>/.claude/concept-workflow.local.md
       Set agent-events.gateway-url = http://${split("/", var.vm_ip_address)[0]}:8787

    5. Run /setup-agent-events from the operator's Claude Code session.
       It will register the Mac as agent-operator-mac, then SSH-dispatch
       agentctl install + tokens to each agent VM.

    See README.md § "Post-provision steps" for full detail.
    ────────────────────────────────────────────────────────────────────────────
  EOT
}
