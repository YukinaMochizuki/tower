# hostd hosts run on ZFS

Status: accepted, 2026-10.

## Context

The point of goldens is that an environment costs only what it changes: a
golden is made once, and every environment of it is a copy-on-write clone.
hangar does this with ZFS: a `CustomImage` is captured from a stopped Disk,
and new Disks are clones of it (`hangar: docs/decisions/custom-images.md`).
tower's policy uses one primitive for both providers: make a golden from a
stopped state, then clone environments from it.

## Decision

- **A hostd host must have a ZFS pool.** A VM's disk is a zvol and a box's
  volumes are datasets; a golden is a snapshot, an environment a clone of it.
- Unlike hangar's capture, which copies the source whole, hostd may clone a
  snapshot directly.

Not qcow2 backing files and overlayfs: VMs would be served, but a box's
volumes would need overlay mounts managed by hostd, which then grows, and the
semantics would differ from hangar's (a backing file cannot go first).

## Consequences

- main-workstream, or any other host, needs a zpool before it can run hostd.
