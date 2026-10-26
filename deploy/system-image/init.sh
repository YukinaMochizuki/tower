#!/bin/sh
# init.sh is a hangar target's system container's process
# (internal/system): it starts tailscaled, in userspace, its state on the
# container's disk, and buildkitd, which reaches tower's registry through
# tailscaled's proxy, and ends when either does, so hangar starts the
# container again. tower-system runs beside them, a call at a time, as the
# provider asks through hangar's exec.
set -eu

root=${TOWER_SYSTEM_ROOT:-/var/lib/tower}
# What calls cut short left in scratch space goes as the container starts.
rm -rf "$root/scratch"
mkdir -p "$root/tailscale" "$root/buildkit" "$root/scratch" /var/run/tailscale

# A registry served over plain HTTP, as one on the tailnet alone may be,
# whose traffic the tailnet encrypts (TOWER_REGISTRY_PLAIN_HTTP=1).
config=/run/buildkitd.toml
: >"$config"
if [ "${TOWER_REGISTRY_PLAIN_HTTP:-}" = 1 ] && [ -n "${TOWER_REGISTRY:-}" ]; then
	printf '[registry."%s"]\n  http = true\n  insecure = true\n' "$TOWER_REGISTRY" >"$config"
fi

tailscaled --tun=userspace-networking --state="$root/tailscale/tailscaled.state" \
	--socket=/var/run/tailscale/tailscaled.sock \
	--outbound-http-proxy-listen=127.0.0.1:1055 &
ts=$!

# The registry is on the tailnet; tailscaled's proxy dials it there, and
# anything else as the container would.
HTTP_PROXY=http://127.0.0.1:1055 HTTPS_PROXY=http://127.0.0.1:1055 NO_PROXY=127.0.0.1,localhost \
	buildkitd --config "$config" --root "$root/buildkit" --addr unix:///run/buildkit/buildkitd.sock &
bk=$!

trap 'kill "$ts" "$bk" 2>/dev/null' TERM INT
# Whichever ends first ends the container.
while kill -0 "$ts" 2>/dev/null && kill -0 "$bk" 2>/dev/null; do
	sleep 5
done
kill "$ts" "$bk" 2>/dev/null || true
exit 1
