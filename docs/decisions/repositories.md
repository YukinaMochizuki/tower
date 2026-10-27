# tower reads repositories itself, and pushes branches

Status: accepted, 2026-10.

## Context

Until now workstream copied a repository into a box from its own clone cache,
and the box never reached a forge; a separate, short-lived publish box pushed
a branch, because the commits lived in the work box. With boxes on other
hosts, there is no local copy to make. To decide when a golden is rebuilt,
tower must itself read a repository's devcontainer, lockfiles and tool
versions at a given ref.

## Decision

- **tower holds its own forge credentials and keeps a git mirror on each
  target.** It resolves devcontainers (`devcontainers.md`), builds images,
  and may bake a recent checkout into a golden, so a new box only fetches
  what changed; what a box installs it installs itself, as
  `devcontainers.md` says.
- **workstream applies a `Repository` per product repository** (its URL and
  which credential reads it), so tower knows them before any box asks and can
  pre-warm.
- **Repositories hosted by workstream itself** are served to tower over the
  tailnet, to tower's identity alone; to tower they are one more git source.
- **A box's `origin/*` comes from tower's mirror,** and refreshing it is
  tower's operation. workstream keeps its own clone cache for its own reading;
  the two may briefly disagree.
- **tower pushes branches.** A push follows the commits, which now live on the
  target. tower fetches them from the box into its mirror and pushes from
  there; a token never enters a work box. A human's approval stays in
  workstream: workstream calls tower's push only after it. Push takes only
  workstream's role, names its branch, and forces only with a lease. Pull
  requests (opening, editing, merging, reading checks) stay workstream's.
- tower's write token is the narrowest the forge allows (on GitHub, a
  fine-grained token with contents:write), not workstream's own.
- **On a hangar target, the mirrors are in its system container**
  (`internal/system`), on its disk, kept by the same git driver as on a
  hostd host; a token reaches only the one git command there that needs
  it, as on a host. Its git reaches the repositories through the
  container's tailscaled's HTTP proxy (`TOWER_SYSTEM_GIT_PROXY`, which
  the system image sets), as the container is on the tailnet in
  userspace alone: tailnet names, such as workstream's own git service,
  resolve there, and the proxy dials GitHub and the like directly. The
  proxy never sees a token because a Repository's URL must be https
  (the apiserver's `validateRepository` refuses any other): git reaches
  it through the proxy as a CONNECT tunnel, TLS inside. Over plain
  http the token's header would reach the proxy in the clear. hostd's
  git has no proxy. A box takes a checkout or an origin from the
  container's server, and posts a push's `.git` to it, over hangar's
  endpoint service, each on a single-use credential tower grants for
  that operation and mirror alone, from the box's network alone, for a
  mirror of the box's project alone; where the endpoint cannot carry
  it, tower relays it (`traffic.md`).

## Consequences

- tower holds write credentials, and needs the same care as the workstream
  host.
- workstream needs a small git service for the repositories it hosts.
