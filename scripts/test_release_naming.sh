#!/usr/bin/env bash
# =============================================================================
# Release artefact naming contract
# =============================================================================
# Three places independently construct the name of a release archive:
#
#   .goreleaser.yaml      archives[].name_template  (produces it)
#   npx/lib/artifact.js   archiveName()             (fetches it)
#   scripts/install.sh    nexus_archive_name()      (fetches it)
#
# Nothing connects them at build time. A change to the template ships a
# release that both installers 404 on, and the only symptom is a failed
# `npx -y @ffxnexus/nexus` in a stranger's terminal.
#
# This asserts the two consumers agree with the producer, using the artefacts
# a snapshot build just wrote to dist/.
#
# Usage:  scripts/test_release_naming.sh [dist-dir]
# Requires: node, and a completed `goreleaser release --snapshot`
# =============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="${1:-${REPO_ROOT}/dist}"

pass=0
fail=0
ok()  { printf '  \033[32mPASS\033[0m %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; fail=$((fail + 1)); }

command -v node >/dev/null 2>&1 || { echo "missing dependency: node" >&2; exit 127; }

if [[ ! -d "$DIST" ]]; then
  echo "no dist directory at $DIST — run: goreleaser release --snapshot --clean --skip=publish" >&2
  exit 1
fi

# The snapshot version is derived from the last tag, so read it back rather
# than assuming. Any archive tells us: nexus_<version>_<os>_<arch>.tar.gz
sample="$(basename "$(ls "$DIST"/nexus_*_*.tar.gz 2>/dev/null | head -1 || true)")"
if [[ -z "$sample" ]]; then
  echo "no archives in $DIST — did goreleaser run?" >&2
  exit 1
fi
version="${sample#nexus_}"
version="${version%%_*}"
printf '\n\033[1mRelease naming contract (version %s, dist %s)\033[0m\n' "$version" "$DIST"

# --- goreleaser built one archive per published platform ---------------------
for target in darwin_amd64 darwin_arm64 linux_amd64 linux_arm64; do
  if [[ -f "$DIST/nexus_${version}_${target}.tar.gz" ]]; then
    ok "goreleaser built nexus_${version}_${target}.tar.gz"
  else
    bad "goreleaser did not build nexus_${version}_${target}.tar.gz"
  fi
done

if [[ -f "$DIST/checksums.txt" ]]; then
  ok "checksums.txt exists (both installers verify against it)"
else
  bad "checksums.txt is missing — installers cannot verify what they download"
fi

# --- the npx wrapper asks for exactly those names ----------------------------
# node platform/arch spellings on the left, goreleaser's on the right.
while read -r node_platform node_arch expected; do
  actual="$(node -e "
    const { archiveName } = require('${REPO_ROOT}/npx/lib/artifact');
    process.stdout.write(archiveName('${version}', '${node_platform}', '${node_arch}'));
  ")"
  if [[ "$actual" == "$expected" ]]; then
    ok "npx wrapper fetches ${expected} on ${node_platform}/${node_arch}"
  else
    bad "npx wrapper would fetch ${actual} on ${node_platform}/${node_arch}, but goreleaser built ${expected}"
  fi
done <<EOF
darwin x64   nexus_${version}_darwin_amd64.tar.gz
darwin arm64 nexus_${version}_darwin_arm64.tar.gz
linux  x64   nexus_${version}_linux_amd64.tar.gz
linux  arm64 nexus_${version}_linux_arm64.tar.gz
EOF

# --- install.sh asks for the name for the machine it runs on -----------------
# It can only speak for this runner's uname, which is the honest limit of a
# shell function that reads uname directly.
# shellcheck disable=SC1091
NEXUS_INSTALL_SOURCE_ONLY=1 source "${REPO_ROOT}/scripts/install.sh"
installer_name="$(nexus_archive_name "$version")"
if [[ -f "$DIST/$installer_name" ]]; then
  ok "install.sh fetches ${installer_name}, which goreleaser built"
else
  bad "install.sh would fetch ${installer_name}, which is not in $DIST"
fi

printf '\n\033[1m=== release naming: %d passed, %d failed ===\033[0m\n' "$pass" "$fail"
[[ "$fail" -eq 0 ]]
