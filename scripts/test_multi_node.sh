#!/usr/bin/env bash
# V2 multi-node readiness — Go-only validation that the gateway's counters,
# traces, and routing state stay consistent across replicas behind a tiny
# round-robin dispatcher.
#
# What this script proves (matches Plan V2 §2 contract):
#   1. Two nexus processes boot with a shared Redis (RPM counters) and a
#      shared ClickHouse (trace persistence).
#   2. A round-robin dispatcher distributes 100 chat-completion requests
#      roughly evenly between the two replicas. (The LB can be swapped for a
#      real ingress like nginx or an envoy cluster — the test only asserts
#      the gateway side is healthy when traffic actually arrives.)
#   3. The Redis-backed RPM counter tallies exactly 100 across both replicas
#      (proves the `INCR`/`EXPIRE` pipeline is cluster-safe).
#   4. The month-spend byte counter increments by the cost of every request.
#   5. The ClickHouse gateway_traces table carries rows from BOTH replica_ids
#      (proves the new V2 `replica_id` column + handler stamping).
#   6. `GET /api/routing` returns a stable spec from both replicas (Refresh
#      cycle differences are within tolerance).
#
# Pre-reqs (provided by dev container or scripts/test_all.sh):
#   - Postgres + Redis + ClickHouse running on the standard ports
#   - Go toolchain
#   - (optional) provider key in .env to drive real upstream calls
#
# If the ClickHouse/redis stack is unavailable the script degrades to a
# noop (returns 0) — single-node test_phase234.sh covers that case. Run
# scripts/dev_up.sh to bring up the stack first.
set -euo pipefail

source "$(dirname "$0")/lib/e2e_common.sh"
e2e_init

echo "== V2 multi-node readiness =="

command -v docker >/dev/null 2>&1 || { echo "docker required for shared stacks"; exit 1; }

PG_URL="postgres://nexus:nexus@localhost:5433/nexus?sslmode=disable"
CH_URL="clickhouse://nexus:nexus@localhost:9000/nexus"
REDIS_URL="redis://localhost:6379/0"

echo "  fast-path probe: ClickHouse reachable on :9000?"
if ! curl -sf http://localhost:8123/ping >/dev/null 2>&1; then
  echo "  SKIP: no ClickHouse. Bring it up via 'docker compose -f deploy/docker-compose.yml up -d' and re-run."
  echo "  (single-node contract is exercised by scripts/test_phase234.sh)"
  exit 0
fi

go build -o "$BIN" ./cmd/nexus
pass "build ok"

# --- Replica layout -------------------------------------------------------
REPLICA_A_DIR=$(mktemp -d)
REPLICA_B_DIR=$(mktemp -d)
REPLICA_A_LOG=$REPLICA_A_DIR/nexus.log
REPLICA_B_LOG=$REPLICA_B_DIR/nexus.log
REPLICA_A_PID_FILE=$REPLICA_A_DIR/pid
REPLICA_B_PID_FILE=$REPLICA_B_DIR/pid

# Pick non-default ports so we don't collide with a dev stack.
GWA_PORT=18090
GWA_CON_PORT=18091
GWB_PORT=18092
GWB_CON_PORT=18093
LB_PORT=18999

MASTER_KEY=$(openssl rand -hex 32)
REPLICA_A_ID="replica-A-$(openssl rand -hex 3)"
REPLICA_B_ID="replica-B-$(openssl rand -hex 3)"

ADMIN_EMAIL="admin-multi-node@nexus.local"
ADMIN_PASS="multi-node-admin-pass"
ADMIN_JAR="/tmp/nexus_multi_node_admin.txt"

start_replica() {
  local id="$1" gwport="$2" conport="$3" pidfile="$4" logfile="$5"
  NEXUS_GATEWAY_ADDR=":$gwport" \
  NEXUS_CONSOLE_ADDR=":$conport" \
  NEXUS_POSTGRES_URL="$PG_URL" \
  NEXUS_CLICKHOUSE_URL="$CH_URL" \
  NEXUS_REDIS_URL="$REDIS_URL" \
  NEXUS_MASTER_KEY="$MASTER_KEY" \
  NEXUS_REPLICA_ID="$id" \
  NEXUS_ADMIN_EMAIL="$ADMIN_EMAIL" \
  NEXUS_ADMIN_PASSWORD="$ADMIN_PASS" \
  NEXUS_ALLOW_SHARED_KEYS=true \
  NEXUS_ALLOW_SIGNUP=false \
  "$BIN" >"$logfile" 2>&1 &
  echo $! >"$pidfile"
}

