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
picks, its services included.

## Decision

- **Each process tower starts in a box runs in a cgroup of its own.**
  `tower-guest supervise` makes it before the command starts and starts the
  command into it (clone3's `CLONE_INTO_CGROUP`), so no child escapes it.
  Its exit records what the cgroup says the process and its children used:
  `memory.peak`, `memory.events`' `oom_kill`, `cpu.stat`'s `usage_usec`,
  `cpu.pressure`'s `some` total, and the wall time. The cgroup is removed
  once the exit is written; what the command left running is moved back to
  the supervisor's own cgroup first, where it would have run without one.
- **The cgroup has `memory.oom.group` set and `memory.max` below the
  instance's memory** by `provider.ProcessMemoryReserve` (at least half the
  instance's memory is left to the process). The process reaches its own
  limit before the box reaches the instance's, and an OOM then kills that
  process's group alone: the box and its other processes go on, and the
  exit says `oom_killed`.
- **The cgroup is made directly under the cgroup2 root,** not under the
  supervisor's own. In hangar's guests the processes of an exec run in
  `/workload`, and a cgroup holding processes cannot enable controllers for
  its children (cgroup2's no-internal-processes rule), so no memory limit
  could be set below it. The root, which holds `agent` and `workload`
  already, enables memory and cpu for its children, and tower's guest calls
  run as root there.
- **hangar's containers alone.** Their guests' cgroup2 is writable and
  delegated (nsdelegate). hostd's boxes are Docker containers sharing one
  host with no reservation, whose limit Docker raises live, so hostd needs
  neither the measure nor a boost: its processes report no usage, and no
  rusage stands in for it. VMs (machines) are left out until a cgroup made
  beside systemd's own is decided on.
- **Measuring never stops a process.** A guest where no cgroup can be made
  (not root, a read-only or v1 cgroupfs, a kernel without
  `CLONE_INTO_CGROUP`) runs the process as it would without one, its usage
  unset. A field whose file the kernel does not offer (`memory.peak` before
  5.19, `cpu.pressure` without PSI) is unset alone.
- **tower keeps the last runs of each command** in an internal record kind,
  `CommandUse`, beside `BoxUse` (`internal/apiserver/commanduse.go`), keyed
  by project, the box's first checkout's repository, the working directory
  relative to `/workspace`, and the command. The apiserver writes it when it
  reads an exit (an attach's end, or a list), each exit once, and prunes a
  record unused for as long as a box's use record is kept.
- **A command is kept under two keys.** The exact key is the command with
  whitespace collapsed, a leading `cd /workspace &&` and trailing output
  filters (`2>&1`, `| tail`, `| head`, `| grep`) stripped. The coarse key
  is its program and first subcommand (`go test`, `make e2e`). The coarse
  key is advisory: it may hint (a box size to suggest, a warning that a
  command of this kind has run out of memory before) but never decides
  where a run goes. `go test ./x` and `go test ./...` share it and differ
  by gigabytes.

## Consequences

- An agent learns from the exit that its job ran out of memory, and the
  box it ran in is still there.
- A process can no longer use the last `ProcessMemoryReserve` of its box's
  memory, which the box's other processes keep.
- A command line is kept in tower's store as the caller gave it, as the
  guest's process list already keeps it; a command's environment, where a
  privileged box's secrets go, is never kept (`secrets.md`).
- Routing to a boost, and suggesting box sizes, read these records; neither
  exists yet.
