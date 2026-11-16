# One Scaleway instance of the release, booted from the release's registered image (cloud_image), or image_id, with the
# cloud-config as its cloud-init user data, a routed public IPv4 address (ip_id's, else its own)
# and, unless one is given, a security group dropping what SSH from its sources and the opened
# ports leave.

# The VM's own firewall narrows these again: SSH from its sources, the front's ports from every
# source, an L4 route's from the sources its allowlist held at create (none when it was empty: no
# rule then); replies and outbound traffic pass.
resource "scaleway_instance_security_group" "vm" {
  count                   = var.security_group_id == "" ? 1 : 0
  name                    = var.name
  description             = var.description
  stateful                = true
  inbound_default_policy  = "drop"
  outbound_default_policy = "accept"

  dynamic "inbound_rule" {
    for_each = local.seed.firewall.ssh_sources
    content {
      action   = "accept"
      protocol = "TCP"
      port     = 22
      ip_range = inbound_rule.value
    }
  }

  dynamic "inbound_rule" {
    for_each = flatten([for p in local.seed.firewall.open_ports : [for s in p.sources : { protocol = p.protocol, port = p.port, source = s }]])
    content {
      action   = "accept"
      protocol = upper(inbound_rule.value.protocol)
      port     = inbound_rule.value.port
      ip_range = inbound_rule.value.source
    }
  }
}

# The instance's public address: the one ip_id names (reserved beforehand, kept by a destroy),
# else one made for it.
resource "scaleway_instance_ip" "vm" {
  count = var.ip_id == "" ? 1 : 0
  type  = "routed_ipv4"
}


data "scaleway_instance_ip" "given" {
  count = var.ip_id == "" ? 0 : 1
  id    = var.ip_id
}

locals {
  ip_id   = var.ip_id == "" ? scaleway_instance_ip.vm[0].id : data.scaleway_instance_ip.given[0].id
  public_address = var.ip_id == "" ? scaleway_instance_ip.vm[0].address : data.scaleway_instance_ip.given[0].address
}

resource "scaleway_instance_server" "vm" {
  name              = var.name
  type              = var.instance_type
  image             = local.image_id
  ip_id             = local.ip_id
  security_group_id = var.security_group_id == "" ? scaleway_instance_security_group.vm[0].id : var.security_group_id

  # Scaleway sizes volumes in GB (10^9 bytes), disk_gib is GiB: rounded up.
  root_volume {
    volume_type = "sbs_volume"
    size_in_gb  = ceil(local.disk_gib * 1.073741824)
  }

  user_data = {
    cloud-init = local.seed.user_data
  }

  lifecycle {
    # A later release reaches the instance as an upgrade, never as a replacement.
    ignore_changes = [image, user_data]
  }
}

# The image `ship --disks` registered for this release's disk (cloud_image), else image_id.
data "scaleway_instance_image" "release" {
  count = var.image_id == "" ? 1 : 0
  name  = local.seed.disk["scaleway"].cloud_image
  zone  = var.zone
}

locals {
  image_id = var.image_id != "" ? var.image_id : data.scaleway_instance_image.release[0].id
}

locals {
  # The address SSH reaches the VM at: vm_address, else its public one.
  address = coalesce(var.vm_address, local.public_address)
}
