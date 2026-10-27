# What passes through tower, and what goes to a target directly

Status: accepted, 2026-10.

## Context

A box is used through many kinds of traffic: commands, file reads and writes,
copies between a box and a machine, image builds, an editor's WebSocket, a
forwarded port, ssh to a machine. Routing all of it through tower puts tower
on the path of every byte; routing all of it directly makes workstream hold
every target's credentials and leaves tower blind to which environments are
in use, which reclaim needs to know (hangar does not report execs).

## Decision

Traffic is split by who needs the bytes, not by size:

1. **Inside a target** (a copy between a box and a machine, an image build, a
   golden, a cache fill): tower orchestrates, and the bytes never leave the
   target. A workstream's environments are all on one target
   (`placement.md`), so such a copy is always inside one.
2. **Small data workstream consumes** (a command and its output, reading and
   writing files, search, assets, job logs): through tower. This is how tower
   learns when each environment was last used, and it keeps target
   credentials out of workstream for routine work.
3. **Long-lived or latency-sensitive streams** (an editor's WebSocket, a
   forwarded port, ssh to a machine): to the target directly, on a ticket
   tower issues for that one environment and port. hostd speaks the same
   data-plane shape as hangar, so workstream has one client for it.

## Consequences

- tower is on the path of every command a director runs, so it must stay up
  and sit close to workstream.
- Last use is kept in memory and written to status now and then, so commands
  do not move the store's revision.
- **A hangar target's direct streams pass through tower,** an exception to
  point 3. hangar forwards any port of a machine (`:portforward`), but its
  service tokens are project-wide and long-lived, so no client is given
  one: tower serves the stream on its own listener
  (`/streams/hangar/<target>/...`), checks the ticket it issued, and
  relays it to hangar's port forward with the project's token
  (`internal/provider/hangar`). Streams go to hangar directly once hangar
  has tokens scoped to one machine and port, short-lived; hangar does not
  take tickets tower signs, which would make it know tower
  (`layers.md`).
- **A hangar target's checkouts and pushes pass through tower,** a second
  exception, to point 1. Its mirrors are in its system container, on a
  network of its own, which hangar keeps apart from every workstream's
  and routes nothing between (a machine joins at most eight networks), so
  a checkout's tar, an origin's and a push's `.git` cross from one to the
  other through tower, read out of one and written into the other
  (`internal/provider/hangar`). Builds read a mirror where it is. The way
  out is a service one machine offers other networks of its project,
  which hangar does not have yet (hangar#219).
- **tower reaches root in a hangar target's VM with its own SSH, over
  hangar's port forward to the VM's port 22,** as hostd reaches its VMs
  over their address (`internal/guest/vmssh`). hangar's exec into a VM is
  an SSH login by hangar's apiserver as the image's default user, never
  root, which tower's guest operations need; and to hangar, tower is an
  ordinary user, which adds nothing to its catalogue or platform. The
  provider keeps one connection to each running VM, each operation a
  session on it, up to 60 at once (the VM's user_data sets its sshd's
  MaxSessions to 64, a drop-in hostd's VMs do not need), one more waiting
  for one to end; it makes the connection again once it drops, and closes
  one whose sshd stops answering its keepalives. A reconcile never
  connects: connecting waits for a VM's sshd, and a reconcile waits on
  nothing of a VM's. tower's key is the
  target's (`docs/runbook/deploy.md`), which cloud-init gives root; no
  private key is ever in a VM's user_data, which the project's readers
  read.
- **A hangar VM's host key is pinned as it first answers tower** (trust
  on first use), on its Instance (`status.ssh_host_key`), so it outlives
  tower's restarts; every later connection must present it, and one that
  presents another is refused, naming the instance and both keys'
  fingerprints, never taken in its place. hostd makes each VM's host key
  and gives it to the VM; a hangar provider could do so only through
  user_data, which the project's readers read, and every other way to
  learn the key (hangar's own record in the VM's status, which only its
  exec makes, or the VM's console) comes through hangar's API as the port
  forward does, so one that impersonates hangar defeats them all alike:
  the first connection trusts nothing beyond hangar. A VM made from a
  golden is a new cloud-init instance, which makes its own host keys, so
  each Instance pins its own; a stop and a start keep the disk, and the
  key. tower does not use hangar's VM exec, nor need its platform SSH CA.
- **A copy between two instances goes across their workstream's
  network,** from one guest to the other (tower-guest's send and receive),
  never through tower: a copy is always within one workstream, whose
  instances share a network.
- **A guest's egress is fenced on hostd.** A box (a container on its
  workstream's Docker bridge) and a VM (behind its launcher's NAT, on the
  same bridge) would otherwise reach every port the host listens on through
  their bridge's gateway, and the tailnet, tower and its registry included,
  through Docker's masquerade. The nftables table `tower_guest`
  (`deploy/hostd/guest-network.nft`) drops what a guest's bridge (named
  `twr*`, or Docker's default) sends to the host, the tailnet and any
  private address; the internet stays open, and guests resolve through
  public servers hostd gives them, not the host's resolver (which may be
  tailscale's, answering every tailnet name). Guests of different
  workstreams are on different bridges, which Docker keeps apart.
