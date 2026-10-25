FROM mcr.microsoft.com/devcontainers/base:trixie

ARG GO_VERSION=1.27.2
ARG NODE_MAJOR=24

RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 xz-utils \
    && rm -rf /var/lib/apt/lists/*

# Login shells reset PATH from /etc/profile, so ENV alone does not reach
# postCreateCommand or a workspace shell; links in /usr/local/bin always do.
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" | tar -xz -C /usr/local \
    && ln -s /usr/local/go/bin/go /usr/local/bin/go \
    && ln -s /usr/local/go/bin/gofmt /usr/local/bin/gofmt \
    && echo 'export PATH="$PATH:$HOME/go/bin"' > /etc/profile.d/go-bin.sh

RUN NODE_VERSION=$(curl -fsSL https://nodejs.org/dist/index.json \
        | python3 -c "import json,sys; print(next(r['version'] for r in json.load(sys.stdin) if r['version'].startswith('v${NODE_MAJOR}.')))") \
    && curl -fsSL "https://nodejs.org/dist/${NODE_VERSION}/node-${NODE_VERSION}-linux-x64.tar.xz" \
        | tar -xJ -C /usr/local --strip-components=1 \
    && corepack enable pnpm
