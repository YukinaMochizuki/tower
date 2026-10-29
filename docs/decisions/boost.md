# Heavy jobs run on a boost, not in a larger box

Status: proposed, 2026-10-29. The direction was discussed with the human;
the details below are a draft for review, not yet built.

## Context

Boxes on a hangar target are sized for their heaviest job, and hangar
reserves a machine's memory in full and never overcommits it
(hangar: `docs/decisions/node-agent.md`, `machine-sizes.md`). Its CPUs are
shared, not reserved, but a tenant's Plan counts the vCPUs of its running
machines. Measured on hangar-ardge, 2026-10-29:

- The host has 32 CPUs and 125G of memory. Over four days its CPUs averaged
  89–92% idle, with no 10-minute sample below 52% idle; inside the boxes,
  steal time and CPU pressure were near zero. Memory, not CPU, is what
  runs out.
- Idle boxes use 230–660M of their 4Gi. Most jobs peak under 1.3Gi
  (tower `go test ./...` 1.0Gi, workstream `bun test` 0.7Gi, its
  typecheck 1.3Gi), but hangar's `go test ./...` peaks at 3.1Gi, and
  still at 3.0Gi with `-p 1`: one package needs it.
- A box never idle-stops while any process runs in it (`idle` in
  `internal/apiserver/boxes.go`). A service or the editor keeps it, and
  the memory it reserves, until it is deleted.

So a box sized for the worst job holds that memory all day. hangar cannot
resize a machine's CPUs or memory while it runs (it grows disks only).

## Decision

- **A boost is a short-lived instance of a box's, larger than the box,
  that heavy jobs run on.** It is made as a box's instance is
  (`makeBoxInstance`): the box's image golden, `/workspace` cloned from
  its first repository's warm golden, on the box's workstream network. Its
  size is chosen for the job, up to 8 vCPU, within hangar's shape rules.
  hangar targets only: a hostd box is a Docker container sharing its
  host, whose memory is a limit and not a reservation, and which can be
  raised while it runs (decided with the human, 2026-10-29).
- **One boost per box, reused, deleted when idle.** The first job routed
  to a boost makes it; later ones run on the same boost while it lives,
  with its caches warm. It is deleted, not stopped, once no job has run
  on it for the target's `boost_idle` (default 10 minutes), freeing its
  cpu, memory and disk. A run of heavy commands thus shares one warm
  boost without the agent holding a lease it could forget.
- **The box is the source of truth; the boost follows it.** Before each
  job, the box's work tree is written as a git tree with a temporary
  index (tracked and untracked files, gitignored ones left out), and only
  what changed since the tree last sent goes to the boost, through
  `Provider.Copy`, so the bytes stay on the target (`traffic.md`). The
  first send is against the warm golden's commit, or a whole tar when the
  box lacks that commit. Gitignored files never travel: build output,
  `.ws-cache` and installed dependencies are the boost's own.
- **Nothing comes back unless asked.** A job's output and exit come back
  as any process's (`EnvironmentService`), so workstream's job tools work
  unchanged. Tracked files the job changed on the boost are listed in its
  result and not copied back. Paths a job names as its artifacts are
  copied back to the box when it ends.
- **tower routes each job, from what it measured.** Every process on a
  hangar box runs in a cgroup of its own and reports its peak memory, an
  OOM kill, its CPU time and wait, and its wall time; tower keeps the last
  runs of each (repository, directory, command) (`process-usage.md`). A command whose recent
  peaks came within 80% of the box's memory runs on a boost of 1.5 times
  that peak. One that runs for minutes with the box's CPUs saturated runs
  on 8 vCPU. A command with no record runs in the box, and if it is OOM
  killed there the agent is told that its next run goes to a boost; it is
  never re-run by itself, since a command may have effects. A record of
  the program and subcommand alone (`go test`) only gives hints. The
  caller may ask for a boost, a size, or no boost.
- **Boosts are budgeted per target and queued.** The target's
  `boost_budget` caps the cpu and memory all its boosts hold, within the
  Plan's quota left. A job that does not fit is queued, first come first
  served across workstreams in turn, and its caller is told at once that
  it is queued and for about how long, so the agent decides whether to
  wait, run it in the box, or do something else (decided with the human,
  2026-10-29).

## Consequences

- Boxes can be small: their memory is what an agent's session and
  services use, not their heaviest job.
- A boosted job pays the boost's start (about 15 seconds) once per burst,
  and a sync of what changed before each job.
- A job that writes source files (code generation) must not be boosted:
  its writes stay on the boost. The result says which files it changed.
- A boost's caches start from the warm golden's and are lost with it;
  what a job builds does not reach the box's caches.
- A gitignored file a box made by hand is not on the boost.
- Routing is only as good as the records: the first heavy run of a
  command may OOM in the box.

## Not decided yet

- What workstream's tools take and show: the `boost` argument of a job,
  artifacts, and how queueing and usage read in a job's result (usage on
  `await_job` is asked for).
- Whether a boost runs Docker (docker-in-docker works in a hangar guest,
  but tower starts no dockerd today), which would move e2e that needs
  Docker or k3s from machines to boosts.
- The thresholds above, to be set from the records once tower keeps them.
