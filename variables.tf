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
  description = "Root disk size in GB (40 GB; Postgres data + room for archive)"
  type        = number
  default     = 40

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
# Network — pick an IP outside the 10.0.5.40-43 range used by agent VMs.
# Default 10.0.5.50 (next free slot above the agent fleet).
# -----------------------------------------------------------------------------

variable "vm_bridge" {
  description = "Proxmox network bridge"
  type        = string
  default     = "vmbr0"
}

variable "vm_ip_address" {
  description = "Static IP in CIDR form (e.g., 10.0.5.50/16)"
  type        = string
  default     = "10.0.5.50/16"
}

variable "vm_gateway" {
  description = "Network gateway"
  type        = string
  default     = "10.0.0.200"
}

variable "vm_mac_address" {
  description = "Static MAC (register in DHCP separately before first apply)"
  type        = string
  default     = "BC:24:11:10:05:50"
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
