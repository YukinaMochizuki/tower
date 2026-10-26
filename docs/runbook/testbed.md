# Testbed: real-host tests (L4)

The real-host tests run what no workspace can (`docs/decisions/
testing-strategy.md`): ZFS, Docker, Cloud Hypervisor under KVM, BuildKit,
a registry and git, driven by hostd's real drivers. They live under
`test/testbed/`, behind the `testbed` build tag, so `make test` and `make
check` never run them (`make lint` compiles them); `make e2e-host` runs
them. Run them only on a disposable VM: they run as root, make datasets,
containers, networks and VMs, and reconfigure Docker.

| Path | What it tests |
|---|---|
| `test/testbed/storage/` | The storage driver (`internal/hostd/driver/zfsstore`) on the pool, as the host sees it: a dataset written on the host, captured into a golden that is a promoted clone, read back in a clone after its disk went; a golden destroyed with a clone deferred (`defer_destroy`), its dataset gone with its last clone; a zvol's device checked by `BLKZNAME`, an open one busy, a clone grown to its size; decoy datasets, zvols and snapshots never listed, touched or taken. |
| `test/testbed/drivers/` | The drivers' conformance suites (`internal/hostd/driver/drivertest`) against the real drivers: storage, images (a pull with the registry's credentials and one the registry lacks, a build pushed and pulled back, one context one digest), containers and their processes, VMs and their processes, git (a fetch, a checkout and its origin with no token in them, a push with and without its lease). Its guests run the drivertest image, built from busybox (pinned) with `test/testbed/guestbin`; its VMs boot the `drivertest` base image, an Alpine carrying the same; its git remote is a repository on the testbed. Beside the suites: a VM's Cloud Hypervisor confined (a user of its own, no capability) and a box's container bounded (`confinement_test.go`); a build step reaching nothing of the host nor any workstream's network (`buildnet_test.go`). |
| `test/testbed/provider/` | The provider contract (`internal/provider/providertest`, the suite the fake passes in CI) against hostd over the real drivers, as `hostdtest` runs it with `Options.Real`: `TestConformance` calls hostd in the test's process; `TestConformanceThroughATunnel` calls tower's provider of the hostd target (`Server.ProviderFor`), which reaches that hostd through the tunnel it opens to the apiserver, as in production. Its guests, VMs and remote are the drivers package's, the remote with a Dockerfile building from the drivertest image. |
| `test/testbed/e2e/` | tower end to end on the real drivers: `tower-apiserver`, `hostd` and `tower` built and run as processes. `TestTheDemo` is `docs/runbook/demo.md`'s scenario: a box built from its own Dockerfile (`image_build`) with a public repository (octocat/Hello-World) checked out, a command, a direct stream, a process keeping it from idling, its idle stop and wake, its process ended by the stop, hostd restarted, its project deleted and the host left empty. `TestAK3sMachine` converges workstream's k3s blueprint (`test/testbed/e2e/k3s.yml`, as workstream has it) on a Cloud Hypervisor VM through tower: its golden phase in a builder booted from `ubuntu-24.04`, captured, a machine cloned from it and configured; its node named `machine`; a box of its workstream reaching its API at `k3s:6443` by its alias, the machine reaching the box by its; a process in the machine. |
| `test/testbed/internal/testbed/` | Not tests: the drivers over a dataset (`tower/testbed-<package>`) and a directory (`/var/tmp/tower-testbed-<package>`) of each package's own, swept as it starts and ends, tower-guest built static into it, and the drivertest image. |
| `test/testbed/setup.sh` | Not tests: makes the testbed. |

## Prepare a testbed VM

A bare Ubuntu 24.04 VM with KVM (`/dev/kvm`; nested is enough), 8 vCPUs,
16 GB of memory and 80 GB of disk, root by sudo, and outbound internet
(Docker Hub for busybox, GitHub for the e2e's repository and k3s, Ubuntu's
and Alpine's images and mirrors). From a checkout of the branch under
test:

```sh
sudo test/testbed/setup.sh
```

It refuses a VM without `/dev/kvm`: VMs never run under TCG. It installs
what a hostd host has (`deploy/hostd/install-deps.sh`: ZFS, Docker with
live-restore, Cloud Hypervisor and its firmware, BuildKit), Go at
`go.mod`'s version, the pool `tower` (40 GB on a sparse file in `/var/tmp`
behind a direct-IO loop device, as hangar's testbed makes its own, never a
file vdev), zot on `127.0.0.1:5000` with a user `hostd` whose password is
in `/etc/tower/testbed-registry.env` (Docker and BuildKit are told the
registry is plain HTTP), and the base images in
`/var/lib/tower-hostd/base-images`: `ubuntu-24.04` with ansible-core
(`deploy/hostd/base-image.sh`) and `drivertest`, made again when
`test/testbed/guestbin` changes, its boot menu's wait taken out. It is
idempotent; after a reboot, run it again: it attaches the pool's file and
imports the pool.

