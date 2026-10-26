# Deploying tower, its registry and hostd

How tower and the registry are installed on their own machine
(`docs/decisions/deployment.md`), and hostd on each host that is a hostd
target. Every listener is on the tailnet alone
(`docs/decisions/authentication.md`): the apiserver's API, the registry and
hostd's direct streams each bind the host's tailnet address, never every
interface. The steps are run as root, from a release unpacked on the host
(`docs/runbook/release.md`), which holds `bin/` and `deploy/`; on a
development machine, `make build-deploy` makes the same `bin/`.

A host is Ubuntu or Debian. Its tailnet tags are the configuration
repository's (`infra: tailnet/base.hujson`): tower's machine is
`tag:tower-control`, a hostd host `tag:tower-hostd-target` (beside its
`tag:host`), and workstream's `tag:workstream`; those grants are the whole
of who reaches the apiserver, the registry and hostd's direct streams.

## On tower's machine

### 1. The apiserver

```sh
useradd --system --home-dir /var/lib/tower --shell /usr/sbin/nologin tower
install -m 0755 bin/tower-apiserver bin/tower /usr/local/bin/
install -d -m 0755 /etc/tower
echo "TOWER_LISTEN=$(tailscale ip -4):8080" > /etc/tower/apiserver.env
# What a box of a repository without a devcontainer is made of when
# workstream names no fallback_image: workstream's own WORKSTREAM_BOX_IMAGE.
echo "TOWER_DEFAULT_BOX_IMAGE=" >> /etc/tower/apiserver.env
install -d -o tower -g tower -m 0700 /var/lib/tower
```

**The key-encryption key** seals every Secret at rest
(`docs/decisions/secrets.md`): a file the `tower` user alone reads, an id
and 32 random bytes. Without it no Secret can be written, and the
apiserver will not start while any is stored. Make it once, then copy it
to the vault: a backup of the database is of no use without it.

```sh
umask 077
printf 'k1 %s\n' "$(head -c 32 /dev/urandom | base64)" > /var/lib/tower/secrets-kek
chown tower:tower /var/lib/tower/secrets-kek
```

To rotate it, move the old one aside, make a new one, and start the
apiserver with `--secrets-kek-previous` naming the old one once (add it to
the unit's `ExecStart`); it seals everything again under the new one.

**The ticket key** signs the tickets of direct streams
(`docs/decisions/traffic.md`). The apiserver makes it on its first start,
`/var/lib/tower/ticket-key`, and its public half beside it,
`ticket-key.pub`, which every hostd verifies tickets with.

```sh
install -m 0644 deploy/tower/tower-apiserver.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now tower-apiserver
```

### 2. The admin token

Root on the machine reaches the apiserver through its admin socket, as an
administrator; the first token is made there, and is shown once:

```sh
TOWER_SERVER=unix:///run/tower/admin.sock tower token create admin --role admin --secret-file /root/tower-admin.token
```

From then on the CLI runs anywhere on the tailnet:

```sh
export TOWER_SERVER=http://<tower's tailnet name>:8080 TOWER_TOKEN_FILE=~/tower-admin.token
```

### 3. The registry

zot, as `zot.service`, every client signing in. Its certificate is the
machine's tailnet one, which `tailscale cert` makes (Docker and BuildKit
trust it as they trust any public certificate):

```sh
tailscale cert --cert-file /etc/tower/zot.crt --key-file /etc/tower/zot.key <tower's tailnet name>
htpasswd -bBc /etc/tower/zot.htpasswd hostd "$(head -c 24 /dev/urandom | base64 | tr -d '/+=' | tee /root/registry-hostd.password)"
TOWER_ZOT_ADDRESS=$(tailscale ip -4) TOWER_ZOT_PORT=5000 TOWER_ZOT_HTPASSWD=/etc/tower/zot.htpasswd \
  TOWER_ZOT_TLS_CERT=/etc/tower/zot.crt TOWER_ZOT_TLS_KEY=/etc/tower/zot.key \
  deploy/registry/install.sh
```

