#!/usr/bin/env bash
# Box add-on "browser": headless Chromium on PATH as `chromium`.
#
# Runs as root in an image layer built FROM the box's base (src/box/addon-image.ts). Options arrive as
# environment variables, upper-cased per the devcontainer Feature spec (PLAYWRIGHTVERSION, NODEVERSION).
#
# The Chromium is Playwright's build, installed with `--with-deps`, because that is the one source that
# covers both Debian and Ubuntu on both amd64 and arm64: Ubuntu's own `chromium` package is a snap and
# does not run in a container. Node is fetched only to run the installer and removed afterwards, so the
# box's own toolchain is untouched. PLAYWRIGHT_BROWSERS_PATH is set only while installing: a repo with its
# own pinned Playwright must keep resolving its own browsers, which is why nothing here exports it.
set -euo pipefail

PLAYWRIGHTVERSION="${PLAYWRIGHTVERSION:-1.63.0}"
NODEVERSION="${NODEVERSION:-24.21.0}"
BROWSERS=/opt/ws-browser

if ! command -v apt-get >/dev/null 2>&1; then
  echo "the browser add-on needs an apt-based image (Debian or Ubuntu)" >&2
  exit 1
fi

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends ca-certificates curl xz-utils \
  fonts-noto-cjk fonts-noto-color-emoji fonts-liberation

case "$(uname -m)" in
  x86_64) NODE_ARCH=x64 ;;
  aarch64 | arm64) NODE_ARCH=arm64 ;;
  *) echo "the browser add-on does not support $(uname -m)" >&2; exit 1 ;;
esac

NODE_DIR="$(mktemp -d)"
curl -fsSL "https://nodejs.org/dist/v${NODEVERSION}/node-v${NODEVERSION}-linux-${NODE_ARCH}.tar.xz" | tar -xJ -C "$NODE_DIR" --strip-components=1
export PATH="$NODE_DIR/bin:$PATH"
export PLAYWRIGHT_BROWSERS_PATH="$BROWSERS"
export npm_config_cache="$NODE_DIR/npm-cache"
npx -y "playwright@${PLAYWRIGHTVERSION}" install --with-deps chromium

CHROME="$(find "$BROWSERS" -type f -name chrome -path '*chromium-*' | head -1)"
if [ -z "$CHROME" ]; then
  echo "Playwright installed no chromium binary under $BROWSERS" >&2
  exit 1
fi
chmod -R a+rX "$BROWSERS"

# Always --no-sandbox. As root Chromium refuses to start with its sandbox on, and as a non-root
# devcontainer user it crashes: the sandbox needs user namespaces, which docker's default seccomp
# profile denies. The box is the isolation boundary here, not Chromium's sandbox.
cat >/usr/local/bin/chromium <<EOF
#!/bin/sh
exec "$CHROME" --no-sandbox "\$@"
EOF
chmod 755 /usr/local/bin/chromium

rm -rf "$NODE_DIR" /var/lib/apt/lists/*
chromium --headless --disable-gpu --dump-dom about:blank >/dev/null
echo "browser add-on: $(chromium --version)"
