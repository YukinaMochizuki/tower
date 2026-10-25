#!/usr/bin/env bash
# Installs what hostd's drivers run on a host (docs/runbook/deploy.md):
# the packages (ZFS, Docker, git, qemu-img), Cloud Hypervisor and its
# firmware, and BuildKit as a service, each at the version
# versions.env pins and checked against its sha256. It leaves Docker with
# live-restore on, so containers and VMs outlive a restart of dockerd.
# Idempotent. Run as root.
#
# TOWER_REGISTRY_HTTP, if set, is a registry (host:port) served over plain
# HTTP, as the testbed's is, which Docker and BuildKit are told to use so.
# A production registry is served over TLS and needs nothing here.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=deploy/hostd/versions.env
. "$here/versions.env"

fail() { echo "FAIL: $*" >&2; exit 1; }
[ "$(id -u)" = 0 ] || fail "must run as root"

# fetch URL SHA256 DEST fetches a file once, checked, and installs it.
fetch() {
	local url=$1 sum=$2 dest=$3 mode=${4:-0755} tmp
	if [ -f "$dest" ] && echo "$sum  $dest" | sha256sum -c --status; then
		return 0
	fi
	tmp=$(mktemp)
	curl -fsSL --retry 3 -o "$tmp" "$url"
	echo "$sum  $tmp" | sha256sum -c --status || { rm -f "$tmp"; fail "$url does not match its sha256"; }
	install -D -m "$mode" "$tmp" "$dest"
	rm -f "$tmp"
	echo "installed $dest"
}

# shellcheck disable=SC2086
DEBIAN_FRONTEND=noninteractive apt-get install -qy --no-install-recommends $TOWER_HOSTD_PACKAGES >/dev/null ||
	{ apt-get update -q && DEBIAN_FRONTEND=noninteractive apt-get install -qy --no-install-recommends $TOWER_HOSTD_PACKAGES; }
modprobe zfs

fetch "$TOWER_CH_URL" "$TOWER_CH_SHA256" /usr/local/bin/cloud-hypervisor
# The firmware is read by Cloud Hypervisor inside each VM's container.
fetch "$TOWER_FW_URL" "$TOWER_FW_SHA256" /usr/local/share/tower/CLOUDHV.fd 0644

# BuildKit: buildkitd as a service on its own socket, root's alone.
bk=/usr/local/lib/tower/buildkit-$TOWER_BUILDKIT_VERSION
if [ ! -x "$bk/bin/buildkitd" ]; then
	fetch "$TOWER_BUILDKIT_URL" "$TOWER_BUILDKIT_SHA256" "$bk.tar.gz" 0644
	mkdir -p "$bk"
	tar -xzf "$bk.tar.gz" -C "$bk"
	rm -f "$bk.tar.gz"
fi
for b in "$bk"/bin/*; do ln -sf "$b" "/usr/local/bin/$(basename "$b")"; done
mkdir -p /etc/buildkit
{
	echo "# Written by deploy/hostd/install-deps.sh."
	echo "[worker.oci]"
	echo "  enabled = true"
	echo "  gc = true"
	echo "[worker.containerd]"
	echo "  enabled = false"
	if [ -n "${TOWER_REGISTRY_HTTP:-}" ]; then
		echo "[registry.\"$TOWER_REGISTRY_HTTP\"]"
		echo "  http = true"
	fi
} >/etc/buildkit/buildkitd.toml
install -m 0644 "$here/buildkitd.service" /etc/systemd/system/buildkitd.service
systemctl daemon-reload
systemctl enable buildkitd.service >/dev/null
systemctl restart buildkitd.service

# Docker: live-restore, and the plain-HTTP registry if there is one.
python3 - "${TOWER_REGISTRY_HTTP:-}" <<'EOF'
import json, os, sys
path = "/etc/docker/daemon.json"
conf = {}
if os.path.exists(path):
    with open(path) as f:
        conf = json.load(f)
conf["live-restore"] = True
reg = sys.argv[1]
if reg:
    regs = conf.setdefault("insecure-registries", [])
    if reg not in regs:
        regs.append(reg)
os.makedirs("/etc/docker", exist_ok=True)
with open(path, "w") as f:
    json.dump(conf, f, indent=2)
EOF
systemctl enable docker.service >/dev/null
systemctl restart docker.service

echo "cloud-hypervisor $(cloud-hypervisor --version)"
echo "$(buildkitd --version)"
docker version --format 'docker {{.Server.Version}}'
zfs version | head -n 1
