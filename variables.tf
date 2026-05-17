# -----------------------------------------------------------------------------
# Proxmox connection (mirrors terraform-mattermost defaults)
# -----------------------------------------------------------------------------

variable "proxmox_host" {
  description = "Proxmox server IP address or hostname"
  type        = string
  default     = "10.0.1.4"
}

variable "proxmox_node" {
  description = "Proxmox node name"
  type        = string
  default     = "apollo"
}

# -----------------------------------------------------------------------------
# VM configuration — agent-hub is smaller than mattermost (no MCP plugin
# overhead, no nginx). Postgres + 3 Go binaries fit comfortably in 2 vCPU /
# 4 GB RAM / 40 GB disk.
# -----------------------------------------------------------------------------

variable "vm_name" {
  description = "VM hostname (single VM — agent-hub is not horizontally scaled in v0.1.x)"
  type        = string
  default     = "agent-hub"
}

variable "vm_cpu_cores" {
  description = "Number of CPU cores"
  type        = number
  default     = 2
}

variable "vm_memory" {
  description = "RAM in MB (4 GB; PG16 + Go binaries fit comfortably)"
  type        = number
  default     = 4096
}

variable "vm_disk_size" {
  description = "Root disk size in GB (60 GB; provides ~10x headroom over steady-state ~6.6 GB for PG16 + Ubuntu + Docker images + logs + JSONL archives, per CHANGELOG sizing analysis)"
  type        = number
  default     = 60

  validation {
    condition     = var.vm_disk_size >= 20
    error_message = "Disk size must be at least 20 GB."
  }
}

variable "vm_storage" {
  description = "Proxmox storage pool for VM disks"
  type        = string
  default     = "local-lvm"
}

variable "vm_pool" {
  description = "Proxmox resource pool"
  type        = string
  default     = "terraform-managed"
}

variable "cloud_image_storage" {
  description = "Datastore for cloud-init snippets (shared with terraform-mattermost / terraform-apollo)"
  type        = string
  default     = "local"
}

# -----------------------------------------------------------------------------
# Network — IP via DHCP reservation (mapped to vm_mac_address). Default
# vm_ip_address 10.0.5.38/16 (within the agent-fleet range; reserve this
# MAC → IP mapping in your DHCP server BEFORE first apply).
# -----------------------------------------------------------------------------

variable "vm_bridge" {
  description = "Proxmox network bridge"
  type        = string
  default     = "vmbr0"
}

variable "vm_ip_address" {
  description = "IP the DHCP server will assign to vm_mac_address (e.g., 10.0.5.38/16). Display-only — the VM itself gets its IP via DHCP; this variable is used only by outputs for operator-facing strings."
  type        = string
  default     = "10.0.5.38/16"
}

variable "vm_mac_address" {
  description = "Static MAC for DHCP reservation. Reserve this MAC → vm_ip_address mapping on the DHCP server BEFORE first apply so the VM gets a predictable IP on boot. Last byte traditionally encodes the IP's last byte for memorability."
  type        = string
  default     = "BC:24:11:10:05:38"
}

variable "vm_dns_servers" {
  description = "DNS servers"
  type        = list(string)
  default     = ["10.0.0.200", "1.1.1.1"]
}

# -----------------------------------------------------------------------------
# SSH access
# -----------------------------------------------------------------------------

variable "vm_user" {
  description = "SSH user (passwordless sudo enabled via cloud-init)"
  type        = string
  default     = "dale"
}

variable "ssh_public_key_path" {
  description = "Path to SSH public key for the vm_user"
  type        = string
  default     = "~/.ssh/dale.pem.pub"
}
