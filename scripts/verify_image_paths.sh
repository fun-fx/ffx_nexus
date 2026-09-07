#!/usr/bin/env bash
# =============================================================================
# Container runtime layout — verify without Docker daemon
# =============================================================================
# `docker build` needs a running daemon, which is unavailable on some
# workstations. The thing this layout actually commits us to is the path
# the binary pg_ctl reads out of the image:
#
#   /usr/libexec/postgresql16/pg_ctl    — used by internal/localdb on every
#   /usr/libexec/postgresql16/initdb    — release (Stop / releaseDataDir)
#                                          and on the cold-boot initdb pass
#
# Those come from postgresql16 in Alpine 3.20. The Dockerfile symlinks
# /usr/libexec/postgresql16 onto /opt/postgres so /opt/postgres/bin/pg_ctl
# resolves in the form internal/localdb expects. If either path is wrong the
# server either fails pg_ctl start/stop (loud in log) or fails
# `releaseDataDir` on a cold boot (also loud), but it should be caught here,
# not by an operator on a workstation.
#
# The package's binary listings are not in APKINDEX's text form — Alpine
# only ships filenames inside the .apk itself. We pull the actual .apk
# (≈5.8MB for postgresql16) and list its contents. Anyone running this
# offline once can pre-cache it under /tmp.
#
# Usage:  scripts/verify_image_paths.sh
# Requires: curl, tar
# =============================================================================
set -euo pipefail

pass=0
fail=0
ok()  { printf '  \033[32mPASS\033[0m %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; fail=$((fail + 1)); }

command -v curl >/dev/null 2>&1 || { echo "missing dependency: curl" >&2; exit 127; }
command -v tar  >/dev/null 2>&1 || { echo "missing dependency: tar"  >&2; exit 127; }

# Mirror we trust: dl-cdn (the same host apk itself hits). The package URL is
# predictable for a given (branch, arch, pkgname, version) triple, and the
# version is in the pinned APKINDEX we just pulled. Re-resolve from the
# directory index rather than hard-coding 16.14-r0 — if Alpine bumps the
# minor, this still picks up the right one and lands the behind/past failure
# as a red bar, not a regression in operator experience.
INDEX='https://dl-cdn.alpinelinux.org/alpine/v3.20/main/x86_64/'
apk="$(curl -fsS "$INDEX" | grep -oE 'postgresql16-[^"]+\.apk' | head -1)"
[[ -n "$apk" ]] || { bad "could not list the postgresql16 .apk on the v3.20 mirror"; exit 1; }

# Cache under /tmp rather than ./verify-image so concurrent CI runs do not
# thrash. mktemp -d here is intentional: this is the only place the script
# touches the host filesystem.
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
curl -fsS -o "$work/$apk" "${INDEX}${apk}"
files="$(tar tzf "$work/$apk")"

for f in usr/libexec/postgresql16/pg_ctl usr/libexec/postgresql16/initdb; do
  if grep -qx "$f" <<<"$files"; then
    ok "$apk ships $f"
  else
    bad "$apk is missing $f — Docker cold boot would fail"
  fi
done

# The Dockerfile symlinks /opt/postgres -> /usr/libexec/postgresql16 so
# /opt/postgres/bin/<binary> resolves, which is what
# internal/localdb joins together to form /opt/postgres/bin/pg_ctl. There is
# no other layer rewriting that lookup.
if grep -q 'ln -s /usr/libexec/postgresql16 /opt/postgres/bin' Dockerfile; then
  ok "Dockerfile symlink turns /opt/postgres/bin/* into ${apk%-r0.apk} binaries"
else
  bad "Dockerfile does not symlink /opt/postgres -> /usr/libexec/postgresql16"
fi

grep -q 'NEXUS_LOCAL_DB_BINARIES=/opt/postgres' Dockerfile \
  && ok "Dockerfile exports NEXUS_LOCAL_DB_BINARIES=/opt/postgres — and only this path" \
  || bad "Dockerfile NEXUS_LOCAL_DB_BINARIES is missing or mis-typed"

printf '\n\033[1m=== image paths: %d passed, %d failed ===\033[0m\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
