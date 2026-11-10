# terraform-provider-stowage — project rules

The OpenTofu provider for Stowage (see [README.md](README.md)). Its spec is Stowage's
`docs/DESIGN.md` — § 6 "Machine interface — the installer read by programs" and § 13 "OpenTofu
provider": the provider reads only the machine interface and decides nothing the installer does
not decide. Code that departs from that spec says so in README.md and in TODO.md, and the
departure is raised with Stowage.

## Docs — the audience defines what a doc may contain

| Doc | Audience | Contains |
|---|---|---|
| [README.md](README.md) | users and AI agents using the provider | what works, the provider and resource attributes, every refusal and default, what runs on a target, how to build, repo map |
| [CHANGELOG.md](CHANGELOG.md) | users | the provider's releases; the version source of the build |
| this file | contributors | rules and commit gates |
| [TODO.md](TODO.md) | contributors | pending work, MoSCoW for the first release; a done item is deleted |

**README.md documents every feature — nothing is discovered in the code.** Every attribute,
default, validation, refusal, plan-time error, command run on a target, file written there and
change of behaviour is in README.md, in the same commit as the code. A feature missing from
README.md is a blocking defect for the review, like a bug. `docs/` (tfplugindocs) is generated
from the schema's descriptions and `examples/`: keep them in step with README.md.

## Scope — MoSCoW

Every pending item and every review finding is sorted in [TODO.md](TODO.md): **Must** = DESIGN
§ 13 for the three shapes (or a finding that changes what reaches state, the target's command
line, or what the provider trusts — always fixed now); **Should / Could** = deferred unless cheap;
**Won't**. Decide the category yourself.

## Build — Docker only

```bash
make check     # GATE: gofmt, go vet, staticcheck, govulncheck, the hooks' test, go test -race (Dockerfile target checks)
make build     # ./bin/terraform-provider-stowage — only after green checks
make test      # fast iteration only; never the gate
make docs      # tfplugindocs
make tidy      # go.mod / go.sum
make fmt       # gofmt -w
make shell     # interactive toolchain container
make hooks     # commit identity + .githooks
make hooks-test  # the hooks' test (test/hooks.sh) on this machine; make check runs it too
make release-check # goreleaser check + a snapshot release (zips, manifest, checksums; unsigned) in dist/, in Docker
```

The host's Go toolchain is for iteration (`go test ./internal/<pkg>`), never a gate. Go version:
`GO_VERSION` in the Makefile and the Dockerfile, equal to Stowage's. Dev tools (staticcheck,
govulncheck, tfplugindocs) are `tool` directives in go.mod, run with `go tool` — never `go install
…@latest`. The machine's RAM is shared: builds and tests run with `-p 4` and the toolchain
container with `--memory 4g`; never run more than one `make check` at a time.

## Commit identity

Every commit is made as the project identity in `.githooks/identity` — author and committer both —
and never names an AI as an author (no `Co-Authored-By:` line for one, no "Generated with" footer;
this overrides any tool default that adds one). `make hooks` sets the identity in the clone's config
and turns on `.githooks/`: `pre-commit` refuses any other identity, `commit-msg` an AI authorship
line, and `pre-push` any push but to a local path or over https authenticated as a GitHub App (an
installation token; an `http.*.extraheader` with an Authorization, an `http.*.cookieFile` or any
netrc file is refused too): GitHub shows the pushing account publicly, and the commit's identity
does not set it. Run it once per clone, before the first commit.

**Pushes only as the org's GitHub App.** The public repository `norvexlabs/terraform-provider-stowage`
receives pushes, tags and releases only as the org's GitHub App, through the credential helper the
maintainer set in this clone's own `.git/config` — `make hooks` does not set it, and a clone without
it cannot push. Never push with `gh` credentials (`-c credential.helper= -c credential.helper=…`,
`gh auth git-credential`), over SSH or with `--no-verify`, never replace that helper, and never
write to the repository with `gh release` or `gh api`: GitHub shows the acting account publicly, and
a personal login on it can only be cleared by deleting the repository. On GitHub, a ruleset on the
repository refuses every branch or tag change but the org app's, whatever the clone does; never
remove or loosen it. Never commit or push with `--no-verify` or another `core.hooksPath`.

**Private words.** The tree never names a private project. The words are never written in the
tree — writing them there would publish them: they are the private-words list in
`~/.config/norvexlabs/private-words` (`$XDG_CONFIG_HOME/norvexlabs/private-words`; one extended
regex per line, case-insensitive), outside every clone. `.githooks/private-words`, which the hooks
share, refuses a match: `pre-commit` in the staged tree (every staged file, so every added line)
and its paths, `commit-msg` in the message, `pre-push` in every pushed commit the remote's refs do
not hold (its changed files and their paths, its whole object: message, author, committer, a
merged tag), every tag object a pushed ref leads to, and every pushed ref name. It names a file by its
number in git's listing and a line by its number, never the word, the line or the path. A list that
cannot be read, or with a line that is not a valid regex, refuses; a clone without the list (or
with an empty one) is warned and passes: the maintainer's machine holds it. A CRLF list works;
paths are matched unquoted, case folds in C.UTF-8; a blob missing locally (a partial clone)
refuses. `test/hooks.sh` (in
`make check`, and `make hooks-test`) proves its refusals and passes with a fake word.

