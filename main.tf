# -----------------------------------------------------------------------------
# Ubuntu 24.04 cloud image — shared resource on the apollo Proxmox node;
# managed by terraform-apollo. Reference by file ID; do not redeclare.
# -----------------------------------------------------------------------------
locals {
  ubuntu_image_file_id = "local:iso/ubuntu-24.04-server-cloudimg-amd64.img"
}

# -----------------------------------------------------------------------------
# Cloud-init user-data — bootstraps Docker + clones the agent-hub repo +
# brings up the docker-compose stack with sourced .env values.
# -----------------------------------------------------------------------------
resource "proxmox_virtual_environment_file" "cloud_init_user_data" {
  content_type = "snippets"
  datastore_id = var.cloud_image_storage
  node_name    = var.proxmox_node

  source_raw {
    data = templatefile("${path.module}/cloud-init/user-data.yaml.tpl", {
      hostname              = var.vm_name
      username              = var.vm_user
      ssh_key               = trimspace(file(pathexpand(var.ssh_public_key_path)))
      docker_compose_content = file("${path.module}/docker-compose.yml")
    })
    file_name = "cloud-init-${var.vm_name}-user-data.yaml"
  }
}

# -----------------------------------------------------------------------------
# agent-hub VM — Ubuntu 24.04 + Docker + docker-compose stack
# (postgres + gateway + outbox-worker + inbox-webhook).
# -----------------------------------------------------------------------------
resource "proxmox_virtual_environment_vm" "agent_hub_vm" {
  name       = var.vm_name
  node_name  = var.proxmox_node
  pool_id    = var.vm_pool
  protection = true

  description = <<-EOT
    Managed by Terraform (agent-hub).
    Postgres-backed agent-events ledger + Mattermost outbox/inbox.
    Serves the concept-workflow plugin's peer-agent fleet (4 VM agents +
    operator-Mac).
    SSH user: ${var.vm_user} (key-pair: ${var.ssh_public_key_path})
    Password auth is DISABLED — SSH key-only.
    Do not modify manually — changes will be overwritten by Terraform.
    See README.md for post-provision steps.
  EOT

  lifecycle {
    prevent_destroy = true
    ignore_changes = [
      disk[0].file_id, # bpg/proxmox quirk; see terraform-mattermost notes
      pool_id,         # bpg/proxmox quirk; see terraform-mattermost notes
    ]
  }

  agent {
    enabled = true
  }

  cpu {
    cores   = var.vm_cpu_cores
    sockets = 1
    type    = "host"
  }

  memory {
    dedicated = var.vm_memory
  }

  disk {
    datastore_id = var.vm_storage
    file_id      = local.ubuntu_image_file_id
    interface    = "scsi0"
    size         = var.vm_disk_size
  }

  network_device {
    bridge      = var.vm_bridge
    mac_address = var.vm_mac_address
  }

  initialization {
    datastore_id = var.vm_storage

    ip_config {
      ipv4 {
        address = var.vm_ip_address
        gateway = var.vm_gateway
      }
    }

    dns {
      servers = var.vm_dns_servers
    }

    user_data_file_id = proxmox_virtual_environment_file.cloud_init_user_data.id
  }
}
