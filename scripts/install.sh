#!/usr/bin/env bash
# install.sh — one-line installer for Nexus (LLM gateway).
#
# Usage:
#   curl -fsSL install.nexus.ffx.ai | bash
#
# What it does:
#   1. Detects docker / docker compose, OS, arch
#   2. Clones the Nexus repo (shallow) into ~/.nexus/src
#   3. Starts the dev stack (postgres/redis/clickhouse/ollama) via docker compose
#   4. Builds the Go binary, starts it on :8090 (gateway) + :8091 (console)
#   5. Opens the console in the browser (or prints the URL)
#   6. Prints the next steps: create account, add a BYOK key, mint a virtual key
#
# Exit codes:
#   0   success
#   10  docker not installed
#   20  git not installed
#   30  docker compose up failed
#   40  nexus build failed
#   50  gateway failed to come up
set -euo pipefail

REPO="${NEXUS_REPO:-https://github.com/fun-fx/ffx_nexus.git}"
REF="${NEXUS_REF:-main}"
SRC_DIR="${NEXUS_HOME:-$HOME/.nexus/src}"
GW_PORT="${NEXUS_GATEWAY_PORT:-8090}"
CON_PORT="${NEXUS_CONSOLE_PORT:-8091}"

# ---- pretty logging ---------------------------------------------------------

_step()  { printf "\033[1;34m==>\033[0m %s\n" "$*"; }
_ok()    { printf "\033[1;32m✓\033[0m %s\n" "$*"; }
_warn()  { printf "\033[1;33m!\033[0m %s\n" "$*"; }
_fail()  { printf "\033[1;31m✗\033[0m %s\n" "$*" >&2; }

# ---- preflight --------------------------------------------------------------

need() {
  if ! command -v "$1" >/dev/null 2>&1; then
    _fail "missing required command: $1 ($2)"
    exit "$3"
  fi
}

_step "Preflight"
need git    "install from https://git-scm.com"            20
need docker "install Docker Desktop or docker-engine"       10
need curl   "install from your package manager"             1
_ok "git, docker, curl present"

if ! docker info >/dev/null 2>&1; then
  _fail "docker daemon is not running — start Docker and retry"
  exit 10
fi
_ok "docker daemon reachable"

if ! docker compose version >/dev/null 2>&1; then
  _fail "docker compose v2 not found (need 'docker compose', not 'docker-compose')"
  exit 10
fi

# ---- clone ------------------------------------------------------------------

if [[ ! -d "$SRC_DIR/.git" ]]; then
  _step "Cloning Nexus into $SRC_DIR ($REF)"
  mkdir -p "$(dirname "$SRC_DIR")"
  git clone --depth 1 --branch "$REF" "$REPO" "$SRC_DIR"
else
  _step "Updating existing clone at $SRC_DIR"
  (cd "$SRC_DIR" && git fetch --depth 1 origin "$REF" && git checkout "$REF" --quiet)
fi
_ok "source ready"

# ---- start dependencies -----------------------------------------------------

_step "Starting postgres + redis + clickhouse + ollama"
(cd "$SRC_DIR" && docker compose -f deploy/docker-compose.yml up -d postgres redis clickhouse ollama)

# healthz loop
wait_url() {
  local url="$1" label="$2" tries=60
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

wait_url "http://localhost:8123/ping" "clickhouse" || exit 30
wait_url "http://localhost:6379"      "redis"      || exit 30

# postgres: docker compose exec returns 1 until the container is ready; retry
# up to ~30s before giving up. The `/var/run/postgresql` socket inside the
# container is created shortly after the entrypoint starts.
_pg_check() {
  (cd "$SRC_DIR" && docker compose -f deploy/docker-compose.yml exec -T postgres pg_isready -U nexus) >/dev/null 2>&1
}
_pg_tries=30
while (( _pg_tries > 0 )); do
  if _pg_check; then
    _ok "postgres ready"
    break
  fi
  sleep 1
  _pg_tries=$((_pg_tries - 1))
done
if (( _pg_tries == 0 )); then
  _fail "postgres never became ready"
  exit 30
fi

# ---- build & start nexus ----------------------------------------------------

_step "Building nexus (go build ./cmd/nexus)"
command -v go >/dev/null 2>&1 || {
  _fail "go toolchain missing — install Go 1.22+ or use a pre-built image"
  exit 40
}
(cd "$SRC_DIR" && go build -o ./bin/nexus ./cmd/nexus)
_ok "binary at $SRC_DIR/bin/nexus"

# pick first available GEMINI / OPENAI / ANTHROPIC key from env (best-effort
# zero-dep mode — Nexus starts without any of these, but real calls need one).
load_dotenv() {
  if [[ -f "$SRC_DIR/.env" ]]; then
    set -a; # shellcheck disable=SC1091
    # shellcheck disable=SC1091
    source "$SRC_DIR/.env"; set +a
  fi
}
load_dotenv

# NEXUS_MASTER_KEY must decode (base64 or hex) to exactly 32 bytes. base64 is
# preferred because the Go side has a known corner case in decodeKey when both
# base64 and hex are present in the alphabet — using `-base64` keeps it
# unambiguous across all locales.
gen_master_key() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -base64 32
  else
    head -c 32 /dev/urandom | base64 | tr -d '\n'
  fi
}

_step "Starting nexus gateway on :$GW_PORT, console on :$CON_PORT"
nohup env \
  NEXUS_GATEWAY_ADDR=":$GW_PORT" \
  NEXUS_CONSOLE_ADDR=":$CON_PORT" \
  NEXUS_POSTGRES_URL='postgres://nexus:nexus@localhost:5433/nexus?sslmode=disable' \
  NEXUS_CLICKHOUSE_URL='clickhouse://nexus:nexus@localhost:9000/nexus' \
  NEXUS_REDIS_URL='redis://localhost:6379/0' \
  NEXUS_MASTER_KEY="$(gen_master_key)" \
  NEXUS_ALLOW_SIGNUP=true \
  "$SRC_DIR/bin/nexus" >"$HOME/.nexus/nexus.log" 2>&1 &
echo $! > "$HOME/.nexus/nexus.pid"

wait_url "http://localhost:$GW_PORT/healthz" "nexus gateway" || {
  tail -30 "$HOME/.nexus/nexus.log" >&2
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
  2. Click "Sign in" → "Create account" (BYOK brings your own LLM key)
  3. Paste at least one provider key (Gemini / OpenAI / Anthropic / The Grid)
  4. Copy the virtual key (nxs_live_...) — shown only once
  5. Point any OpenAI / Anthropic SDK at:
       export OPENAI_BASE_URL=$GW_URL/v1
       export OPENAI_API_KEY=nxs_live_...
  6. Make your first request:
       curl $GW_URL/v1/chat/completions \\
         -H "Authorization: Bearer \$OPENAI_API_KEY" \\
         -d '{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}'

Logs:      $HOME/.nexus/nexus.log
Stop:      kill \$(cat $HOME/.nexus/nexus.pid)
Uninstall: docker compose -f $SRC_DIR/deploy/docker-compose.yml down -v
           rm -rf $HOME/.nexus
EOF
