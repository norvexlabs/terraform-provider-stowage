# libvirt / KVM: a Kairos VM of a Stowage release

A complete OpenTofu root: `stowage_vm_seed` makes the VM's first-boot material once, dmacvicar/libvirt 0.9.9 makes
the VM from it, and `stowage_vm_install` waits for its first boot, gives it what goes over SSH
(secret inputs, a registry login, a kit), retires the one-time host key and keeps the install
(upgrades, rollbacks, re-runs). Copy it into a root of your own.

**Preview**: not applied on libvirt / KVM yet (`tofu validate` and `tofu test` at `command = plan` pass under mocked providers). An example stays a preview until it has been applied on its platform.

Needs: A libvirt connection (`uri`, e.g. `qemu+ssh://root@kvm.example.org/system`); `xorriso` and `jq` on
this machine. Open proofs (TODO.md): a KVM host with AppArmor enforcing, several vCPUs.

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
| `libvirt_volume.image`, `libvirt_volume.disk`, `libvirt_volume.seed` | the volume's key (its path, `virsh vol-key`) |
| `libvirt_domain.vm` | the domain's UUID |
| `stowage_vm_install.vm` | `<address>/<short_name>-stow` |

`stowage_vm_install`'s first apply after its import takes the configuration's settings over (give
`vm_known_hosts_line` the VM's own key: `cat ~/.local/state/<name>/vms/<host>/known_hosts`), then a
plan shows no change. 
