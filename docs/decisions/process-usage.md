# A box's processes are measured each in a cgroup of its own

Status: accepted, 2026-10.

## Context

Boxes on hangar targets are sized for their worst job, and hangar reserves
an instance's memory in full (hangar: `docs/decisions/node-agent.md`). A box
with a service never idle-stops, so an oversized box holds its memory all
day. Measured on 2026-10-29: hangar's `go test ./...` peaks at about 3Gi,
most other jobs stay under 1.3Gi, and idle boxes use 230-660M.

To size boxes per repository, to route a heavy job to a larger instance
(a later "boost", cloned from the warm golden), and to tell an agent plainly
that its job ran out of memory, tower has to know what each command used.
The box as a whole is no measure of that: its peak mixes every process in
it, and a box that runs out of memory loses whichever process the kernel
picks, its services and its agent included.

## Decision

- **Each process tower starts in a box runs in a cgroup of its own.**
  `tower-guest supervise` makes it before the command starts and starts the
  command into it (clone3's `CLONE_INTO_CGROUP`), so no child escapes it.
  Its exit records what the cgroup says the process and its children used:
  `memory.peak`, `memory.events`' `oom_kill`, `cpu.stat`'s `usage_usec`,
  `cpu.pressure`'s `some` total, and the wall time.
- **All of them live under one parent, `/tower`, directly below the
  cgroup2 root.** Not below the supervisor's own cgroup: in hangar's guests
  an exec's processes run in `/workload`, and a cgroup holding processes
  cannot enable controllers for its children (cgroup2's no-internal-
  processes rule). `/tower` holds no process itself and enables memory and
  cpu for its children; the root, which holds `agent` and `workload`
  already, offers them, and tower's guest calls run as root there. Each
  supervisor makes `/tower` if it is not there, so racing supervisors both
  succeed.
- **`/tower` has `memory.max`; each process cgroup has `memory.oom.group`
  and no limit of its own.** The limit is the box's memory less a reserve
  (768Mi, and at least half the box's memory left to `/tower`), so the
  measured processes together, however many, leave the reserve to the
  box's own processes. Past it, the OOM stays inside `/tower`: the kernel
  picks one process cgroup there and kills it whole, its children with it,
  and its exit says `oom_killed`. What is guaranteed is that much and no
  more: `/agent` and `/workload` run on, and one `/tower` child is killed
  whole. Which one the kernel picks is its own choice (the largest,
  usually), and a box's services and editor started through tower are
  `/tower` children too, so any of them may be the one killed. A limit
  per process would not bound two heavy jobs at once.
- **tower's apiserver decides the limit** (`layers.md`) and passes it on
  `provider.ProcessRequest`; the hangar provider says only whether it can
  measure (a container yes, a VM no) and passes the limit to the guest.
- **A process cgroup is removed** once the exit is written. What the
  command left running in it, a child in the background, is moved first to
  the supervisor's own cgroup, where it would have run without one, and
  runs on. A process cgroup whose supervisor died (named by the process's
  id and its supervisor's pid) is swept the same way when the next process
  starts.
- **hangar's containers alone.** Their guests' cgroup2 is writable and
  delegated (nsdelegate). hostd's boxes are Docker containers sharing one
  host with no reservation, whose limit Docker raises live, so hostd needs
  neither the measure nor a boost: its processes report no usage, and no
  rusage stands in for it. VMs (machines) are left out until a cgroup made
  beside systemd's own is decided on.
- **A start alone runs as root; the command runs as the container's
  user.** Only root makes a cgroup under `/tower`, and a devcontainer's
  image usually runs as a user of its own (`remoteUser`), as hangar's exec
  does every command of the container: a supervisor run as that user
  measured nothing, and said nothing of it. At a container's first start,
  and never for anything else, the hangar provider reads who a shim run as
  the container's user is (its uid, gid, supplementary groups and HOME, as
  hangar's runtime gives them) and whether a shim runs as root there. Where
  one does, a start runs its shim as root, and the supervisor, root, drops
  the command to that user (`SysProcAttr.Credential`, its groups set, no
  capability kept): the same uid, gid, groups and HOME, unless the start
  sets HOME, and the rest of the environment, which hangar's exec gives
  root and the user alike (USER, LOGNAME and SHELL as the image sets them,
  which is unset unless it does). A user who is the root shim, by uid, gid
  and groups alike, drops to no one. The user cannot move the command out
  of its cgroup, which is root's.
- **Every other operation runs as the user**: exec, files, copies, a
  boost's pack, apply and clean, and a process's signal, list and log. The
  command's process group is the user's (`Setpgid`; the supervisor is in a
  session of its own, outside it), so the user may signal it, and no
  signal sent to the group reaches the supervisor, which writes the exit
  however the command ended. A process's directory and files are root's,
  in the user's group, `0750` and `0640` (`proclog.GroupReadable`): the
  user reads them, lists them and takes their lock shared, and writes
  none; no other user of the guest reads a process's output or command
  line, as before, when they were the user's, `0700` and `0600`.
  `/.tower/processes` is the user's and its group's, `0750`
  (`processesDirLine`), so no other user lists the processes either.
  (The user's group is its own in a devcontainer's image; a group it
  shares with other users would let them read too.) Root never reads a
  file the user may write to decide whom to signal: a pid the user
  forged is sent with the user's own rights.
- **The supervisor writes nothing by its path.** `/.tower/processes` is
  the user's (an earlier tower's shim, run as the user, makes processes
  there), so the user may rename or replace a process's directory under it.
  The supervisor makes the directory, opens it, checks it is root's and
  empty, and writes every file of it (meta, lock, pid, log, exit) through
  that handle, each new (`O_EXCL`, `O_NOFOLLOW`): a renamed directory takes
  the writes with it, and a symlink or a directory put in its place gets
  none. It prunes the finished ones confined to `/.tower/processes`
  (`os.Root`), and one it cannot remove is left, the others removed all
  the same.
- **No root, no measure, said.** A container where no shim runs as root,
  or one asked for root runs as another, starts its processes as before,
  as the user, unmeasured. It is logged once per container, said on its
  Instance, and root is tried again five minutes later, at a start, not
  before. The Instance's `RootStart` condition says what is known: true
  (`Root`) once a root supervisor starts its processes, which measuring
  them needs (the guest's cgroups must allow it too: an exit's usage says
  whether one was measured); false (`NoRoot`) with why not.
- **The rollback limit.** A tower rolled back to one whose shim runs as
  the user (rc.10) lists, reads, locks shared and signals the processes
  this one started, but cannot prune them. Its prune stops at the first
  directory it cannot remove, a root one, so it prunes none at all, its
  own among them: after a rollback, finished processes' directories and
  logs build up on the box's root disk until the instance goes, each
  bounded by its log's cap. Rolled forward again, this tower's prune,
  which skips what it cannot remove and goes on, removes them.
  (`internal/guest/guestbin/runas.go`, `supervise.go`,
  `internal/guest/proclog`, `internal/provider/hangar/ops.go`.)
- **Measuring never stops a process.** A guest where no cgroup can be made
  (not root, a read-only or v1 cgroupfs, a kernel without
  `CLONE_INTO_CGROUP`) runs the process as it would without one, its usage
  unset. A field whose file the kernel does not offer (`memory.peak` before
  5.19, `cpu.pressure` without PSI) is unset alone.
- **tower keeps the last 10 runs of each command** in an internal record
  kind, `CommandUse`, beside `BoxUse` (`internal/apiserver/commanduse.go`),
  keyed by project, the box's first checkout's repository, the directory
  the process ran in relative to `/workspace`, and the command. The
  directory is the one the guest ran it in, so a process that asked for
  none counts with one that asked for its box's default directory. The
  apiserver writes a record when it reads an exit (an attach's end, or a
  list), each exit once: a box's processes are claimed before they are
  recorded, and the recording goes on once the call that read them ends.
  A record not run for 90 days goes, and a project keeps 1000 at most, the
  least recently run going first; both are pruned once an hour.
- **A command is kept under two keys, and its text under neither.** A
  command's arguments may hold a secret, and records outlive the box, so a
  record holds no command text and no environment:
  - the exact key is the sha256 of the command normalized: whitespace
    collapsed, a leading `cd /workspace &&` stripped and, on a line that is
    one pipeline alone, trailing output filters (`2>&1`, `| tail`,
    `| head`, `| grep`) stripped. A line holding a list (`;`, `&&`, `||`),
    a background `&`, a substitution or a subshell keeps its filters,
    whatever follows them;
  - the coarse key is the program's name and its first subcommand (`go
    test`, `make e2e`), past variables set before it, wrappers (`env`,
    `time`, `timeout`, `sudo`, `nice`) and a leading `cd DIR &&`. It is the
    only command text a record holds, so each word in it is a plain name:
    lower-case letters, digits, `-` and `_`, starting with a letter, 32
    characters at most, and not hex or digit-heavy as a token is. A
    subcommand that is not one is left out; a program that is not one, a
    script's name, say, is `<program>`, with no subcommand.

  The coarse key is advisory: it may hint (a box size to suggest, a warning
  that a command of this kind ran out of memory before) but never decides
  where a run goes. `go test ./x` and `go test ./...` share it and differ
  by gigabytes.

## Consequences

- A process run on a boost runs under `tower-guest boost-job`, which,
  once its command ends, lists in the same cgroup what the command
  changed: its usage includes that git diff (`boost.md`).
- An agent learns from the exit that its job, or the process the OOM
  picked instead, ran out of memory, and the box it ran in and its agent
  are still there; its other processes started through tower may not be.
- The box's measured processes together can no longer use the last 768Mi
  of its memory, which its own processes keep.
- A record says which command it is only to whoever already knows the
  command: routing and hints look a command up by its key, never list
  what was run.
- Routing to a boost, and suggesting box sizes, read these records; neither
  exists yet.
