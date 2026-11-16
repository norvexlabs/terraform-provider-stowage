# TrueNAS SCALE: a Kairos VM of a Stowage release

A complete OpenTofu root: `stowage_vm_seed` makes the VM's first-boot material once, truenas/truenas 1.9.1 makes
the VM from it, and `stowage_vm_install` waits for its first boot, gives it what goes over SSH
(secret inputs, a registry login, a kit), retires the one-time host key and keeps the install
(upgrades, rollbacks, re-runs). Copy it into a root of your own.

**Proven**: applied for real on TrueNAS SCALE with `truenas/truenas` 1.9.1 by `make acc-vm` (create, its first install with a registry login and a secret input over SSH, no drift after the apply, a re-run, an upgrade and a rollback, an import, a destroy refused by `deletion_protection`, then the destroy, nothing left on TrueNAS). The `vm truenas` steps are a stopgap of Stowage's until `truenas/truenas` writes a file to the host, converts an image into a zvol and stops a VM before deleting its devices. Not proven yet: `extra_nics` (TODO.md).

Needs: `TRUENAS_ENDPOINT` (`wss://<host>/api/current`) and `TRUENAS_API_KEY` in the environment, for the
provider and the Stowage steps; `insecure = true` for TrueNAS's self-signed certificate; `xorriso` and
`jq` on this machine (the seed ISO, `seed-iso.sh`).

```bash
tofu init
tofu apply -var installer=<prefix>/<name>-installer@sha256:<64 hex> -var name=… -var hostname=…
tofu test        # the plan tests, under mocked providers
```

The VM is sized from the release (`stowage_vm_seed`'s `size`): `cpus`, `memory_mib` and `app_gib`
left out are its recommended values; under its least the plan is refused, under its recommended a
check warns. A destroy runs `deletion_protection = false` first (an apply), then `tofu destroy`:
the install is forgotten (with a warning when the registry holds no disk set to make the VM
again), then the VM goes.

## Adopting a VM made by Stowage 0.39.0 or earlier

`vm create --create opentofu` kept its own state; this release of Stowage reads it no more. Either
destroy the VM with Stowage 0.39.0's `vm destroy` before moving on, or adopt it into this root with
`import` blocks, one per resource:

| Resource | Import ID |
|---|---|
| `truenas_dataset.staging` | `<pool>/<parent_dataset>/<name>-staging` (the dataset's name) |
| `truenas_zvol.disk` | `<pool>/<parent_dataset>/<name>` (the zvol's name) |
| `truenas_vm.vm` | the VM's id (`vm.query` on TrueNAS, its URL in the UI) |
| `truenas_vm_device.disk`, `truenas_vm_device.nic["0"]…`, `truenas_vm_device.seed`, `truenas_vm_device.extra_nic["<role>"]` | each device's id (`vm.device.query`) |
| `stowage_vm_install.vm` | `<vm_address>/<short_name>-stow` |

`stowage_vm_install`'s first apply after its import takes the configuration's settings over (give
`vm_known_hosts_line` the VM's own key: `cat ~/.local/state/<name>/vms/<host>/known_hosts`), then a
plan shows no change. `truenas_dataset` and `truenas_zvol` import with `force_destroy = false`: apply once before a
destroy. The `terraform_data` steps cannot be imported: their first apply runs them again —
`import-image` leaves a zvol that already holds the image as it is, `seed` uploads the seed ISO
again; `stowage_vm_seed` has no import either (a seed is generated once): it makes a new seed, whose
ISO the seed device then names, which the VM reads at its next start; the VM, its install done,
keeps its own host key. `truenas_vm_power` is not imported but created: creating it over a running
VM leaves it running, where an import records no `restart_triggers` and its first apply restarts
the VM. The modules up to Stowage 0.39.0 used the community
`pjsalty/truenas` provider: a `moved` block cannot carry their state over; `import` blocks can.
