#!/usr/bin/env bash
# install.sh — one-line installer for Nexus (LLM gateway).
#
# Usage:
#   curl -fsSL install.nexus.ffx.ai | bash
#
# This is the curl equivalent of `npx -y @ffxnexus/nexus`, and it produces the
# same thing: the released binary for this machine, running with its own
# Postgres, gateway on :8080 and console on :8081.
#
# What it does:
#   1. Resolves the latest release (or NEXUS_VERSION)
#   2. Downloads the matching archive and verifies it against checksums.txt
#   3. Caches the binary under ~/.nexus/bin/<version>/
#   4. Starts it with `serve --local` and waits for /healthz
#   5. Prints how to open the console and make the first request
#
# It does not need Docker, Go, or a git checkout. Everything it writes lives
# under ~/.nexus, and `rm -rf ~/.nexus` is a complete uninstall.
#
# Exit codes:
#   0   success
#   10  a required command is missing
#   20  could not resolve a release to install
#   30  download or checksum verification failed
#   50  the gateway never answered /healthz
set -euo pipefail

REPO="${NEXUS_REPO_SLUG:-fun-fx/ffx_nexus}"
RELEASE_BASE="${NEXUS_RELEASE_BASE_URL:-}"
STATE_DIR="${NEXUS_LOCAL_STATE_DIR:-$HOME/.nexus}"
GW_PORT="${NEXUS_GATEWAY_PORT:-8080}"
CON_PORT="${NEXUS_CONSOLE_PORT:-8081}"

# ---- pretty logging ---------------------------------------------------------

_step()  { printf "\033[1;34m==>\033[0m %s\n" "$*"; }
_ok()    { printf "\033[1;32m✓\033[0m %s\n" "$*"; }
_warn()  { printf "\033[1;33m!\033[0m %s\n" "$*"; }
_fail()  { printf "\033[1;31m✗\033[0m %s\n" "$*" >&2; }

# ---- artefact naming --------------------------------------------------------
#
# These two functions mirror .goreleaser.yaml's name_template and npx/lib/
# artifact.js. scripts/test_release_naming.sh sources this file and compares
# what they produce against what goreleaser actually built, because the only
# other place a mismatch shows up is a 404 in someone's terminal.

nexus_target() {
  local os arch
  case "$(uname -s)" in
    Darwin) os=darwin ;;
    Linux)  os=linux ;;
    *)
      _fail "no Nexus binary is published for $(uname -s). Use the container image instead:"
      _fail "  docker run -p 8080:8080 -p 8081:8081 -v \"\$PWD/data:/app/data\" ghcr.io/${REPO}"
      exit 10
      ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64)  arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *)
      _fail "no Nexus binary is published for $(uname -m). Use the container image instead:"
      _fail "  docker run -p 8080:8080 -p 8081:8081 -v \"\$PWD/data:/app/data\" ghcr.io/${REPO}"
      exit 10
      ;;
  esac
  printf '%s_%s' "$os" "$arch"
}

nexus_archive_name() {
  printf 'nexus_%s_%s.tar.gz' "$1" "$(nexus_target)"
}

nexus_release_base() {
  if [[ -n "$RELEASE_BASE" ]]; then
    printf '%s' "${RELEASE_BASE%/}"
  else
    printf 'https://github.com/%s/releases/download/v%s' "$REPO" "$1"
  fi
}

# Sourcing this file exposes the naming helpers without installing anything.
# The contract test relies on it; so does anyone debugging a bad URL.
if [[ -n "${NEXUS_INSTALL_SOURCE_ONLY:-}" ]]; then
  return 0 2>/dev/null || exit 0
fi

# ---- preflight --------------------------------------------------------------

need() {
  if ! command -v "$1" >/dev/null 2>&1; then
    _fail "missing required command: $1 ($2)"
    exit 10
  fi
}

_step "Preflight"
need curl "install from your package manager"
need tar  "install from your package manager"
if command -v shasum >/dev/null 2>&1; then
  SHA_CMD=(shasum -a 256)
elif command -v sha256sum >/dev/null 2>&1; then
  SHA_CMD=(sha256sum)
else
  _fail "missing required command: shasum or sha256sum"
  exit 10
fi
_ok "curl, tar and a sha256 tool present"

# ---- resolve the version ----------------------------------------------------

VERSION="${NEXUS_VERSION:-}"
if [[ -z "$VERSION" ]]; then
  _step "Resolving the latest release"
  # The redirect from /releases/latest carries the tag, which avoids both an
  # API token and the 60-per-hour unauthenticated rate limit that makes a
  # popular installer fail for reasons the user cannot act on.
  latest_url="$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
    "https://github.com/${REPO}/releases/latest" 2>/dev/null || true)"
  VERSION="${latest_url##*/tag/v}"
  if [[ -z "$VERSION" || "$VERSION" == "$latest_url" ]]; then
    _fail "could not resolve the latest release of ${REPO}"
    _fail "pick one explicitly: NEXUS_VERSION=0.7.0 curl -fsSL install.nexus.ffx.ai | bash"
    exit 20
  fi
