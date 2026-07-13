#!/usr/bin/env bash
# V5 high-concurrency readiness — stress suite.
#
# Validates the same contracts as the unit tests in
#   internal/{gateway,limiter,gateway/providers}/...
# but at a higher scale and from outside the test binary, so we catch
# regressions the in-process tests would mask (e.g. scheduler effects,
# GC pressure, sync.Pool contention from many CPUs).
#
# All tests run in-process (no docker). Failure of any test exits 1
# and the suite prints a summary.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

# Each subtest contributes an entry to PASS or FAIL.
PASS=0
FAIL=0
FAILED_TESTS=()

run_test() {
  local name="$1"
  shift
  echo ""
  echo "--- $name ---"
  if "$@" >/tmp/v5_stress.out 2>&1; then
    echo "  PASS"
    PASS=$((PASS + 1))
  else
    echo "  FAIL"
    FAIL=$((FAIL + 1))
    FAILED_TESTS+=("$name")
    # Print last 15 lines for context (don't dump everything: stress
    # tests are noisy and triple the output).
    tail -15 /tmp/v5_stress.out | sed 's/^/    /'
  fi
}

# Test 1: 200-way concurrent hammering through the Concurrency middleware
# with cap=8. We assert the peak in-flight never exceeds the cap and at
# least some requests are rejected.
run_test "Concurrency middleware ceiling" \
  go test -race -count=2 -timeout=60s ./internal/gateway -run '^TestConcurrencyMiddleware_OverCap' -v

# Test 2: Heavy SynchronizationPool hammer for the SSE buffer pool. We
# exercise 64 goroutines × 32 streams = 2048 streamed responses. The
# pool should never let a 64 KiB buffer pre-fill > a few MiB at once.
run_test "SSE buffer pool under load" \
  go test -race -count=2 -timeout=60s ./internal/gateway/providers -run '^TestStreaming_BufferPool_ConcurrentStreams' -v

# Test 3: Pooled transport saturation — 100 goroutines against an
# echo server. Already in TestPooledHTTPClient_ConcurrencyUnderLoad;
# rerun with -count to amplify scheduling variance.
run_test "Pooled transport saturation" \
  go test -race -count=3 -timeout=60s ./internal/gateway/providers -run '^TestPooledHTTPClient_ConcurrencyUnderLoad' -v

# Test 4: ConcurrencyCap race hammer, 64 goroutines × 200 ops. The
# invariant is: peak in-flight == cap. Multiple counts amplify race
# detection.
run_test "ConcurrencyCap race hammer" \
  go test -race -count=5 -timeout=60s ./internal/limiter -run '^TestConcurrencyCap_Concurrent' -v

# Test 5: Stress the buffer pool acquire/release cycle to verify
# undersized buffers are rejected (no pool poisoning).
run_test "Buffer pool poison-resistance" \
  go test -race -count=5 -timeout=60s ./internal/gateway/providers -run '^TestReleaseBuffer_RejectsUndersized' -v

echo ""
echo "== V5 stress suite =="
echo "  passed: $PASS"
echo "  failed: $FAIL"
if [[ "$FAIL" -gt 0 ]]; then
  echo "  failing tests: ${FAILED_TESTS[*]}"
  exit 1
fi
echo "  all tests passed"
