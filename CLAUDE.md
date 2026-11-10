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
make check     # GATE: gofmt, go vet, staticcheck, govulncheck, go test -race (Dockerfile target checks)
make build     # ./bin/terraform-provider-stowage — only after green checks
make test      # fast iteration only; never the gate
make docs      # tfplugindocs
make tidy      # go.mod / go.sum
make fmt       # gofmt -w
make shell     # interactive toolchain container
make hooks     # commit identity + .githooks
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
installation token). GitHub shows the pushing account publicly. This repository has **no remote**
until the maintainer creates the public one with the org's app: never add a remote, never push.
Never commit with `--no-verify` or another `core.hooksPath`.

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
