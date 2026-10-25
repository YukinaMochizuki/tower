#!/usr/bin/env bash
# Installs zot, the registry beside tower (docs/decisions/images.md), as
# the service zot.service, at the version deploy/hostd/versions.env pins
# (docs/runbook/deploy.md). Every client signs in: the users are those of
# an htpasswd file, and nothing is served anonymously. Idempotent. Run as
# root.
#
#   TOWER_ZOT_ADDRESS   the address it listens on: the host's tailnet
#                       address, never a public one
#   TOWER_ZOT_PORT      its port, 5000 unless set
#   TOWER_ZOT_HTPASSWD  the htpasswd file of its users (bcrypt, htpasswd -B)
#   TOWER_ZOT_TLS_CERT, TOWER_ZOT_TLS_KEY
#                       a certificate and its key to serve HTTPS with, such
#                       as `tailscale cert` makes; unset serves plain HTTP,
#                       for a testbed on loopback alone
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=deploy/hostd/versions.env
. "$here/../hostd/versions.env"

fail() { echo "FAIL: $*" >&2; exit 1; }
[ "$(id -u)" = 0 ] || fail "must run as root"
: "${TOWER_ZOT_ADDRESS:?the address zot listens on}"
: "${TOWER_ZOT_HTPASSWD:?the htpasswd file of its users}"
port=${TOWER_ZOT_PORT:-5000}
[ -f "$TOWER_ZOT_HTPASSWD" ] || fail "$TOWER_ZOT_HTPASSWD is not there"

if ! { [ -f /usr/local/bin/zot ] && echo "$TOWER_ZOT_SHA256  /usr/local/bin/zot" | sha256sum -c --status; }; then
	tmp=$(mktemp)
	curl -fsSL --retry 3 -o "$tmp" "$TOWER_ZOT_URL"
	echo "$TOWER_ZOT_SHA256  $tmp" | sha256sum -c --status || { rm -f "$tmp"; fail "zot does not match its sha256"; }
	install -m 0755 "$tmp" /usr/local/bin/zot
	rm -f "$tmp"
fi

id zot >/dev/null 2>&1 || useradd --system --home-dir /var/lib/zot --shell /usr/sbin/nologin zot
install -d -o zot -g zot -m 0700 /var/lib/zot
install -d -m 0755 /etc/zot
install -o root -g zot -m 0640 "$TOWER_ZOT_HTPASSWD" /etc/zot/htpasswd
tls=
if [ -n "${TOWER_ZOT_TLS_CERT:-}" ]; then
	install -o root -g zot -m 0644 "$TOWER_ZOT_TLS_CERT" /etc/zot/tls.crt
	install -o root -g zot -m 0640 "$TOWER_ZOT_TLS_KEY" /etc/zot/tls.key
	tls=', "tls": { "cert": "/etc/zot/tls.crt", "key": "/etc/zot/tls.key" }'
fi
cat >/etc/zot/config.json <<EOF
{
  "distSpecVersion": "1.1.1",
  "storage": { "rootDirectory": "/var/lib/zot", "gc": true, "dedupe": true },
  "http": {
    "address": "$TOWER_ZOT_ADDRESS",
    "port": "$port",
    "auth": { "htpasswd": { "path": "/etc/zot/htpasswd" } }$tls
  },
  "log": { "level": "info" }
}
EOF
chmod 0644 /etc/zot/config.json
install -m 0644 "$here/zot.service" /etc/systemd/system/zot.service
systemctl daemon-reload
systemctl enable zot.service >/dev/null
systemctl restart zot.service
echo "zot $TOWER_ZOT_VERSION on $TOWER_ZOT_ADDRESS:$port"
