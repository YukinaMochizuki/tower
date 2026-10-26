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
- hangar serves no port-forward to an arbitrary port yet, and its service
  tokens are project-wide and long-lived, so a ticket scoped to one
  environment exists only on hostd for now.
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
