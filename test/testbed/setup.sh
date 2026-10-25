#!/usr/bin/env bash
# Makes a disposable Ubuntu 24.04 VM a testbed for the real-host tests
# (docs/runbook/testbed.md): what a hostd host has (deploy/hostd/
# install-deps.sh: ZFS, Docker, Cloud Hypervisor and its firmware,
# BuildKit), Go, a ZFS pool named tower on a sparse file behind a
# direct-IO loop device, a zot registry on 127.0.0.1:5000 with a user of
# its own, and the base images the tests boot: ubuntu-24.04
# (deploy/hostd/base-image.sh) and drivertest, a small Alpine whose guest
# runs the commands drivertest names. Idempotent; after a reboot, run it
# again: it attaches the file and imports the pool.
#
# Not a file vdev: its writes go through the page cache and can deadlock zvol
# writeback (hangar: docs/runbook/testbed.md).
#
# Run as root on a throwaway VM only.
set -euo pipefail

repo=$(cd "$(dirname "$0")/../.." && pwd)
# shellcheck source=deploy/hostd/versions.env
. "$repo/deploy/hostd/versions.env"
pool=tower
pool_file=${TOWER_TESTBED_POOL_FILE:-/var/tmp/tower-testbed-pool.img}
pool_size=${TOWER_TESTBED_POOL_SIZE:-40G}
registry=127.0.0.1:5000
base_images=/var/lib/tower-hostd/base-images

fail() { echo "FAIL: $*" >&2; exit 1; }
[ "$(id -u)" = 0 ] || fail "must run as root"
[ -e /dev/kvm ] || fail "no /dev/kvm: the VM tests need KVM, and never run under TCG"

apt-get update -q
DEBIAN_FRONTEND=noninteractive apt-get install -qy --no-install-recommends \
	build-essential make apache2-utils jq python3 >/dev/null

TOWER_REGISTRY_HTTP=$registry "$repo/deploy/hostd/install-deps.sh"

# Go, at go.mod's version, for whoever runs make.
if [ "$(/usr/local/go/bin/go env GOVERSION 2>/dev/null)" != "go$TOWER_GO_VERSION" ]; then
	curl -fsSL --retry 3 -o /tmp/go.tgz "$TOWER_GO_URL"
	echo "$TOWER_GO_SHA256  /tmp/go.tgz" | sha256sum -c --status || fail "Go does not match its sha256"
	rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz && rm -f /tmp/go.tgz
fi
ln -sf /usr/local/go/bin/go /usr/local/bin/go
ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt

# The pool, as hangar's testbed makes its own.
new_file=0
if [ ! -e "$pool_file" ]; then
	truncate -s "$pool_size" "$pool_file"
	new_file=1
fi
loop=$(losetup -n -O NAME -j "$pool_file" | head -n 1)
if [ -z "$loop" ]; then
	loop=$(losetup --find --show --direct-io=on "$pool_file")
fi
[ "$(losetup -n -O DIO "$loop" | tr -d ' ')" = 1 ] || fail "$loop is not in direct-IO mode"
if ! zpool list -H -o name "$pool" >/dev/null 2>&1; then
	if [ "$new_file" = 1 ]; then
		zpool create -o ashift=12 -o autotrim=on -O compression=lz4 -O atime=off \
			-O mountpoint=none "$pool" "$loop"
	else
		zpool import -d "$loop" "$pool"
	fi
fi

# zot on loopback, with a user hostd pulls and pushes as. Its password is
# the testbed's, in /etc/tower/testbed-registry.env, which the tests read.
install -d -m 0700 /etc/tower /var/lib/zot
if [ ! -f /etc/tower/testbed-registry.env ]; then
	pw=$(head -c 24 /dev/urandom | base64 | tr -d '/+=')
	umask 077
	printf 'TOWER_REGISTRY=%s\nTOWER_REGISTRY_USER=hostd\nTOWER_REGISTRY_PASSWORD=%s\n' "$registry" "$pw" >/etc/tower/testbed-registry.env
	htpasswd -bBc /etc/tower/zot.htpasswd hostd "$pw" >/dev/null
	umask 022
fi
TOWER_ZOT_ADDRESS=127.0.0.1 TOWER_ZOT_PORT=5000 TOWER_ZOT_HTPASSWD=/etc/tower/zot.htpasswd \
	"$repo/deploy/registry/install.sh"

# The base images.
"$repo/deploy/hostd/base-image.sh" ubuntu-24.04
make_drivertest() {
	local out=$base_images/drivertest.raw work mnt loop
	[ -e "$out" ] && return 0
	work=$(mktemp -d "$base_images/.making-drivertest.XXXXXX")
	mnt=$work/root
	curl -fsSL --retry 3 -o "$work/image" "$TOWER_ALPINE_URL"
	echo "$TOWER_ALPINE_SHA256  $work/image" | sha256sum -c --status || fail "Alpine does not match its sha256"
	qemu-img convert -O raw "$work/image" "$work/image.raw"
	loop=$(losetup --find --show --partscan "$work/image.raw")
	udevadm settle
	mkdir -p "$mnt"
	# The root filesystem is the image's last partition.
	mount "$(lsblk -lnpo NAME "$loop" | tail -n 1)" "$mnt"
	install -m 0755 "$repo"/test/testbed/guestbin/* "$mnt/usr/local/bin/"
	# The echo drivertest dials on its EchoPort, 7777.
	cat >"$mnt/etc/local.d/drivertest-echo.start" <<'EOF'
#!/bin/sh
nc -lk -p 7777 -e /bin/cat >/dev/null 2>&1 &
EOF
	chmod 0755 "$mnt/etc/local.d/drivertest-echo.start"
	ln -sf /etc/init.d/local "$mnt/etc/runlevels/default/local"
	umount "$mnt"
	losetup -d "$loop"
	mv "$work/image.raw" "$out"
	rm -rf "$work"
	echo "made $out"
}
make_drivertest

zpool status "$pool" | head -n 3
ls -l "$base_images"
