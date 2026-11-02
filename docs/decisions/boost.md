# Heavy jobs run on a boost, not in a larger box

Status: accepted in part, 2026-10-29; one box, and a full node,
2026-10-30. The boost
itself (its kind, its life, the pending answer, the settings and the
queue) and its sync (the box's files sent before each job, what a job
changed written back once it ends), under "Built" below, are accepted
and built. Routing by a command's records was built as records only and
is withdrawn; routing by the repository's task, under "Routing by task",
was drafted 2026-11-02 and is built as its "As built" says, where it
fills in or departs from the draft; its numbers are still a start.

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
  come back: the job's processes and ports, `.ws-cache`, a path the box
  changed while the job ran (the box wins, listed), and two named
  exceptions to "as if it ran in the box", each said in its exit:
  git's state (a commit or `git add` the job made on the boost; a later
  change may bring commits back), and a file past the cap, which stays
  only on the boost, where the next sync not beside a running job
  removes it.
- **A job's output and exit come back as any process's**
  (`EnvironmentService`), so workstream's job tools work unchanged; its
  exit also says what came back (`BoostRun.back`).
- **tower routes a job by the task it runs**, a target or script the
  repository declares, from what that task does and what its runs
  measured, when the caller asks for `auto` ("Routing by task"). The
  caller may ask for a boost, a size, or no boost, and that wins.
- **Boosts are budgeted per target and queued.** The target's
  `boost_budget` caps the cpu and memory all its boosts hold, within the
  Plan's quota left. A job that does not fit is queued, first come first
  served across workstreams in turn, and its caller is told at once that
  it is queued and for about how long, so the agent decides whether to
  wait, run it in the box, or do something else (decided with the human,
  2026-10-29).

## Routing by task (2026-11-02)

Routing by a command's records was withdrawn (decided with the human,
2026-11-02). tower kept them (`process-usage.md`) under the sha256 of the
command line and under its first word, and four days of them on
hangar-ardge (2026-10-29 to 11-02) showed the key was wrong: 76 exact keys
over 80 runs, 73 of them run once, since an agent seldom writes the same
line twice (`| tail -50`, another test file, a `cd … &&` before it); and
the first word was mostly `export`, `cd`, `eval` or `source`. The same
runs show what a job runs short of: no peak above 1.28Gi of a 2Gi box
and no OOM, but 15 runs of 30 seconds or more (of the 80) waited for a
CPU for a fifth of their time or more. And tower is one tenant of its Plan,
with no claim to more: boxes stay small, heading to 1 vCPU, so heavy
work has to find its own way to a boost.

- **The unit is a task the repository declares**, not a command line.
  Make targets, `package.json` scripts and `just`/`task` recipes are few,
  named and stable; however an agent writes the line around one, it
  runs the same task.
- **Finding the task.** The line is parsed as shell (`mvdan.cc/sh`), and
  each simple command of its lists and pipelines read past setup (`cd`,
  `export`, `NAME=value`, `source` and `.`, `set`), wrappers (`env`,
  `time`, `timeout`, `nice`, `nohup`, `sudo`) and output filters after a
  pipe (`tail`, `head`, `grep`, `tee`). A task entry is:
  - `make` (its `-C` directory, its targets, or its default goal), `just`
    and `task` with their recipe;
  - `npm`, `pnpm`, `yarn` and `bun` running a script, by `run` or by a
    shorthand that runs one (`pnpm test`, `pnpm lint` when `lint` is a
    script);
  - a heavy tool called directly: `go test` and `go build` with their
    package pattern (`./...` and `./pkg/x` are different tasks, and differ
    by gigabytes), `cargo test` and `cargo build`, `tsc`, `vitest`, `jest`.

  A line with several entries (`make gen && make test`) is their list, in
  order. A line with none is no task and runs in the box.
- **A task's key** is the project, the repository, the directory
  relative to `/workspace`, the entry and its target, script or pattern.
  Flags and other arguments are not in it. Targets and scripts are names
  the repository gives, not a caller's values, so the key is kept as
  text, each word held to the plain-name rule `process-usage.md` sets
  for the coarse key; a word that fails it makes the line no task.
