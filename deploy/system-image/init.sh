#!/bin/sh
# init.sh is a hangar target's system container's process
# (internal/system): it starts tailscaled, in userspace, its state on the
# container's disk, buildkitd, which reaches tower's registry through
# tailscaled's proxy, and tower-system serve, which boxes reach over the
# target's EndpointService, and ends when any of them does, so hangar
# starts the container again. tower-system also runs beside them, a call
# at a time, as the provider asks through hangar's exec.
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

# tailscaled's proxy, which the image's environment names
# (deploy/system-image/build.sh), so that tower-system's calls, which
# hangar's exec starts in it, have it too: the tailnet, and its names, are
# reached through it alone.
proxy=${TOWER_SYSTEM_GIT_PROXY:-http://127.0.0.1:1055}
export TOWER_SYSTEM_GIT_PROXY="$proxy"
# tailscaled listens on its host:port; anything but http://host:port, a
# trailing slash allowed, ends the container rather than leave git
# without the tailnet.
listen=
case $proxy in http://*) listen=${proxy#http://} ;; esac
listen=${listen%/}
case $listen in *:*) port=${listen##*:} ;; *) port= ;; esac
case $listen in :* | */* | *@*) listen= ;; esac
case $port in '' | *[!0-9]*) listen= ;; esac
if [ -z "$listen" ]; then
	echo "tower-system-init: TOWER_SYSTEM_GIT_PROXY=$proxy is not http://host:port" >&2
	exit 1
fi

tailscaled --tun=userspace-networking --state="$root/tailscale/tailscaled.state" \
	--socket=/var/run/tailscale/tailscaled.sock \
	--outbound-http-proxy-listen="$listen" &
ts=$!

# The registry and the tailnet's repositories are on the tailnet;
# tailscaled's proxy dials them there, and anything else, such as GitHub,
# as the container would. tower-system gives git the proxy itself.
HTTP_PROXY=$proxy HTTPS_PROXY=$proxy NO_PROXY=127.0.0.1,localhost \
	buildkitd --config "$config" --root "$root/buildkit" --addr unix:///run/buildkit/buildkitd.sock &
bk=$!

tower-system serve &
sv=$!

trap 'kill "$ts" "$bk" "$sv" 2>/dev/null' TERM INT
# Whichever ends first ends the container.
while kill -0 "$ts" 2>/dev/null && kill -0 "$bk" 2>/dev/null && kill -0 "$sv" 2>/dev/null; do
	sleep 5
done
kill "$ts" "$bk" "$sv" 2>/dev/null || true
exit 1
