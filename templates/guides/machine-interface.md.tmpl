---
page_title: "The machine interface, as read - stowage"
subcategory: "Guides"
description: |-
  How the provider reads a Stowage installer: its JSON documents, exit status and formats.
---

# The machine interface, as read

- A verb (install and re-run, `upgrade`, `rollback`, `uninstall`) runs with `--json`: its
  standard output is one object, `{format, result, release, refused}`; its text goes to standard
  error. Every verb runs after a `describe --json` of the installer it runs (the fetched one, or
  the kept one), so an installer without the machine interface is stopped there: a `describe`
  exiting 1 (cobra's unknown command, as such an installer answers) says that an installer made
  by Stowage before 0.34.0 has none; another exit says nothing of the installer's age. A released
  installer cannot be changed after the fact, so the hint is all the provider can say: it never
  takes such an installer for one it reads.
- The installer's exit status:
  - **0** with the object, `result` `done` or `nothing`, no `refused`: success. State is then what
    `status --json` reads (`nothing` — the install already runs that release — changes nothing).
  - **3** with the object, `result` `nothing` and `refused.reason` set: refused before anything
    changed. The diagnostic's summary is `refused.reason`; its detail is `refused.fix` (advice for
    you: the provider shows it and never runs it), then `<step> was refused by the installer
    (<verb>, exit 3) before anything changed on the target`. That step saves no state (a refused
    upgrade after a re-run keeps the re-run's state, as a failed one does).
  - any other exit: failed, whatever standard output holds (a failure prints no object), with
    the end of its error output (4 KiB). Exit 255 is noted as possibly ssh's own failure.
  - A read (`status --json`, `vm seed --json`) refused prints `{format, refused}` alone on
    standard output with exit 3; `refused.absent` (`true`) says no install is there (no install in
    the directory, no helm release in the namespace), which the provider takes for "none": a
    create installs, a read removes the resource, a delete is done. Any other refusal of a read is
    an error.
  - Exit 0 or 3 without exactly that object — none, text, data before or after it, a `result`
    other than `done`/`nothing`, a refusal with exit 0, exit 3 without a refusal or with `result`
    `done`, a `release` without `version` — is a failure naming the breach (`the installer breaks
    the machine interface`), never a refusal or a success.
- On a VM, the event journal (`<short>-stow events --after N --json`, one entry per line, each
  with its `kind`) is read after an upgrade's or a rollback's reboot for the health gate's
  `boot.judged` verdicts; a line that is not an entry, or a run that fails, is an error the wait
  tries again.
- **Unknown fields are tolerated** (an installer adds fields without moving `format`); a known
  field of the wrong type, data after the object, a missing `name` or `version`, and a
  `describe` `short_name` that is not one path element (`[a-z0-9][a-z0-9._-]*`) are errors.
- **An unknown `format` is refused**: missing from `describe`, `status --json` or a verb's result
  (an installer older than Stowage 0.34.0), newer than 1 (upgrade the provider), or any other.
- **Stowage 0.35.0 or later**: `status --json` listing every input key an install records, the
  settings its first install recorded (`registry`, `short_name`, `address`, `registry_ca`), the
  refusals' `absent`, `describe`'s `routes`, `k8s --image-pull-login`, and `vm seed`'s
  `meta_data`, sizes, `disk_file`, `--await-secrets` and `--tls byo`, and the VM's `<short>-stow
  host-key --json` are read as Stowage 0.35.0
  gives them. An installer of Stowage 0.34.0 speaks format 1 without them: its `describe` lists no
  `routes`, which is refused (`no routes: the installer predates the machine interface this
  provider reads`), before any verb; an install whose kept installer is one is refused at its
  update or destroy, naming the hand upgrade that moves it on (its kept `<short>-stow upgrade
  --version V`; on Kubernetes the release's own `k8s upgrade --version V`, on a VM `sudo
  <short>-stow upgrade --version V`), after which the provider drives it.

Stowage documents the interface itself in its README (`describe --json`, `status --json`, `--json` on the verbs).
