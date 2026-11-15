---
page_title: "A Kairos VM on TrueNAS with the TrueNAS provider - stowage"
subcategory: "Guides"
description: |-
  A Kairos VM made on TrueNAS SCALE by the upstream TrueNAS provider and a small SSH step script, from stowage_vm_seed, then installed by stowage_install.
---

# A Kairos VM on TrueNAS with the TrueNAS provider

`stowage_vm_seed` gives a new VM's first-boot material; the VM itself is made by your hypervisor's
own resources. This guide makes it on TrueNAS SCALE 25.10 with the community TrueNAS provider
`pjsalty/truenas` 3.4.0 (over TrueNAS's JSON-RPC API; its VMs are `truenas_vm`), and no module of
Stowage's: the root below is the whole of it, in `examples/guides/truenas-vm/` of the provider's
repository. Pin the TrueNAS provider exactly, and check a new release's schema (`tofu providers
schema -json`) before moving to it: `tofu validate` does not check the keys of a device's
`attributes` map, which the provider passes to TrueNAS as they are.

## What the TrueNAS provider does, and what the step script does

| | By |
|---|---|
| The staging dataset, the VM's zvol | `truenas_dataset`, `truenas_zvol` |
| The VM, its disk, NICs and seed CD-ROM | `truenas_vm`, `truenas_vm_device` |
| The disk image's upload and conversion into the zvol (`vm.device.convert`) | `truenas-step.sh import-image` |
| The NoCloud seed ISO (volume `cidata`: `user-data`, `meta-data`) and its upload | `truenas-step.sh seed` |
| Starting the VM, and stopping it before its devices are deleted | `truenas-step.sh start`, `stop` |
| The snapshots a periodic task took, which block a dataset's delete | `truenas-step.sh drop-snapshots` |

The provider has no resource for the last four: the script runs them from `local-exec`
provisioners over SSH to TrueNAS's shell, whose `midclt` calls the same middleware the provider
does. It needs `ssh`, `jq`, `sha256sum` and `xorriso` (or `genisoimage`) on the machine running
OpenTofu, TrueNAS's SSH host key already in `~/.ssh/known_hosts` (`StrictHostKeyChecking=yes`), and
an SSH user whose shell runs `midclt` as an administrator. The provider reads its API key from
`TRUENAS_API_KEY`: no credential of TrueNAS's is an argument or in state.

## How the pieces join

1. `stowage_vm_seed` (`platform = "truenas"`, `disk_dir`) writes the release's disk as a file
   (`disk_file`: its path, SHA-256 and the VM's disk size `disk_gib`), and gives `user_data`,
   `meta_data`, the one-time host key (`known_hosts_line`) and the NICs (`nics`).
2. The zvol is `disk_file.disk_gib` large; `import-image` uploads the disk into the staging
   dataset (checked against its SHA-256 on both ends), converts it into the zvol, and removes the
   upload. It runs once, for a new zvol, and refuses a zvol that already holds data (more than
   64 MiB referenced): the VM upgrades itself on its own disk. A conversion that failed part-way
   leaves such a zvol: replace it (`tofu apply -replace=truenas_zvol.disk`) to try again.
3. `seed` builds the NoCloud ISO in a private directory and uploads it, named by the seed's `id`.
   `user_data` carries the one-time host key's private key: it goes in the step's environment,
   never an argument. Uploads are mode 0600, as Stowage's own TrueNAS steps make them (libvirt
   hands a VM's files to its process as it starts it); the key retires once the VM's install is
   done and it rebooted.
4. One NIC device per role of `var.nic_roles` (the release's `host.interfaces` NICs, the primary
   first; empty: one NIC), keyed by position so the plan knows them before the seed exists. Each
   takes the MAC the seed's cloud-config names its role by (`nics[role].mac`; null for a VM of one
   NIC, which TrueNAS then gives one) and its bridge (`nics[role].attach`, else the primary's,
   else `var.bridge`). A NIC of another MAC is not named, and the VM's network does not come up as
   the release expects: the VM's precondition refuses a seed naming more than one NIC when
   `nic_roles` does not list them, the primary first.
5. `terraform_data.power` starts the VM. TrueNAS applies a changed definition at the VM's next
   start only, so a change of the VM or of a device replaces it: the VM is powered off, then on.
6. `stowage_install`'s `vm` block reaches the VM at its static address under the seed's one-time
   key (`known_hosts_line`), waits for its first boot, and from then on keeps it: `vm.version`
   (`var.release_version`) upgrades it (see [the VM target](vm-target.md)).

The seed is the first boot's alone (`ignore_changes = all`): a new seed would draw new MACs, make
a new seed ISO and power the VM off and on. A later `installer` or setting is ignored; the VM
upgrades itself. `tofu apply -replace=stowage_vm_seed.notes` makes a new one for a VM made again
(its zvol replaced too); the earlier seed ISO stays in the staging dataset until it is deleted.

A static address: TrueNAS does not report a VM's address. A destroy, once `stowage_install`'s
`deletion_protection` is off, forgets the install, powers the VM off, deletes its devices, the
VM, the snapshots, the zvol and the staging dataset.

## The root

`tests/plan.tftest.hcl` beside it plans the root under a mocked TrueNAS provider (`tofu test`, no
TrueNAS reached): one NIC, NICs by role with their MACs and bridges, the one NIC of any role
taken as the primary, and the precondition refusing a missing `nic_roles`.

```terraform
# A Kairos VM on TrueNAS SCALE made with the upstream TrueNAS provider alone, then installed and
# kept by stowage_install. The TrueNAS provider declares the datasets, the zvol, the VM and its
# devices; truenas-step.sh, run by local-exec over SSH to TrueNAS's shell (`midclt`), does what it
# has no resource for: the disk image's upload and conversion into the zvol, the NoCloud seed ISO,
# the power, and the snapshots that block a delete. No credential of TrueNAS's API enters a
# command line or the state: the provider reads TRUENAS_API_KEY from the environment.
terraform {
  required_version = ">= 1.11.0"
  required_providers {
    truenas = {
      source  = "pjsalty/truenas"
      version = "3.4.0"
    }
    stowage = {
      source  = "norvexlabs/stowage"
      version = "~> 0.2"
    }
  }
}

provider "truenas" {
  url = var.truenas_url # https://<truenas>; the API key from TRUENAS_API_KEY
}

provider "stowage" {}

locals {
  name    = "notes"
  host    = split("/", var.address)[0]
  step    = "${path.module}/truenas-step.sh"
  staging = "${var.pool}/${var.parent_dataset}/${local.name}-staging"
  ssh_env = {
    TNAS_SSH_HOST = var.truenas_ssh_host
    TNAS_SSH_USER = var.truenas_ssh_user
    TNAS_SSH_PORT = "22"
    TNAS_SSH_KEY  = pathexpand(var.truenas_ssh_key_file)
  }
  # The seed ISO's path on TrueNAS, named by the seed: a new seed is a new file.
  seed_path = "/mnt/${local.staging}/seed-${substr(sha256(stowage_vm_seed.notes.id), 0, 16)}.iso"
  # The VM's NICs, by var.nic_roles (known at plan; one NIC when it is empty), each with the MAC
  # the seed's cloud-config names its role by (null for a VM of one NIC: TrueNAS gives it one) and
  # its bridge (the seed's attach=, else the primary's, else var.bridge).
  # With nic_roles empty, the one NIC is the seed's primary, whatever its role.
  seed_nic = { for i, role in(length(var.nic_roles) > 0 ? var.nic_roles : ["-"]) : tostring(i) => (
    length(var.nic_roles) > 0 ? try(stowage_vm_seed.notes.nics[role], null) : try(one([for n in values(stowage_vm_seed.notes.nics) : n if n.primary]), null)
  ) }
  primary_attach = coalesce(try(local.seed_nic["0"].attach, null), var.bridge)
  nics = { for k, n in local.seed_nic : k => {
    mac    = try(n.mac, null)
    attach = coalesce(try(n.attach, null), local.primary_attach)
  } }
}

resource "stowage_vm_seed" "notes" {
  installer      = var.installer
  platform       = "truenas"
  hostname       = local.name
  user           = "ops"
  ssh_public_key = file(pathexpand("~/.ssh/id_ed25519.pub"))
  # A static address: TrueNAS does not report the VM's.
  network = "static"
  address = var.address
  gateway = var.gateway
  dns     = [var.gateway]
  tls     = "internal"
  inputs  = { ADMIN_EMAIL = var.admin_email }
  # The disk written here as a file (disk_file), for truenas-step.sh to upload.
  disk_dir = pathexpand("~/.cache/notes/disks")

  # The first boot's: a later installer or setting would make a new seed (new MACs, a new seed ISO,
  # the VM powered off and on). The VM upgrades by stowage_install's vm.version (var.release_version).
  lifecycle {
    ignore_changes = all
  }
}

# Holds the uploads (the disk image on its way into the zvol, the seed ISO).
resource "truenas_dataset" "staging" {
  pool           = var.pool
  parent_dataset = var.parent_dataset
  name           = "${local.name}-staging"
}

# The VM's disk, the size the seed asks for (the app's space and what Kairos takes).
resource "truenas_zvol" "disk" {
  pool         = var.pool
  name         = "${var.parent_dataset}/${local.name}"
  volsize      = stowage_vm_seed.notes.disk_file.disk_gib * 1024 * 1024 * 1024
  volblocksize = "16K"
  compression  = "LZ4"
}

# Destroyed before the zvol and the staging dataset: drops their snapshots first (a periodic
# snapshot task may have taken some; TrueNAS refuses to delete a dataset that has any).
resource "terraform_data" "snapshots" {
  input = merge(local.ssh_env, { DATASETS = join(" ", [truenas_zvol.disk.id, truenas_dataset.staging.id]) })

  provisioner "local-exec" {
    when        = destroy
    command     = "\"$STEP\" drop-snapshots"
    environment = merge(self.input, { STEP = "${path.module}/truenas-step.sh" })
  }
}

# The disk image written into the new zvol once: the VM upgrades itself on its own disk, so a later
# seed's image is ignored, and the step refuses a zvol that already holds data.
resource "terraform_data" "image" {
  triggers_replace = [truenas_zvol.disk.id]
  input            = { path = stowage_vm_seed.notes.disk_file.path, sha256 = stowage_vm_seed.notes.disk_file.sha256 }

  lifecycle {
    ignore_changes = [input]
  }

  provisioner "local-exec" {
    command = "\"${local.step}\" import-image"
    environment = merge(local.ssh_env, {
      IMAGE        = self.input.path
      IMAGE_SHA256 = self.input.sha256
      STAGING_DIR  = "/mnt/${truenas_dataset.staging.id}"
      ZVOL         = truenas_zvol.disk.id
    })
  }
}

# The NoCloud seed ISO (volume cidata) of the seed's user-data and meta-data. user_data holds the
# one-time host key's private key: it goes in the step's environment, never an argument.
resource "terraform_data" "seed" {
  triggers_replace = [truenas_dataset.staging.id, stowage_vm_seed.notes.id]
  input            = local.seed_path

  provisioner "local-exec" {
    command = "\"${local.step}\" seed"
    environment = merge(local.ssh_env, {
      SEED_USER_DATA = stowage_vm_seed.notes.user_data
      SEED_META_DATA = stowage_vm_seed.notes.meta_data
      WORK_DIR       = pathexpand("~/.cache/notes/seeds")
      DEST           = self.input
    })
  }
}

resource "truenas_vm" "notes" {
  lifecycle {
    # Every NIC the seed's cloud-config names by MAC needs its device: var.nic_roles lists them.
    precondition {
      condition = (try(length(stowage_vm_seed.notes.nics), 0) <= 1 && length(var.nic_roles) <= 1) || try(
        toset(keys(stowage_vm_seed.notes.nics)) == toset(var.nic_roles) && stowage_vm_seed.notes.nics[var.nic_roles[0]].primary, false
      )
      error_message = "nic_roles must list the roles of stowage_vm_seed.notes.nics, the primary first, when the release names more than one NIC (else leave it empty)."
    }
  }

  name             = local.name
  vcpus            = 1
  cores            = 2
  threads          = 1
  memory           = 4096
  bootloader       = "UEFI"
  cpu_mode         = "HOST-PASSTHROUGH"
  time             = "UTC"
  autostart        = true
  shutdown_timeout = 90
}

resource "truenas_vm_device" "disk" {
  vm    = tonumber(truenas_vm.notes.id)
  dtype = "DISK"
  order = 1001
  attributes = {
    path   = "/dev/zvol/${truenas_zvol.disk.id}"
    type   = "VIRTIO"
    iotype = "THREADS"
  }
}

resource "truenas_vm_device" "nic" {
  for_each = local.nics
  vm       = tonumber(truenas_vm.notes.id)
  dtype    = "NIC"
  order    = 1002 + tonumber(each.key)
  attributes = {
    type       = "VIRTIO"
    nic_attach = each.value.attach
    mac        = each.value.mac
  }
}

resource "truenas_vm_device" "seed" {
  vm    = tonumber(truenas_vm.notes.id)
  dtype = "CDROM"
  order = 9999
  attributes = {
    path = terraform_data.seed.output
  }
}

# Starts the VM unless it runs. TrueNAS applies a changed definition at the VM's next start only,
# so a change of the VM or of a device replaces this resource, which powers the VM off, then on.
resource "terraform_data" "power" {
  triggers_replace = [
    terraform_data.image.id,
    truenas_vm.notes.id,
    [truenas_vm.notes.cores, truenas_vm.notes.memory],
    truenas_vm_device.disk.attributes,
    [for d in truenas_vm_device.nic : d.attributes],
    truenas_vm_device.seed.attributes,
  ]
  input = merge(local.ssh_env, { VM_ID = truenas_vm.notes.id })

  provisioner "local-exec" {
    command     = "\"${local.step}\" start"
    environment = self.input
  }

  # Destroyed before the devices it depends on: TrueNAS refuses to delete a running VM's device.
  provisioner "local-exec" {
    when        = destroy
    command     = "\"$STEP\" stop"
    environment = merge(self.input, { STEP = "${path.module}/truenas-step.sh" })
  }
}

# The install, over SSH once the VM's first boot answers.
resource "stowage_install" "notes" {
  inputs = { ADMIN_EMAIL = var.admin_email }
  tls    = "internal"

  vm {
    host             = local.host
    user             = "ops"
    private_key_file = pathexpand("~/.ssh/id_ed25519")
    known_hosts_line = stowage_vm_seed.notes.known_hosts_line # the one-time key, until the VM's own is read
    short_name       = stowage_vm_seed.notes.short_name
    version          = coalesce(var.release_version, stowage_vm_seed.notes.version) # a later release upgrades the VM
  }

  # deletion_protection = false   # a destroy then forgets the install; the resources above remove the VM

  depends_on = [terraform_data.power]
}

variable "installer" {
  type        = string
  description = "The release's installer by digest, as `stowage ship` prints it: the release the seed's disk boots first."
}

variable "release_version" {
  type        = string
  description = "The release the VM runs; null: the seed's. A later one upgrades the VM, the previous one rolls it back with allow_rollback."
  default     = null
}

variable "nic_roles" {
  type        = list(string)
  description = "The roles of the NICs the release names (host.interfaces), the primary first, when there are more than one; empty: one NIC."
  default     = []
}

variable "admin_email" {
  type = string
}

variable "truenas_url" {
  type        = string
  description = "TrueNAS's URL, https://<host>."
}

variable "truenas_ssh_host" {
  type        = string
  description = "TrueNAS's SSH host, its host key already in ~/.ssh/known_hosts."
}

variable "truenas_ssh_user" {
  type        = string
  description = "TrueNAS's SSH user, whose shell runs midclt (the middleware's client) as an administrator."
  default     = "root"
}

variable "truenas_ssh_key_file" {
  type    = string
  default = "~/.ssh/id_ed25519"
}

variable "pool" {
  type    = string
  default = "tank"
}

variable "parent_dataset" {
  type        = string
  description = "An existing dataset of pool the VM's zvol and staging dataset go under."
  default     = "vms"
}

variable "bridge" {
  type        = string
  description = "The bridge the VM's NICs attach to (a NIC's attach= in the seed's interfaces wins)."
  default     = "br0"
}

variable "address" {
  type        = string
  description = "The VM's static address with its prefix, e.g. 192.0.2.10/24."
}

variable "gateway" {
  type = string
}
```

## The step script

`truenas-step.sh`, beside `main.tf`, executable:

```shell
#!/usr/bin/env bash
# truenas-step.sh STEP — what the TrueNAS provider has no resource for, run by the root's
# local-exec provisioners over SSH to TrueNAS's shell, whose `midclt` calls the middleware (the
# same API the provider uses). Every setting comes from the environment:
#
#   TNAS_SSH_HOST, TNAS_SSH_USER, TNAS_SSH_PORT, TNAS_SSH_KEY   how TrueNAS is reached (its host key
#                                                                must already be known: no other is
#                                                                accepted)
#   import-image     IMAGE, IMAGE_SHA256, STAGING_DIR, ZVOL: upload the disk image (checked against
#                    its SHA-256 here and there), convert it into the zvol (vm.device.convert),
#                    remove the upload. Refuses a zvol that already holds data.
#   seed             SEED_USER_DATA, SEED_META_DATA, WORK_DIR, DEST: a NoCloud seed ISO (volume
#                    cidata) built in a private directory, uploaded to DEST, removed here.
#   start | stop     VM_ID: start the VM unless it runs; power it off when it runs.
#   drop-snapshots   DATASETS: delete every snapshot of each dataset (space-separated).
set -euo pipefail

die() { printf 'truenas-step: %s\n' "$*" >&2; exit 1; }
need() { [[ -n ${!1:-} ]] || die "$1 is not set"; }
for v in TNAS_SSH_HOST TNAS_SSH_USER TNAS_SSH_PORT TNAS_SSH_KEY; do need "$v"; done

# nas_in CMD — CMD in TrueNAS's shell, given this script's standard input; nas CMD — given none
# (ssh would otherwise read what a loop around it reads).
nas_in() {
	ssh -i "$TNAS_SSH_KEY" -p "$TNAS_SSH_PORT" -o IdentitiesOnly=yes -o BatchMode=yes \
		-o StrictHostKeyChecking=yes -o ConnectTimeout=15 "$TNAS_SSH_USER@$TNAS_SSH_HOST" "$@"
}
nas() { nas_in "$@" </dev/null; }
# q ARG… — each argument quoted for the remote shell.
q() { printf ' %q' "$@"; }
# midclt METHOD [ARGS…] — a middleware call on TrueNAS; its JSON on standard output.
midclt() { nas "midclt call$(q "$@")"; }
midjob() { nas "midclt call -job$(q "$@")"; }

sha_of() { sha256sum "$1" | cut -d' ' -f1; }
remote_sha() { nas "if [ -f $(printf %q "$1") ]; then sha256sum $(printf %q "$1") | cut -d' ' -f1; fi"; }

# upload SRC DEST SHA256 — SRC to DEST through a .part file, checked on TrueNAS before it takes
# DEST's name; a DEST already holding those bytes is kept. Mode 0600 (the seed holds the one-time
# host key, the disk the release's pull account): libvirt hands a VM's file to its process as it
# starts it.
upload() {
	local src=$1 dest=$2 sha=$3
	[[ $(remote_sha "$dest") == "$sha" ]] && return 0
	nas_in "umask 077; cat > $(printf %q "$dest.part")" <"$src"
	[[ $(remote_sha "$dest.part") == "$sha" ]] || { nas "rm -f $(printf %q "$dest.part")"; die "the upload of $src to $dest does not match its SHA-256"; }
	nas "chmod 0600 $(printf %q "$dest.part") && mv -f $(printf %q "$dest.part") $(printf %q "$dest")"
}

case "${1:-}" in
import-image)
	for v in IMAGE IMAGE_SHA256 STAGING_DIR ZVOL; do need "$v"; done
	[[ $(sha_of "$IMAGE") == "$IMAGE_SHA256" ]] || die "$IMAGE does not match IMAGE_SHA256"
	# A zvol a VM already wrote to is never overwritten: only a new zvol (a few KiB referenced) is.
	used=$(nas "zfs get -Hp -o value referenced $(printf %q "$ZVOL")")
	[[ $used =~ ^[0-9]+$ ]] || die "could not read how much $ZVOL holds"
	((used < 64 * 1024 * 1024)) || die "$ZVOL already holds data ($used bytes): refusing to write an image over it"
	ext=${IMAGE##*.}
	staged="$STAGING_DIR/disk-${IMAGE_SHA256:0:16}.$ext"
	upload "$IMAGE" "$staged" "$IMAGE_SHA256"
	midjob vm.device.convert "$(jq -cn --arg s "$staged" --arg d "$ZVOL" '{source: $s, destination: $d}')" >/dev/null
	nas "rm -f $(printf %q "$staged")"
	;;
seed)
	for v in SEED_USER_DATA SEED_META_DATA WORK_DIR DEST; do need "$v"; done
	mkdir -p "$WORK_DIR"
	umask 077
	work=$(mktemp -d "$WORK_DIR/.seed.XXXXXX")
	trap 'rm -rf "$work"' EXIT
	mkdir "$work/src"
	printf '%s' "$SEED_USER_DATA" >"$work/src/user-data"
	printf '%s' "$SEED_META_DATA" >"$work/src/meta-data"
	if command -v xorriso >/dev/null; then
		xorriso -no_rc -report_about SORRY -outdev "$work/seed.iso" -volid cidata -joliet on -rockridge on -map "$work/src" / >&2
	elif command -v genisoimage >/dev/null; then
		genisoimage -quiet -output "$work/seed.iso" -volid cidata -joliet -r "$work/src/user-data" "$work/src/meta-data"
	else
		die "neither xorriso nor genisoimage is installed"
	fi
	upload "$work/seed.iso" "$DEST" "$(sha_of "$work/seed.iso")"
	;;
start)
	need VM_ID
	state=$(midclt vm.status "$VM_ID" | jq -r .state)
	[[ $state == RUNNING ]] || midclt vm.start "$VM_ID" >/dev/null
	;;
stop)
	need VM_ID
	state=$(midclt vm.status "$VM_ID" | jq -r .state)
	[[ $state != RUNNING ]] || midjob vm.stop "$VM_ID" '{"force_after_timeout": true}' >/dev/null
	;;
drop-snapshots)
	need DATASETS
	for ds in $DATASETS; do
		midclt pool.snapshot.query "$(jq -cn --arg d "$ds" '[["dataset", "=", $d]]')" '{"select": ["id"]}' |
			jq -r '.[].id' | while read -r snap; do
				midclt pool.snapshot.delete "$snap" >/dev/null
			done
	done
	;;
*)
	sed -n '2,19p' "$0" | sed 's/^# \{0,1\}//' >&2
	exit 2
	;;
esac
```
