#!/usr/bin/env bash
# 전체 검증을 한꺼번에 돌리는 러너. 각 단계의 출력을 /tmp/v_*.txt 에 남기고,
# 마지막에 PASS/FAIL 만 한 줄씩 요약한다 — 실패가 어디서 났는지 알 수 있다.
#
# 개별 단계는 그 자체로도 CI에서 단독 잡으로 돌릴 수 있도록 만들어져 있다.
# 이 스크립트는 "머지 직전에 한 번 돌려서 PR 상태를 한눈에 보고 싶다"는
# 로컬 사용을 위한 것이다.

set -uo pipefail
REPO="${REPO:-$(cd "$(dirname "$0")/.." && pwd)}"
cd "$REPO"

run_step() {
  local label="$1"; shift
  echo
  echo "==>  $label"
  if "$@" 2>&1 | tee "/tmp/v_${1##*/}.txt"; then
    echo "  ^ ok"
  else
    echo "  ^ FAILED — see /tmp/v_${1##*/}.txt"
    fail=1
  fi
}

fail=0

echo "==>  Go unit + integration"
go test -race -count=1 ./... 2>&1 | tee /tmp/v_go.txt | tail -3
[[ "${PIPESTATUS[0]}" -eq 0 ]] || fail=1

echo
echo "==>  npx wrapper tests"
(cd npx && node --test test/) 2>&1 | tee /tmp/v_npx.txt | tail -5
[[ "${PIPESTATUS[0]}" -eq 0 ]] || fail=1

echo
echo "==>  goreleaser naming contract"
if [[ -x /tmp/gobin/goreleaser ]]; then
  GR=/tmp/gobin/goreleaser
else
  GR="$(command -v goreleaser || true)"
fi
[[ -n "$GR" ]] || { echo "  goreleaser not installed; skipping live snapshot"; }

if [[ -n "$GR" ]]; then
  rm -rf dist
  "$GR" release --snapshot --clean --skip=publish >/tmp/v_gr.log 2>&1 \
    && echo "  snapshot build: ok"
  ./scripts/test_release_naming.sh 2>&1 | tee /tmp/v_names.txt | tail -3 \
    && [[ "${PIPESTATUS[0]}" -eq 0 ]] || fail=1
fi

echo
echo "==>  Helm render regression"
./scripts/test_helm_render.sh 2>&1 | tee /tmp/v_helm.txt | tail -3
[[ "${PIPESTATUS[0]}" -eq 0 ]] || fail=1

echo
echo "==>  Local mode end-to-end (downloads Postgres on first run)"
./scripts/test_local_mode.sh 2>&1 | tee /tmp/v_local.txt | tail -3
[[ "${PIPESTATUS[0]}" -eq 0 ]] || fail=1

echo
echo "==>  Container runtime layout — no daemon required"
./scripts/verify_image_paths.sh 2>&1 | tee /tmp/v_img.txt | tail -3
[[ "${PIPESTATUS[0]}" -eq 0 ]] || fail=1

echo
echo "=========================================="
echo "  Verification summary"
echo "=========================================="
echo "  Go tests:        $(grep -c FAIL /tmp/v_go.txt || echo 0) FAIL"
echo "  npx tests:       $(grep -c FAIL /tmp/v_npx.txt || echo 0) FAIL"
echo "  goreleaser:      $(grep -c FAIL /tmp/v_names.txt 2>/dev/null || echo 0) FAIL"
echo "  helm render:     $(grep -c FAIL /tmp/v_helm.txt || echo 0) FAIL"
echo "  local mode e2e:  $(grep -c FAIL /tmp/v_local.txt || echo 0) FAIL"
echo "  image layout:    $(grep -c FAIL /tmp/v_img.txt || echo 0) FAIL"

if [[ "$fail" -eq 0 ]]; then
  echo
  echo "  all checks green"
  exit 0
fi
echo
echo "  failures detected — open /tmp/v_*.txt for full output"
exit 1
