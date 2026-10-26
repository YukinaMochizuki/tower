#!/usr/bin/env bash
# build.sh OUT builds a hangar target's system image (internal/system) as
# an image tarball at OUT, from the checked-out commit: BuildKit's own
# image (TOWER_SYSTEM_BASE, which carries git) with a layer of tower-system,
# tailscale and init.sh, its entrypoint. No Docker: crane appends the layer
# to the base by digest, in a registry of its own on loopback for the
# build's length. The deploy pushes OUT to tower's registry and names it
# with tower-apiserver's --system-image (docs/runbook/deploy.md). Run from
# the repository's root.
set -euo pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }
[ $# = 1 ] || fail "usage: build.sh OUT"
out=$(realpath -m "$1")
# shellcheck source=../hostd/versions.env
. deploy/hostd/versions.env
# crane's own binary: the registry below runs it directly, so that
# cleanup's kill stops the registry, not a parent that leaves it running.
cranebin=$(go tool -modfile=tools/tools.mod -n crane)
crane() { "$cranebin" "$@"; }

work=$(mktemp -d)
reg=
cleanup() {
	[ -z "$reg" ] || kill "$reg" 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT
layer=$work/layer
mkdir -p "$layer/usr/local/bin"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$layer/usr/local/bin/" ./cmd/tower-system
curl -fsSL -o "$work/tailscale.tgz" "$TOWER_TAILSCALE_URL"
echo "$TOWER_TAILSCALE_SHA256  $work/tailscale.tgz" | sha256sum -c --quiet
tar -xzf "$work/tailscale.tgz" -C "$work"
install -m 0755 "$work/tailscale_${TOWER_TAILSCALE_VERSION}_amd64/tailscale" "$work/tailscale_${TOWER_TAILSCALE_VERSION}_amd64/tailscaled" "$layer/usr/local/bin/"
install -m 0755 deploy/system-image/init.sh "$layer/usr/local/bin/tower-system-init"
# Owned by root and dated by the commit, so the same commit packs the same.
when=$(git show -s --format=%cI HEAD)
chmod 0755 "$layer" "$layer/usr" "$layer/usr/local" "$layer/usr/local/bin"
tar --sort=name --owner=0 --group=0 --numeric-owner --mode=go-w --mtime="$when" -C "$layer" -cf "$work/layer.tar" .

# A registry of the build's own, on a port of the kernel's choosing.
port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')
"$cranebin" registry serve --address "127.0.0.1:$port" >"$work/registry.log" 2>&1 &
reg=$!
for _ in $(seq 50); do
	curl -fs "http://127.0.0.1:$port/v2/" >/dev/null && break
	sleep 0.1
done
ref=127.0.0.1:$port/tower/system:build
crane append --platform linux/amd64 -b "$TOWER_SYSTEM_BASE" -f "$work/layer.tar" -t "$ref"
crane mutate --entrypoint /usr/local/bin/tower-system-init --cmd "" \
	--env TOWER_SYSTEM_ROOT=/var/lib/tower -t "$ref" "$ref" >/dev/null
crane pull --format oci "$ref" "$out.dir"
tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="$when" -C "$out.dir" -cf "$out" .
rm -rf "$out.dir"
echo "system image $(crane digest "$ref") in $out"
