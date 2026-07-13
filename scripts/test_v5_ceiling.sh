#!/usr/bin/env bash
# V5 single-pod ceiling measurement.
#
# Goal: quantify "one pod" capacity so HPA metric thresholds
# (CPU 70%, in-flight req X, ...) can be calibrated against actual
# delivered throughput, not vibes. Plan V5 calls this out as a
# prerequisite for HPA activation.
#
# Tooling:
#   - wrk 4.x against the dev profile's nexus gateway (:8080)
#   - mock-upstream with bounded worker pool (workers=8) as the LLM
#     backend so we measure *gateway* latency, not provider variability
#
# Outputs (printed, not saved):
#   - per-phase p50/p75/p90/p99 (ms) + req/s + transfer
#
# Up: docker compose -f deploy/docker-compose.yml --profile dev up -d
# Run: ./scripts/test_v5_ceiling.sh

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

GATEWAY="${GATEWAY:-http://localhost:8080}"
CONSOLE="${CONSOLE:-http://localhost:8081}"
DURATION="${DURATION:-15}"
WRK_WORKERS="${WRK_WORKERS:-4}"

for bin in wrk docker curl awk sed; do
  command -v "$bin" >/dev/null 2>&1 || { echo "missing dep: $bin" >&2; exit 2; }
done

echo "== V5 single-pod ceiling measurement =="
echo "  gateway       : $GATEWAY"
echo "  duration/phase: ${DURATION}s   (phase-4 is 4× longer)"
echo "  wrk workers   : $WRK_WORKERS"
echo

if ! curl -fsS "$GATEWAY/healthz" >/dev/null 2>&1; then
  cat <<EOF >&2
gateway is not reachable at $GATEWAY. Bring the dev stack up first:
  docker compose -f deploy/docker-compose.yml --profile dev up -d
EOF
  exit 2
fi

# Mint a virtual key the wrk client can present.
echo "[mint vkey via console signup]"
EMAIL="ceil_$(date +%s)@nexus.local"
curl -fsS -o /tmp/ceil_signup.json -X POST -H 'Content-Type: application/json' \
  --data "{\"email\":\"$EMAIL\",\"password\":\"ceil_test_pw_1\",\"provider\":\"openai\",\"provider_secret\":\"sk-ceil-fake\"}" \
  "$CONSOLE/api/auth/register"
VKEY=$(sed -n 's/.*"virtual_key":"\([^"]*\)".*/\1/p' /tmp/ceil_signup.json || true)
if [ -z "$VKEY" ]; then
  VKEY=$(sed -n 's/.*"vkey":"\([^"]*\)".*/\1/p' /tmp/ceil_signup.json || true)
fi
[ -n "$VKEY" ] || { echo "could not parse vkey:"; cat /tmp/ceil_signup.json; exit 3; }
export VKEY
echo "  vkey (truncated): ${VKEY:0:24}..."

# Warm-up.
echo
echo "[warmup 5s @ 50 concurrent]"
wrk -t"$WRK_WORKERS" -c50 -d5 --latency "$GATEWAY/v1/chat/completions" \
  -s "$ROOT/scripts/wrk_ceiling.lua" \
  | grep -E 'Latency|Req/sec' | sed 's/^/    /'

# 4 phases. Each phase writes its raw wrk output to /tmp/v5_p{n}.txt.
PHASES=( "200,${DURATION}" "500,${DURATION}" "1000,${DURATION}" "1000,$((DURATION * 4))" )
NEXUS_CID="$(docker compose -f "$ROOT/deploy/docker-compose.yml" ps -q nexus 2>/dev/null | head -1 || true)"

