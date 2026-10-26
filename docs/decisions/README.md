# Decisions

One file per decision: the context that forced it, what was decided, and what
follows from it. Read the relevant one before proposing to change something it
covers; if the context has genuinely changed, update the file in the same PR as
the change.

| File | Decision |
|---|---|
| `layers.md` | workstream states intent, tower makes every decision, hangar and hostd only provide resources; hostd is a node agent that keeps no state |
| `placement.md` | All of a project's environments run on one target, set by an administrator; tower holds the targets and never spills a project elsewhere; a target goes once what tower made on it is gone, or an admin abandons it |
| `declarative-api.md` | Clients apply intents and watch status, so a reclaimed box keeps its identity; policy is controllers; commands and files are RPCs on a resource |
| `engine-from-hangar.md` | The resource engine is copied from hangar at a recorded commit, keeping hangar's approach, so it can be extracted later; which of hangar's decisions hold unchanged |
| `traffic.md` | Traffic inside a target stays there; small data workstream consumes passes through tower; long-lived streams go to the target on a ticket |
| `images.md` | One registry beside tower; images are built on the target when tower decides |
| `hostd-storage.md` | hostd hosts run on ZFS: goldens are snapshots, environments clones |
| `failures.md` | A failed golden or instance step waits out a backoff (10 min doubling to 6 h) kept across restarts, its object FAILED saying why and when it runs again |
| `devcontainers.md` | A box's devcontainer is read from the target's mirror as workstream reads it, built on the target; each box runs its setup steps; one tower cannot resolve fails the box |
| `box-addons.md` | A box's add-ons are workstream's Features, copied at a recorded commit and built as a layer on the resolved image as workstream builds it |
| `machine-blueprints.md` | A machine's golden is its playbook but the tasks tagged configure, which each machine runs; every VM has the hostname `machine` |
| `repositories.md` | tower mirrors repositories with its own credentials and pushes branches after workstream's approval; pull requests stay workstream's |
| `secrets.md` | Every product secret is tower's, sealed at rest; a secret reaches only the environment that uses it; privileged boxes run through tower |
| `authentication.md` | Service tokens with one fixed role each (admin, workstream, hostd); root on the host through a unix socket; tailnet only |
| `deployment.md` | tower and the registry run on a dedicated machine on the tailnet |
| `testing-strategy.md` | hangar's test layers and rules; fakes behind every outside dependency, kept honest by conformance suites |