The `hostd` user is what every hostd pulls and pushes as; its password goes
to each host (below). `tailscale cert` must run again before the
certificate expires (a timer of the operator's), then `systemctl restart
zot`.

zot keeps its images in `/var/lib/zot/storage`: give `/var/lib/zot` a disk
of its own, mounted there before install.sh runs. A request may take up to
an hour (`readTimeout`, `writeTimeout`): a layer pushed from a target behind
a slow uplink takes minutes, and zot's own default cuts it off at one. Images are rebuilt on demand, so the
registry is not backed up.

### 4. Backing up the store

The store is `/var/lib/tower/tower.db` (SQLite). litestream replicates it
continuously to an S3 bucket, here the configuration repository's Garage
(`infra: stacks/garage`), with an access key that writes that bucket alone.
The key-encryption key is never in that bucket: it is in the vault, and a
backup restores only with both.

```sh
cat > /etc/litestream.yml <<EOF
dbs:
  - path: /var/lib/tower/tower.db
    replicas:
      - type: s3
        endpoint: https://<garage's tailnet name>
        bucket: tower-litestream
        path: tower.db
        region: garage
        force-path-style: true
EOF
cat > /etc/default/litestream <<EOF
LITESTREAM_ACCESS_KEY_ID=<key id>
LITESTREAM_SECRET_ACCESS_KEY=<secret>
EOF
chmod 0600 /etc/default/litestream
```

Install litestream from its release (a `.deb`), run its unit as the `tower`
user (`systemctl edit litestream`: `User=tower`, `EnvironmentFile=/etc/default/litestream`),
and enable it. To restore, with the apiserver stopped:
`litestream restore -o /var/lib/tower/tower.db /var/lib/tower/tower.db`,
then the KEK from the vault into `/var/lib/tower/secrets-kek`.

### 5. A hangar target

A hangar target is one project of a hangar endpoint, which tower uses
with that project's service token (`docs/decisions/layers.md`); tower
runs its provider itself, and nothing is installed on the hangar host.
Its boxes run tower-guest, which the apiserver writes into them; its
mirrors and builds run in a system container of the target's own,
`tower-sys-*`, made from the release's system image, which joins the
tailnet to push to the registry (`docs/decisions/images.md`).

```sh
install -d -m 0755 /usr/local/lib/tower
install -m 0755 bin/tower-guest /usr/local/lib/tower/tower-guest
cat >> /etc/tower/apiserver.env <<END
TOWER_GUEST=/usr/local/lib/tower/tower-guest
TOWER_PUBLIC_URL=http://$(tailscale ip -4):8080
TOWER_REGISTRY=<the registry's tailnet name>:5000
TOWER_SYSTEM_IMAGE=<below>
END
```

**The system image** is the release's `tower-system_TAG_linux_amd64.tar`,
an OCI layout. Push it to the registry from a machine on the tailnet with
this repository, and set `TOWER_SYSTEM_IMAGE` to it by the digest crane
prints, named by `TOWER_REGISTRY` (each target pulls it by its own
`registry_host`):

```sh
mkdir tower-system && tar -xf tower-system_TAG_linux_amd64.tar -C tower-system
go tool -modfile=tools/tools.mod crane push tower-system <registry>/tower/system:TAG
```

**The registry's public side.** hangar pulls by a name the hangar host
reaches, which the tailnet is not: the registry's public one, read-only,
open to the hangar host's address alone, with a login of its own
(`docs/decisions/images.md`). zot as installed above listens on the
tailnet alone, with the tailnet name's certificate; these steps do not
make the public side. Before a hangar target is made, the registry must
answer at the target's `registry_host` with a certificate for that name,
and serve nothing but pulls there.

**The target**: its project's service token, a read-only registry login
for hangar's pulls, a writing one for its builds, and a tailnet auth key
for its builder, each a Secret in the reserved project `system`, then the
Target:

```yaml
apiVersion: targets.tower.hangarlab.cloud/v1alpha1
kind: Target
metadata: {name: hangar-main}
spec:
  hangar:
    endpoint: https://api.hangar.example
    project: tower
    credential_secret: hangar-token           # key "token"
    registry_pull_secret: registry-pull       # key ".dockerconfigjson"
    registry_host: registry.example.com:5000  # as hangar pulls
    registry_push_secret: registry-push       # keys "username", "password"
    tailnet_auth_secret: builder-authkey      # key "authkey"
```

`tower get target hangar-main` says whether hangar answers, the project's
room by its quota, and whether the system container answers and is on
the tailnet. The project's Plan must allow a network per workstream and
one more, and the disk of every box plus the system container's 50Gi.
Every editor stream and followed log holds one of hangar's machine
calls the whole time: where the Plan has `machine_calls`, set it to
that many at once and more; a hangar without it has a fixed cap.

## On each hostd host

A hostd host needs a ZFS pool (`docs/decisions/hostd-storage.md`), KVM
(`/dev/kvm`: VMs never run without it) and the tailnet.

### 1. What its drivers run on

```sh
deploy/hostd/install-deps.sh        # ZFS, Docker (live-restore), Cloud Hypervisor, its firmware, BuildKit and its build network
zfs create -o mountpoint=none <pool>/hostd   # or tower/hostd on a pool named tower
deploy/hostd/base-image.sh ubuntu-24.04      # the base image machines boot, with ansible
install -m 0755 bin/hostd /usr/local/bin/
install -D -m 0755 bin/tower-guest /usr/local/lib/tower/tower-guest
```

hostd's flags default to these paths and to the dataset `tower/hostd`;
change `--zfs-dataset` in the unit for another pool.

Build steps run on the bridge `tower-build0`, on 10.231.0.0/24
(`docs/decisions/images.md`); where the host already has that network,
run install-deps.sh with `TOWER_BUILD_SUBNET` set to another.

install-deps.sh also fences the guests (`docs/decisions/traffic.md`): the
unit `tower-guest-network` loads `/etc/tower/guest-network.nft` before
Docker starts, and Docker requires it. Other containers on the host, such
as upkeep's stacks on their own `br-*` networks, are not fenced; one run
with `docker run` on Docker's default bridge is. Guests resolve through
hostd's `--guest-dns`, public servers (1.1.1.1 and 9.9.9.9 unless set),
since the fence drops what they send to the host's own resolver. `nft list
table inet tower_guest` shows its counters, and the journal its refusals,
prefixed `tower-guest:`.

### 2. Its target and token

As an administrator, the Target and its ROLE_HOSTD token:

```sh
tower apply -f - <<'EOF'
apiVersion: targets.tower.hangarlab.cloud/v1alpha1
kind: Target
metadata: {name: host-c}
spec: {hostd: {}}
EOF
tower token create hostd-host-c --role hostd --target host-c --secret-file hostd.token
```

Copy `hostd.token` to the host as `/etc/tower/hostd.token`, root's alone
(0600): hostd refuses a token file anyone else can read. Copy tower's
`/var/lib/tower/ticket-key.pub` to `/etc/tower/ticket-key.pub`, and the
registry user's password to `/etc/tower/registry.password` (0600).

```sh
cat > /etc/tower/hostd.env <<EOF
TOWER_URL=http://<tower's tailnet name>:8080
TOWER_TARGET=host-c
HOSTD_DIRECT_LISTEN=$(tailscale ip -4):7443
TOWER_REGISTRY=<tower's tailnet name>:5000
TOWER_REGISTRY_USER=hostd
EOF
install -m 0644 deploy/hostd/hostd.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now hostd
tower get target host-c     # Reachable and Reporting, its pool and drivers among its facts
```

### What runs where

| What | Where |
|---|---|
| disks | datasets and zvols under `tower/hostd/disks`, a box's mounted under `/var/lib/tower-hostd/disks` |
| goldens | `tower/hostd/goldens/<name>@<name>` |
| mirrors | `tower/hostd/mirrors`, one dataset per repository |
| containers' processes | `/var/lib/tower-hostd/processes/<container>` |
| VMs | a Docker container each (`tower.hangarlab.cloud/driver=vm`), its console log in `/var/lib/tower-hostd/vms/<name>` |
| hostd's ssh key for VMs | `/var/lib/tower-hostd/vm-ssh` |

Nothing of a Secret's value is written on the host's disks: it reaches a
guest as the environment of one command or process, or as a box's secret
file, which is held in memory, on a tmpfs every box container has at
`/run/tower/secrets`, its path in the box a link to it; and git's tokens as
the environment of one git command. A machine takes no secret files.

### Known limitations

- **A machine's playbook runs as root unless its blueprint names a
  user.** tower runs a blueprint in its VM with `ansible-playbook
  --connection=local`, as root; workstream runs it over ssh as its VM
  user. A blueprint written for workstream's way (become tasks through
  sudo, the others and `~` the user's), such as `workstream-dev`
  (`workstream: src/machine/playbooks/workstream-dev.yml`), converges as
  written only when its client sets `Blueprint.run_as` to that user:
  tower then makes the user, with a home and passwordless sudo, and runs
  both phases, the ready check, and the commands, processes and files the
  client's calls make in the machine as it
  (`docs/decisions/machine-blueprints.md`). Without it, its `become:
  false` tasks land in root's home and its ready check reads root's. A
  blueprint reading `ansible_user` is given it by its client, in its
  golden vars; tower sets none. The blueprint is carried unchanged.

### Upgrading

Replace `/usr/local/bin/hostd` and `/usr/local/lib/tower/tower-guest`, then
`systemctl restart hostd`: containers and VMs run on, as Docker's; a VM
gets the new tower-guest the next time hostd reaches it.

A workstream network made by a hostd older than the guest fence keeps its
`br-*` bridge, which the fence does not match: `docker network ls --filter
label=tower.hangarlab.cloud/driver=network` lists them, and `ip link`
shows which are not `twr*`. Each is made again, fenced, once its
workstream's guests are gone.