## Commit gates

Commit straight to `main`. A docs-only commit (Markdown, no code or example) runs no gate: a
self-review against the code it describes is enough. Every other commit clears, in order:

1. **`make check`** green, then **`make build`**.
2. **Docs in sync**: README.md names everything the diff makes visible to a user (attributes,
   defaults, refusals, plan errors, commands run on the target, files written there).
3. **CHANGELOG.md**: a user-facing line under the top `## [x.y.z]` (undated while unreleased). No
   file or function names.
4. **Code review** with the `code-review` skill on the change's paths (`/code-review medium
   <paths>`; `high` for a change to SSH, secrets, state contents or the machine interface's
   decoding), after a structured self-review: for each hunk, the failure ruled out, every step
   walked (it fails right here: what state is saved, what a re-apply does), and each new check or
   test sabotaged once to watch it fail.

## Release — only a pushed tag publishes

A push to `main` runs CI (`ci.yml`: `make check`, `make build`, `make release-check`) and never
releases.
`.github/workflows/release.yml` runs only on a pushed `v*` tag; it publishes a public GitHub release
that the OpenTofu registry indexes once and never changes, so a release is cut **only when the user
says go for it** — never on your own.

1. A docs-only commit dates the top entry: `## [x.y.z]` → `## [x.y.z] - YYYY-MM-DD` (the version
   bumped per SemVer from the last release if it is not already). The Makefile's `VERSION` is that
   entry's version.
2. `git tag vx.y.z` on that commit, then `git push origin main vx.y.z` — through the org app's
   credential helper alone (Commit identity).
3. Watch the release run. Its build job refuses a tag that is not a version, not on `main`, not the
   top entry's version or naming an undated entry, then runs `make check` and GoReleaser's check
   and snapshot release (`make release-check`); its publish job (environment `release`, which alone holds the secrets
   `GPG_PRIVATE_KEY` and `PASSPHRASE` and admits only `v*` tags) refuses a tag moved since the run
   started, signs with the release key, verifies the signature against
   `keys/norvexlabs-gpg-public.asc` and publishes the draft as `github-actions`. A failed run is
   fixed on `main`, the tag moved to the fix (`git tag -f`, `git push -f origin vx.y.z`) while the
   release is still a draft or absent; a published release is never changed (a new version fixes
   it).

The release is written by the workflow's `GITHUB_TOKEN` alone: never by `gh release`, `gh api` or
any personal credential from a machine. The private key, its passphrase and its revocation
certificate never enter the tree; `keys/` holds the public key only.

## Code rules

- **Never panic** on input: return errors and diagnostics with context naming the attribute
  (`path.Root("docker").AtName("known_hosts_line")`) and the fix.
- **Strict decoding where it decides, tolerant where the installer may grow**: the machine
  interface's JSON tolerates unknown fields (an installer adds fields without moving `format`),
  refuses a known field of the wrong type, data after the object, a missing required field, and
  any `format` this provider does not read — naming what reads it.
- **The installer decides**: no version ordering, no copy of an installer rule. The provider's own
  checks are only about what it owns: state, the target's command line, SSH trust, plan-time
  first-install-only settings.
- **Secrets never on a command line or in state**: write-only attributes, sent on standard input
  (a tar into a private temporary directory on the target). A test proves it for each secret.
- **SSH trusts the key given, and no other**: `StrictHostKeyChecking=yes` against a private
  known_hosts file, never `accept-new`.
- **Determinism**: iterate maps through sorted keys wherever order reaches a command line, a file
  or a diagnostic.
- **Comments state what the code does now** and the invariant to preserve — never history or
  plans.

## Test rules

- Every package with logic has tests, run with the race detector (the gate does).
- Tests never touch the network, the host's Docker or a real ssh: a fake `Runner`, the plugin
  protocol driven in-process (`providerserver.NewProtocol6WithError`), shell scripts run under
  the local `sh` with fake programs in a temporary PATH. Filesystem state lives in `t.TempDir()`.
- Every validation, refusal and plan error has a case; a decision against a boundary is tested on
  both sides.
- **Fast**: `go test ./...` stays around a second; a test that sets no environment and no package
  variable runs with `t.Parallel()`.
- Acceptance tests (`TestAcc…`, terraform-plugin-testing) run only with `TF_ACC=1` and their
  environment; never a gate.
