#!/usr/bin/env bash
# post-create.sh — runs once when the dev container is built.
# Goal: warm caches, install CLI tools used by our scripts, and verify the
# full toolchain renders identically on dev box, CI, and other contributors'
# machines.
set -euo pipefail

log() { printf '\033[1;36m[devcontainer]\033[0m %s\n' "$*"; }

cd /workspace

log "go version"
go version

log "go mod download (warm /root/go/pkg/mod)"
go mod download

log "go env"
go env GOROOT GOPATH GOMODCACHE

log "make sure scripts/ is executable"
chmod +x scripts/*.sh || true

log "docker compose plugin check"
docker compose version

log "install gh (best-effort)"
if ! command -v gh >/dev/null 2>&1; then
    arch="$(dpkg --print-architecture)"
    case "${arch}" in
        amd64) gh_arch="amd64" ;;
        arm64) gh_arch="arm64" ;;
        *) gh_arch="${arch}" ;;
    esac
    gh_version="2.65.0"
    curl -fsSL "https://github.com/cli/cli/releases/download/v${gh_version}/gh_${gh_version}_linux_${gh_arch}.tar.gz" \
        | tar -xz -C /tmp
    mv "/tmp/gh_${gh_version}_linux_${gh_arch}/bin/gh" /usr/local/bin/gh
    rm -rf "/tmp/gh_${gh_version}_linux_${gh_arch}"
fi
gh --version || true

log "wrk (best-effort) — for V5 stress + 1000 concurrent burst"
if ! command -v wrk >/dev/null 2>&1; then
    if command -v apt-get >/dev/null 2>&1; then
        apt-get install -y --no-install-recommends wrk 2>/dev/null || true
    fi
fi
wrk --version 2>/dev/null || true

log "jq smoke"
echo '{"ok":true}' | jq .

log "ready"
