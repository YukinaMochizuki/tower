# tower

tower is the control layer for development environments, between workstream
(which states intents) and the resources (hangar, or `hostd` on a host). Every
decision about an environment is tower's: where it runs, when its golden is
rebuilt, what is cached and reclaimed. Its API is declarative, Kubernetes-style
(Connect + protobuf), and its resource engine is copied from hangar on purpose.
One person maintains it, and agents are first-class users of both the code and
the API.

Status: early. The engine, authentication, secrets, the environment kinds
and their controllers, workstream's data plane (EnvironmentService), the
tunnel, hostd with its real drivers (ZFS, Docker, Cloud Hypervisor,
BuildKit, git) and the CLI exist; there is no hangar provider yet. `make
e2e` runs the whole of it on a fake host (`docs/runbook/demo.md`), `make
e2e-host` on a testbed's real one (`docs/runbook/testbed.md`).
`docs/architecture.md` describes the target.

## Layout

| Path | Contents |
|---|---|
| `proto/tower/<group>/<version>/` | API schema, the single source of truth for resources |
| `gen/go`, `gen/ts` | Generated code. Committed, never edited by hand |
| `cmd/` | Binaries: `tower-apiserver`, `hostd`, the `tower` CLI, and `tower-guest`, which hostd runs in every guest |
| `internal/` | Implementation, split by function rather than by binary |
| `tools/` | Our own codegen; `tools/tools.mod` pins third-party CLIs |
| `test/` | End-to-end harnesses; `test/testbed` the real-host tests |
| `deploy/` | What a host installs: units, scripts and pinned versions (`docs/runbook/deploy.md`) |
| `docs/` | See the index below |

Directories appear as the work reaches them. Only `gen/go` is importable from
outside; everything else is `internal/` and carries no compatibility promise.

## Commands

| Command | Does |
|---|---|
| `make gen` | Regenerate everything from `proto/` and SQL |
| `make check` | Lint, test, and fail if `make gen` would change anything. Run before every commit |
| `make e2e` | Hermetic end-to-end tests (fake providers and drivers) |
| `make e2e-host` | Real-host tests on a testbed, as root (`docs/runbook/testbed.md`) |
| `make build-deploy` | The binaries `docs/runbook/deploy.md` installs |
| `make dev` | Run the apiserver locally in dev auth mode |
| `make tools` | Print the pinned tool versions |

The only toolchains needed are Go and Node (versions in `go.mod` and `.nvmrc`).
Every other tool is pinned in the repo (`go.mod`, `tools/tools.mod`, or
`package.json`) and run through the Makefile; never install one globally.

## Rules that are easy to break

- Generated code changes only through `proto/` (or SQL) plus `make gen`.
- All text in the repo (code, comments, docs, commit messages) is English.
- Comments follow `docs/conventions/comments.md`.
- Policy lives in tower's controllers, never in a provider or in hostd
  (`docs/decisions/layers.md`).
- A secret's value never reaches an answer, a log, an error, or a spec of a
  lower layer (`docs/decisions/secrets.md`).
- A change to a package copied from hangar says in its commit message whether
  hangar needs the same change (`docs/decisions/engine-from-hangar.md`).
- Refer to other docs by file path, never by section number.

## Docs index

| Doc | Read it when |
|---|---|
| `docs/architecture.md` | You need the map: layers, components, data flow |
| `docs/decisions/` | You are about to question why something is the way it is |
| `docs/runbook/dev-env.md` | You run tower locally (`make dev`) and need the first admin token |
| `docs/runbook/demo.md` | You run tower and hostd on a fake host by hand, as `make e2e` does |
| `docs/runbook/testbed.md` | You run or change the real-host tests, or make a testbed |
| `docs/runbook/deploy.md` | You install tower, its registry or hostd on a host |
| `docs/conventions/comments.md` | You write or review a code comment |
| hangar's `docs/conventions/testing.md` | You write or change a test: its tools and examples hold here (`docs/decisions/testing-strategy.md`) |
| `docs/conventions/testing.md` | You run tests: how tower's runs differ from hangar's |
| hangar's `docs/checklists/new-resource.md` and `new-controller.md` | You add a kind or a controller; read "tower" for "hangar" |
| hangar's `docs/checklists/review.md` | You review a change, or before you ask for one; its node and guest sections do not apply |

When you add a doc, add it to this index.
