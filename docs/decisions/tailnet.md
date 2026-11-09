# A box or machine joins a tailnet as a node of its own

Status: accepted, 2026-11.

## Context

An agent's box or machine sometimes needs the tailnet itself: to reach a
service only the tailnet serves, or to be reached by a person's tools as
`<name>.<tailnet>.ts.net`. workstream mints a single-use auth key for it and
names the Secret holding it in the box's or machine's `spec.tailnet`. The
guests are otherwise fenced from the tailnet (`traffic.md`,
`deploy/hostd/guest-network.nft` drops what a guest bridge sends to
`tailscale0`), and the hosts' and system containers' own nodes are not
theirs to share.

## Decision

- **Joining is a provider primitive.** tower's controllers say "this
  instance joins with key K as hostname H" (`layers.md`); each provider
  does it its own way and reports `TailnetJoined` on the Instance, which
  tower copies onto the Box. tower holds no OAuth client and knows no
  tags: the key decides what the node may do, and workstream retags a
  device through Tailscale's API. A join that fails is a condition, never
  a reason to hold a box or machine back from READY.
- **A tailnet comes and goes with the spec.** An agent may find mid-work
  that its box needs the tailnet, or no longer does: `spec.tailnet` is
  mutable, and a change is applied to the running box or machine. Added,
  it joins; removed, its node logs out, so the tailnet forgets it, and its
  state goes, so a later add is a new node with a new key; a new hostname
  or `accept_routes` is set on the node as it runs. `TailnetJoined` goes
  with `spec.tailnet` (no `Removed` reason): a box that joins no tailnet
  says nothing of one, as one that never did. A box deleted logs its node
  out too, as far as it can; workstream also deletes the device, which
  tower does not count on.
- **The key is tower's for one call.** It stays in the Secret
  (`secrets.md`) and is opened for the call that hands it on alone: the
  Instance names the Secret, never the value. A hangar target's provider,
  which runs in tower, opens it before the Container names it
  (`provider.TailnetKeys`) and copies it into a `tailscale-auth` hangar
  Secret of that instance's, which `ContainerSpec.tailscale` names; the
  Secret goes once the Container no longer names it, or with the
  instance, whose Container leaves the tailnet before it is deleted.
  hostd is given it on the tunnel (`HostService.JoinTailnet`), and passes
  it on the stdin of `tower-guest tailnet-up` in the container. A
  machine's VM is given it on the stdin of the join over tower's SSH,
  which `tailscale up` reads as `--auth-key=file:/dev/stdin`: no argument,
  file or environment holds it. No log, event, status or disk of the host
  holds it; the hangar Secret is the one copy at rest, in the project
  hangar already holds the box's container in.
- **A key not joined with is given again.** While an instance says it has
  not joined for want of a key (`NoKey`), a key refused (`KeyRejected`) or
  a failure (`Failed`), tower gives it the key again: at once when the
  Secret's content, the instance's run or its spec changes, else once a
  wait after the last give passes, as long as a failed step's
  (`failures.md`). A give is no failure: it is recorded on the instance,
  one value gone with it, and the box says when the key is given again
  unless it joins. A machine's join tried and not joined is spaced out the
  same way, the one in its configure phase included; one that failed on
  the way (a download, a tailscaled not ready) is a failed step, tried
  again after its backoff.
- **Only tailscaled has NET_ADMIN.** A hostd box that joins gets
  `/dev/net/tun` and `NET_ADMIN` from Docker, and its container runs as
  root so `tower-guest init` can start `tailscaled` (TUN, `--accept-dns`
  off) before the image's entrypoint. Init then runs the entrypoint, and
  every exec the driver runs in the container, as root only long enough
  to drop `NET_ADMIN` from the bounding, effective, permitted, inheritable
  and ambient sets: the shims, which then become the image's user, and
  tower-guest's own subcommands (`give-dir`, `tailnet-up`, `tailnet-set`,
  `tailnet-down`), run by absolute path, which run nothing of the
  image's. Nothing the box runs changes its interfaces, routes or packet
  filter itself, and with `no-new-privileges` nothing gets the capability
  back. A box running as root still reaches tailscaled's socket, so it can
  change its own node's settings (`tailscale set`, `up`, `down`) as far as
  the key's tags and the tailnet's policy let that node: tower bounds the
  network, the tailnet's policy bounds the node. A box without `tailnet`
  is made as before. Docker gives a container its devices and capabilities
  only as it makes it, so a hostd box whose tailnet is added or removed is
  made again on the same disk: `/workspace` and its processes' records are
  kept, its processes end as in any restart, and what it changed outside
  `/workspace` goes with the old container. A new hostname or routes are
  `tailscale set` in place. A box stopped whose tailnet is removed, or
  that is deleted or swept, logs its node out only if the node has state
  (its `tailscaled.state`); then it is started in leave mode: a marker in
  a directory of hostd's, bound read-only into the container, has
  `tower-guest init` start tailscaled, log out and exit, running nothing
  of the image's.
- **A node's state is kept with its instance,** each provider where its
  instance lives and goes: hangar's as hangar keeps it; a VM's on its disk;
  a hostd box's in hostd's state directory, bound into the container and
  removed with it, not on its `/workspace` disk, which a boost's sync or a
  golden copies, and two nodes must never share an identity. hostd reads
  what the box writes there (how its join stands) as it reads a process's
  log: a regular file alone, never through a link, bounded in size and
  time. A machine's golden is scrubbed of any tailscale state its golden
  phase left; tower joins a machine only in its own configure phase.
- **DNS splits in the box.** tailscaled leaves the box's resolver alone.
  `tower-guest init` points the box's resolv.conf at a forwarder of its own
  on `127.0.0.53` (its other lines kept), which sends names under `ts.net`
  to MagicDNS (`100.100.100.100`, served by the node itself) and every
  other name to the servers the container had: Docker's embedded resolver
  on a workstream network, which answers peers' aliases and forwards to the
  public servers hostd gives guests. A query no server answers is answered
  SERVFAIL, so the asking resolver fails at once.
- **A machine joins over SSH, its DNS split by systemd-resolved.** A VM has
  no tower-guest init: tower, which already runs its configure phase as
  root over its SSH, installs the pinned tailscale if the VM has none
  (`deploy/hostd/versions.env`), enables `tailscaled.service` with its
  state on the VM's disk, and runs `tailscale up` only while the node has
  no state; later changes run over the same SSH (`tailscale set`; or
  `tailscale logout`, tailscaled stopped and disabled, its state deleted),
  with no restart. A VM's peers' aliases are single labels its
  systemd-resolved sends to the workstream network's resolver
  (`internal/guest/vmssh`); a tailnet whose DNS settings override the
  local resolver would take them from it if tailscale managed the VM's
  DNS. So a VM too joins with `--accept-dns=false`, and a drop-in of
  tailscaled's routes `~ts.net` alone to `100.100.100.100` on
  `tailscale0`: aliases resolve as before, MagicDNS names through the
  node. A VM without systemd-resolved running is left as it is, and
  resolves no MagicDNS name: reach its peers by their tailnet addresses.
  This is not yet checked on a real VM.

## Consequences

- A box's tailscaled sends WireGuard and DERP to public addresses, which
  the fence lets through as any guest traffic; the fence is not loosened,
  and MagicDNS never leaves the container.
- A key is single-use: a box or machine made again (a reclaimed box, a
  vanished instance), or whose tailnet was removed and added again, is a
  new node, and needs a new key in its Secret, which is given at once.
- A hostd host needs tailscale's static build beside hostd
  (`deploy/hostd/install-deps.sh`); one without it fails a box that joins.