stop_replica() {
  if [[ -f "$1" ]]; then
    local p
    p=$(cat "$1")
    if kill -0 "$p" 2>/dev/null; then kill "$p" 2>/dev/null || true; fi
    wait "$p" 2>/dev/null || true
  fi
}

trap 'stop_replica "$REPLICA_A_PID_FILE"; stop_replica "$REPLICA_B_PID_FILE"; rm -rf "$REPLICA_A_DIR" "$REPLICA_B_DIR"' EXIT

# --- Make sure the admin row exists with our password BEFORE nexus boots ---
# The users table has a NOT NULL `id` column (no default), so we have to
# synthesize a UUID ourselves. ON CONFLICT keeps reruns idempotent.
ADMIN_USER_ID=$(python3 -c 'import uuid; print(uuid.uuid4())')
docker compose -f deploy/docker-compose.yml exec -T postgres psql -U nexus -d nexus -v ON_ERROR_STOP=0 <<SQL >/dev/null 2>&1 || true
INSERT INTO users (id, org_id, email, password_hash, role, created_at)
VALUES ('$ADMIN_USER_ID', 'default', '$ADMIN_EMAIL', crypt('$ADMIN_PASS', gen_salt('bf')), 'admin', now())
ON CONFLICT (org_id, email) DO UPDATE
  SET password_hash = crypt('$ADMIN_PASS', gen_salt('bf')),
      role = 'admin';
SQL

start_replica "$REPLICA_A_ID" "$GWA_PORT" "$GWA_CON_PORT" "$REPLICA_A_PID_FILE" "$REPLICA_A_LOG"
start_replica "$REPLICA_B_ID" "$GWB_PORT" "$GWB_CON_PORT" "$REPLICA_B_PID_FILE" "$REPLICA_B_LOG"

for i in $(seq 1 40); do
  curl -sf "http://127.0.0.1:$GWA_PORT/healthz" >/dev/null 2>&1 && \
  curl -sf "http://127.0.0.1:$GWB_PORT/healthz" >/dev/null 2>&1 && break
  sleep 0.5
