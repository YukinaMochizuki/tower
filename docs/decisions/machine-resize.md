# A machine is resized by restarting it at its new size

Status: accepted, 2026-11.

## Context

An agent sizes a machine as it provisions it (workstream's
`provision_machine`, `MachineSpec.resources`) and later finds it too small
or too large. `resources` was never immutable, but the machine controller
copied it into the machine's Instance only as it made it, so a change did
nothing and said nothing.

What the two providers can do decides most of the rest:

- hangar restarts a VM whose cpu or memory changes, on the disk it has; it
  checks its node has room for more memory, and if not keeps the VM running
  at its old size, its Ready False `ResizeDoesNotFit`. Its quota and
  machine shapes refuse a size as it is applied. A Disk only grows; a VM
  whose root Disk grows is restarted, its guest growing the filesystem at
  boot.
- hostd's Cloud Hypervisor VM is a container running a launcher; its vCPUs
  and memory are fixed when it boots, and nothing hotplugs. Its root disk
  is a ZFS volume, which can grow, never shrink, and which the guest sees
  from its next boot.

Neither can resize a running VM in place.

## Decision

- **A ready machine given a new size restarts at it**, now, not at its next
  start. Its disk, and so what its configure phase did, stay: configure
  does not run again. Its ready check runs again, as after any start (the
  instance's run changes). What ran in memory, its processes included, is
  gone, as after a hibernate; workstream starts its recorded services
  again after any restart that applies a size, one it asked for or one
  tower applied later once there was room. While the restart is under way
  the machine is CONFIGURING, its Ready and Resized conditions `Resizing`.
- **A stopped machine takes the new size and stays stopped.**
- **A field left empty keeps the size the machine has**; tower's defaults
  apply only as a machine is made, so a change of a default never restarts
  machines. A field the spec asks keeps the instance's when it is the same
  by value ("4Gi" is "4096Mi").
- **A disk only grows.** A smaller disk is refused, Resized False
  `DiskCannotShrink`, while the vCPUs and memory asked with it are applied.
  Admission does not refuse it, which would refuse the rest as well.
- **A size the target has no room for keeps the machine at the size it
  has**, READY, Resized False with the target's reason and words: hangar's
  `QuotaExceeded`, `Refused` (a machine shape it does not allow) or
  `ResizeDoesNotFit` (its node has no memory for the machine, or no disk
  for its root Disk's growth, which hangar's disk controller holds
  `Allocated` False); on hostd, tower's own `ResizeDoesNotFit` for more
  vCPUs than the host has, or, for a running machine, more memory than the
  host reports free (a stopped one asks for its memory as it starts, as
  any hostd instance does), and the host's `NoSpace` for a disk its pool
  cannot grow. The spec keeps the new size and it is applied once there is
  room: hangar retries by itself; tower checks a hostd host again at each
  resync of the machine (five minutes).
- **A refused size never fails a VM that exists.** The hangar provider
  applies the rest of a VirtualMachine's spec at the size it has when
  hangar refuses the size, so a machine started, or changed otherwise,
  while a resize is refused starts at its old size; only a new VM hangar
  refuses is FAILED. A provider's Resized speaks for the Instance
  generation it observed: a refusal of an older size says nothing of one
  asked since.
- **What the provider applied is the Instance's `status.resources`**, which
  the machine copies: a VM's vCPUs and memory as it runs (or, stopped,
  will boot) with them, and its disk's size. A resize is under way while it
  differs from `spec.resources`; the provider's condition `Resized` on the
  Instance, False, says why it is not applied, and True says nothing holds
  it (reason `Applied` once the size runs; hangar's `Applying` while a Disk
  just grown waits for its disk controller, during which the machine reads
  CONFIGURING, `Resizing`, for that interval; a growth the controller then
  holds turns into Resized False, `ResizeDoesNotFit`). The machine's `start_time` is
  its instance's: a restart, for a size or otherwise, moves it, which is how
  workstream tells a size applied by a restart from one applied without.
  The machine's `Resized` is False `NotReported` while its target has
  reported no size (a provider older than this).
- **The check is the controller's, the work the providers'**
  (`layers.md`): tower decides whether a size goes to the Instance; hostd
  grows the disk while the VM still runs (a pool without room for it
  stops nothing, and says `NoSpace`), then stops the VM for new vCPUs or
  memory or a grown disk, remakes its container with the new limits, and
  the converge that follows starts it; the hangar provider writes the
  VirtualMachine and the root Disk and reports what hangar did. A hangar
  VM's vCPUs and memory are reported applied once its node runs its
  machine's spec, or, stopped, once that spec is written.
- **A VM whose guest went while its disk stayed is made again on it.**
  chvm remakes a VM's container to resize it; a resize cut short between
  removing and making it, or hostd stopping there, leaves the disk, which
  holds all the VM is. hostd makes the guest again from the Instance's
  spec rather than report it GONE, which would have tower delete the
  disk. A container whose guest went is still GONE: what it held outside
  `/workspace` went with it.

## Consequences

- A resize interrupts the machine. workstream asks a person before every
  one, its card saying the machine restarts.
- A disk once grown stays grown, on both providers.
- On hostd, the disk grows while the VM still runs, then the VM restarts:
  a hostd that stops between the two leaves the guest to see the larger
  disk at its next boot, though the status already reports it.
- hostd's memory check is against what the host reports free, which
  counts nothing a stopped machine will ask for as it starts: starting
  several stopped machines can still overcommit a host, as before.
- Boxes are not resized this way; a boost is their larger size
  (`boost.md`).
