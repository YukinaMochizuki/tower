# Every decision about an environment is tower's

Status: accepted, 2026-10.

## Context

workstream drives coding agents. Its own rule is that it records decisions
(facts) and never policy: there is no reconciler in it, and anything that "it
depends" goes back to its director, an agent. But keeping development
environments healthy is full of policy that is deterministic and must run in
the background: rebuild a golden when a devcontainer or a lockfile changes,
reclaim boxes when a disk fills up, keep warm boxes ready. Left to workstream,
it was done by a person archiving workstreams by hand when the disk was full.

The resources themselves come from more than one place: hangar (an IaaS of
microVMs) on one host or more, and plain hosts running Docker and QEMU.

## Decision

- **Three layers.** workstream states intent ("a box of repo X at ref Y with
  these add-ons"; "keep this box"). tower decides everything else and
  reconciles. hangar and hostd provide resources and decide nothing.
- **Policy is written once,** in tower's controllers, against a provider
  interface that hangar and hostd both implement. A provider never chooses:
  it creates, deletes, runs, connects, snapshots, clones and reports.
- **hostd is resources only.** On a host without hangar, hostd operates
  Docker, QEMU and ZFS and reports their state, following hangar's node agent
  (`hangar: docs/decisions/node-agent.md`): it dials tower for its whole
  control path, watches the objects placed on its host, and keeps no state
  of its own. It listens only for direct streams, on an address it is given
  (the host's tailnet one), each on a ticket tower issued for one instance
  and port (`traffic.md`).
- **Toward hangar, tower is an ordinary client** with a service token in one
  hangar project. hangar learns nothing of devcontainers, goldens or
  workstream.

Not policy in hostd: it would be written twice, once for hangar inside tower
and once in hostd, and hostd would grow into a second hangar.

## Consequences

- Every provider must offer the same primitives, which shapes the hosts:
  hostd hosts need ZFS (`hostd-storage.md`), and images come from one
  registry (`images.md`).
- workstream's own box and machine drivers retire as projects move to tower;
  a hook runner and crawl4ai stay local to workstream.
