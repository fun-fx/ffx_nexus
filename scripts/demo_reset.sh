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

EMB_PORT=8300

_step "Stop existing nexus / vite / fake embeddings"
pkill -f "bin/nexus" 2>/dev/null || true
pkill -f "vite"      2>/dev/null || true
pkill -f "npm run dev" 2>/dev/null || true
pkill -f "fake_embeddings.py" 2>/dev/null || true
sleep 1
_ok "no leftover processes on :8090/:8091/:5173/:${EMB_PORT}"

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

_step "Launch fake embeddings stub (:${EMB_PORT}) for semantic cache demo"
nohup python3 "$ROOT/scripts/lib/fake_embeddings.py" "$EMB_PORT" \
  >"$HOME/.nexus/embeddings.log" 2>&1 &
echo $! > "$HOME/.nexus/embeddings.pid"
for i in $(seq 1 10); do
  if curl -fsS --max-time 1 -X POST "http://127.0.0.1:${EMB_PORT}/v1/embeddings" \
    -H "Content-Type: application/json" \
    -d '{"input":"probe"}' >/dev/null 2>&1; then
    _ok "fake embeddings ready (pid $(cat "$HOME/.nexus/embeddings.pid"))"
    break
  fi
  sleep 0.3
done

_step "Launch Nexus gateway (:8090) and console (:8091)"
mkdir -p "$HOME/.nexus"
nohup env \
  NEXUS_GATEWAY_ADDR=:8090 \
  NEXUS_CONSOLE_ADDR=:8091 \
  NEXUS_POSTGRES_URL='postgres://nexus:nexus@localhost:5433/nexus?sslmode=disable' \
  NEXUS_CLICKHOUSE_URL='clickhouse://nexus:nexus@localhost:9000/nexus' \
  NEXUS_REDIS_URL='redis://localhost:6379/0' \
  NEXUS_MASTER_KEY="$(openssl rand -base64 32)" \
  NEXUS_GEMINI_API_KEY="${GEMINI_API_KEY:-}" \
  NEXUS_GRID_API_KEY="${GRID_API_KEY:-}" \
  NEXUS_ALLOW_SIGNUP=true \
  NEXUS_SEMANTIC_CACHE_ENABLED=true \
  NEXUS_EMBEDDINGS_URL="http://127.0.0.1:${EMB_PORT}/v1" \
  NEXUS_SEMANTIC_CACHE_THRESHOLD=0.99 \
  NEXUS_GUARDRAILS_ENABLED=true \
  NEXUS_GUARDRAILS_BLOCK_PII_INPUT=true \
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
(cd "$ROOT/web" && VITE_API_PROXY="http://localhost:8091" nohup npm run dev > "$HOME/.nexus/vite.log" 2>&1 &)
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

Demo features pre-enabled (no extra env vars needed):
  - NEXUS_ALLOW_SIGNUP=true        → "Create account" tab visible
  - NEXUS_SEMANTIC_CACHE_ENABLED   → step 7 cache badge
  - NEXUS_GUARDRAILS_ENABLED       → step 8 blocked badge
  - quality-aware routing (`auto`) → step 9 routes across all registered providers

Providers auto-detected from env:
  GEMINI_API_KEY  → gemini provider
  GRID_API_KEY    → grid provider (The Grid spot market)
  OPENAI_API_KEY  → openai provider
  ANTHROPIC_API_KEY → anthropic provider
If only one provider is set, `auto` will pick that single model — the routing
*mechanism* still runs, but Model routing will show one row. Export two or more
of the above before reset for a multi-vendor table:

  export GRID_API_KEY=... GEMINI_API_KEY=...    # then bash scripts/demo_reset.sh

To record the demo:
  - Press Cmd+Shift+5 (Mac) → record selected portion of the screen
  - Walk through the steps in docs/demo-script.md
  - Stop the recording with Cmd+Shift+5 → Stop

Cleanup:
  bash scripts/demo_reset.sh        # resets everything
  kill \$(cat $HOME/.nexus/nexus.pid)
  pkill -f vite
EOF
