# Heavy jobs run on a boost, not in a larger box

Status: accepted in part, 2026-10-29. The boost itself (its kind, its
life, the pending answer, the settings and the queue) and its sync (the
box's work tree sent before each job, what a job changed, artifacts),
under "Built" below, are accepted and built; routing by the records is
a draft for review, not yet built.

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
- **Before each boosted process, the box's work tree is synced.**
  tower-guest's subcommands, run through Exec as argv, never a shell
  line; every path they give git is a path, never a pattern
  (`GIT_LITERAL_PATHSPECS`), and each git call takes the checkout as a
  safe directory for itself alone. `boost-pack` in the box writes the
  work tree as a tree through a copy of the box's index
  (`GIT_INDEX_FILE`; the box's own is never touched), tracked files, one
  an ignore rule matches too, and untracked ones git does not ignore (a
  box without an index tracks HEAD's paths), and packs what changed
  since a base, read from the object store, with modes and symlinks,
  the paths deleted, what is staged where it differs from the work tree, and a
  bundle of HEAD's commits no `origin` ref reaches. tower copies the
  pack to the boost (`Provider.Copy`), fetches the mirror there
  (`UpdateOrigin`), and `boost-apply` puts back what the boost's own
  jobs changed since the base, applies the pack, and checks the tree
  made is the one sent. Each tree the boost makes of its work tree, to
  put back, to check, or for a job's report, goes through an index of
  its own, first given the paths of the tree it is measured against
  (`read-tree -m`, which keeps its stat cache): the base, the tree sent
  (made there from the base and the pack), the tree synced. So a file
  the box tracks that an ignore rule matches is tracked on the boost
  too, and one ignored and untracked in the box never travels. A whole
  tree that still makes another fails the start (`INTERNAL`), saying
  so; then sets HEAD to the box's commit on its
  branch and the index to the box's, so `git status` and `git diff` say
  there what they say in the box. The base is the tree last sent to
  that boost's instance (a `BoostSync` record per box), else the commit
  the boost was checked out at, else none: the whole tree. A base the
  boost lacks, or a tree that does not match, sends the whole tree once
  more in the same start; a bundle whose commits the boost lacks the
  parents of sends HEAD's whole history once more. A sync past 2GiB,
  what changed and the bundle of commits together, is refused
  (`FAILED_PRECONDITION`), naming its largest paths or the bundle. The
  sync removes and replaces paths in the boost's checkout through an
  `os.Root` of it, so a symlink there never leads it outside. Each
  staging directory is removed after the sync, in the box and on the
  boost, and what a crash leaves is swept after a day.
- **Submodules' checkouts are never sent** (`submodules_not_synced`):
  a committed submodule, or an untracked nested repository `git add -A`
  takes as one, is a gitlink of the tree sent, and the tree made on the
  boost is checked with the tree's gitlinks in place of whatever the
  boost holds there. The boost keeps its own checkout of each, or none.
- **A path that is a directory on the boost and a file in the box** is
  the box's file: the directory goes with what it holds, ignored files
  too.
- **A sync beside another process of the box on the boost** leaves what
  that one wrote and does not check the work tree; the next process's
  exit says so (`base_shared`). Whether one runs is read from the
  boost's processes when the sync applies, under the box's sync lock,
  which a start holds as it starts its process: no start is between a
  sync's look and its apply. A start being started counts as a process
  that runs too, whether or not it holds the lock. A start whose sync
  another start's sync overtook, or that ended more than 40 seconds ago
  (the 10-second wait and 30 seconds for the caller to ask again), syncs
  again before it starts. A sync that is not shared first settles the
  box's ended processes on the boost, so their artifacts are copied back
  before it puts back what they wrote; if that settling fails, the sync
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
- **A boosted process runs under `tower-guest boost-job`**, in its
  process's group and cgroup, which once its command ends writes what
  it changed on the boost relative to the tree synced (new files not
  ignored count; 200 listed, with the count) and its artifacts' sizes,
  then exits as its command did, by the same signal. That diff runs in
  the process's cgroup: a boosted process's peak memory, CPU time and
  wall time include it (`process-usage.md`). Its changed files are not
  copied back; a box keeps the list for its last 10 processes, the count
  for all.
- **Artifacts** (`StartProcessRequest.artifacts`, paths in /workspace,
  none in a `.git` directory) are copied back to the same paths in the
  box (`Provider.Copy`, boost to box) when the process is settled: at
  the first read of its exit, by the boost controller once it ends,
  before a sync puts back what it wrote, or before its boost goes; once,
  the settling kept before its report is removed. Each is measured
  again on the boost right before it is copied; they copy in their order
  up to 1GiB together; one not there, leading out of /workspace by a
  name or a symlink, past the cap, or not copied (the box stopped) says
  why in the exit, whose code it never changes.
- `test/boostcheck` checks the above on a real tower and hangar target
  (`docs/runbook/testbed.md`).

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
  artifacts, and how a pending start and usage read in a job's result
  (usage on `await_job` is asked for). The API they call is above.
- Whether a boost runs Docker (docker-in-docker works in a hangar guest,
  but tower starts no dockerd today), which would move e2e that needs
  Docker or k3s from machines to boosts.
- The thresholds above, to be set from the records once tower keeps them.
