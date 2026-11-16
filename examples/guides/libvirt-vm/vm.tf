# One libvirt / KVM domain of the release: its disk a copy-on-write qcow2 volume over the release's
# image, grown to disk_gib, and its NoCloud seed a second, read-only volume (the OS reads the cidata
# label from any disk). The firmware is libvirt's UEFI autoselection, Secure Boot off, with ACPI
# and the APIC on. A preview: not run end to end on a libvirt host yet.

resource "libvirt_volume" "image" {
  name = "${var.name}-image-${substr(local.image_sha256, 0, 12)}.${local.image_format}"
  pool = var.pool
  target = {
    format = { type = local.image_format }
  }
  create = {
    content = { url = local.image }
  }
}

resource "libvirt_volume" "disk" {
  name     = "${var.name}.qcow2"
  pool     = var.pool
  capacity = local.disk_gib * 1024 * 1024 * 1024
  target = {
    format = { type = "qcow2" }
  }
  backing_store = {
    path   = libvirt_volume.image.path
    format = { type = local.image_format }
  }
}

# The seed's format as libvirt probes an ISO 9660 image: iso (another would drift at every plan).
resource "libvirt_volume" "seed" {
  name = "${var.name}-seed-${substr(local.seed_sha256, 0, 12)}.iso"
  pool = var.pool
  target = {
    format = { type = "iso" }
  }
  create = {
    content = { url = local.seed_iso }
  }
}

resource "libvirt_domain" "vm" {
  name        = var.name
  description = var.description
  type        = var.domain_type
  vcpu        = local.cpus
  memory      = local.memory_mib
  memory_unit = "MiB"
  autostart   = true
  running     = true

  # ACPI and the APIC stated: the provider adds no default feature, and libvirt refuses a UEFI
  # domain without ACPI on x86_64.
  features = {
    acpi = true
    apic = {}
  }

  os = {
    type         = "hvm"
    type_machine = "q35"
    firmware     = "efi"
    # In libvirt's order, by name: the provider compares its read-back list by position, and an
    # apply whose features differ in order alone fails as an inconsistent result.
    firmware_info = {
      features = [
        { name = "enrolled-keys", enabled = "no" },
        { name = "secure-boot", enabled = "no" },
      ]
    }
    boot_devices = [{ dev = "hd" }]
  }

  # The host's CPU as it is under KVM; QEMU's emulation offers its most.
  cpu = {
    mode = var.domain_type == "kvm" ? "host-passthrough" : "maximum"
  }

  devices = {
    # Each disk by its volume's path (type='file'): the AppArmor driver's virt-aa-helper writes a
    # file disk and its qcow2 backing chain into the domain's profile, never a pool volume
    # (type='volume'), which confined QEMU may then not open.
    disks = [
      {
        source = {
          file = { file = libvirt_volume.disk.path }
        }
        target = { dev = "vda", bus = "virtio" }
        driver = { name = "qemu", type = "qcow2" }
      },
      {
        source = {
          file = { file = libvirt_volume.seed.path }
        }
        target    = { dev = "vdb", bus = "virtio" }
        driver    = { name = "qemu", type = "raw" }
        read_only = true
      },
    ]
    # The release's NICs, the primary first, each on its network (default var.network) with its
    # MAC (libvirt's own for a release that declares no NIC).
    interfaces = [for i, n in local.nics : {
      model = { type = "virtio" }
      # A queue per vCPU, each on its own CPU in the guest, and the largest receive ring virtio
      # offers: the guest's tuning grows its ring to it. One vCPU sets no queues: one is libvirt's
      # default, which it leaves out of the domain, and the provider fails an apply that reads back
      # a value it set as null.
      driver = { queues = local.cpus > 1 ? local.cpus : null, rx_queue_size = 1024 }
      mac    = n.mac != "" ? { address = n.mac } : null
      source = {
        network = { network = n.attach != "" ? n.attach : var.network }
      }
      # With DHCP the apply waits for the network's lease of an IPv4 address, on the primary.
      wait_for_ip = (var.addressing == "dhcp") && i == 0 ? { timeout = 300, source = "lease", network = "0.0.0.0/0" } : null
    }]
  }
}

# The network's DHCP leases of the domain, read once it has one (with DHCP only).
data "libvirt_domain_interface_addresses" "vm" {
  count  = (var.addressing == "dhcp") ? 1 : 0
  domain = libvirt_domain.vm.uuid
  source = "lease"
}

locals {
  # The network's DHCP lease, else vm_address, else the static address.
  address = coalesce(var.vm_address, try(split("/", var.address)[0], null), try([for a in flatten(data.libvirt_domain_interface_addresses.vm[0].interfaces[*].addrs) : a.addr if a.type == "ipv4"][0], null))
}
