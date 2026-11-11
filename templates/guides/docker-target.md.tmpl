---
page_title: "The Docker target - stowage"
subcategory: "Guides"
description: |-
  What each operation of stowage_install runs on a Docker host, and how SSH reaches it.
---

# The Docker target

Every remote command is one `sh -c` script with its values as positional arguments, quoted for
the remote shell; nothing secret is ever an argument. On the machine running OpenTofu (no `host`)
the installer runs with OpenTofu's environment but for the variables named as an input the
installer declares (those it reads before an answers file), each left out of the program's
environment before it starts — but those the run
needs itself, the Kubernetes target's list below; an input of one of those names while the
environment sets it is refused, a key of `given_wo`'s `inputs` included (`<KEY> comes from the
environment`). The scripts' own variables are named `__stowage_*`, so an exported variable of
another name reaches the installer unchanged. Over ssh no environment is carried.

- **Create**: `docker run --rm <installer> > <work_dir>/<name>.sh` (written through a temporary
  file, so a failed fetch leaves no half script), the bootstrap script's `describe --json` (its
  `format` checked, its `name` compared, its shapes must include `docker` when it lists any, its
  `short_name` recorded); with `work_dir` left out, the home resolved (`$HOME`, or `cd` there and `pwd -L`, as the `work_dir` row says; a
  failure is an error before anything runs); then its `status --json --dir ./<name>`: a refusal
  whose object says `absent` (no install in the directory) is a fresh install, any other refusal
  an error; an install of another project is an error;
  an install of the project already
  there (a create that failed or timed out after the install began, or an install a destroy
  kept with its data) is a warning (`An install is already there`), and the install below runs
  without `--secrets` and without those of `--address`, `--registry` and `--registry-ca` that
  `describe` lists in `first_install_only`, which the installer's re-run refuses: the install
  keeps the ones it recorded, which must be the configuration's — an `address` given, the
  provider's `registry` or `registry_ca` (by digest) other than the ones its `status --json`
  reports is an error before anything runs (`The install there was made otherwise`, naming each);
  another release's install there is the installer's to refuse
  (its re-run runs the release it carries alone, exit 3). Then
  `sh <name>.sh --no-input --json --dir ./<name> [--set K=V]… [--tls] [--domains] [--ports]
  [--address] [--registry]`, with `--answers`, `--given`, `--secrets` and `--registry-ca` files
  sent on standard input as a tar into a private (0700) temporary directory removed afterwards;
  then `status --json`, read again every 10 seconds while it reports `installing` (the first
  install not finished) until it no longer does, within the create timeout. A failed or refused
  create, an `install_failed` and a timeout save no state: applying again runs the installer
  again on the install left, which finishes it (the installer refuses a run while another holds
  the install; a destroy cannot clean what state does not hold).
- **The kept installer** is `<work_dir>/<name>/<short_name>-stow` (`short_name` from `describe`),
  else, when that file is missing or not executable (a release that renamed its installer), the
  one `*-stow` executable there; none or two is an error. A `short_name` in state that is not one
  path element is an error naming the re-import. It runs with `--dir ./<name>` after its arguments
  (not `describe`), and with files on standard input as the install does.
- **Read**: the kept installer's `status --json`. A missing install directory removes the
  resource from state (the script exits 100 with `stowage-provider: no install directory` as
  the last line of stderr, after an SSH banner, and nothing on stdout; any other exit 100 is a
  failure), and so does the installer's refusal saying `absent`. What the first install recorded
  goes to state: `registry_ca_digest`, `short_name`, and `address` or `registry` where state's
  differ from the install's (above). A release other than the configured one
  (`installed.installer`) replaces `installer` in state, so the next plan shows the change back.
  When `status` lists the recorded input keys — every key the install records, whichever release
  declared it — a key of `inputs` with a non-empty value it does not list is removed from state, so
  the next plan gives it again. `inputs` absent (unknown to the installer) removes nothing. An imported
  remote state (no `known_hosts_line` yet) is not read: the configuration's connection reads it at
  the first apply.
- **Update**: a change of the provider's own settings alone (the connection,
  `deletion_protection`, `delete_data`, `allow_rollback`, `timeouts`) is saved without touching
  the target. Any other change, in this order:
  1. `status --json` and the kept installer's `describe --json` (the running release's inputs and
     routes), the kept installer found by the short name `status` reports. No install directory
     is an error; a kept installer of another project too. Then the configuration's `address`,
     the provider's `registry` and `registry_ca` are compared with what the install recorded, each
     left out meaning its default (`127.0.0.1`, the release's own registry by the kept
     installer's `describe`, no CA), as a create over an install does: another is an error before
     anything runs (`The install was made otherwise`) — an imported state's first apply, or a
     setting the plan could not tell.
  2. The release to reach: none when `installed.installer` is the desired `installer`; `rollback`
     when it is `previous.installer` (with `allow_rollback`); else the new installer is fetched
     (`<name>.sh` written again, as at create) and described — another project, or no `docker`
     shape, is an error — and when it carries the release installed (the same `version` and
     `revision`) there is nothing to upgrade — an error when the install records another digest of
     its installer (`installer is another digest of the release installed`, naming the one to set:
     Read would put it back at every refresh) — else its `upgrade` runs (the installer refuses an
     older release: exit 3, the diagnostic).
  3. The **re-run** of the running release, when there is anything to give it: `<short>-stow
     --no-input --json [--set K=V]… [--tls] [--domains] [--ports] --dir ./<name>`, the secret
     inputs as `--answers` on standard input. It takes each input of `inputs` changed or added
     that the running release declares (every one without an upgrade), each input taken out that
     the running release declares (`--set KEY=`; with an upgrade, only one the new release still
     declares: the upgrade drops the others), the secret
     inputs the running release declares when `secret_inputs_wo_version` changed, the changed
     `tls`, the changed and added `domains`, and the changed and added `ports` but those of a route
     only the new release has. When a release change follows, state is saved here: the release still the old one,
     `inputs` and `ports` as the re-run applied them. A failed or refused re-run changes no state.
  4. The **upgrade**: `sh <name>.sh upgrade --no-input --json --dir ./<name> [--set K=V]…
     [--ports]`, given the inputs the running release does not declare (those the new release
     adds; the installer refuses any other), the secret inputs it does not declare (`--answers`,
     sent with every upgrade), and the ports of the routes the new release adds (its `describe`
     `routes` against the running release's). Or the
     **rollback**, which goes alone: a change of inputs, secret inputs, TLS mode, domains or ports
     with it is an error before anything runs (`a rollback goes alone`), and so is a rollback of an
     imported install never applied (`a rollback of an imported install`: apply first with the
     release it runs): `<short>-stow rollback --json --dir ./<name>`. A failure leaves step 3's state,
     so the next plan shows the release change alone.
  5. `status --json` fills the computed attributes; one that cannot be read keeps state's, with
     the error.
- **Import**: `tofu import stowage_install.x '[<host>:]<work_dir>/<name>/<short>-stow'`, the
  installer the install keeps (`ls <work_dir>/<name>/*-stow`), `<work_dir>` an absolute, clean
  path, or left out (`<name>/<short>-stow`, or `./<name>/…`) for the SSH user's home (another
  relative one is refused); a host is what comes before the
  first colon when no slash does (an `@` is refused: the user comes from the configuration).
  State records the target, the directory and `short_name`, `deletion_protection = true`; the
  first apply, with the configuration's connection, runs the update above with every configured
  input, secret input (when `secret_inputs_wo_version` is set), `tls`, domain and port given to
  the re-run (the installer counts a value given as recorded as no change), and records the
  provider's `registry`.
- **Delete**: refused while `deletion_protection`; else the kept installer's `describe --json`
  (an installer without the machine interface stops there, before `uninstall` is given `--json`),
  then `<short>-stow uninstall --yes --no-input --json [--data] --dir ./<name>`. An install
  directory already gone is success, and runs nothing; so is the installer's refusal saying
  `absent`.

## SSH

The system `ssh` runs with `BatchMode=yes`, `StrictHostKeyChecking=yes`, a private 0600
`known_hosts` file (in a 0700 temporary directory, removed after the run) holding the given key
(on a VM, the keys it may serve: see `vm.seed_boot_id`) under `HostKeyAlias=stowage-target`, `GlobalKnownHostsFile=/dev/null`, `KnownHostsCommand=none`,
`CheckHostIP=no`, `UpdateHostKeys=no`, `VerifyHostKeyDNS=no` (no SSHFP record trusted),
`ControlMaster=no` and `ControlPath=none` (no connection another ssh opened is reused),
`ConnectTimeout=30`, and with a key file `IdentitiesOnly=yes`. The user's `~/.ssh/config` still
applies to everything else (a `ProxyJump`, for one). Output is kept up to 16 MiB per stream.

A program on a remote target runs as the login shell parses its command line, each word quoted
for a POSIX shell (`sh`, `bash`, `dash`, `zsh`, `ksh`): a csh-family login shell is not one.
A run cut by its timeout kills the local `ssh` (or, on this machine, the program it started, the
`sh` running a script — the programs that script started go on), then waits at
most 10 seconds for its output to close; so does a program that exited while one it started holds
the output, which keeps its exit status — what that process writes meanwhile is part of the output,
which a `--json` reader then refuses as data after the object (an installer's verbs start none
that write to it). The output is never staged in a file, so a secret a verb prints (`vm seed`'s
host key) reaches no disk, and a verb whose machine reboots right after its result (the VM's
upgrade and rollback) has it read before. A cut run does not stop the program on a remote target,
by design: an install or upgrade stopped halfway would be left half done, while one cut there goes
on and the installer refuses another run until it ends (the next apply then says so).
The target's Docker daemon must trust a mirror's CA itself to fetch the installer image (its
`/etc/docker/certs.d`): the provider never changes a daemon's configuration, which every
container on the host shares.

