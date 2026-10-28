# A box's devcontainer is read as workstream reads it

Status: accepted, 2026-10.

## Context

A box of a repository runs an image made of the repository's
devcontainer. workstream makes one on its own host today
(`workstream: src/box/devcontainer.ts`, `src/box/docker.ts`), and a box
workstream opens through tower must come out the same for the same
repository. tower reads the repository from its mirror on the project's
target (`repositories.md`), and builds on the target (`images.md`).

## Decision

- **tower reads what workstream reads, and nothing else**
  (`internal/devcontainer`, `devcontainer.Real`): `.devcontainer/
  devcontainer.json`, else `.devcontainer.json`, as JSONC; `image`, or
  `build.dockerfile` with `build.context`, both relative to the file's
  directory; `containerEnv`; `remoteUser`, else `containerUser`;
  `onCreateCommand`, `updateContentCommand` and `postCreateCommand` (a
  string, an array joined by spaces, or named commands joined by `&&` in
  the order written); and
  whether the repository declares a mise toolchain (`mise.toml`,
  `.mise.toml`, `.config/mise/config.toml`, `.tool-versions`,
  `.devcontainer/mise.toml`, or `MISE_CONFIG_FILE` in `containerEnv`). It
  reads them at the box's ref, from the target's mirror
  (`Provider.ReadMirror`).
- **What workstream skips, tower skips:** a devcontainer's `features`,
  `build.args`, `forwardPorts`, `mounts`, `customizations`, and the image's
  own devcontainer metadata. workstream's one exception, docker-in-docker
  (a daemon in a `--privileged` container), has no place on tower, whose
  instances are never privileged: the feature is named and not installed.
- **Two substitutions differ from workstream's on purpose.**
  `${containerWorkspaceFolder}` and `${localWorkspaceFolder}` are
  `/workspace` as there; `${localEnv:NAME}`, which workstream fills from
  its server's environment, is empty: the apiserver's environment is not a
  box's to read.
- **The image is built on the target.** An `image` is resolved to its
  digest in its registry without pulling it (`Provider.ResolveImage`); a
  `build.dockerfile` is built from the mirror's work tree on the host, its
  context a directory of the tree and its Dockerfile anywhere in it, as
  `docker build -f` takes them, so the tree never passes through tower.
  On that base a layer adds the add-ons (`box-addons.md`) and the
  `remoteUser` as the image's `USER`, as workstream runs every command as
  it; a user that is not a user's name or a uid is refused before it
  reaches a Dockerfile. hostd gives `/workspace` to that user, a name
  looked up in the container's own `/etc/passwd`. The layer is built even
  when it adds nothing, so every image a box runs is in tower's registry.
- **`containerEnv` is the box's environment, as written,** as workstream's
  `docker run -e` gives it: `InstanceSpec.env`, set on the container, not
  an `ENV` of the image, which can write no newline. Its names must be
  variables' names and its values hold no NUL.
- **What identifies the image** (`BoxImageInputs`): the base by digest; the
  devcontainer file's content with the git objects of its Dockerfile and
  its build context's directory, so a change to any file the build can
  copy is another image and a change elsewhere is not; the add-ons with a
  hash of their files. A golden is built from the base digest its inputs
  name, though the tag has moved since.
- **Each box sets itself up, as workstream's do:** once its repositories
  are checked out, in `/workspace`, as the image's user, with the box's
  privileged secrets, `mise trust && mise install` if the repository has a
  toolchain, then `onCreateCommand`, then `postCreateCommand`, each through
  `bash -lc` and given 3 minutes (workstream's default for a box
  operation). A step that fails does not fail the box, which workstream
  keeps too: the box is READY and its `SetUp` condition says which step
  failed and how.
