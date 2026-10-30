# Heavy jobs run on a boost, not in a larger box

Status: accepted in part, 2026-10-29; one box, 2026-10-30. The boost
itself (its kind, its life, the pending answer, the settings and the
queue) and its sync (the box's files sent before each job, what a job
changed written back once it ends), under "Built" below, are accepted
and built; routing by the records is a draft for review, not yet built.

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
- **To an agent there is one box** (decided with the human,
  2026-10-30). A boost only means a job runs on a larger machine: when a
  boosted job ends, the box's `/workspace` is what it would be had the
  job run in the box. Before each job the boost's `/workspace` is made
  the box's, and once it ends what it changed is written back to the
  box: every file, ignored ones too (build output, `node_modules`,
  `bin/`), never `.git` (git's own state keeps its own sync) or the top
  `/.ws-cache` (each instance's caches). The bytes go through
  `Provider.Copy`, staying on the target (`traffic.md`). What does not
  come back: the job's processes and ports, `.ws-cache`, a file past the
  cap, a path the box changed while the job ran (the box wins), and
  git's state: a commit or `git add` the job made on the boost is said
  in its exit, not brought back, the one exception to "as if it ran in
  the box" (a later change may bring commits back).
- **A job's output and exit come back as any process's**
  (`EnvironmentService`), so workstream's job tools work unchanged; its
  exit also says what came back (`BoostRun.back`).
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

## Built

What is accepted and built, in the API's terms:

- **A `Boost` kind** (`proto/tower/environments/v1alpha1/boost.proto`),
  named as its box and owned by it, so a box has at most one and deleting
  the box deletes it. tower alone writes it; workstream reads it. Its
  status says its phase (PENDING, STARTING, READY, FAILED), its instance
  and alias (`<box>-boost`), the goldens it was made from, when it was
  last used, its processes running, and when the box's setup steps
  started and ended in it: a slow devcontainer setup is why a boost is
  slow to start. It is made of its box's image and its box's warm golden
  while that is still ready (else the newest ready one), with the box's
  disk, env, checkouts, secret files, setup steps and privileged
  secrets; its cpu and memory are its own. Its status is made again from
  its Instance at each reconcile.
- **`StartProcessRequest.boost`**: unset runs a process in the box, as
  before; `on` asks for the target's `boost_resources`; `resources` asks
  for a size, held to `boost_max` and its vCPUs moved into hangar's
  shapes. A boost smaller than asked is deleted and made again once
  nothing runs on it, never resized.
- **A boosted start never waits.** It answers at once: started, with the
  process's id and `ran_on`, when the box's boost is READY and large
  enough; else `pending`, with a ticket, a reason (`QUEUED_BUDGET`,
  `BOOST_STARTING`, `BOOST_BUSY`, or `BOOST_GOING` while the box's boost
  is being deleted, with no estimate), a message, its place in the queue
  and about how long. The caller asks again with the ticket, or gives it up
  (`cancel`); asking again keeps the ticket's place. A ticket not asked
  about for five minutes is dropped; while one waits for its boost to
  start, the boost is not reclaimed. A boost that cannot fit the Plan
  even with no boost held is refused at once (`FAILED_PRECONDITION`):
  the Plan's quota left as reported, plus every boost the report may
  count (each whose instance is there), up to the Plan's whole
  (`cpus`, `memory_total`). A start on a box whose boost is there or
  being made, of a size that suffices, holds its room already: it is
  never refused so, nor queued again, whatever the Plan says since.
- **Every process call on the box reaches its boost.** A process started
  on a boost has an id of tower's, `boost-<instance uid end>-<id there>`,
  so no id is used twice, a boost's instance made again included; tower
  keeps, per box, which instance runs it and its command's coarse key
  alone (`process-usage.md`). ListProcesses lists the box's and its
  boost's together, each with `ran_on`. A boost's processes keep its box
  in use. However a boost goes (idle, its box stopped or deleted, FAILED),
  a finalizer holds it until its processes' exits are read and kept and
  their usage recorded with `ranOn.kind = boost`; an attach after
  answers the exit, the output gone. A process whose exit could not be
  read, as one that still ran when its box was stopped or whose instance
  vanished, is reported ended, its exit `lost` with code 255, so a reader
  that does not look at `lost` takes it as failed.
- **Settings** on the Target: `boost_resources` (default 8 vCPU, 8Gi),
  `boost_max` (8 vCPU, 16Gi), `boost_budget` (16 vCPU, 16Gi),
  `boost_idle` (10 minutes) and `boost_start_timeout` (15 minutes: four
  devcontainer setup steps at their 3-minute bound, and 3 minutes for the
  instance's start and the checkouts). A boost fits when the target's
  boosts with it stay within `boost_budget`, or the target holds none
  (so a budget below one boost lets one at a time), and when it fits the
  Plan's quota left as the target reports it (`Capacity.memory_free`,
  and `cpu_free`, which the hangar provider reports from the project's
  limits and usage), less the boosts its last report does not count for
  sure: all but those whose instance was RUNNING before
  `last_report_time`. hangar counts a machine from when it is made, not
  when it boots, so a boost made is taken off the quota left twice
  until the first report after it booted (its image pulled): a start
  queued meanwhile may wait that long for room it could have had, and
  never takes room the Plan does not have.
- **The queue is in the apiserver's memory**, per target, admitted in
  order and stopping at the first that does not fit, so a large boost is
  not passed forever. An apiserver restarted loses it: a ticket it no
  longer knows is queued again at the back. The boosts themselves are in
  the store and survive it.
- **A boost does not hold room it does not use.** One not READY within
  `boost_start_timeout`, whose instance failed, whose box failed, or
  whose box's image golden is gone, is FAILED with the reason: its
  instance is deleted at once and its room freed; the box's next boosted
  start replaces it, and it is deleted after `boost_idle`. A READY boost
  is deleted once nothing ran on it for `boost_idle`, no start waits for
  it, and a listing of its processes succeeds and shows none running (a
  listing that fails, even as not found, keeps it for a later try); or
  at once when its box is stopped at workstream's request, whatever runs
  on it. A boost's job that fails or must try again later (a step
  waiting on the target, a retire that found it busy) is held back by a
  backoff from 100ms doubling to a minute, counted across its restarts.
- **Before each boosted process, the boost is made the box's.**
  tower-guest's subcommands, run through Exec as argv, never a shell
  line; every path they give git is a path, never a pattern
  (`GIT_LITERAL_PATHSPECS`), and each git call takes the checkout as a
  safe directory for itself alone. Each side walks its `/workspace`
  into a manifest: a record a path, of its type, mode, size, mtime (in
  nanoseconds) and a link's target, and, for that side alone, its inode
  and ctime. `boost-manifest` walks the boost's; tower copies it to the
  box, where `boost-pack` walks the box's and packs what differs:
  added, changed (type, mode, size or mtime), deleted, a link's target,
  read from the files as they are, with their modes and mtimes. Two
  records the same are the same file: a box and its boost are both made
  of the box's warm golden, so its files are the same from the start,
  and a file moved keeps its mtime, so is the same once moved. A file
  the box rewrote to the same size and mtime since its last sync, which
  its inode or ctime tell, is sent anyway. The box also sends git's
  state as before: its work tree's tree (as trees alone), what is
  staged apart from it, and a bundle of HEAD's commits no `origin` ref
  reaches. tower copies the pack to the boost (`Provider.Copy`),
  fetches the mirror there (`UpdateOrigin`), and `boost-apply` deletes,
  then writes each file whole beside its path and renames it on, with
  its mode and mtime, and links verbatim, wherever they point (a venv's
  `bin/python -> /usr/bin/python3` stays one); all through an
  `os.Root` of the checkout, and a directory in a path's way that is a
  link in the boost is replaced, so nothing is ever written or deleted
  through a link. Then it checks its work tree, as git sees it, against
  the box's tree: a tracked file whose content differs though its
  records are the same is sent again once, by path; if it still
  differs, the start fails (`INTERNAL`), saying so. Then HEAD is the
  box's commit on its branch, and the index the box's, so `git status`
  and `git diff` say there what they say in the box. A sync sends up to
  2GiB: a file past it is not sent, is listed in the exit
  (`sync_over_cap`), and the job runs; git's state past it (staged
  content, commits) is refused (`FAILED_PRECONDITION`). The box keeps
  each sync's manifest as sent (`sent-<id>`) while a process synced by
  it is not settled, and its last (in the `BoostSync` record); each
  staging directory is removed after use, and what a crash leaves is
  swept after a day. This replaces 2/4's restore: what only the boost
  changed, a job's leftovers, is undone by the boost being made the
  box's.
