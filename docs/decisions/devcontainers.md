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
  `onCreateCommand` and `postCreateCommand` (a string, an array joined by
  spaces, or named commands joined by `&&` in the order written); and
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
  On that base a layer adds the add-ons (`box-addons.md`), the
  `containerEnv`, baked in as `ENV` with `$` escaped so a value is taken as
  written, as workstream's `docker run -e` takes it, and the `remoteUser`
  as the image's `USER`, as workstream runs every command as it. hostd
  gives `/workspace` to that user, a name looked up in the container's own
  `/etc/passwd`. The layer is built even when it adds nothing, so every
  image a box runs is in tower's registry.
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
- **A warm checkout installs nothing.** Its inputs are the repository's
  mise files and lockfiles, by their git objects; what workstream installs,
  mise's tools and the devcontainer's commands, reaches outside
  `/workspace` (mise's tools live in the user's home), which a warm golden,
  a disk of `/workspace`, does not hold, so every box runs its own setup.
- **A devcontainer tower cannot make an image of fails the box at once**
  (`DevcontainerUnresolvable`), saying why: none in the repository, one
  that does not parse, one that names neither `image` nor
  `build.dockerfile`, an image its registry lacks, a build from outside the
  repository. Where workstream falls back to a plain image, tower does
  not: the box asked for this one, and workstream asks for a plain image
  itself (`BoxSpec.image`) when it wants one.

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
  src/box/build-cache.ts`) has no counterpart: an instance has one disk,
  and a cache shared by a repository's boxes would be a new channel
  between them, which needs a decision of its own.
