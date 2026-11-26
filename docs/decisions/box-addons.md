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

- **An add-on set's image is made ahead of its boxes, for the sets a
  repository is used with** (decided with the human, 2026-11-25). Each
  box's use record keeps the add-ons it asked for, when its image is its
  repository's devcontainer's. The warm controller makes the image golden
  of each set the repository's boxes asked for within the target's
  `warm_window`, of its branch's head, as a box asking for it would
  (`prebuildAddonImages`), once a new head has been the head for
  `warm_settle`, so a burst of commits builds once; the same head is
  looked at again each `warm_settle`, which finds a base tag moved under
  it. So a new base is built ahead, not in the next box: a browser layer
  on a new base took 3 to 9 minutes of a box's open. A set no box asked
  for within the window is not made, so a repository holds an image for
  each add-on set it is used with rather than one for every set there is.
  The set without add-ons is left to `keepWarm`, which makes it for a warm
  repository. A failed look is tried again at the next, not at every
  reconcile. Nothing reclaims these goldens: an image golden holds no disk
  on the target, so the golden budget does not weigh it, and tower deletes
  no image from its registry. Each used set leaves one Golden and one
  registry image per change of its repository's image inputs, until
  registry pruning exists.

Not fetched from workstream at build time: tower builds on the target,
which has no reason to reach workstream, and an add-on that changed under
a box would change what an image golden of the same inputs holds.

## Consequences

- A change to an add-on is made in workstream and copied here, or made
  here, saying in its commit whether workstream needs it too.
- An add-on's version in BoxImageInputs is a hash of its files, so a
  changed copy builds new images.