- **Submodules and nested repositories** move as any files; their
  `.git` never does (`submodules_not_synced`), and the boost's check
  takes the tree's gitlinks for whatever the boost holds there.
- **A sync beside another process of the box on the boost**
  (`base_shared`) sends only what the box changed since its last sync,
  compared with that sync's manifest, so nothing that one writes is
  deleted or reverted; the work tree is not checked. Without a last
  sync tower knows, the start answers `PENDING_REASON_SYNCING` until the
  running process ends. Whether one runs is read from the boost's
  processes when the sync applies, under the box's sync lock, which a
  start holds as it starts its process: no start is between a sync's
  look and its apply. A start being started counts as a process that
  runs too, whether or not it holds the lock. A start whose sync another
  start's sync overtook, or that ended more than 40 seconds ago (the
  10-second wait and 30 seconds for the caller to ask again), syncs
  again before it starts. A sync that is not shared first settles the
  box's ended processes on the boost, so what they wrote reaches the
  box before the sync undoes it there; if that settling fails, the sync
  is shared and what they wrote stays.
- **A sync answers within 10 seconds or pends.** It runs as a job,
  holding the box in use; a start whose sync is not done in 10 seconds
  answers `PENDING_REASON_SYNCING` with its ticket, and starts from that
  sync when asked again. Its wait for the box's sync lock, behind another
  start's sync or start, counts in the same 10 seconds: past them it
  answers `PENDING_REASON_SYNCING` too. A caller asks again at once on
  `SYNCING` (each ask waits up to 10 seconds), and no later than 30
  seconds after, or its sync is made again. A box tower stopped for
  idleness is woken first, as by any data-plane call. The sync's time,
  files, deletions and bytes are in the start's answer and the
  process's exit (`BoostRun`).
