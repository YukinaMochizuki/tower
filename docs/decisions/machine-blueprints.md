# A machine's golden is its playbook but the configure tasks

Status: accepted, 2026-10.

## Context

A machine is workstream's blueprint, an ansible playbook, converged on a
VM. tower converges it in two phases: once into a golden, in a builder VM
that is then captured, and in each machine cloned from the golden, with
the machine's own vars and secrets, which never enter the golden
(`secrets.md`). tower first ran the golden phase as the tasks tagged
`golden`. workstream's blueprints (its `k3s` and `workstream-dev`) are
untagged, and workstream builds its goldens from every task but those
tagged `configure`, running only those in each clone
(`workstream: src/machine/drivers/ansible-qemu.ts`). Under tower's
convention its k3s blueprint converged to nothing.

k3s, as other software a golden carries, writes the VM's hostname into
what it keeps: the node's name, the etcd member. workstream keeps a VM's
hostname the same from its golden to every clone of it.

## Decision

- **The golden phase is `ansible-playbook --skip-tags=configure`, the
  configure phase `--tags=configure`**, both run in the VM with
  `--connection=local`, as workstream runs its blueprints. A blueprint is
  carried unchanged; a task needing a machine's vars or secrets is tagged
  `configure`.
- **Every VM hostd runs has one hostname, `machine`**, set by its
  cloud-init, the same in a golden's builder and in each clone. A machine
  is reached by its alias on its workstream's network, which is its own:
  its blueprint's `net_alias`, else the machine's name, as a box's is the
  box's name, on every provider (decided 2026-10-27: a machine has an
  identity of its own, and two machines of one blueprint are two names).
  A machine or box whose alias another of its workstream already answers
  to is refused as it is made, so an alias names one environment on
  hostd and hangar alike.
- A base image carries ansible (`deploy/hostd/base-image.sh`); one that
  does not, as a hangar target's catalogue images, gets ansible-core in
  the golden's builder before its golden phase.
- **A machine is READY when its blueprint's ready check passes**, not when
  its configure phase ends: a service it starts can take minutes more
  (k3s reports its node Ready well after the playbook). The check is
  workstream's `readyCheck`, a shell command, run as `sh -c` in the VM
  every 5 seconds until it exits 0, for 200 seconds unless the blueprint
  says otherwise (workstream tries 40 times, 5 seconds apart). One that
  never passes fails the machine with the end of its output, and runs
  again once its backoff passes (`failures.md`); it is run without the
  machine's secrets, so that output can be shown. It runs again after each
  start of the machine. A blueprint without one is READY
  once configured.

- **A blueprint may name the user it runs as (`Blueprint.run_as`); the
  client chooses it, and tower names none.** workstream's blueprints (and
  hangar's `hangar-testbed`) are written to be run over ssh by a user with
  passwordless sudo: `become: true` tasks get root through sudo,
  `become: false` tasks and `~` are the user's. With `run_as` set, tower
  makes the user as the first step of the golden phase, as root through
  the provider's exec in the builder VM: `useradd --create-home
  --user-group` unless the VM has a user of the name, a home if it lacks
  one, a passwordless sudo rule in `/etc/sudoers.d/tower-run-as`, and the
  blueprint's files under `/opt/tower` handed to it. Each clone of the
  golden has the user already; the same step runs again before the
  configure phase, changing nothing but handing it that phase's files.
  The golden phase, the configure phase and the ready check then run
  through `runuser -u USER --`, which sets the user's `HOME`, `SHELL`,
  `USER` and `LOGNAME` and keeps the rest of the environment, so the
  configure phase's secrets, given as its environment, still reach it
  (sudo would drop them). The user is part of the golden's inputs
  (`MachineInputs.run_as`): a different user is a different golden. A
  blueprint without `run_as` runs everything as root, as before, and its
  golden's hash is unchanged. The name is a portable POSIX one and not
  `root`, which is what no `run_as` means.

- **A machine's data-plane work is its `run_as` user's too.** What a
  client runs in a machine through EnvironmentService (`Exec`,
  `StartProcess`) runs through `runuser -u USER --` as well, in the
  user's home unless the call names a directory, its arguments passed
  through `"$@"` rather than parsed again; what `WriteFile` or
  `CopyFile` puts there, as root, is handed to the user (`chown -R`): a
  copy whole, a written file with the directories its write made above
  it, from the topmost. A client's VM convention is that of a login over
  ssh as that user, which is what workstream did before tower: the
  machine the blueprint built as the user is worked in as the user, and
  `sudo` reaches root.
  runuser keeps the process group, so a signal to a process's group
  reaches the command. A machine without `run_as`, and a box (which runs
  as its image's user), are unchanged. There is no per-call user: the
  machine's own is the only one a client has asked for.

Not a hostname per blueprint: a golden is shared by every machine of its
blueprint, golden vars and base image, whatever their aliases, and nothing
else names it to hostd.

Not workstream's convention adopted by tower (a fixed user every
blueprint runs as): tower would then name a client's user, and a
blueprint written another way would break; the knob is generic, and
workstream maps its convention onto it.

Not the user made by hostd in the VM's cloud-config: the user is what
the blueprint runs as, a decision about the machine, which is tower's
(`layers.md`); made by tower through exec, it needs no new field in the
provider interface, the instance spec or hostd's API, works the same on
any provider of VMs, and lives in the golden's disk, from which every
clone has it.

## Consequences

- A blueprint that needs a VM to know its own name reads its alias, not
  its hostname.
- A ready check runs as the blueprint's `run_as` user, root without one:
  a check reading `~` reads that user's home. workstream runs one over
  ssh as its VM user; it gives the same user as `run_as` for the two to
  agree.
- A command a client runs in a machine with `run_as` that needs root
  asks for it through `sudo`.
- `ansible_user` is not set by tower: a blueprint reading it (as
  workstream's `workstream-dev` does, with a default) is given it by its
  client, in its golden vars.
- The base image needs `sudo`, `useradd` and `runuser` for `run_as`
  (Ubuntu's cloud image has them); a VM without sudo fails the golden
  phase saying so.
- Several VMs of one cluster (k3s agents joining a server) would share a
  hostname, and need their node names set otherwise.
