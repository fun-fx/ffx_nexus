#!/usr/bin/env bash
# P1-3 gate: same mock upstream, Go binary vs Elixir Mix, same load_baseline.
set -euo pipefail

GO_PID=""
EX_PID=""
MOCK_PID=""

ulimit -n 65536 2>/dev/null || true

FFX="${FFX_NEXUS:-$(cd "$(dirname "$0")/.." && pwd)}"
EX="${NEXUS_EX:-$FFX/../nexus_ex}"
STREAMS="${STREAMS:-256}"
DURATION="${DURATION:-15s}"
GW_PORT="${GW_PORT:-18080}"
CON_PORT="${CON_PORT:-18081}"
MOCK_PORT="${MOCK_PORT:-19090}"
OUTDIR="${OUTDIR:-/tmp/nexus-p1-gate}"
MODEL="${MODEL:-gpt-4o-mini}"

mkdir -p "$OUTDIR"
cd "$FFX"

echo ">> build Go binary"
go build -o "$OUTDIR/nexus-go" ./cmd/nexus

echo ">> mock SSE on :$MOCK_PORT"
go run ./scripts/load_baseline -mock-listen "127.0.0.1:$MOCK_PORT" &
MOCK_PID=$!
cleanup() {
  kill "$GO_PID" "$EX_PID" "$MOCK_PID" 2>/dev/null || true
  wait "$GO_PID" "$EX_PID" "$MOCK_PID" 2>/dev/null || true
}
trap cleanup EXIT
sleep 0.4

wait_health() {
  local url="$1" n=0
  until curl -sf -o /dev/null "$url"; do
    n=$((n+1))
    if [[ $n -gt 120 ]]; then
      echo "timeout waiting for $url" >&2
      return 1
    fi
    sleep 0.25
  done
}

echo ">> Go nexus :$GW_PORT (zero-dep, shared OpenAI → mock)"
env -u NEXUS_POSTGRES_URL -u NEXUS_CLICKHOUSE_URL -u NEXUS_REDIS_URL \
  NEXUS_GATEWAY_ADDR=":$GW_PORT" \
  NEXUS_CONSOLE_ADDR=":$CON_PORT" \
  NEXUS_KEY_MODE=shared \
  NEXUS_ALLOW_SHARED_KEYS=true \
  NEXUS_DEV_MODE=true \
  OPENAI_API_KEY=sk-gate \
  OPENAI_BASE_URL="http://127.0.0.1:$MOCK_PORT/v1" \
  "$OUTDIR/nexus-go" >"$OUTDIR/go-server.log" 2>&1 &
GO_PID=$!
wait_health "http://127.0.0.1:$GW_PORT/healthz"

echo ">> load against Go ($STREAMS streams, $DURATION)"
go run ./scripts/load_baseline \
  -base-url "http://127.0.0.1:$GW_PORT" \
  -pid "$GO_PID" \
  -model "$MODEL" \
  -streams "$STREAMS" \
  -duration "$DURATION" \
  -out "$OUTDIR/go-sse.json"
kill "$GO_PID" 2>/dev/null || true
wait "$GO_PID" 2>/dev/null || true
GO_PID=""
sleep 0.5

echo ">> Elixir Mix release :$GW_PORT → mock"
(
  cd "$EX"
  MIX_ENV=prod mix release --overwrite --quiet
)
RELEASE="$EX/_build/prod/rel/nexus"
RELEASE_DISTRIBUTION=none \
NEXUS_UPSTREAM_BASE="http://127.0.0.1:$MOCK_PORT" \
NEXUS_GATEWAY_PORT="$GW_PORT" \
NEXUS_CONSOLE_PORT="$CON_PORT" \
  "$RELEASE/bin/nexus" start >"$OUTDIR/ex-server.log" 2>&1 &
EX_PID=$!
echo ">> elixir pid $EX_PID"
wait_health "http://127.0.0.1:$GW_PORT/healthz"

echo ">> load against Elixir ($STREAMS streams, $DURATION)"
go run ./scripts/load_baseline \
  -base-url "http://127.0.0.1:$GW_PORT" \
  -pid "$EX_PID" \
  -model "$MODEL" \
  -streams "$STREAMS" \
  -duration "$DURATION" \
  -out "$OUTDIR/ex-sse.json"
kill "$EX_PID" 2>/dev/null || true
wait "$EX_PID" 2>/dev/null || true
EX_PID=""

python3 - "$OUTDIR" <<'PY'
import json, sys, pathlib
d = pathlib.Path(sys.argv[1])
go = json.loads((d/"go-sse.json").read_text())
ex = json.loads((d/"ex-sse.json").read_text())

def check(name, ev, gv, limit):
    ok = ev <= gv * limit + 1e-9
    print(f"{'PASS' if ok else 'FAIL'}  {name}: elixir={ev} go={gv} limit=×{limit}")
    return ok

oks = []
oks.append(check("ttft p50 ns", ex["ttft_ns"]["p50"], go["ttft_ns"]["p50"], 1.10))
oks.append(check("ttft p99 ns", ex["ttft_ns"]["p99"], go["ttft_ns"]["p99"], 1.10))
oks.append(check("rss/stream kb", ex["rss_per_stream_kb"], go["rss_per_stream_kb"], 1.20))
print(f"go errors={go['errors']} elixir errors={ex['errors']} disconnect go={go['disconnect_cleanup_ok']} ex={ex['disconnect_cleanup_ok']}")
oks.append(go["errors"] == 0 and go["disconnect_cleanup_ok"])
oks.append(ex["errors"] == 0 and ex["disconnect_cleanup_ok"])
verdict = "PASS" if all(oks) else "FAIL"
(d/"verdict.txt").write_text(verdict + "\n")
print("VERDICT", verdict)
sys.exit(0 if verdict == "PASS" else 2)
PY