fi
VERSION="${VERSION#v}"
_ok "installing v$VERSION"

# ---- download ---------------------------------------------------------------

BIN_DIR="$STATE_DIR/bin/$VERSION"
BIN="$BIN_DIR/nexus"

if [[ -x "$BIN" ]]; then
  _ok "already downloaded: $BIN"
else
  ARCHIVE="$(nexus_archive_name "$VERSION")"
  BASE="$(nexus_release_base "$VERSION")"
  TMP="$(mktemp -d)"
  trap 'rm -rf "$TMP"' EXIT

  _step "Downloading $ARCHIVE"
  curl -fsSL "$BASE/$ARCHIVE" -o "$TMP/$ARCHIVE" || {
    _fail "download failed: $BASE/$ARCHIVE"
    exit 30
  }
  curl -fsSL "$BASE/checksums.txt" -o "$TMP/checksums.txt" || {
    _fail "could not fetch checksums.txt from $BASE"
    exit 30
  }

  # Verify before running. This script is piped straight into bash, so nobody
  # inspected what it fetched, and what it fetched then holds the user's
  # provider API keys.
  want="$(awk -v f="$ARCHIVE" '$2 == f || $2 == "*" f {print $1}' "$TMP/checksums.txt")"
  if [[ -z "$want" ]]; then
    _fail "checksums.txt for v$VERSION does not list $ARCHIVE"
    exit 30
  fi
  got="$("${SHA_CMD[@]}" "$TMP/$ARCHIVE" | awk '{print $1}')"
  if [[ "$want" != "$got" ]]; then
    _fail "checksum mismatch for $ARCHIVE"
    _fail "  expected $want"
    _fail "  got      $got"
    exit 30
  fi
  _ok "checksum verified"

  mkdir -p "$BIN_DIR"
  tar -xzf "$TMP/$ARCHIVE" -C "$BIN_DIR" nexus
  chmod +x "$BIN"
  _ok "binary at $BIN"
fi

# ---- start ------------------------------------------------------------------

mkdir -p "$STATE_DIR"
LOG="$STATE_DIR/nexus.log"
PIDFILE="$STATE_DIR/nexus.pid"

_step "Starting nexus (gateway :$GW_PORT, console :$CON_PORT)"
nohup env \
  NEXUS_GATEWAY_ADDR=":$GW_PORT" \
  NEXUS_CONSOLE_ADDR=":$CON_PORT" \
  NEXUS_LOCAL_STATE_DIR="$STATE_DIR" \
  "$BIN" serve --local >"$LOG" 2>&1 &
echo $! > "$PIDFILE"

wait_url() {
  local url="$1" label="$2" tries="${3:-120}"
  while (( tries > 0 )); do
    if curl -fsS --max-time 2 "$url" >/dev/null 2>&1; then
      _ok "$label ready ($url)"
      return 0
    fi
    sleep 1
    tries=$((tries - 1))
  done
  _fail "$label never became ready at $url"
  return 1
}

# The first run also initialises a database, which is slower than a restart.
wait_url "http://localhost:$GW_PORT/healthz" "nexus gateway" || {
  tail -30 "$LOG" >&2
  exit 50
}

# ---- done -------------------------------------------------------------------

_ok "Nexus is up"

CONSOLE_URL="http://localhost:$CON_PORT"
GW_URL="http://localhost:$GW_PORT"

cat <<EOF

  Console (UI):  $CONSOLE_URL
  Gateway (API): $GW_URL

Next steps:
  1. Open the console in your browser:
       open $CONSOLE_URL     # macOS
       xdg-open $CONSOLE_URL # Linux
  2. Click "Create account" — the first account on this machine is the admin
  3. Paste at least one provider key (Gemini / OpenAI / Anthropic / The Grid)
  4. Copy the virtual key (nxs_live_...) — shown only once
  5. Point any OpenAI / Anthropic SDK at:
       export OPENAI_BASE_URL=$GW_URL/v1
       export OPENAI_API_KEY=nxs_live_...
  6. Make your first request:
       curl $GW_URL/v1/chat/completions \\
         -H "Authorization: Bearer \$OPENAI_API_KEY" \\
         -d '{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}'

Logs:      $LOG
Stop:      kill \$(cat $PIDFILE)
Uninstall: rm -rf $STATE_DIR

Traces are live-only and rate limits are in-process here; both need
ClickHouse and Redis, which a laptop install deliberately skips. For a
cluster, use the Helm chart: docs/customer-self-hosted-install.md
EOF