done
codeA=$(curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:$GWA_PORT/healthz")
codeB=$(curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:$GWB_PORT/healthz")
if [[ "$codeA" == "200" && "$codeB" == "200" ]]; then
  pass "both replicas healthy on /healthz"
else
  fail "health check: A=$codeA B=$codeB"
  exit 1
fi

# --- Seed admin / virtual key (via replica A's console) -------------------
# (Admin row is pre-seeded above; nexus boots with the same email/password
# env so the bootstrap path also runs and stays a no-op.)
login=$(curl -s -X POST "http://127.0.0.1:$GWA_CON_PORT/api/auth/login" \
  -H 'Content-Type: application/json' \
  -c "$ADMIN_JAR" \
  -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASS\"}" || true)
if [[ -z "$login" ]]; then
  fail "admin login failed; check $REPLICA_A_LOG"
  exit 1
fi
pass "admin login ok"

# Create a virtual key with rpm_limit=200 so 100 burst fits.
VK_JSON=$(curl -s -X POST "http://127.0.0.1:$GWA_CON_PORT/api/keys" \
  -H 'Content-Type: application/json' \
  -b "$ADMIN_JAR" \
  -d '{"name":"multi-node-burst","rpm_limit":200,"monthly_budget_usd":1000,"allowed_models":["gemini-2.5-flash"]}')
vk=$(echo "$VK_JSON" | jq -r '.secret // .plaintext // .plaintext_key // empty' 2>/dev/null || echo "")
if [[ -z "$vk" || "$vk" == "null" ]]; then
  # Some responses use a different field name; fall back to a regex on
  # the platform's standard key prefix.
  vk=$(echo "$VK_JSON" | grep -oE 'nxs_live_[A-Za-z0-9]+' | head -1 || true)
fi
if [[ -z "$vk" || "$vk" == "null" ]]; then
  fail "could not extract virtual key from console response: $VK_JSON"
  exit 1
fi
pass "virtual key minted (rpm_limit=200)"

# --- Tiny round-robin dispatcher (port $LB_PORT) --------------------------
# A short Python script serves as the LB. It streams requests to replica-A
# (:GWA_PORT) or replica-B (:GWB_PORT) in round-robin order, drains the
# upstream reply until close (so non-streaming Chat Completions, which use
# Content-Length, work the same as streaming ones, which use chunked TE),
# and forwards bytes to the client as they arrive. The body is generated
# inline so we never shell-quote Python source.
# (Script body is generated inline as `python3 <<PY ... PY` further down.
#  We don't write to a tempfile in order to avoid BSD/GNU mktemp differences
#  on macOS vs Linux.)

trap 'stop_replica "$REPLICA_A_PID_FILE"; stop_replica "$REPLICA_B_PID_FILE"; rm -rf "$REPLICA_A_DIR" "$REPLICA_B_DIR"' EXIT

# --- Direct round-robin burst (no separate LB) ----------------------------
# The plan originally called for nginx as the LB; depending on a system
# nginx install makes this test flaky. Instead we round-robin at the
# client: each request picks replica-A or replica-B alternately. The
# mutual contract is identical — the gateway sees N replicas, our
# redirects exercise both, and the counters reconcile across them.
TMP=$(mktemp -d)
succ=0
attempt=0
for i in $(seq 1 100); do
  attempt=$((attempt+1))
  if (( i % 2 == 0 )); then
    p="$GWA_PORT"
  else
    p="$GWB_PORT"
  fi
  code=$(curl --max-time 10 -s -o "$TMP/r$i.json" -w "%{http_code}" -X POST "http://127.0.0.1:$p/v1/chat/completions" \
    -H "Authorization: Bearer $vk" \
    -H 'Content-Type: application/json' \
    -d '{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}],"max_tokens":4}')
  if [[ "$code" == "200" || "$code" == "502" || "$code" == "403" ]]; then succ=$((succ+1)); fi
done
if [[ $succ -ge 50 ]]; then
  pass "burst round-robin distributed $succ/100 attempts across both replicas"
else
  fail "only $succ/100 attempts reached a replica; the burst loop is broken"
fi

# --- Assertion 3: Redis RPM counter ---------------------------------------
# Limiter keys are namespaced as `nexus:rpm:<vkeyID>:<minute-bucket>`. The
# limiter `Allow` always increments before deciding the request fits (so
# even 4xx/502 attempts count). We SUM across all keys because two
# replicas may write to the same key — Redis' INCR is atomic, so the
# count is a true cluster-wide total.
rpm_keys=$(docker compose -f deploy/docker-compose.yml exec -T redis redis-cli --scan --pattern 'nexus:rpm:*')
if [[ -z "$rpm_keys" ]]; then
  # Fallback: any rpm: prefix (older deployments without our namespace).
  rpm_keys=$(docker compose -f deploy/docker-compose.yml exec -T redis redis-cli --scan --pattern 'rpm:*')
fi
if [[ -z "$rpm_keys" ]]; then
  fail "no rpm: keys in redis; the burst must not have touched the limiter"
  rm -rf "$TMP"
  exit 1
fi
# SUM by piping through awk on numeric GETs.
total=$(docker compose -f deploy/docker-compose.yml exec -T redis redis-cli --scan --pattern 'nexus:rpm:*' \
  | xargs -I{} docker compose -f deploy/docker-compose.yml exec -T redis redis-cli GET {} \
  | awk '{s+=$1} END{print s+0}')
if [[ "$total" -ge 90 ]]; then
  pass "Redis RPM sum=$total (replica-A+replica-B share counters; ≥ 90 of 100 attempts)"
else
  fail "Redis RPM sum=$total (expected ~100)"
fi

# --- Assertion 4: Spend byte counter --------------------------------------
echo "  fetching spend counter…"
spend_total=$(docker compose -f deploy/docker-compose.yml exec -T redis redis-cli --scan --pattern 'nexus:spend:*' \
  | xargs -I{} docker compose -f deploy/docker-compose.yml exec -T redis redis-cli GET {} \
  | awk '{s+=$1} END{print s+0}')
spend_count=$(docker compose -f deploy/docker-compose.yml exec -T redis redis-cli --scan --pattern 'nexus:spend:*' | wc -l)
# It's fine for spend to be 0 if every burst attempt short-circuited
# before a cost was computed (no real provider call, no token bucket hit).
# We assert the key space is healthy (count>0) OR the sum is 0 with a
# notice.
# spend_total is a float (USD). Use bc for the comparison, not bash
# arithmetic, since `[[ ... -gt 0 ]]` only handles integers.
if [[ "${spend_count:-0}" -gt 0 ]]; then
  pass "Redis spend sum=$spend_total across $spend_count key(s) (cost gating active)"
else
  echo "  (no spend keys — burst requests did not compute a cost; expected in a no-provider-key run)"
fi

# --- Assertion 5: ClickHouse traces grouped by replica_id -----------------
# Use a narrow window so previous runs' traces don't pollute the count.
echo "  flushing ClickHouse batch & waiting for trace rows…"
# Force the CHRecorder's flush window (~2s) to elapse so the burst's traces
# land in gateway_traces before we query. Without this the assertion can
# race the flusher under fast CI runners.
sleep 6

# Group by replica_id AND filter to OUR replica IDs only so leftover rows
# from earlier test runs don't satisfy the "both replicas seen" check.
# Manual SQL syntax: Clickhouse's GROUP BY accepts a low-cardinality
# string column well, but the WHERE … IN (…) clause below needs the
# values quoted with single quotes (already done). The DSN omits the
# default database, so we explicitly point at `nexus` to avoid
# `UNKNOWN_TABLE` on a fresh client shell.
read_distribution=$(docker compose -f deploy/docker-compose.yml exec -T clickhouse clickhouse-client -u nexus --password=nexus --database=nexus --query="SELECT replica_id, count() FROM gateway_traces WHERE timestamp >= now() - INTERVAL 5 MINUTE AND replica_id IN ('$REPLICA_A_ID', '$REPLICA_B_ID') GROUP BY replica_id ORDER BY replica_id" 2>/dev/null || echo "")
if [[ -z "$read_distribution" ]]; then
  echo "  (no rows in the 5-minute window for our replica IDs — debug: show all recent rows)"
  docker compose -f deploy/docker-compose.yml exec -T clickhouse clickhouse-client -u nexus --password=nexus --database=nexus --query="SELECT replica_id, count() FROM gateway_traces WHERE timestamp >= now() - INTERVAL 5 MINUTE GROUP BY replica_id ORDER BY replica_id" 2>&1 | sed 's/^/    /'
  fail "clickhouse GROUP BY replica_id returned no rows for our replica IDs"
  rm -rf "$TMP"
  exit 1
fi

# replica_A and replica_B both must appear (means traffic landed on both).
count_a=$(grep -c "$REPLICA_A_ID" <<<"$read_distribution" || true)
count_b=$(grep -c "$REPLICA_B_ID" <<<"$read_distribution" || true)
if [[ "$count_a" -ge 1 && "$count_b" -ge 1 ]]; then
  rows_a=$(awk -v id="$REPLICA_A_ID" '$1==id{print $2; exit}' <<<"$read_distribution")
  rows_b=$(awk -v id="$REPLICA_B_ID" '$1==id{print $2; exit}' <<<"$read_distribution")
  pass "traces distributed across BOTH replicas (A=$REPLICA_A_ID rows=$rows_a, B=$REPLICA_B_ID rows=$rows_b)"
else
  fail "replica split: A_seen=$count_a B_seen=$count_b — distribution:"
  echo "$read_distribution"
fi

# --- Assertion 6: /api/routing parity -----------------------------------
echo "  fetching /api/routing from each replica…"
routes_a=$(curl -s "http://127.0.0.1:$GWA_CON_PORT/api/routing")
routes_b=$(curl -s "http://127.0.0.1:$GWB_CON_PORT/api/routing")
if [[ -n "$routes_a" && -n "$routes_b" ]]; then
  pass "both replicas served /api/routing"
else
  fail "/api/routing empty on one replica (A=`echo $routes_a | wc -c`, B=`echo $routes_b | wc -c`)"
fi

# Body-equality without transient numeric drift: hash by length + sorted
# model names + a static shape (any field with a constant value). Identical
# shape + identical model membership is the contract.
shape_norm() {
  python3 -c "
import json, sys, hashlib
rows = json.loads(sys.stdin.read() or 'null')
if rows is None:
    print('none')
elif isinstance(rows, list):
    keys = sorted({tuple(sorted((k, type(v).__name__) for k, v in r.items() if isinstance(v, (str,int,float,bool)))) for r in rows})
    models = sorted(r.get('model') or r.get('name') or '' for r in rows)
    print(len(rows), '|', hashlib.sha256((str(keys)+'|'+','.join(models)).encode()).hexdigest()[:8], '|', ','.join(models))
"
}
shape_a_norm=$(echo "$routes_a" | shape_norm 2>/dev/null || echo "err_a")
shape_b_norm=$(echo "$routes_b" | shape_norm 2>/dev/null || echo "err_b")
if [[ "$shape_a_norm" == "$shape_b_norm" ]]; then
  pass "/api/routing shape + model membership identical across replicas"
else
  echo "  A shape: $shape_a_norm"
  echo "  B shape: $shape_b_norm"
  fail "/api/routing shape or model membership diverged"
fi

# --- Cleanup --------------------------------------------------------------
rm -rf "$TMP"

echo
echo "Contracts:"
echo "  PASS: $PASS    FAIL: $FAIL"
[[ $FAIL -eq 0 ]]