- **A box's caches live in its `/workspace`** (decided with the human,
  2026-10-28): the box's environment (`InstanceSpec.env`) points
  `GOCACHE`, `GOMODCACHE`, `npm_config_cache`, `npm_config_store_dir`,
  `PIP_CACHE_DIR`, `UV_CACHE_DIR`, `BUN_INSTALL_CACHE_DIR`,
  `PUPPETEER_CACHE_DIR` and `MISE_DATA_DIR` at directories of
  `/workspace/.ws-cache/`. The list follows workstream's
  (`workstream: src/box/build-cache.ts`), with mise's tools added, and the
  checkout at `/workspace` lists `/.ws-cache/` in `.git/info/exclude`, so
  git never sees them. A variable the devcontainer's `containerEnv` or the
  image's own `ENV` sets is left alone, a cache placed on purpose not
  redirected: `npm_config_*` in any case, as npm reads them, and
  `MISE_DATA_DIR` and `PUPPETEER_CACHE_DIR` also when `XDG_DATA_HOME` or
  `XDG_CACHE_HOME` places their defaults. Why: the targets' NVMe is short,
  and most of a box's space is build output and caches; on the container's
  root disk they were rebuilt in every box and never in a warm golden. In
  `/workspace` they grow one disk, and a warm golden, a disk of
  `/workspace`, carries them. This holds on hostd targets too.
- **mise's and puppeteer's directories hold tools, not only caches**
  (decided with the human, 2026-10-28): an image may ship them at their
  default places (`${XDG_DATA_HOME:-~/.local/share}/mise`,
  `${XDG_CACHE_HOME:-~/.cache}/puppeteer` of the image's user), which the
  variables would hide. So every box and builder, once its repositories
  are checked out and before anything installs, looks as the image's
  user: where the default place holds anything and the directory under
  `/workspace/.ws-cache/` is not there yet, that directory is made a link
  to the default place, and the image's tools stay where they are, on the
  root disk, outside a warm golden. Not decided before the instance is
  made, from the image: its ENV is in its registry's config, its files
  only in its layers, which neither provider reads without pulling it;
  and a variable set from a login shell's profile would reach `bash -l`
  alone, not every command a box runs.
- **A warm golden is pre-built** (decided with the human, 2026-10-28). Its
  builder, made as a box of the image is, checks the repository out and
  runs, in `/workspace`, as the image's user, with the box's environment,
  each through `bash -lc`: `mise trust && mise install` if the repository
  declares a toolchain; the installs its files call for, `go mod download`
  for `go.mod`, `npm ci` for `package-lock.json`, `pnpm install
  --frozen-lockfile` for `pnpm-lock.yaml`, `yarn install --frozen-lockfile`
  for `yarn.lock`, `uv sync` for `uv.lock`, each skipped where the image
  lacks its tool; then the devcontainer's `updateContentCommand` if it
  declares one, else `make build` if the repository's root Makefile (the
  one make reads of `GNUmakefile`, `makefile`, `Makefile`) has a `build`
  target, else nothing more. `onCreateCommand` does not run in the
  builder, so an `updateContentCommand` must not depend on what it does.
  Each step may run 30 minutes (a default of tower's configuration, not a
  target setting). One whose output says `no space left on device` runs
  again after 45 seconds, three of hangar's looks at a disk that grows by
  itself, as long as `/workspace` grew in the meantime (`df` in the
  builder), and at most as many times as the disk takes to double from
  its start to its max, plus one. A step that fails does not fail the golden,
  which is captured with what did install: its `PreBuilt` condition says
  which step failed and how. Its inputs (`BoxWarmInputs`) are the
  repository's mise files and lockfiles by their git objects and a hash of
  the pre-build, the commands it runs and the root Makefile, so a change
  to either makes another golden at the next rebuild, whose cadence is the
  target's `warm_max_age` and `warm_max_commits_behind`. The pre-build is
  what a warm golden adds: a box cloned from one starts with it, and a box
  without one, on a blank disk, starts without it and runs none of it, so
  a build that fails never fails a box and opening a box never waits for a
  build. Either sets itself up as below. Repositories duplicate what they
  install, bounded by the warm budget.