## Run

```sh
make e2e-host                 # every package, one after another
make e2e-host PKG=drivers RUN='TestVMs'
```

The tests are built as you and run as root (`go test -exec 'sudo -E'`);
they build `tower-guest` (and, in e2e, the three binaries) with the `go`
setup.sh linked into `/usr/local/bin`. A run takes about an hour: a VM of
the suites boots in some 45 s (cloud-init's first boot), and the k3s
machine converges in some 15 minutes.

Each package sweeps its dataset, its directory and every container, VM
and network hostd's drivers made on the testbed when it starts and when it
ends: the testbed runs one package at a time. `docker ps -a --filter
label=tower.hangarlab.cloud/driver` shows what is left after a run killed
outright, which the next run sweeps.

## What a run checks on a real host that the fakes cannot

- A box's processes outlive the call and hostd, their logs capped and kept
  on the host, read back by a hostd started again; a container stopped
  ends them; signals reach their group; their environment is in no file.
- A VM's network: its alias resolves to it from a box and its machine
  resolves the box's, both ways over the workstream's Docker network.
- Tokens: no mirror, checkout, guest or host file holds a repository's
  token or a process's environment.
- What tower asks of a provider, every call of the contract, holds of
  hostd on a real host, called directly and through its tunnel.
- A build step runs on a network of its own (`deploy/hostd/
  build-network.nft`): it reaches the internet, and neither the host nor a
  box on a workstream network.
- A guest is fenced (`deploy/hostd/guest-network.nft`): its network's
  bridge is named for the fence, and a box reaches the internet and a peer
  by alias, and nothing listening on the host.
- The k3s blueprint, untagged, converges with the golden phase
  `--skip-tags=configure` (`docs/decisions/machine-blueprints.md`), and its
  machine is READY once workstream's ready check sees its node Ready.
- Boxes of public repositories' devcontainers
  (`docs/decisions/devcontainers.md`): one built from its Dockerfile on the
  host, from the mirror; one whose `remoteUser` runs it and owns
  `/workspace`, with the browser add-on built on it; one whose
  `postCreateCommand` installed its dependencies; and one of a repository
  without a devcontainer, which fails. Their images are pulled from
  mcr.microsoft.com: the first run takes some minutes more.

## A real hangar (the hangar provider)

`make e2e-hangar` holds tower's hangar provider (`internal/provider/
hangar`) to the provider contract over a real hangar, and reads a page
an instance serves through hangar's port forward (`test/hangar`). It needs
no testbed of tower's and no root: a hangar host of its own, or hangar's
testbed (`hangar: docs/runbook/testbed.md`), with

- a project for tower, whose Plan allows the networks, machines and disk
  the suite makes (about ten of each, 100Gi), and a service token of it
  that may manage containers, disks, networks and secrets, exec and
  forward ports;
- a box image in a registry the hangar node pulls from, by digest, with
  `test/testbed/guestbin` in `/usr/local/bin`, as `setup.sh` puts it in
  the testbed's images, and `busybox`.

Point the run at it:

```sh
TOWER_HANGAR_ENDPOINT=https://hangar.example \
TOWER_HANGAR_PROJECT=tower \
TOWER_HANGAR_TOKEN_FILE=/path/to/token \
TOWER_HANGAR_IMAGE=registry.example/tower/box@sha256:... \
TOWER_HANGAR_PULL_SECRET_FILE=/path/to/dockerconfig.json \
make e2e-hangar
```

`TOWER_HANGAR_PULL_SECRET_FILE` is a Docker config holding a login of
the image's registry, which the provider gives hangar as a registry-auth
Secret; leave it out for a registry read anonymously. A run deletes what
it made as it ends, but for its networks, which the next run's sweep
takes.
