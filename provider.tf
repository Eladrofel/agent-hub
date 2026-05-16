provider "proxmox" {
  # Authentication via environment variables (NEVER hardcode):
  #   PROXMOX_VE_ENDPOINT     = "https://apollo.litts.link:8006"
  #   PROXMOX_VE_API_TOKEN    = "terraform@pve!provider=<token-value>"
  #   PROXMOX_VE_SSH_USERNAME = "root"
  insecure = true # Self-signed certs common in homelab

  ssh {
    agent = true
    node {
      name    = var.proxmox_node
      address = var.proxmox_host
    }
  }
}