- **A task with no record**: a heavy tool called directly (the list
  above) goes to a boost of the target's `boost_resources` by its name; a
  script is judged by its text in `package.json`, following the scripts
  it runs, and goes to a boost if it runs a heavy tool (the list above,
  plus `go vet`, `bun test`, and bundlers and builds: `vite build`,
  `next build`, `webpack`, `docker build`); a make target, or a `just` or
  `task` recipe, runs in the box. **tower never runs anything to judge a
  task** (decided with the human, 2026-11-02): `make -n`, drafted here
  first, runs whole `$(MAKE)` lines, `+` lines and `$(shell …)`, and
  remakes the makefiles it includes. **A target learns from its first
  run**: that run, in the box, is measured, and its record, shared by
  every box of the repository, routes the next.
- **A task with records is judged by them**, its last runs in the box or
  on a boost, the memory against what the box holds its measured
  processes to (`processMemoryMax`, `process-usage.md`: the box's memory
  less 768Mi, at least half), not its memory as a whole:
  - in the box, waiting for a CPU a fifth of its time or more, or busy on
    all the box's CPUs, for 30 seconds or more: next time a boost of the
    target's `boost_resources`;
  - a peak within 80% of what the box holds its processes to, or an OOM:
    a boost that holds them to 1.5 times that peak;
  - on a boost, using no more CPUs than the box has and a peak the box
    holds its processes to: next time in the box.

  The numbers are a start, to be read again against the records.
- **Each routed exit says why**, in its task's words ("`make test`: of
  its last 3 runs, one in this 1 vCPU box waited for a CPU 40% of its
  1m0s: on a boost of 8 vCPU and 8192Mi").
- **Records are kept per task**: `CommandUse`, keyed by the line, gives
  way to a record keyed by the task, which holds no line text either;
  the records kept so far are dropped.

### As built

`internal/apiserver/taskfind.go` finds a line's tasks,
`taskroute.go` routes them, `taskuse.go` keeps their runs. Where this
fills in or departs from the draft above:

- **The API.** `BoostIntent.auto` asks for it; `on`, a size, or no
  intent (no boost) win as before. `StartProcessResponse.routed`, on the
  started and the pending answers, and `ProcessExit.routed`, on a box's
  process's exit however it is read, say where it went and why, as
  "`make test`: of its last 3 runs, one in this 1 vCPU box waited for a
  CPU 40% of its 1m0s: on a boost of 8 vCPU and 8192Mi". The words are
  the task's plain names and tower's numbers, never the line's other
  words. A pending start asked again keeps the size and the why it was
  routed with; a ticket tower no longer knows (a restart) asks for
  `boost_resources`, and says so. Routing a start is bounded to 10
  seconds.
- **`auto` is advisory** (decided with the director, 2026-11-02): it is
  never refused for want of a boost. A machine, or a box on a target
  with no boosts (hostd), runs it where it is. A boost the target can
  never fit, even with no boost held (what `on` is refused for,
  `FAILED_PRECONDITION`), runs it in the box instead, `routed` saying so
  ("`go test ./...` would go to a boost of 8 vCPU and 8192Mi, but the
  target can never fit it (its Plan has 4 vCPU and 49152Mi left): ran
  in the box"); asked again with a ticket that has come to never fit,
  likewise. A routed size is held to `boost_max` as any size asked for
  is, so `boost_max` alone never refuses one. A boost that only has to
  wait (queued for `boost_budget`, a full node, starting) answers
  pending as `on` does, so the agent still decides whether to wait.
- **Finding the task.** The line is parsed as bash by `mvdan.cc/sh/v3`
  (BSD-3-Clause). A simple command that is no entry is passed over, not
  only setup and filters: `git status && make test` is `make test`. Its
  lists, pipelines (each stage in a subshell of its own), subshells,
  `{ }` and `time` are read; conditionals, loops and functions are not,
  and `sh`/`bash`/`dash`/`zsh -c '...'` is read as a line of its own. A
  line that does not parse is no task. `exec` and `command` are
  wrappers too, and `env -C` and `sudo -D` move the directory; `command
  -v`/`-V`, `type` and `hash` are lookups, which run nothing: that
  command is no task.
- **What is an entry.** make's `-C`/`--directory` and targets; its
  variables (`V=1`) and flags are not in the key, and a `-f`, or an
  `-E`/`--eval`, makes the line no task (another makefile, or more of
  one). `just` and `task`
  with their recipe (task's `-d` moves the directory; just's `-f`/`-d`
  and task's `-t` are no task). npm runs a script by `run` (and its
  aliases) or `test`/`t`/`tst`/`start`/`stop`/`restart`; pnpm, yarn and
  bun by `run`, or by `<name>` when the box's package.json in that
  directory has the script, a package's binary otherwise (a heavy tool
  if it is one: `pnpm vitest run` is `vitest`); `npx`, `bunx`, `npm
  exec`, `pnpm exec`/`dlx`, `yarn exec`/`dlx` and `bun x` run a binary.
  `pnpm -C`/`--dir`, `npm --prefix`, `yarn --cwd` and `bun --cwd` move
  the directory; `pnpm -r`/`--filter`, `npm -w` and the like (other
  packages) are no task. `go test`/`go build`, and `go vet`, with
  their patterns (none is `.`; `-C` moves the directory), `cargo
  test`/`build` with their `-p` packages, and `bun test` (bun's own
  test runner, CPU-heavy on workstream) are heavy tools, called directly
  or in a script: `go vet` and `bun test` beyond the draft's list
  (decided with the director, 2026-11-02).
