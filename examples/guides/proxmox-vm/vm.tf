# One Proxmox VE VM of the release: its disk imported from the release's image, uploaded to a
# datastore with Import content, and its NoCloud seed, uploaded to one with ISO content, on the
# CD-ROM. Both uploads go through the API, with the token alone (no SSH to the node). A preview: not
# run on a Proxmox VE host yet.

resource "proxmox_virtual_environment_file" "image" {
  node_name    = var.node
  datastore_id = var.import_datastore
  content_type = "import"

  source_file {
    path      = local.image
    file_name = "${var.name}-${substr(local.image_sha256, 0, 12)}.${local.image_format}"
    checksum  = local.image_sha256
  }
}

resource "proxmox_virtual_environment_file" "seed" {
  node_name    = var.node
  datastore_id = var.iso_datastore
  content_type = "iso"

  source_file {
    path      = local.seed_iso
    file_name = "${var.name}-seed-${substr(local.seed_sha256, 0, 12)}.iso"
    checksum  = local.seed_sha256
  }
}

resource "proxmox_virtual_environment_vm" "vm" {
  node_name   = var.node
  name        = var.name
  description = var.description
  bios        = "ovmf"
  machine     = "q35"
  on_boot     = true
  boot_order  = ["virtio0"]

  cpu {
    cores = local.cpus
    type  = "host"
  }

  memory {
    dedicated = local.memory_mib
  }

  efi_disk {
    datastore_id = var.datastore
    type         = "4m"
  }

  disk {
    datastore_id = var.datastore
    import_from  = proxmox_virtual_environment_file.image.id
    interface    = "virtio0"
    size         = local.disk_gib
  }

  cdrom {
    file_id   = proxmox_virtual_environment_file.seed.id
    interface = "ide2"
  }

  # Virtio with a queue per vCPU (Proxmox's limit is 64), each on its own CPU in the guest: the
  # release's NICs, the primary first, each on its bridge (default var.bridge) with its MAC
  # (default Proxmox's); or one.
  dynamic "network_device" {
    for_each = local.nics
    content {
      bridge      = network_device.value.attach != "" ? network_device.value.attach : var.bridge
      model       = "virtio"
      queues      = min(local.cpus, 64)
      mac_address = network_device.value.mac != "" ? network_device.value.mac : null
    }
  }

  operating_system {
    type = "l26"
  }
}

locals {
  # No guest agent reports the address: vm_address gives it (the DHCP server's name for the VM), else the static one.
  address = coalesce(var.vm_address, try(split("/", var.address)[0], null))
}
