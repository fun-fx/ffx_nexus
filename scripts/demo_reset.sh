#!/usr/bin/env bash
# demo_reset.sh — reset the local Nexus dev environment for a demo recording.
#
# What it does:
#   1. Kills any nexus / vite process bound to :8090 / :8091 / :5173
#   2. Wipes Postgres user data (keeps admin@nexus.test)
#   3. Truncates ClickHouse traces and eval_scores
#   4. Launches Nexus (fresh NEXUS_MASTER_KEY, NEXUS_ALLOW_SIGNUP=true)
#   5. Launches the Vite dev server for the React dashboard
#
# Usage:
#   bash scripts/demo_reset.sh
#
# Test data after this script:
#   - One admin user (admin@nexus.test / admin-e2e-pass, role=admin)
#   - Zero virtual keys, zero credentials, zero audit log entries
#   - Zero traces, zero eval scores
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

_step()  { printf "\n\033[1;34m==>\033[0m %s\n" "$*"; }
_ok()    { printf "  \033[1;32m✓\033[0m %s\n" "$*"; }

_step "Stop existing nexus / vite"
pkill -f "bin/nexus" 2>/dev/null || true
pkill -f "vite"      2>/dev/null || true
pkill -f "npm run dev" 2>/dev/null || true
sleep 1
_ok "no leftover processes on :8090/:8091/:5173"

_step "Wipe Postgres user data (keep admin@nexus.test)"
docker exec deploy-postgres-1 psql -U nexus -d nexus -c "
DELETE FROM provider_credentials;
DELETE FROM virtual_keys;
DELETE FROM audit_log;
DELETE FROM user_sessions;
DELETE FROM users WHERE email != 'admin@nexus.test';
" | sed -n 's/DELETE /  wiped /p'
_ok "postgres: 1 admin user remains, all keys/credentials/audit cleared"

_step "Truncate ClickHouse observability tables"
curl -sS "http://localhost:8123/" -u "nexus:nexus" --data-binary \
  "TRUNCATE TABLE nexus.gateway_traces" && \
  curl -sS "http://localhost:8123/" -u "nexus:nexus" --data-binary \
  "TRUNCATE TABLE nexus.eval_scores"
_ok "clickhouse: traces + eval_scores truncated"

_step "Launch Nexus gateway (:8090) and console (:8091)"
mkdir -p "$HOME/.nexus"
nohup env \
  NEXUS_GATEWAY_ADDR=:8090 \
  NEXUS_CONSOLE_ADDR=:8091 \
  NEXUS_POSTGRES_URL='postgres://nexus:nexus@localhost:5433/nexus?sslmode=disable' \
  NEXUS_CLICKHOUSE_URL='clickhouse://nexus:nexus@localhost:9000/nexus' \
  NEXUS_REDIS_URL='redis://localhost:6379/0' \
  NEXUS_MASTER_KEY="$(openssl rand -base64 32)" \
  NEXUS_ALLOW_SIGNUP=true \
  "${GEMINI_API_KEY:-}" \
  "$ROOT/bin/nexus" >"$HOME/.nexus/nexus.log" 2>&1 &
echo $! > "$HOME/.nexus/nexus.pid"

# Wait for /healthz
for i in $(seq 1 30); do
  if curl -fsS --max-time 1 http://localhost:8090/healthz >/dev/null 2>&1; then
    _ok "nexus healthy after $i × 0.5 s (pid $(cat "$HOME/.nexus/nexus.pid"))"
    break
  fi
  sleep 0.5
done

_step "Launch Vite dev server (:5173) for the React dashboard"
(cd "$ROOT/web" && nohup npm run dev > "$HOME/.nexus/vite.log" 2>&1 &)
for i in $(seq 1 60); do
  if curl -fsS --max-time 1 http://localhost:5173 >/dev/null 2>&1; then
    _ok "vite ready after $i s"
    break
  fi
  sleep 1
done

cat <<EOF

Demo environment is ready.

  Gateway (API):   http://localhost:8090
  Console (UI):    http://localhost:8091
  Dashboard (dev): http://localhost:5173

Sign-in credentials:
  email:    admin@nexus.test
  password: admin-e2e-pass

For the very first demo run (so a brand-new user can hit "Create account"):
  NEXUS_ALLOW_SIGNUP=true is already set — no extra env var needed.

To record the demo:
  - Press Cmd+Shift+5 (Mac) → record selected portion of the screen
  - Walk through the steps in docs/demo-script.md
  - Stop the recording with Cmd+Shift+5 → Stop

Cleanup:
  bash scripts/demo_reset.sh        # resets everything
  kill \$(cat $HOME/.nexus/nexus.pid)
  pkill -f vite
EOF
