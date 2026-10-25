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

- **A hostd host must have a ZFS pool.** A VM's disk is a zvol, which
  Cloud Hypervisor boots through its EDK2 firmware, and a box's disk a
  dataset, mounted at the container's /workspace; a golden is a snapshot,
  an environment a clone of it.
- Unlike hangar's capture, which copies the source whole, hostd clones a
  snapshot directly: a golden is a clone of the disk's snapshot, promoted,
  so it outlives the disk it was captured from, which becomes a clone of
  it (`internal/hostd/driver/zfsstore`). A golden with clones is destroyed
  as `zfs destroy -d` does, once its last clone goes.

Not qcow2 backing files and overlayfs: VMs would be served, but a box's
volumes would need overlay mounts managed by hostd, which then grows, and the
semantics would differ from hangar's (a backing file cannot go first).

## Consequences

- main-workstream, or any other host, needs a zpool before it can run hostd.