- **A ticket that started its process answers that process.** Asked
  again within the ticket's 5 minutes after its start, as by a caller
  that lost the answer (a restart, a dropped connection), a ticket is
  answered the same process and `ran_on`, never a second start; while
  its start is under way, `PENDING_REASON_SYNCING`. The record is the
  apiserver's memory: one restarted meanwhile starts it anew.
- **A boosted process runs under `tower-guest boost-job`**, in its
  process's group and cgroup, which walks the boost's `/workspace`
  before its command starts and once it ends: what differs, by type,
  mode, size, mtime, inode or ctime, is what the job added, changed or
  deleted, kept for its write-back; it reads git's HEAD and a hash of
  its index at both ends too. It exits as its command did, by the same
  signal. Both walks run in the process's cgroup: a boosted process's
  peak memory, CPU time and wall time include them
  (`process-usage.md`).
- **Once a boosted process ends, what it changed is written back** to
  the box, at its settle, before its exit is answered: at the first
  read of its exit, by the boost controller once it ends, before a sync
  undoes it on the boost, or before its boost goes; once, the settle
  kept before its report is removed. `boost-back` packs on the boost
  each path the job added or changed as it is then, with its mode,
  mtime or link target, and what it deleted, up to 2GiB in path order
  (a file past it stays, listed in `over_cap`); tower copies it to the
  box, where `boost-writeback` applies it through an `os.Root` of the
  checkout. The box wins: a path the box changed after the job's sync
  began, as that sync's manifest of the box says, by any record field,
  inode or ctime too, or one under a directory the box removed or made
  a file or a link, stays the box's and is listed in `conflicts`; a
  directory the job removed goes only if the box put nothing in it
  since. Nothing is written through a link. Files keep the boost's
  mtimes, so the next sync finds them the same. The exit says what came
  back (`BoostRun.back`): files, deletions, bytes, the first 200 paths
  with the count, conflicts, what was past the cap, the time it took,
  and whether the job moved git's HEAD or changed its index. A box not
  running when the process is settled gets nothing back, and its exit
  says so; a process started before this tower, whose sync kept no
  manifest, likewise. A box keeps the paths' list for its last 10
  processes, the count for all.
- `test/boostcheck` checks the above on a real tower and hangar target
  (`docs/runbook/testbed.md`).

## Consequences

- Boxes can be small: their memory is what an agent's session and
  services use, not their heaviest job.
- A boosted job pays the boost's start (about 15 seconds) once per burst,
  and a sync of what changed before each job.
- A job's writes come back once it ends, a file at a time: a large
  build output costs its bytes each way, up to 2GiB a move; past that,
  files stay where they were, said in the exit. A change in place to a
  large file moves the whole file.
- A boost's caches (`/.ws-cache`) start from the warm golden's and are
  lost with it; what a job puts there does not reach the box's.
- A commit or `git add` a job makes on the boost does not come back,
  though its files do; the exit says so.
- Routing is only as good as the records: the first heavy run of a
  command may OOM in the box.

## Not decided yet

- What workstream's tools take and show: the `boost` argument of a job,
  and how a pending start, usage and what came back read in a job's result
  (usage on `await_job` is asked for). The API they call is above.
- Whether a boost runs Docker (docker-in-docker works in a hangar guest,
  but tower starts no dockerd today), which would move e2e that needs
  Docker or k3s from machines to boosts.
- The thresholds above, to be set from the records once tower keeps them.
