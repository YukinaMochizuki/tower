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
  is reached by its alias on its workstream's network, which is its own.
- The base image carries ansible (`deploy/hostd/base-image.sh`), so the
  golden phase can run at all.
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

Not a hostname per blueprint: a golden is shared by every machine of its
blueprint, golden vars and base image, whatever their aliases, and nothing
else names it to hostd.

## Consequences

- A blueprint that needs a VM to know its own name reads its alias, not
  its hostname.
- workstream runs a ready check over ssh as its VM user (`ubuntu`), tower
  as root: a check reading `~` reads root's home under tower.
- Several VMs of one cluster (k3s agents joining a server) would share a
  hostname, and need their node names set otherwise.
