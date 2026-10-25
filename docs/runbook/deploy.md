# Deploying tower, its registry and hostd

How tower and the registry are installed on their own machine
(`docs/decisions/deployment.md`), and hostd on each host that is a hostd
target. Every listener is on the tailnet alone
(`docs/decisions/authentication.md`): the apiserver's API, the registry and
hostd's direct streams each bind the host's tailnet address, never every
interface. The steps are run as root; `make` builds the binaries where Go
is (`CGO_ENABLED=0` for `tower-guest`).

```sh
make build-deploy   # bin/tower-apiserver, bin/tower, bin/hostd, bin/tower-guest
```

## On tower's machine

### 1. The apiserver

```sh
useradd --system --home-dir /var/lib/tower --shell /usr/sbin/nologin tower
install -m 0755 bin/tower-apiserver bin/tower /usr/local/bin/
install -d -m 0755 /etc/tower
echo "TOWER_LISTEN=$(tailscale ip -4):8080" > /etc/tower/apiserver.env
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

## On each hostd host

A hostd host needs a ZFS pool (`docs/decisions/hostd-storage.md`), KVM
(`/dev/kvm`: VMs never run without it) and the tailnet.

### 1. What its drivers run on

```sh
deploy/hostd/install-deps.sh        # ZFS, Docker (live-restore), Cloud Hypervisor, its firmware, BuildKit
zfs create -o mountpoint=none <pool>/hostd   # or tower/hostd on a pool named tower
deploy/hostd/base-image.sh ubuntu-24.04      # the base image machines boot, with ansible
install -m 0755 bin/hostd /usr/local/bin/
install -D -m 0755 bin/tower-guest /usr/local/lib/tower/tower-guest
```

hostd's flags default to these paths and to the dataset `tower/hostd`;
change `--zfs-dataset` in the unit for another pool.

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

Nothing of a Secret's value is written on the host: it reaches a guest as
the environment of one command or process, or as a box's secret file
inside the guest, and git's tokens as the environment of one git command.

### Upgrading

Replace `/usr/local/bin/hostd` and `/usr/local/lib/tower/tower-guest`, then
`systemctl restart hostd`: containers and VMs run on, as Docker's; a VM
gets the new tower-guest the next time hostd reaches it.