# Sample RSS via docker stats while a phase is running. We snapshot
# before, then sample at 2-second intervals, then aggregate to
# peak / mean. We use perl's number parsing because macOS awk lacks
# strtonum and one-line unit conversions are brittle.
sample_rss() {
  local duration="$1"
  local out_prefix="$2"
  local end_ts=$(( $(date +%s) + duration ))
  : > "${out_prefix}.rss"
  while [ "$(date +%s)" -le "$end_ts" ]; do
    if [ -n "$NEXUS_CID" ]; then
      docker stats "$NEXUS_CID" --no-stream --format '{{.MemUsage}}' 2>/dev/null \
        | head -1 \
        | perl -ne 'if (/([\d.]+)\s*(KiB|MiB|GiB|TiB)/) { $v=$1; $u=$2; $v*=1024 if $u eq "MiB"; $v*=1024*1024 if $u eq "GiB"; $v*=1024*1024*1024 if $u eq "TiB"; $v/=1024 if $u eq "KiB"; printf "%d\n", $v; }' \
        >> "${out_prefix}.rss"
      # Sanity: only emit when numeric.
    fi
    sleep 2
  done
}

# Capture GC events that occurred *during* a phase by snapshotting
# the cumulative gc count before and after, then `docker logs` reads
# lines and we filter to lines whose `@Ts` (boot-relative elapsed
# seconds) lies inside the phase's wall-clock window.
#
# Why this is needed: `docker logs --since` re-emits *all* historical
# gctrace lines on every call (the timestamp filter applies to log
# record metadata, but gctrace lines are emitted at a single time at
# boot when the GC stats were eagerly flushed). Filtering by `Ts`
# duration-into-boot matches reality.
sample_gc() {
  local duration="$1"
  local out_prefix="$2"
  local phase_start_epoch
  # `date +%s.%N` is GNU-only; macOS `date` returns the literal "N"
  # which silently rounds the diffs to .000 in awk. Use python (BSD-
  # and Linux-defaulted, ships inside macOS) to get epoch seconds as
  # a precise float.
  phase_start_epoch=$(python3 -c 'import time; print(time.time())' 2>/dev/null \
    || perl -e 'use Time::HiRes; print Time::HiRes::time(), "\n"' \
    || date +%s 2>/dev/null)
  # Container boot time as epoch seconds. Docker's StartedAt is an
  # RFC3339 string; we parse the front (date + time, no nanos) into
  # epoch seconds via perl.
  local boot_epoch
  boot_epoch=$(docker inspect --format '{{.State.StartedAt}}' "$NEXUS_CID" \
    | perl -MTime::Piece -ne '
        if (/^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})/) {
          my $t = Time::Piece->strptime("$1-$2-$3 $4:$5:$6", "%Y-%m-%d %H:%M:%S");
          print $t->epoch;
        }
      ')
  : > "${out_prefix}.gc"
  sleep "$duration"
  # After phase ends, dump log once. Filter gctrace lines whose
  # `Ts` (seconds since boot) is within this phase's window.
  local lower
  lower=$(awk -v s="$phase_start_epoch" -v b="$boot_epoch" -v m="5" 'BEGIN{printf "%.3f", s - b - m}')
  local upper
  upper=$(awk -v s="$phase_start_epoch" -v d="$duration" -v b="$boot_epoch" -v m="5" 'BEGIN{printf "%.3f", s - b + d + m}')
  docker logs "$NEXUS_CID" 2>/dev/null | grep -E 'gc [0-9]+ @' \
    | LO="$lower" HI="$upper" perl -ne 'if (/gc \d+ \@([0-9.]+)s/ && $1 >= $ENV{LO} && $1 <= $ENV{HI}) { print; }' \
    >> "${out_prefix}.gc" || true
}

# Background sampler for one phase.
spawn_samplers() {
  local pid_rss pid_gc
  local dur="$1" prefix="$2"
  (
    sample_rss "$dur" "$prefix"
  ) &
  pid_rss=$!
  (
    sample_gc "$dur" "$prefix"
  ) &
  pid_gc=$!
  echo "$pid_rss $pid_gc"
}

wait_samplers() {
  for pid in "$@"; do
    wait "$pid" 2>/dev/null || true
  done
}

if [ -z "$NEXUS_CID" ]; then
  echo "warning: couldn't resolve nexus container id; memory/GC sections will be empty" >&2
fi

