#!/usr/bin/env bash
# Box add-on "desktop": an X desktop viewable in a browser.
#
# Runs as root in an image layer built FROM the box's base (src/box/addon-image.ts). Options arrive
# upper-cased per the devcontainer Feature spec (RESOLUTION, PORT) and are baked into `ws-desktop`.
#
# Nothing starts at boot: a box's entrypoint is `sleep infinity`, owned by workstream. `ws-desktop` runs
# the whole stack in the FOREGROUND so it can be handed to run_service_box as one service, and exits
# when any part of it dies, so a broken desktop reads as a stopped service rather than a black screen.
# x11vnc listens on localhost only; noVNC (websockify) is the one port meant to be exported.
set -euo pipefail

RESOLUTION="${RESOLUTION:-1440x900x24}"
PORT="${PORT:-6080}"

if ! command -v apt-get >/dev/null 2>&1; then
  echo "the desktop add-on needs an apt-based image (Debian or Ubuntu)" >&2
  exit 1
fi

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends \
  xvfb x11vnc fluxbox novnc websockify xterm x11-utils x11-xserver-utils dbus-x11 procps \
  fonts-noto-cjk fonts-dejavu-core
rm -rf /var/lib/apt/lists/*

# noVNC's package ships vnc.html but no index; the exported URL should land on a connected, scaled view.
cat >/usr/share/novnc/index.html <<'EOF'
<!doctype html>
<meta http-equiv="refresh" content="0; url=vnc.html?autoconnect=1&resize=scale&reconnect=1">
EOF

# fluxbox's styles set their wallpaper through fbsetbg, which with no wallpaper setter installed pops an
# error dialog onto every new desktop, whatever the style or its overlay says. This fbsetbg shadows the
# real one on PATH (/usr/local/bin comes first) and paints a plain root instead.
cat >/usr/local/bin/fbsetbg <<'EOF'
#!/bin/sh
exec xsetroot -solid '#2e3440'
EOF
chmod 755 /usr/local/bin/fbsetbg

cat >/usr/local/bin/ws-desktop <<EOF
#!/bin/bash
# Start the virtual desktop in the foreground: Xvfb on \$DISPLAY, fluxbox, x11vnc (localhost:5900),
# and noVNC on port ${PORT}. Logs go to /tmp/ws-desktop/. Exits when any of them dies.
set -euo pipefail
DISPLAY="\${DISPLAY:-:99}"
export DISPLAY
LOGS=/tmp/ws-desktop
mkdir -p "\$LOGS"
Xvfb "\$DISPLAY" -screen 0 ${RESOLUTION} -nolisten tcp >"\$LOGS/xvfb.log" 2>&1 &
i=0
until xdpyinfo >/dev/null 2>&1; do
  i=\$((i + 1))
  if [ \$i -gt 50 ]; then echo "Xvfb did not come up, see \$LOGS/xvfb.log" >&2; exit 1; fi
  sleep 0.1
done
fluxbox >"\$LOGS/fluxbox.log" 2>&1 &
x11vnc -display "\$DISPLAY" -localhost -rfbport 5900 -forever -shared -nopw -quiet >"\$LOGS/x11vnc.log" 2>&1 &
websockify --web /usr/share/novnc ${PORT} localhost:5900 >"\$LOGS/novnc.log" 2>&1 &
echo "desktop on \$DISPLAY, noVNC on port ${PORT}"
trap 'kill 0' INT TERM EXIT
wait -n
EOF
chmod 755 /usr/local/bin/ws-desktop
echo "desktop add-on: installed (run ws-desktop, then export port ${PORT})"
