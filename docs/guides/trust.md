---
page_title: "Trust - stowage"
subcategory: "Guides"
description: |-
  What an installer the provider fetches is checked against: the provider's public_keys, the keys of the release installed, else its digest alone.
---

# Trust

Every installer the provider uses is named by digest (`installer`, `<prefix>/<name>-installer@sha256:…`):
the registry cannot serve another file under it. What the digest cannot say is whether the
project published it. The provider checks that as Stowage checks a release: a sigstore bundle
referring to the image's digest (an OCI referrer, as `cosign sign --key` v3 writes it,
`https://sigstore.dev/cosign/sign/v1` over that digest), signed by a trusted public key, with no
transparency log. Never against the keys the fetched installer itself carries: whoever made it
would choose them.

| When | The installer must be signed by | Else |
|---|---|---|
| any fetch, with the provider's `public_keys` | one of `public_keys` | refused |
| an upgrade over an install (Docker, Kubernetes) | one key the release installed carries (on Docker, read from the installer the install keeps when the registry cannot give its image), besides `public_keys`; a release installed that carries none (its project not signing) adds nothing: without `public_keys`, the digest alone | refused |
| an upgrade of a VM | the VM's own `upgrade --version` checks it against the keys of the release it runs | refused there |
| a first install (`stowage_docker_install`, `stowage_kubernetes_install`, `stowage_vm_seed`) without `public_keys` | nothing: its digest alone is trusted | a plan warning, `The installer is trusted by its digest alone` |

Under a trust, the release the installer carries must carry keys of its own (its payload's
`keys/`): a release of a project that does not sign is refused (`the release carries no public
key`), since its install could trust no key and a later release would go unchecked.

## The provider's `public_keys`

PEM public keys (`-----BEGIN PUBLIC KEY-----`), the keys the project signs its releases with —
the ones its release carries in `keys/`, got from the project by a channel you trust:

```terraform
provider "stowage" {
  public_keys = [file("notes-release.pub")]
}
```

A value that is not a PEM public key is a configuration error naming its index
(`public_keys[1] is not a PEM public key`).

## The keys of the release installed

An upgrade's new installer is checked against the keys the release running carries, read the way
Stowage's own `upgrade --version` reads them: from that release's installer, the image the install
records by digest — `status --json`'s `installed.installer` (machine interface), which the release's
signed manifest gave the install, else the `installer` state records (the one this provider
installed). That installer is read from the registry on the machine running OpenTofu (its payload's
`keys/`, nothing run): on Kubernetes it is the program the run fetches anyway; on Docker the keys are
kept in the provider's cache by its digest (`<index digest>/keys.json`). A release installed that
carries no key checks nothing (a project that does not sign), said on Docker in a warning (`The
release installed carries no key`: the upgrade is checked by `public_keys` alone, or by its digest
alone without them). On Docker, keys that can be read neither from the registry nor from the
installer the install keeps on the target refuse the upgrade before anything is fetched or run (`The
keys of the release installed cannot be read`, naming both sources, and `public_keys` among the
fixes) — but with `public_keys`, which then check the new installer alone, said in a warning. On
Docker, an install that names no
installer (made `--offline`, or by a Stowage that did not record it) and a state that names none:
an upgrade is refused (`The keys of the release installed cannot be told`) — without `public_keys`
before anything is fetched or run on the target, a plan for the release installed included; with
them, the new installer is read under `public_keys` alone, and the release installed is taken
over: upgrade it by hand with the installer it keeps
(`<short>-stow upgrade --version V --dir <dir>`, which checks the release against those keys), then
refresh. A rollback reaches a release the install ran before, and is not checked again.

## Where the check runs

- **Kubernetes and `stowage_vm_seed`**: the installer runs on the machine running OpenTofu. The
  entry of this machine's platform is checked before its layer is read; the file is then streamed
  into the provider's cache. A program the cache keeps is checked again against the trust of each
  run that takes it (its image's digest is kept beside it).
- **Docker**: the target fetches the installer itself (`docker run`). Before it does, every
  platform's image of the installer's index is checked on the machine running OpenTofu, and the
  keys of its release read from one of them, streamed, nothing stored: the image the target pulls
  by that digest is one checked.
- **VM**: the provider fetches nothing after the seed; the VM's `upgrade --version` checks the new
  release against the keys of the release it runs.

The registry is read with the logins the provider holds for its host (the provider's pull account
for the host of an installer the configuration names or of the provider's `registry`; the mirror's
own login for the mirror), else the machine's Docker configuration; the provider's `registry_ca`
is trusted besides the system's.