for i in 0 1 2 3; do
  IFS=',' read -r CONN DUR <<< "${PHASES[$i]}"
  PHASE_PREFIX="/tmp/v5_p$((i+1))"
  echo
  echo "[phase-$((i+1))] $CONN concurrent, ${DUR}s"
  samplers=( $(spawn_samplers "$DUR" "$PHASE_PREFIX") )
  wrk -t"$WRK_WORKERS" -c"$CONN" -d"$DUR" --latency \
    "$GATEWAY/v1/chat/completions" \
    -s "$ROOT/scripts/wrk_ceiling.lua" >"${PHASE_PREFIX}.txt" 2>&1 || true
  wait_samplers "${samplers[@]}"
  grep -E 'Latency|Req/sec|Transfer' "${PHASE_PREFIX}.txt" | sed 's/^/    /'
done

# Summary: latency distribution + req/sec per phase, plus RSS summary
# from the docker stats samples captured during each phase.
echo
echo "== V5 ceiling summary =="
printf '%-18s %-8s %-8s %-8s %-8s %-10s %-14s %-14s\n' phase p50 p75 p90 p99 req/s rss-mean rss-peak
for n in 1 2 3 4; do
  f="/tmp/v5_p${n}.txt"
  rss="/tmp/v5_p${n}.rss"
  [ -s "$f" ] || continue
  # Get RSS peak in MiB (input is in KiB).
  rss_peak="n/a"
  rss_mean="n/a"
  if [ -s "$rss" ]; then
    rss_peak=$(awk 'BEGIN{m=0} { if ($1>m) m=$1 } END { printf "%.1f MiB", m/1024 }' "$rss")
    rss_mean=$(awk 'BEGIN{s=0; n=0} {s+=$1; n++} END { if (n>0) printf "%.1f MiB", s/n/1024; else print "n/a" }' "$rss")
  fi
  awk -v ph="phase-$n" -v rp="$rss_peak" -v rm="$rss_mean" '
    /50%/  {p50=$2}
    /75%/  {p75=$2}
    /90%/  {p90=$2}
    /99%/  {p99=$2}
    /Requests\/sec:/ {rps=$2}
    END {printf "%-18s %-8s %-8s %-8s %-8s %-10s %-14s %-14s\n", ph, p50, p75, p90, p99, rps, rm, rp}
  ' "$f"
done

# GC summary: pull all .gc files across phases and report max + count.
echo
echo "== GC trace summary (GODEBUG=gctrace=1) =="
total_count=0
max_pause=0
for n in 1 2 3 4; do
  gc="/tmp/v5_p${n}.gc"
  if [ -s "$gc" ]; then
    while IFS= read -r line; do
      # Extract the *clock* (wall) GC pause: "N ms clock". This is the
      # operator-visible pause (the *cpu* counterpart can be much
      # larger on multi-core but isn't what request latency sees).
      pause=$(echo "$line" | sed -n 's/.* \([0-9.]\+\) ms clock.*/\1/p')
      if [ -n "$pause" ]; then
        # Compare via awk (shell floats are unreliable on macOS).
        cmp=$(awk -v a="$pause" -v b="$max_pause" 'BEGIN{print (a>b)?1:(a<b)?-1:0}')
        if [ "$cmp" = "1" ]; then
          max_pause="$pause"
        fi
      fi
    done < "$gc"
    phase_count=$(wc -l < "$gc")
    total_count=$((total_count + phase_count))
    echo "  phase-$n: $phase_count GC events"
  fi
done
if [ "$total_count" -gt 0 ]; then
  echo "  total GC events across phases: $total_count"
  echo "  max wall-clock GC pause: ${max_pause} ms"
else
  total_log=$(docker logs "$NEXUS_CID" 2>/dev/null | grep -cE 'gc [0-9]+ @' || true)
  if [ "${total_log:-0}" -gt 0 ]; then
    echo "  no GC events captured this run's window (gctrace is live,"
    echo "  but heap didn't trigger a stop-the-world while stress was on)."
    echo "  Total gctrace events in container log so far: $total_log"
  else
    echo "  no GC events captured (gctrace disabled or container log unreadable)"
  fi
fi
echo
echo "raw output preserved at /tmp/v5_p{1..4}.{txt,rss,gc}"
echo "next: feed p99 + req/s into HPA metric threshold. Until then, HPA is hold."
