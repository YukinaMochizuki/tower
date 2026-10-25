# Box add-ons are workstream's, copied

Status: accepted, 2026-10.

## Context

A box's add-ons (BoxSpec.addons) are layers built on whatever image the box
resolved to: a repository's devcontainer, an explicit image, its own
ImageBuild. workstream has two, `browser` and `desktop`, each a
devcontainer Feature (`devcontainer-feature.json` and `install.sh`) under
`workstream: src/box/addons/<id>/`, and builds the layer itself
(`workstream: src/box/addon-image.ts`, `docs/decisions/box-addons.md`). A
box workstream opens on tower must come out the same as one it opens on
its own host.

## Decision

- **tower carries its own copies of workstream's add-ons** and builds them
  as an image layer on the resolved image. The copy was taken at workstream
  `978417c0350aab38ec5d279320d16f4fa411d895`, where the add-ons were last
  changed in `ba6786b1a04bb2039b499592d250ed1660d7f37a`; the commit adding
  `internal/devcontainer/addons` is that copy unchanged, so `diff -r`
  against workstream's `src/box/addons` shows every later change.
- **The layer is built as workstream builds it**, with the same subset of
  the Feature format: option defaults given to `install.sh` as upper-cased
  variables, `containerEnv` as `ENV`, the `_REMOTE_USER` variables, the
  add-ons installed in the order asked for, the base's `USER` restored
  after them, and an apt-based base checked before any installer runs.

Not fetched from workstream at build time: tower builds on the target,
which has no reason to reach workstream, and an add-on that changed under
a box would change what an image golden of the same inputs holds.

## Consequences

- A change to an add-on is made in workstream and copied here, or made
  here, saying in its commit whether workstream needs it too.
- An add-on's version in BoxImageInputs is a hash of its files, so a
  changed copy builds new images.