- **A repository without a devcontainer falls back, as workstream's setup
  "auto" does:** to the box's `fallback_image`, which workstream sets to
  the repository's `boxImage` or its own default, else to tower-apiserver's
  `--default-box-image`. The image is made as a box of it named explicitly
  would be, add-ons layered on, and sets nothing up. workstream's setup
  "image" is `BoxSpec.image`.
- **A devcontainer tower cannot make a box of falls back too, as workstream
  falls back:** one that does not parse or breaks what reaches a container,
  one that names neither `image` nor `build.dockerfile`, an image its
  registry lacks, a build from outside the repository, and one whose image
  does not build. The box is made of the image to fall back to, sets
  nothing up, and its `DevcontainerFellBack` condition says why, so a human
  and workstream see it. Only with no image to fall back to does the box
  fail (`DevcontainerUnresolvable`, or its image's build failure), saying
  why.
- **A box fallen back from an image that did not build asks for that image
  until it builds,** for the boxes made after it: its status names the
  failed golden (`devcontainer_golden`), its `DevcontainerFellBack` says
  why it failed, when, and when it is built again (reason `BuildFailed`),
  then `Rebuilding`, then `BuiltSince`. The golden is built again when its
  backoff passes, or at once when the box is retried
  (`docs/decisions/failures.md`). The box itself stays on the image it fell
  back to: moving it would lose what is in it, so a person or workstream
  makes a new box instead.

Not workstream's code run by tower: tower is Go and builds on a target it
reaches through a provider, and workstream's builder runs Docker on its own
host. The port is kept to workstream's behavior line by line, and its tests
are workstream's cases.

## Consequences

- A change to what workstream reads of a devcontainer is made in both, as
  a change to an add-on is.
- A Dockerfile's own `FROM` by tag is resolved when the image is built; a
  tag that moves later does not build it again, as on workstream's host.
- workstream's per-repository build cache volume (`workstream:
  src/box/build-cache.ts`) has no shared counterpart: a box's caches are
  its own, in its `/workspace`, filled from its warm golden. A cache
  shared by a repository's live boxes would be a new channel between
  them, which needs a decision of its own. Nothing trims them but the
  toolchains themselves (Go drops build cache entries unused for five
  days); a disk that grows is bounded by its max.
- The caches are inside the checkout, so what works on the whole tree
  reaches them: `git clean -fdx` deletes them, and mise's tools with them
  where they are not the image's; Go's module cache is read-only, so `rm
  -rf` or `git clean` of it partly fails (`go clean -modcache` empties
  it); and `docker build .` at the root sends them as context unless
  `.dockerignore` lists `.ws-cache`.
- Known limitation: where the image ships mise tools at the default place,
  `/workspace/.ws-cache/mise` is a link to it, so the builder's `mise
  install` writes the repository's tools through the link onto the
  builder's root disk: they are not in the warm golden, and every box
  cloned from it installs them again in its setup.
- Known limitation: `git clean -fdx` removes that link too. The image's
  own tools then stay hidden from mise and puppeteer until the box is made
  again, since the link is made only before anything installs in a new
  box.
- A box on a target that grows disks (hangar's) starts small and grows as
  it fills, its `/workspace` disk and its container's root disk each up
  to a max, as the target's settings say (`TargetSettings`
  `box_workspace_disk`, `box_root_disk` and their `_max`). A warm golden's
  builder's first `/workspace` starts as a box's (decided with the human,
  2026-10-28), so the golden, and every box cloned from it, is as large as
  the build needed; a pre-build that outruns the disk's growth is run
  again as above. A rebuild starts a quarter over what the repository's
  last warm golden on the target held.
- Boxes read the image's ENV through the target (`ImageConfig.env`): a
  hangar target's system container and a hostd host from before this
  change return none, and every cache variable is set. Upgrade the system
  image, then hostd, before relying on it (`docs/runbook/deploy.md`).
