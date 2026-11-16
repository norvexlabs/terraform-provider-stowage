# Amazon EC2: a Kairos VM of a Stowage release

A complete OpenTofu root: `stowage_vm_seed` makes the VM's first-boot material once, hashicorp/aws 6.69.1 makes
the VM from it, and `stowage_vm_install` waits for its first boot, gives it what goes over SSH
(secret inputs, a registry login, a kit), retires the one-time host key and keeps the install
(upgrades, rollbacks, re-runs). Copy it into a root of your own.

**Preview**: not applied on Amazon EC2 yet (`tofu validate` and `tofu test` at `command = plan` pass under mocked providers). An example stays a preview until it has been applied on its platform.

Needs: The AWS SDK's credentials (`AWS_PROFILE`, …); the image `ship --disks` registered for the release's
disk (stowage_vm_seed's `disk.aws.cloud_image`, looked up by name among your AMIs), else `image_id`
— `image/` registers one, a root of its own shared by every VM of the disk.

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

`image/` is a root of its own: it registers the release's disk as an image (upload, import, register), for a release whose `ship --disks` registered none on this cloud. Give it the disk as a file (`stowage_vm_seed`'s `disk_file` with `disk_dir`, or `vm seed --json --disk-dir`) and keep its state: every VM of that disk boots the image.

## Adopting a VM made by Stowage 0.39.0 or earlier

`vm create --create opentofu` kept its own state; this release of Stowage reads it no more. Either
destroy the VM with Stowage 0.39.0's `vm destroy` before moving on, or adopt it into this root with
`import` blocks, one per resource:

| Resource | Import ID |
|---|---|
| `aws_instance.vm` | the instance id (`i-…`) |
| `aws_security_group.vm[0]` | the group id (`sg-…`) |
| `aws_eip_association.vm[0]` | the association id (`eipassoc-…`) |
| `stowage_vm_install.vm` | `<address>/<short_name>-stow` |

`stowage_vm_install`'s first apply after its import takes the configuration's settings over (give
`vm_known_hosts_line` the VM's own key: `cat ~/.local/state/<name>/vms/<host>/known_hosts`), then a
plan shows no change. 