- **Plain names.** A make target is a plain name as
  `process-usage.md` sets it (`make bin/tower` is no task). A script's
  or recipe's name may be plain names joined by `:` (`test:unit`, just's
  `mod::recipe`), 64 characters at most: a departure, as `:` is how
  package.json names its scripts. A directory or a package pattern is a
  path whose parts are letters, digits, `.`, `-` and `_` (upper case
  too: a directory is the repository's), each not shaped as a token is
  (all hex, or a quarter digits); `..` above `/workspace` and a `cd` the
  line cannot tell (`cd $X`, `cd -`) are no task. **A directory is in a
  key only if the box has it**: the task's own (`cd X`, `make -C X`) and
  each `go` package pattern's (`./pkg/...` is `pkg`, which must be a
  directory; a module's path or a file is none), read through the
  provider as package.json is (a read of a directory fails as one), so
  a word a line holds that is no directory of the repository's, a
  secret say, never reaches a record, a log or `routed`.
- **A line of several tasks** is kept as one, the list: its runs are
  the line's. With no runs of its own, each of its tasks is judged on its
  own (its runs, else as one with none), and the largest boost any asks
  for wins.
- **Scripts.** A script's text is read as shell, all of it
  (conditionals, loops, substitutions), for heavy tools and for the
  scripts it runs, followed 8 deep in the package.json of the directory
  each runs in, npm's `pre`/`post` scripts with it. It is read again at
  each start that needs it, one read of a file, so nothing of it is kept.
  A make target, or a `just` or `task` recipe, has nothing read: with no
  runs, it runs in the box.
- **Judging by runs**, the last 3, in this order: a run that ran out of
  memory, or peaked at 80% or more of what the box now holds its
  processes to (`processMemoryMax` of its memory), wherever it ran,
  sends the next to a boost that holds them to 1.5 times the largest
  such peak (an OOM with no peak counts what the instance it ran on held
  them to): the least memory M with `processMemoryMax(M)` that large,
  min(T + 768Mi, 2T) for a limit T, its cpu the target's default moved
  into hangar's shapes, as a size asked for is, and held to `boost_max`;
  a box of 2Gi holds its processes to 1280Mi, so a peak of 1100Mi there
  sends the next to a boost of 2418Mi. Then a run
  in the box of 30 seconds or more that waited for a CPU a fifth of its
  time, or whose CPU time was 90% or more of all the box's CPUs over its
  wall time ("busy on all", read so as a busy run never quite uses them
  whole), to a boost of `boost_resources`; else, the last run on a
  boost, to the box unless it used more CPUs on average than the box
  has (then `boost_resources` again), its peak being under 80% of what
  the box holds its processes to, or the memory rule would have held it;
  else the box. A single-threaded job
  thus goes to a boost after a busy run in the box, back once three
  runs on a boost have pushed that run out of the window, and so on: one
  run in four in the box.
- **Records.** `TaskUse` (internal, `internal/apiserver/taskuse.go`),
  keyed by project, repository and the line's tasks (directory, entry,
  name each), its last 10 runs, pruned as `CommandUse` was; nothing else
  (routing writes nothing). Every
  `CommandUse` is deleted at the first hourly prune. `BoxProcesses`
  keeps a boost process's tasks and the word it is listed by once its
  boost is gone, its tasks' or its program's name, and why each `auto`
  start was routed, the box's own processes' too (the last 100).

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
  is being deleted, with no estimate; `NODE_FULL`, below), a message,
  its place in the queue and about how long. The caller asks again with
  the ticket, or gives it up (`cancel`); asking again keeps the ticket's
  place. A ticket not asked
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
  keeps, per box, which instance runs it and, of its command, its
  tasks and the word it is listed by alone (`process-usage.md`). ListProcesses lists the box's and its
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
- **A full node.** A stopped hangar machine gives its node's memory
  back (hangar#237), so starting it again asks the node for room: with
  too little free, hangar leaves it stopped, its Ready reason
  `StartDoesNotFit` saying how much is free, and starts it by itself once
  a machine there stops, shrinks or goes; a new machine no ready node
  has the memory or disk for is `Unschedulable`, naming the shortfall,
  and is placed once one does (hangar: `docs/decisions/machine-model.md`).
  The hangar provider reports both, from the object's current generation,
  as the Instance's Ready reason `WaitingForRoom` with hangar's words,
  phase PENDING; an `Unschedulable` for want of a ready or uncordoned
  node is not room, and is passed on as it was (decided 2026-10-30):
  - **A box woken on a full node** is STARTING, its Ready reason
    `WaitingForRoom`, "waiting for room on its node: " and hangar's
    words. The data-plane call that woke it waits 5 seconds
    (`wakeRoomWait`) and then answers `UNAVAILABLE` with those words and
    that the box starts by itself once there is room, rather than at
    its deadline: a node rarely gets room within a call's wait, and an
    agent told why decides what to do meanwhile, where one left hanging
    learns nothing. The 5 seconds let room freed just then (a machine
    stopping as the box wakes) start the box within the call. The box
    stays asked to run (`stopped_reason` empty), so hangar starts it once
    there is room, and the next call finds it READY; it is not idle while
    STARTING, so nothing stops it again meanwhile.
  - **A boost on a full node** is STARTING, its Ready reason
    `WaitingForRoom`; a start asking for it answers
    `PENDING_REASON_NODE_FULL` with hangar's words and no estimate, not
    `BOOST_STARTING`'s fixed one. A boost is always a new machine, so
    hangar says `Unschedulable` of it, not `StartDoesNotFit`.
    `boost_start_timeout` still bounds it, counted as for any boost not
    READY, and fails it saying what it waited for. It keeps its room in
    `boost_budget` meanwhile: it is admitted, and hangar starts it the
    moment the node has room; releasing it would admit the next start,
    whose boost would wait on the same node, and both would hold more
    than the budget once room came.
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
  records the same are taken as the same file only where neither side
  can have rewritten it unseen: a file moved keeps its mtime, so is the
  same once moved; and a file the box touched since it last synced to
  that boost, which its inode or ctime tell (any write moves the
  ctime, which nothing can set back), is sent whatever its records say.
  To a boost it never synced to (a new one), every ignored file the box
  touched since its **baseline** is sent: the box's manifest, with
  inodes and ctimes, taken on a target that makes boosts (hangar) once
  its repositories are checked out from its warm golden and before its
  setup steps run (`boost-baseline`), which is what a boost made of the
  same golden holds but for its own setup's writes. A file of the box's
  git tree (tracked, or untracked and not ignored) is not sent for
  being touched: the boost checks its content against that tree, and
  one that differs is sent again, as below. The baseline is not moved
  by a sync: a new boost is made of the golden, not of the box's last
  state, so the ignored files the box touched since the golden are sent
  to each new boost, equal or not (comparing their hashes first, where
  their records match, is a follow-up). A box made before tower took
  baselines has none, and sends every ignored file to each new boost,
  up to the cap. What can still slip, and only for an
  ignored file (a tracked one is checked against the box's tree, as
  below): one the boost's own setup steps rewrote to the size and mtime
  of the box's untouched copy with other content (as `npm ci` with
  fixed mtimes would, were the lockfile's content not the same), and
  one written unseen by a hand outside a job between a job's end and
  the next sync on the boost, which nothing there does. For a tracked
  file, the check sees what git sees: one rewritten in place to its
  size and mtime within the second git last looked at it, which `git
  status` in the box also takes as unchanged, is not sent to a new
  boost. The box also sends git's state as before: its work tree's
  tree (as trees alone), what is staged apart from it, and a bundle of HEAD's commits no `origin` ref
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
  again within 5 minutes of its start, as by a caller that lost the
  answer (a dropped connection, an apiserver restarted: the ticket is
  kept on the process's record in `BoxProcesses`), a ticket is answered
  the same process and `ran_on`, never a second start; while its start
  is under way, `PENDING_REASON_SYNCING`. Cancelled once started, its
  process is sent TERM and the start is answered; cancelled while being
  started, TERM once it is. Started tickets are pruned each minute.
- **A boosted process runs under `tower-guest boost-job`**, in its
  process's group and cgroup, which walks the boost's `/workspace`
  before its command starts and once it ends: what differs, by type,
  mode, size, mtime, inode or ctime, is what the job added, changed or
  deleted, kept for its write-back; it reads git's HEAD and a hash of
  its index at both ends too. It exits as its command did, by the same
  signal. Both walks run in the process's cgroup: a boosted process's
  peak memory, CPU time and wall time include them
  (`process-usage.md`). Moving them out (the wrapper into its parent
  cgroup around each walk) needs write access to that cgroup's
  `cgroup.procs` as the job's user, which cgroup v2's delegation does
  not give it as tower sets processes up: a follow-up.
- **What a sync costs.** A walk takes about 1 second per 100,000 paths
  with the page cache warm (0.9s measured, the manifest about 6KiB per
  1,000 paths gzipped, 50MiB peak), more when cold. A sync walks the
  boost once and the box once, and copies the boost's manifest to the
  box; a boosted job walks the boost twice, before and after its
  command, in its usage; its write-back reads only the paths it changed
  in the box. What a sync sends is what differs, and, to a new boost,
  the ignored files the box touched since its golden (an `npm install`
  in the box: its `node_modules`, up to the cap), once per boost; the
  box's git tree is checked on the boost instead, its trees alone
  sent. Directories are not skipped by their mtime: a file
  rewritten in place does not move its directory's.
- **Once a boosted process ends, what it changed is written back** to
  the box, at its settle, before its exit is answered: at the first
  read of its exit, by the boost controller once it ends, before a sync
  undoes it on the boost, or before its boost goes; once, the settle
  kept before its report is removed. `boost-back` packs on the boost
  each path the job added or changed as it is then, with its mode,
  mtime or link target, and what it deleted, up to 2GiB in path order
  (a file past it stays only on the boost, listed in `over_cap`, and
  the next sync not beside a running job removes it there); tower
  copies it to the box, where `boost-writeback` applies it through an `os.Root` of the
  checkout. The box wins: a path the box changed after the job's sync
  began, as that sync's manifest of the box says, by any record field,
  inode or ctime too, or one under a directory the box removed or made
  a file or a link, stays the box's and is listed in `conflicts`; a
  directory the job removed goes only if the box put nothing in it
  since. Nothing is written through a link. The box is checked path by
  path right before each is written: a box edit in the instant between
  that check and the write's rename (a few system calls) is lost to the
  job's. A path the box already has
  as the job left it, as when another job at once wrote it and was
  written back first, is neither moved nor a conflict, so `moved` lists
  what this job's write-back wrote. Files keep the boost's mtimes, so
  the next sync finds them the same. The exit says what came back
  (`BoostRun.back`): files, deletions, bytes, the first 200 paths with
  the count, conflicts, what was past the cap, the time it took, and
  whether the job moved git's HEAD or changed its index.
- **A write-back is never dropped while its box can take it.** A box
  starting, or made again, leaves its boost's ended processes unsettled
  (reading their exit waits for the box; listed, they run until
  settled), keeps its boost from going
  idle, and makes a sync beside them shared, so nothing they wrote is
  undone; they are written back once it is ready. A box stopped, at
  workstream's request or for idleness, has its boost's ended processes
  written back before its instance stops and its boost is deleted;
  those still running are lost with the boost. A box gone, failed or
  already stopped gets nothing back, and the exit says why; a process
  started before this tower, whose sync kept no manifest, likewise. The
  sent manifests none needs are pruned by a sync, under the box's sync
  lock. A box keeps the paths' list for its last 10 processes, the
  count for all.
- **Leftovers.** A file a sync writes beside its path
  (`.tower-boost-*`) is never walked, and one a crash left is removed
  by a later walk once a day old, as staging directories are.
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
- A line that runs no task the repository declares, a script of its own
  say, never goes to a boost unless its caller asks. A heavy tool's first
  run, or a script's that runs one, goes to a boost by its name, before
  any record; a make target's, or a just or task recipe's, first run is
  in the box, a cost paid once per repository for what it measures.

## Not decided yet

- Whether a boost runs Docker (docker-in-docker works in a hangar guest,
  but tower starts no dockerd today), which would move e2e that needs
  Docker or k3s from machines to boosts.
- Whether a script file the line runs (`./scripts/e2e.sh`) counts as a
  task, keyed by its path.
- Whether `auto` becomes the default of workstream's `run_async_box`.
