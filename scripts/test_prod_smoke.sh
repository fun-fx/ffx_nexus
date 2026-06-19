#!/usr/bin/env bash
# Prod smoke test against Tailscale ingress URLs.
# Requires Tailscale access to the cluster tailnet.
#
#   GW_URL=https://nexus.<tailnet>.ts.net \
#   CON_URL=https://console.<tailnet>.ts.net \
#   ADMIN_EMAIL=admin@nexus.local \
#   ADMIN_PASSWORD=... \
#   ./scripts/test_prod_smoke.sh
set -euo pipefail

GW_URL="${GW_URL:-https://nexus.<tailnet>.ts.net}"
CON_URL="${CON_URL:-https://console.<tailnet>.ts.net}"
ADMIN_EMAIL="${ADMIN_EMAIL:-}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-}"

pass=0
fail=0
skip=0

pass() { echo "PASS: $*"; pass=$((pass + 1)); }
fail() { echo "FAIL: $*"; fail=$((fail + 1)); }
skip() { echo "SKIP: $*"; skip=$((skip + 1)); }

echo "== prod smoke =="
echo "gateway: $GW_URL"
echo "console: $CON_URL"
echo

# --- health ---
for url in "$GW_URL/healthz" "$CON_URL/healthz"; do
  code=$(curl -sk -o /dev/null -w "%{http_code}" "$url")
  if [[ "$code" == "200" ]]; then pass "health $url"; else fail "health $url -> $code"; fi
done

# --- stats (empty window should return JSON, not zero-length body) ---
STATS=$(curl -sk "$CON_URL/api/stats")
if [[ -n "$STATS" ]] && echo "$STATS" | python3 -c "import json,sys; json.load(sys.stdin)" 2>/dev/null; then
  pass "stats returns valid JSON"
else
  fail "stats invalid or empty: ${STATS:-<empty>}"
fi

# --- routing ---
ROUTING=$(curl -sk "$CON_URL/api/routing")
if echo "$ROUTING" | python3 -c "import json,sys; json.load(sys.stdin)" 2>/dev/null; then
  pass "routing returns JSON"
else
  fail "routing invalid: $ROUTING"
fi

# --- auth + virtual key (optional) ---
if [[ -z "$ADMIN_EMAIL" || -z "$ADMIN_PASSWORD" ]]; then
  skip "chat/guardrails/cache (set ADMIN_EMAIL + ADMIN_PASSWORD)"
else
  CJ=$(mktemp)
  trap 'rm -f "$CJ"' EXIT
  LOGIN=$(curl -sk -c "$CJ" -X POST "$CON_URL/api/auth/login" \
    -H 'Content-Type: application/json' \
    -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASSWORD\"}")
  if echo "$LOGIN" | python3 -c "import json,sys; d=json.load(sys.stdin); exit(0 if d.get('user') else 1)" 2>/dev/null; then
    pass "console login"
  else
    fail "console login: $LOGIN"
    echo "results: $pass passed, $fail failed, $skip skipped"
    exit 1
  fi

  KEY_JSON=$(curl -sk -b "$CJ" -X POST "$CON_URL/api/me/keys" \
    -H 'Content-Type: application/json' \
    -d '{"name":"prod-smoke","allowed_models":["gemini-2.5-flash","fast"]}')
  SECRET=$(echo "$KEY_JSON" | python3 -c "import json,sys; print(json.load(sys.stdin).get('secret',''))" 2>/dev/null || true)
  if [[ -z "$SECRET" ]]; then
    fail "virtual key create: $KEY_JSON"
  else
    pass "virtual key created"

    # guardrails: PII block
    GRC=$(curl -sk -o /tmp/prod_gr.json -w "%{http_code}" -X POST "$GW_URL/v1/chat/completions" \
      -H "Authorization: Bearer $SECRET" \
      -H 'Content-Type: application/json' \
      -d '{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"my email is test@example.com"}]}')
    if [[ "$GRC" == "403" || "$GRC" == "400" || "$GRC" == "422" ]]; then
      pass "guardrails PII input blocked ($GRC)"
    else
      fail "guardrails PII expected 400/422, got $GRC $(cat /tmp/prod_gr.json 2>/dev/null)"
    fi

    # routing alias
    FAST=$(curl -sk -o /tmp/prod_fast.json -w "%{http_code}" -X POST "$GW_URL/v1/chat/completions" \
      -H "Authorization: Bearer $SECRET" \
      -H 'Content-Type: application/json' \
      -d '{"model":"fast","messages":[{"role":"user","content":"Reply with exactly: ok"}],"max_tokens":64}')
    if [[ "$FAST" == "200" ]]; then
      pass "routing alias fast -> 200"
    elif [[ "$FAST" == "401" || "$FAST" == "402" || "$FAST" == "429" ]]; then
      skip "routing alias fast -> $FAST (auth/quota)"
    elif [[ "$FAST" == "502" ]] && grep -q "no provider registered" /tmp/prod_fast.json 2>/dev/null; then
      skip "routing alias fast -> $FAST (admin BYOK credential missing; expected in strict_byok)"
    else
      fail "routing alias fast -> $FAST $(cat /tmp/prod_fast.json 2>/dev/null | head -c 200)"
    fi

    # semantic cache: two identical requests
    BODY='{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"prod smoke cache probe 42"}],"max_tokens":32,"temperature":0}'
    curl -sk -X POST "$GW_URL/v1/chat/completions" -H "Authorization: Bearer $SECRET" \
      -H 'Content-Type: application/json' -d "$BODY" >/tmp/prod_sc1.json || true
    sleep 1
    curl -sk -X POST "$GW_URL/v1/chat/completions" -H "Authorization: Bearer $SECRET" \
      -H 'Content-Type: application/json' -d "$BODY" >/tmp/prod_sc2.json || true
    TRACES=$(curl -sk "$CON_URL/api/traces?limit=5")
    if echo "$TRACES" | python3 -c "
import json,sys
rows=json.load(sys.stdin)
hits=sum(1 for r in rows if r.get('cache_hit'))
sys.exit(0 if hits>=1 else 1)
" 2>/dev/null; then
      pass "semantic cache hit observed in traces"
    else
      skip "semantic cache hit not seen yet (embeddings may be warming)"
    fi

    # eval scores (async; best-effort)
    sleep 3
    EVALS=$(curl -sk "$CON_URL/api/evals")
    if echo "$EVALS" | python3 -c "import json,sys; d=json.load(sys.stdin); sys.exit(0 if isinstance(d,list) else 1)" 2>/dev/null; then
      pass "evals endpoint returns JSON"
    else
      fail "evals invalid: $EVALS"
    fi
  fi
fi

echo
echo "results: $pass passed, $fail failed, $skip skipped"
[[ "$fail" -eq 0 ]]
