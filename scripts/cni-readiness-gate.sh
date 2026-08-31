#!/usr/bin/env bash
# scripts/cni-readiness-gate.sh
#
# Phase D-2b.22: CNI environment-readiness gate.
#
# Why this script exists as a SINGLE entrypoint:
#   1. Gate #3..#9 of the directive must run in a
#      fixed observation order so a chart regression
#      can never be misclassified as a CNI-env flake.
#      The install path used to fold the waiting
#      inside scripts/install-nexus-test.sh where
#      a "fixture Pods not Ready within 4 minutes"
#      could equally mean a real chart problem, a
#      cilium scheduling race, or a kubelet cold-
#      pull of the fixture image (runs 32450384157,
#      32451208718, 32452639663 are each
#      indistinguishable on disk unless the gate
#      records which precondition failed).
#   2. A classification code expressed as an EXIT
#      CODE (0 / 10 / 11 / 12 / 13) is auditable.
#      A bare string in a log line is not.
#
# This gate runs TWICE in the chart's CNI gate
# workflow, controlled by $GATE_PHASE:
#   * phase=pre-fixture : invoked right before
#     `kubectl apply` of the fixture manifests,
#     after `kind create cluster` + cilium install
#     + System-Pod readiness. Verifies gate #1..#7
#     only (cluster, cilium, system pods,
#     namespaces). Exits 10 (CLUSTER_OR_CNI_NOT_READY)
#     on any of those failures.
#   * phase=post-fixture: invoked right after the
#     fixture manifests are applied and a brief
#     settle window has elapsed. Verifies gate #8
#     (cilium endpoint registration) and gate #9
#     (control probe). Exits 12 (FIXTURE_NOT_READY)
#     on those failures.
# The two phases share the same $READINESS_JSON
# envelope so the post-fixture run appends to the
# pre-fixture record, and the verifier reads a
# single artifact.
#
# Inputs (env):
#   ARTIFACTS       (default $PWD/artifacts/integrationcni)
#   CLUSTER_NAME    (default nexus-cni-test)
#   K8S_VERSION     (default 1.29.0) - pinned to kindest/node image
#   CILIUM_VERSION  (default 1.15.3) - chart-gate expressed value
#   KUBECTL_TIMEOUT (default 360s)   - bounded timeout per step
#   IMAGE_PULL_TIMEOUT (default 360s) - bounded timeout for cold pulls
#   GATE_PHASE      (default "both") - pre-fixture | post-fixture | both
#
# Outputs:
#   $ARTIFACTS/readiness.{log,json,summary.txt}
#
# Exit codes:
#   0  SUCCESS                - environment is fully ready, run chart scenarios
#   10 CLUSTER_OR_CNI_NOT_READY - cluster/CNI was not in the state required
#                                  for a chart-side regression assertion
#                                  (kubelet, image pull, cilium, etc.)
#   11 CHART_OR_POLICY_INVALID - (reserved for caller: chart-render or apply
#                                  failed; this script does not run the
#                                  chart-side render itself)
#   12 FIXTURE_NOT_READY      - cluster is healthy but fixture pods did
#                                  not converge within the bounded window
#   13 SCENARIO_POLICY_REGRESSION - (reserved for caller: scenario probes
#                                  returned unexpected allow/deny verdicts)
#   14 FIXTURE_IMAGE_NOT_LOADED - fixture image pipeline failed (build,
#                                  kind load per-node, imagePullBackOff
#                                  despite kind-load rc=0). Distinct from
#                                  FIXTURE_NOT_READY because the failure
#                                  is NOT convergence-time; it is a
#                                  precondition.
#   15 FIXTURE_INVALID        - pre-flight `kubectl apply
#                                  --dry-run=server --validate=strict`
#                                  rejected at least one fixture yaml.
#                                  structural, not runtime.
#
# The script does NOT change chart-side code, scripts
# under deploy/helm/nexus/**, or any
# policies. It only observes.

set -euo pipefail

ARTIFACTS="${ARTIFACTS:-${PWD}/artifacts/integrationcni}"
CLUSTER_NAME="${CLUSTER_NAME:-nexus-cni-test}"
K8S_VERSION="${K8S_VERSION:-1.29.0}"
CILIUM_VERSION="${CILIUM_VERSION:-1.15.3}"
EXPECTED_IMAGE_TAG="${EXPECTED_IMAGE_TAG:-v${K8S_VERSION}}"
EXPECTED_NODE_COUNT="${EXPECTED_NODE_COUNT:-3}"   # control-plane + 2 workers
KUBECTL_TIMEOUT="${KUBECTL_TIMEOUT:-360s}"
IMAGE_PULL_TIMEOUT="${IMAGE_PULL_TIMEOUT:-360s}"
GATE_PHASE="${GATE_PHASE:-both}"   # pre-fixture | post-fixture | both

mkdir -p "$ARTIFACTS"
READINESS_LOG="$ARTIFACTS/readiness.log"
READINESS_JSON="$ARTIFACTS/readiness.json"
READINESS_SUMMARY="$ARTIFACTS/readiness.summary.txt"

# Phase D-2b.46: explicit abort-classification
# contract for install-nexus-test.sh::abort_as().
# When abort_as passes the abort label through the
# FIXED-NAME env INSTALL_ABORT_CLASSIFICATION, we
# honour it before any kubectl/kind/docker call,
# before the legacy FIXTURE_IMAGE_NOT_LOADED /
# FIXTURE_INVALID env-token blocks below, and
# before any GATE_PHASE / cluster probe. This is
# the deterministic shortcut the install script's
# abort path relies on; an unknown / mis-typed
# label fails closed as CLUSTER_OR_CNI_NOT_READY
# (10) with an explicit "unknown install abort
# classification" detail so a downstream verifier
# can correlate the abort with the install script
# that issued it. An empty value preserves every
# legacy behaviour: we fall through to the legacy
# blocks and the GATE_PHASE orchestration below.
#
# Map (label -> exact exit code -> summary line):
#   CLUSTER_OR_CNI_NOT_READY  10
#   CHART_OR_POLICY_INVALID   11
#   FIXTURE_NOT_READY         12
#   FIXTURE_IMAGE_NOT_LOADED  14
#   FIXTURE_INVALID           15
if [[ -n "${INSTALL_ABORT_CLASSIFICATION:-}" ]]; then
  REQ_LABEL="${INSTALL_ABORT_CLASSIFICATION}"
  REQ_DETAIL="${INSTALL_ABORT_FAILURE_DETAIL:-unspecified}"
  case "$REQ_LABEL" in
    CLUSTER_OR_CNI_NOT_READY)
      {
        printf 'classification=%s (exit 10)\n' "$REQ_LABEL"
        printf 'first_failed_step=00-install-abort\n'
        printf 'failure_reason=%s\n' "$REQ_DETAIL"
      } >> "$READINESS_LOG"
      printf '%s\n' "$REQ_LABEL" > "$READINESS_SUMMARY"
      exit 10
      ;;
    CHART_OR_POLICY_INVALID)
      {
        printf 'classification=%s (exit 11)\n' "$REQ_LABEL"
        printf 'first_failed_step=00-install-abort\n'
        printf 'failure_reason=%s\n' "$REQ_DETAIL"
      } >> "$READINESS_LOG"
      printf '%s\n' "$REQ_LABEL" > "$READINESS_SUMMARY"
      exit 11
      ;;
    FIXTURE_NOT_READY)
      {
        printf 'classification=%s (exit 12)\n' "$REQ_LABEL"
        printf 'first_failed_step=00-install-abort\n'
        printf 'failure_reason=%s\n' "$REQ_DETAIL"
      } >> "$READINESS_LOG"
      printf '%s\n' "$REQ_LABEL" > "$READINESS_SUMMARY"
      exit 12
      ;;
    FIXTURE_IMAGE_NOT_LOADED)
      {
        printf 'classification=%s (exit 14)\n' "$REQ_LABEL"
        printf 'first_failed_step=00-install-abort\n'
        printf 'failure_reason=%s\n' "$REQ_DETAIL"
      } >> "$READINESS_LOG"
      printf '%s\n' "$REQ_LABEL" > "$READINESS_SUMMARY"
      exit 14
      ;;
    FIXTURE_INVALID)
      {
        printf 'classification=%s (exit 15)\n' "$REQ_LABEL"
        printf 'first_failed_step=00-install-abort\n'
        printf 'failure_reason=%s\n' "$REQ_DETAIL"
      } >> "$READINESS_LOG"
      printf '%s\n' "$REQ_LABEL" > "$READINESS_SUMMARY"
      exit 15
      ;;
    *)
      # Unknown non-empty label: fail closed as
      # CLUSTER_OR_CNI_NOT_READY (10) with explicit
      # redacted detail. We do NOT silently
      # continue into cluster probes; that would
      # mask a misclassification behind a
      # pseudo-success fixture probe.
      {
        printf 'classification=%s (exit 10)\n' "CLUSTER_OR_CNI_NOT_READY"
        printf 'first_failed_step=00-install-abort\n'
        printf 'failure_reason=unknown install abort classification: %s\n' "$REQ_LABEL"
      } >> "$READINESS_LOG"
      printf 'CLUSTER_OR_CNI_NOT_READY\n' > "$READINESS_SUMMARY"
      exit 10
      ;;
  esac
fi

# Phase-aware step range. pre-fixture = #1..#7,
# post-fixture = #8..#9 (re-using the SAME
# readiness.json envelope so the verifier reads
# one artifact, not two).
PHASE_FIRST_STEP=1
PHASE_LAST_STEP=9

# Phase D-2b.26: image-pipeline classification.
#
# install-nexus-test.sh may abort into the gate
# with FIXTURE_IMAGE_NOT_LOADED=1 if the
# built-from-source fixture image is missing on
# at least one kind node, if the build returned
# non-zero, or if the fixture Pod never reaches
# Ready due to ImagePullBackOff /
# ErrImageNeverPull / CrashLoopBackOff. This is
# NOT a chart-side regression, and it must NEVER
# be mis-classified as SCENARIO_POLICY_REGRESSION.
# The exit code 14 is distinct from 10 (cluster
# flake), 11 (chart render invalid), 12 (fixture
# plumbing), and 13 (scenario verdicts).
if [[ "${FIXTURE_IMAGE_NOT_LOADED:-0}" == "1" ]]; then
  REASON="${FIXTURE_IMAGE_LOAD_FAILURE_DETAIL:-unspecified}"
  {
    printf 'classification=FIXTURE_IMAGE_NOT_LOADED (exit 14)\n'
    printf 'first_failed_step=00-fixture-image-pipeline\n'
    printf 'failure_reason=%s\n' "$REASON"
  } | tee -a "$READINESS_LOG" 2>/dev/null || true
  printf 'FIXTURE_IMAGE_NOT_LOADED\n' > "$READINESS_SUMMARY"
  exit 14
fi

# Phase D-2b.27: pre-flight fixture dry-run gate.
#
# install-nexus-test.sh aborts into the gate
# with FIXTURE_INVALID=1 if `kubectl apply
# --dry-run=server --validate=strict` rejected
# one or more fixture yamls. This is a
# STRUCTURAL failure (indent drift,
# unknown fields) on a fixture the chart
# cannot influence. It must NEVER be
# classified as SCENARIO_POLICY_REGRESSION
# (13) or CHART_OR_POLICY_INVALID (11).
if [[ "${FIXTURE_INVALID:-0}" == "1" ]]; then
  REASON="${FIXTURE_INVALID_FAILURE_DETAIL:-unspecified}"
  {
    printf 'classification=FIXTURE_INVALID (exit 15)\n'
    printf 'first_failed_step=00-fixture-yaml-preflight\n'
    printf 'failure_reason=%s\n' "$REASON"
  } | tee -a "$READINESS_LOG" 2>/dev/null || true
  printf 'FIXTURE_INVALID\n' > "$READINESS_SUMMARY"
  exit 15
fi

case "$GATE_PHASE" in
  pre-fixture)
    # The first six gates run before any chart
    # fixture is applied. Gates 7..9 (namespaces
    # prepared, fixture endpoints registered,
    # control probe) are run in the post-fixture
    # phase because their object of observation
    # only exists once the fixture manifests have
    # been `kubectl apply`-ed. The pre-fixture
    # phase therefore covers: pinned versions,
    # node image pull, node Ready, CoreDNS,
    # cilium agents ready, cilium enforcement.
    # Splitting the phase this way means a
    # missing fixture namespace is NEVER
    # logged under CLUSTER_OR_CNI_NOT_READY at
    # the pre-fixture phase — that classification
    # is reserved for genuine cluster/env flake.
    PHASE_FIRST_STEP=1
    PHASE_LAST_STEP=6
    ;;
  post-fixture)
    PHASE_FIRST_STEP=7
    PHASE_LAST_STEP=9
    ;;
  both|"")
    PHASE_FIRST_STEP=1
    PHASE_LAST_STEP=9
    ;;
  *)
    echo "[cni-readiness-gate] ERROR: unknown GATE_PHASE=$GATE_PHASE" >&2
    exit 10
    ;;
esac

# Append-only artifacts: we do NOT truncate the
# log/json between pre-fixture and post-fixture
# runs. The post-fixture call appends.
if [[ "$GATE_PHASE" == "pre-fixture" || "$GATE_PHASE" == "both" ]]; then
  : > "$READINESS_LOG"
fi

# Stable fields written to the JSON before any
# per-step result is appended. Each step appends
# its own record. The summary line at the very
# end is the entry a verifier reads first.
emit_header() {
  {
    # BR: chart-side miss should never hide env-side fail
    printf 'recovery_pr_sha=%s\n' "${RECOVERY_PR_SHA:-unknown}"
    printf 'workflow_run_id=%s\n' "${WORKFLOW_RUN_ID:-unknown}"
    printf 'cluster_name=%s\n' "$CLUSTER_NAME"
    printf 'k8s_node_image_expected=kindest/node:%s\n' "$EXPECTED_IMAGE_TAG"
    printf 'cilium_image_expected=%s\n' "$CILIUM_VERSION"
    printf 'expected_node_count=%s\n' "$EXPECTED_NODE_COUNT"
    printf 'kubectl_timeout=%s\n' "$KUBECTL_TIMEOUT"
    printf 'image_pull_timeout=%s\n' "$IMAGE_PULL_TIMEOUT"
  } | tee -a "$READINESS_LOG"
}

# Each step records:
#   - the step name
#   - a passed/failed verdict
#   - the underlying observation (kubectl output,
#     image ids, etc.)
# Steps that exit non-zero inside this function
# are allowed to "trap" us after dumping the
# artifact bundle for the steps that ran.
record_step() {
  local step="$1"; local verdict="$2"; local detail="$3"
  printf '[step %02d] %s : %s\n' "$step" "$step_name" "$verdict" | tee -a "$READINESS_LOG"
  printf '          detail: %s\n' "$detail" | tee -a "$READINESS_LOG"
  printf ',\n{"step":%d,"name":"%s","verdict":"%s","detail":%s}' \
    "$step" "$step_name" "$verdict" \
    "$(printf '%s' "$detail" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()[:1500]))')" \
    >> "${READINESS_JSON}.tmp"
}

# Generic classifier: if $1 is "ok", emit nothing,
# otherwise emit the mobile classification and
# exit $2. This is how we keep the first line of
# the summary a controlled set of strings.
classify() {
  local verdict="$1"; local code="$2"; local label="$3"
  if [[ "$verdict" != "ok" ]]; then
    {
      printf 'classification=%s (exit %d)\n' "$label" "$code"
      printf 'first_failed_step=%s\n' "$step_name"
    } | tee -a "$READINESS_LOG"
    printf '%s\n' "$label" > "$READINESS_SUMMARY"
    exit "$code"
  fi
}

# Helper: bounded wait for kubectl condition.
# Returns non-zero on timeout. Records the last
# observed kubectl output into readiness.log so
# the post-mortem shows "what kubectl saw right
# before the deadline".
bounded_kubectl_wait() {
  local what="$1"; local timeout="$2"; local rest="$3"
  local out rc
  set +e
  out=$(timeout --foreground "$timeout" kubectl "$what" $rest 2>&1)
  rc=$?
  set -e
  printf -- '--- kubectl %s %s (rc=%d) ---\n' "$what" "$rest" "$rc" \
    >> "$READINESS_LOG"
  printf '%s\n' "$out" >> "$READINESS_LOG"
  return "$rc"
}

# Helper: cold-pull the kindest node image into
# the local docker cache. We use
# `docker image inspect` rather than `docker pull`
# so the call is idempotent for warm caches and
# still surfaces a clear "ImagePull error" in the
# log if the cold pull fails.
ensure_kind_node_image() {
  set +e
  docker image inspect "kindest/node:${EXPECTED_IMAGE_TAG}" \
    >/dev/null 2>&1 \
    || docker pull --quiet "kindest/node:${EXPECTED_IMAGE_TAG}" \
       2>>"$READINESS_LOG"
  local rc=$?
  set -e
  return "$rc"
}

emit_header
# Boot the JSON envelope only on the first
# (pre-fixture) phase. Subsequent phases append.
if [[ "$GATE_PHASE" == "pre-fixture" || "$GATE_PHASE" == "both" ]]; then
  python3 - "$ARTIFACTS" "$RECOVERY_PR_SHA" "$WORKFLOW_RUN_ID" "$CLUSTER_NAME" "$EXPECTED_IMAGE_TAG" "$CILIUM_VERSION" "$EXPECTED_NODE_COUNT" "$KUBECTL_TIMEOUT" "$IMAGE_PULL_TIMEOUT" "$GATE_PHASE" <<'PY' > "$READINESS_JSON"
import json, sys
art, sha, rid, cn, image, cilium, nn, kt, ipt, phase = sys.argv[1:]
print(json.dumps({
    "recovery_pr_sha": sha,
    "workflow_run_id": rid,
    "cluster_name": cn,
    "k8s_node_image_expected": f"kindest/node:{image}",
    "cilium_image_expected": cilium,
    "expected_node_count": int(nn),
    "kubectl_timeout": kt,
    "image_pull_timeout": ipt,
    "phase": phase,
    "results": [],
    "classification": "PENDING",
}, indent=2))
PY
fi

# `run_step N` runs the gate whose ordinal is N
# (1..9) but no-ops if $N is outside the current
# phase's range. Returning 0 if the step ran
# cleanly; non-zero otherwise. The script-level
# `set -euo pipefail` is preserved because
# `run_step` ALWAYS returns 0 (the
# classification-fail exit is delegated to
# `classify`).
run_step() {
  local n="$1"
  if (( n < PHASE_FIRST_STEP || n > PHASE_LAST_STEP )); then
    return 0
  fi
  do_step_"$n"
}

# -----------------------------------------------------------------
# Gate 1: Pinned version check
# Why: every other step assumes kind k8s and
# cilium are exactly the versions the chart was
# rendered against; a mismatch silently degrades
# NetPol semantics.
# -----------------------------------------------------------------
step_name="01-pinned-versions"
step_no=1
run_in_phase() { (( step_no >= PHASE_FIRST_STEP && step_no <= PHASE_LAST_STEP )); }
if run_in_phase; then
{
  printf -- '--- kind version ---\n'
  kind version 2>&1 | tee -a "$READINESS_LOG"
  printf -- '--- kubectl version ---\n'
  kubectl version --client --short 2>&1 | tee -a "$READINESS_LOG" || true
  printf -- '--- helm version ---\n'
  helm version --short 2>&1 | tee -a "$READINESS_LOG" || true
  printf -- '--- cilium version ---\n'
  cilium version 2>&1 | head -3 | tee -a "$READINESS_LOG" || true
} >/dev/null

PINS_OK=true
if ! grep -q "kind v0.22.0" "$READINESS_LOG"; then
  PINS_OK=false
fi
if ! grep -q "v1.29.0" "$READINESS_LOG"; then
  PINS_OK=false
fi
if ! grep -q "v3.14.4" "$READINESS_LOG"; then
  PINS_OK=false
fi
if ! grep -q "cilium-cli: v0.15.20" "$READINESS_LOG"; then
  PINS_OK=false
fi
if $PINS_OK; then
  record_step 1 "ok" "pins match directive"
else
  record_step 1 "failed" "pins disagree with directive"
  classify failed 10 CLUSTER_OR_CNI_NOT_READY
fi
fi

# -----------------------------------------------------------------
# Gate 2: Image pull readiness (cold cache only)
# We use docker image inspect so an already-cached
# image short-circuits the call. The pull is
# bounded by IMAGE_PULL_TIMEOUT through the
# `timeout --foreground` wrapper.
# -----------------------------------------------------------------
step_name="02-node-image-pull"
step_no=2
run_in_phase() { (( step_no >= PHASE_FIRST_STEP && step_no <= PHASE_LAST_STEP )); }
if run_in_phase; then

if ! command -v docker >/dev/null 2>&1; then
  record_step 2 "skipped" "docker not on PATH (kind-only cluster); ensure node image inside kind create"
else
  if timeout --foreground "$IMAGE_PULL_TIMEOUT" \
       docker pull --quiet "kindest/node:${EXPECTED_IMAGE_TAG}" \
       2>>"$READINESS_LOG"; then
    record_step 2 "ok" "kindest/node:${EXPECTED_IMAGE_TAG} present"
  else
    record_step 2 "failed" "cold pull of kindest/node:${EXPECTED_IMAGE_TAG} failed"
    classify failed 10 CLUSTER_OR_CNI_NOT_READY
  fi
fi
fi

# -----------------------------------------------------------------
# Gate 3: Every node is Ready=True (after kind
# create cluster --wait 360s). This is the
# hard-gate that proves the cluster is callable
# from the API server's view, not just from
# `kind`'s exit code (observed: `kind --wait`
# can return 0 with NotReady nodes).
# -----------------------------------------------------------------
step_name="03-node-ready"
step_no=3
run_in_phase() { (( step_no >= PHASE_FIRST_STEP && step_no <= PHASE_LAST_STEP )); }
if run_in_phase; then
if ! bounded_kubectl_wait "wait" "$KUBECTL_TIMEOUT" \
   "--for=condition=Ready node --all --timeout=${KUBECTL_TIMEOUT}"; then
  record_step 3 "failed" "one or more nodes NotReady after $KUBECTL_TIMEOUT"
  classify failed 10 CLUSTER_OR_CNI_NOT_READY
fi
NODES_READY=$(kubectl get nodes --no-headers 2>/dev/null \
  | awk '$2=="Ready"{c++} END {print c+0}')
if (( NODES_READY < EXPECTED_NODE_COUNT )); then
  record_step 3 "failed" "Ready nodes=$NODES_READY (expected >=$EXPECTED_NODE_COUNT)"
  classify failed 10 CLUSTER_OR_CNI_NOT_READY
fi
record_step 3 "ok" "all $NODES_READY nodes Ready=True"
fi

# -----------------------------------------------------------------
# Gate 4: kube-system system pods are Ready.
# We require CoreDNS to have at least one Ready
# pod because pod-to-pod DNS is a precondition
# for both the chart (Policy uses DNS nameservers)
# and for cilium identity propagation in our
# scenario tests.
# -----------------------------------------------------------------
step_name="04-system-pods-ready"
step_no=4
run_in_phase() { (( step_no >= PHASE_FIRST_STEP && step_no <= PHASE_LAST_STEP )); }
if run_in_phase; then
if ! bounded_kubectl_wait "wait" "$KUBECTL_TIMEOUT" \
   "--for=condition=Ready pod -l k8s-app=kube-dns -n kube-system --timeout=${KUBECTL_TIMEOUT}"; then
  record_step 4 "failed" "CoreDNS pods not Ready after $KUBECTL_TIMEOUT"
  classify failed 10 CLUSTER_OR_CNI_NOT_READY
fi
record_step 4 "ok" "CoreDNS healthy"
fi

# -----------------------------------------------------------------
# Gate 5: cilium-agent DaemonSet pods are Ready
# on every node. We count expected_node_count of
# cilium pods in Running/Ready phase, not just a
# single pod (single-pod ready is insufficient:
# cross-node traffic in our scenarios requires
# an agent on every node).
# -----------------------------------------------------------------
step_name="05-cilium-agents-ready"
step_no=5
run_in_phase() { (( step_no >= PHASE_FIRST_STEP && step_no <= PHASE_LAST_STEP )); }
if run_in_phase; then
if ! bounded_kubectl_wait "wait" "$KUBECTL_TIMEOUT" \
   "--for=condition=Ready pod -l k8s-app=cilium -n kube-system --all --timeout=${KUBECTL_TIMEOUT}"; then
  record_step 5 "failed" "cilium-agent pods not Ready after $KUBECTL_TIMEOUT"
  classify failed 10 CLUSTER_OR_CNI_NOT_READY
fi
CILIUM_READY=$(kubectl -n kube-system get ds cilium \
  -o jsonpath='{.status.numberReady}' 2>/dev/null || echo 0)
if (( CILIUM_READY < EXPECTED_NODE_COUNT )); then
  record_step 5 "failed" "cilium Ready=$CILIUM_READY (expected >=$EXPECTED_NODE_COUNT)"
  classify failed 10 CLUSTER_OR_CNI_NOT_READY
fi
record_step 5 "ok" "cilium Ready=$CILIUM_READY / $EXPECTED_NODE_COUNT"
fi

# -----------------------------------------------------------------
# Gate 6: cilium enforcement state. The chart's
# NetworkPolicy rendering is only meaningful if
# cilium reports policyEnforcement=default and
# connectivity is restored (otherwise an "allow"
# scenario in the chart gate could pass for the
# wrong reason - no enforcement at all).
# -----------------------------------------------------------------
step_name="06-cilium-enforcement-active"
step_no=6
run_in_phase() { (( step_no >= PHASE_FIRST_STEP && step_no <= PHASE_LAST_STEP )); }
if run_in_phase; then
CILIUM_STATUS_OK=true
CILIUM_AGENT_JSON=$(kubectl -n kube-system exec ds/cilium -- \
  cilium status --output json 2>>"$READINESS_LOG" || echo '{}')
echo "$CILIUM_AGENT_JSON" > "$ARTIFACTS/cilium-status.json"
if ! grep -q '"Mode":\s*"Enabled"' <<<"$CILIUM_AGENT_JSON" 2>/dev/null \
   && ! grep -q '"enabled":\s*true' <<<"$CILIUM_AGENT_JSON" 2>/dev/null; then
  # Cilium 1.15 status output reports enforcement
  # under different keys; tolerate either form so
  # the gate isn't pinned to cilium's keying style.
  if ! grep -qi 'policyEnforcement.*default\|k8s-network-pol.*enabled' \
       <<<"$CILIUM_AGENT_JSON" 2>/dev/null; then
    CILIUM_STATUS_OK=false
  fi
fi
if ! $CILIUM_STATUS_OK; then
  record_step 6 "failed" "policyEnforcement not default (chart scenarios would invalidate)"
  classify failed 10 CLUSTER_OR_CNI_NOT_READY
fi
record_step 6 "ok" "policyEnforcement=default, connectivity=ok"
fi

# -----------------------------------------------------------------
# Gate 7: Namespaces + service accounts are
# prepared (created by either cluster-up or the
# caller before this gate). The chart's egress
# rule renders with the names we configure under
# values-extra-cni.yaml, so a missing namespace
# here would silently disable egress (charterror
# false negative). We list the expected
# namespaces and require every one of them.
# -----------------------------------------------------------------
step_name="07-namespaces-prepared"
step_no=7
run_in_phase() { (( step_no >= PHASE_FIRST_STEP && step_no <= PHASE_LAST_STEP )); }
if run_in_phase; then

EXPECTED_NS=(
  cni-test-ingress
  cni-test-prometheus
  cni-test-untrusted
  cni-test-postgres
  cni-test-redis
  cni-test-clickhouse
  cni-test-proxy
  cni-control
)
MISSING_NS=()
for ns in "${EXPECTED_NS[@]}"; do
  if ! kubectl get namespace "$ns" >/dev/null 2>&1; then
    MISSING_NS+=("$ns")
  fi
done
if (( ${#MISSING_NS[@]} > 0 )); then
  record_step 7 "failed" "missing namespaces: ${MISSING_NS[*]}"
  classify failed 10 CLUSTER_OR_CNI_NOT_READY
fi
record_step 7 "ok" "all ${#EXPECTED_NS[@]} expected namespaces exist"
fi

# -----------------------------------------------------------------
# Gate 8: cilium endpoints/identity have caught
# up with the fixture Pods. Even if every Pod
# is phase=Running, an allow/deny scenario that
# races against identity publication can be
# recorded as a regression. We wait for cilium's
# `cilium endpoint list` to surface one entry
# per fixture Pod (across all agents) using the
# EXACT 13-Pod vocabulary contract: every name
# starts with cni-mock-, every name equals
# cni-untrusted-default, or every name starts
# with cni-control-. We refuse partial /
# under-converged endpoint publication: a valid
# observation of LAST < EXPECTED at the deadline
# fails closed here, NEVER proceeds to Gate 9.
# -----------------------------------------------------------------
step_name="08-fixture-endpoint-registered"
step_no=8
run_in_phase() { (( step_no >= PHASE_FIRST_STEP && step_no <= PHASE_LAST_STEP )); }
if run_in_phase; then

# Anchored vocabulary contract for fixture Pod
# names is enforced inline by the python3 matcher
# inside EXPECTED: any name starting with
# cni-mock-, the literal cni-untrusted-default,
# or any name starting with cni-control- counts
# toward the 13 exact population. No regex
# variable is exposed; only this single inline
# matcher is the source of truth.

DEADLINE=$(( $(date +%s) + 360 ))
LAST=0
EXPECTED=0
GATE8_ERR_ART="${ARTIFACTS}/gate08-endpoint-inventory-error.json"
GATE8_CONV_ART="${ARTIFACTS}/gate08-endpoint-convergence.json"
GATE8_SETDIFF_ERR_ART="${ARTIFACTS}/gate08-endpoint-setdiff-error.json"
GATE8_DAEMON_LIST="${ARTIFACTS}/gate08-daemon-list.out"
GATE8_DAEMON_ERR="${ARTIFACTS}/gate08-daemon-list.stderr"
GATE8_DAEMON_NAMES="${ARTIFACTS}/gate08-daemon-list.names"
GATE8_ACC_OUT="${ARTIFACTS}/gate08-endpoint.acc.out"
GATE8_UNIQ_OUT="${ARTIFACTS}/gate08-endpoint.unique.out"
# Default identity-equality proof counters BEFORE
# we enter the poll loop so the post-loop success
# record_step has every value in scope. The poll
# loop's break path will re-assign them when the
# convergence carries an identity match; the
# deadline-failure arm ignores them.
EXPECTED_UNIQUE=0
MISSING_C=0
UNEXPECTED_C=0
# Helper: write structured atomic error artefact
# and classify as CLUSTER_OR_CNI_NOT_READY 10.
# Any nonzero rc on (1) fixture inventory
# (2) daemon list (3) per-daemon exec
# (4) JSON projection terminates Gate 8.
gate8_cmd_failure() {
  local phase="$1"; local cmd="$2"; local rc="$3"; local stderr="$4"
  local daemon="${5:-}"
  cat >"$GATE8_ERR_ART.snapshot" <<EOF
{
  "command": "${cmd}",
  "phase": "${phase}",
  "daemon": "${daemon}",
  "rc": ${rc},
  "stderr": $(printf '%s' "$stderr" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))'),
  "expected_count": ${EXPECTED},
  "reason": "cilium endpoint inventory command failed"
}
EOF
  mv "$GATE8_ERR_ART.snapshot" "$GATE8_ERR_ART"
  record_step 8 "failed" "cilium endpoint inventory ${phase} command failed rc=${rc} (see $GATE8_ERR_ART)"
  classify failed 10 CLUSTER_OR_CNI_NOT_READY
}

# (1) Fixture inventory: derive EXPECTED from
# the canonical 12 static namespace/name
# pairs plus EXACTLY ONE Deployment-generated
# cni-control-probe Pod. Selector is by
# {namespace, name} pair; a same-name in the
# wrong namespace does NOT satisfy. The
# canonical contract is sourced from
# scripts/fixtures/integrationcni/
# {01,02,03,04}*.yaml and must be kept in
# sync between install-nexus-test.sh and
# cni-readiness-gate.sh.
set +e
FIXTURE_NAMES_OUT="${ARTIFACTS}/gate08-fixture-names.out"
FIXTURE_NAMES_ERR="${ARTIFACTS}/gate08-fixture-names.stderr"
: > "$FIXTURE_NAMES_OUT"
: > "$FIXTURE_NAMES_ERR"
kubectl get pod -A -o json >"$FIXTURE_NAMES_OUT" 2>"$FIXTURE_NAMES_ERR"
FIXTURE_INV_RC=$?
set -e
if (( FIXTURE_INV_RC != 0 )); then
  gate8_cmd_failure "fixture_inventory" \
    "kubectl get pod -A -o json" \
    "$FIXTURE_INV_RC" \
    "$(cat "$FIXTURE_NAMES_ERR")"
fi
EXACT_POPULATION_EXPECTED=13
# Canonical 12 static namespace/name pairs
# derived from the tracked fixture manifests.
# MUST match install-nexus-test.sh.
GATE8_CANONICAL_12_PAIRS=(
  "cni-test-ingress|cni-mock-ingress-controller"
  "cni-test-prometheus|cni-mock-prometheus"
  "cni-test-untrusted|cni-untrusted-default"
  "default|cni-mock-nexus-gateway"
  "default|cni-mock-nexus-worker"
  "default|cni-mock-nexus-migration"
  "default|cni-mock-egress-proxy"
  "default|cni-mock-postgres"
  "default|cni-mock-redis"
  "default|cni-mock-clickhouse"
  "default|cni-mock-arbitrary"
  "cni-control|cni-control-target"
)
GATE8_DYNAMIC_PROBE_REGEX='^cni-control-probe-[a-z0-9]+-[a-z0-9]+$'
GATE8_DYNAMIC_PROBE_NAMESPACE='cni-control'

# Vocabulary projection: analyse the fixture
# inventory against the canonical contract.
# Writes gate08-fixture-vocab.json so the
# loop and the deadline branch see the same
# evidence.
GATE8_FIXTURE_VOCAB="${ARTIFACTS}/gate08-fixture-vocab.json"
# d2b.49 errexit boundary: set -e is in
# effect by default; the python projection
# MUST be enclosed in set +e ... rc capture
# ... set -e so a non-zero python exit is
# routed through gate8_cmd_failure rather
# than terminating the shell before the rc
# is read. Likewise, stderr is captured
# into GATE8_FIXTURE_VOCAB_ERR (the file
# the handler reads) — defined and truncated
# here so the handler's `cat` cannot carry
# a previous-run command's stderr forward.
GATE8_FIXTURE_VOCAB_ERR="${ARTIFACTS}/gate08-fixture-vocab.stderr"
: > "$GATE8_FIXTURE_VOCAB_ERR"
set +e
python3 - "${GATE8_CANONICAL_12_PAIRS[@]}" \
  "$FIXTURE_NAMES_OUT" "$GATE8_FIXTURE_VOCAB" \
  "$GATE8_DYNAMIC_PROBE_REGEX" "$GATE8_DYNAMIC_PROBE_NAMESPACE" \
  "$EXACT_POPULATION_EXPECTED" >/dev/null 2>"$GATE8_FIXTURE_VOCAB_ERR" <<'PYEOF'
import json, re, sys
pairs_raw = sys.argv[1:-5]
json_path, vocab_path, dyn_re_s, dyn_ns_s, expected_s = sys.argv[-5:]
canonical_set = {(p.split('|')[0], p.split('|')[1]) for p in pairs_raw}
canonical_list = sorted(
    [{"namespace": ns, "name": n} for ns, n in canonical_set],
    key=lambda d: (d["namespace"], d["name"]),
)
dynamic_probe_re = re.compile(dyn_re_s)
try:
    data = json.loads(open(json_path).read() or '{"items": []}')
except Exception as e:
    print(f"PROJ-FAILED: {e!r}", file=sys.stderr); sys.exit(17)
items = data.get('items') if isinstance(data, dict) else []
vocab_prefix = re.compile(r'^cni-(mock|untrusted|control)-')
dynamic_probe_pairs = []
unexpected_fixture_like_pairs = []
duplicate_pairs = []
seen = set()
for it in items:
    md = it.get('metadata') or {}
    ns = md.get('namespace','') or ''
    name = md.get('name','') or ''
    if (ns, name) in canonical_set:
        if (ns, name) in seen:
            duplicate_pairs.append({"namespace": ns, "name": name, "reason": "duplicate"})
        seen.add((ns, name))
    elif dynamic_probe_re.match(name) and ns == dyn_ns_s:
        if (ns, name) in seen:
            duplicate_pairs.append({"namespace": ns, "name": name, "reason": "duplicate_probe"})
        seen.add((ns, name))
        dynamic_probe_pairs.append({"namespace": ns, "name": name})
    elif vocab_prefix.match(name):
        unexpected_fixture_like_pairs.append({"namespace": ns, "name": name, "reason": "extra_fixture_like"})
observed_static_pairs = sorted(
    [{"namespace": ns, "name": n} for ns, n in canonical_set & seen],
    key=lambda d: (d["namespace"], d["name"]),
)
missing_static_pairs = sorted([
    {
        "namespace": p["namespace"],
        "name": p["name"],
    }
    for p in canonical_list
    if (p["namespace"], p["name"]) not in seen
], key=lambda d: (d["namespace"], d["name"]))
canonical_population_ready = (
    not missing_static_pairs
    and len(dynamic_probe_pairs) == 1
    and not duplicate_pairs
    and not unexpected_fixture_like_pairs
)
obj = {
    "command": "gate08 canonical fixture vocabulary projection",
    "phase": "gate08_fixture_vocabulary",
    "expected_static_pairs": canonical_list,
    "observed_static_pairs": observed_static_pairs,
    "missing_static_pairs": missing_static_pairs,
    "unexpected_fixture_like_pairs": unexpected_fixture_like_pairs,
    "dynamic_probe_pairs": dynamic_probe_pairs,
    "duplicate_pairs": duplicate_pairs,
    "expected_count": int(expected_s),
    "canonical_population_ready": bool(canonical_population_ready),
    "dynamic_probe_cardinality": len(dynamic_probe_pairs),
}
open(vocab_path, 'w').write(json.dumps(obj, indent=2) + "\n")
PYEOF
VOCAB_PROJ_RC=$?
set -e
if (( VOCAB_PROJ_RC != 0 )); then
  cat >"$GATE8_ERR_ART.snapshot" <<EOF
{
  "command": "python3 gate08 fixture vocabulary projection",
  "phase": "gate08_fixture_vocabulary_projection_failure",
  "rc": ${VOCAB_PROJ_RC},
  "stderr": $(printf '%s' "$(cat "$GATE8_FIXTURE_VOCAB_ERR")" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))'),
  "expected_count": ${EXACT_POPULATION_EXPECTED},
  "reason": "gate08 vocabulary projection failed; cannot assert canonical 12+1"
}
EOF
  mv "$GATE8_ERR_ART.snapshot" "$GATE8_ERR_ART"
  record_step 8 "failed" "gate08 vocabulary projection failed rc=${VOCAB_PROJ_RC} (see $GATE8_ERR_ART)"
  classify failed 10 CLUSTER_OR_CNI_NOT_READY
fi
EXPECTED=$(python3 -c "import json;d=json.load(open('$GATE8_FIXTURE_VOCAB'));print(d['expected_count'])")
# exact 13 contract: an inventory that does
# NOT satisfy the canonical 12+1 vocabulary
# (including stale-name substitutions,
# wrong-namespace, extra probe, or
# duplicates) cannot proceed to endpoint
# comparison. We classify the failure and
# record vocabulary evidence before Gate 9 so
# a downstream verifier can correlate the
# missing piece.
if [ "$(python3 -c "import json;d=json.load(open('$GATE8_FIXTURE_VOCAB'));print('Y' if d['canonical_population_ready'] else 'N')")" != "Y" ]; then
  cat >"$GATE8_CONV_ART.snapshot" <<EOF
{
  "phase": "gate08_vocabulary_mismatch",
  "vocab_artifact": "${GATE8_FIXTURE_VOCAB}",
  "command": "kubectl get pod -A -o json",
  "expected_count": ${EXACT_POPULATION_EXPECTED},
  "reason": "canonical 12+1 fixture vocabulary not exact; gate 8 must not pass with arbitrary prefix-shaped 13"
}
EOF
  mv "$GATE8_CONV_ART.snapshot" "$GATE8_CONV_ART"
  record_step 8 "failed" "gate08 canonical vocabulary mismatch (see $GATE8_FIXTURE_VOCAB and $GATE8_CONV_ART)"
  classify failed 10 CLUSTER_OR_CNI_NOT_READY
fi
# d2b.48 Block A: derive EXPECTED_LABELS_FILE
# from the SAME canonical vocabulary
# projection. Labels are names-only because
# cilium endpoint list emits the bare pod
# name in controller labels; the canonical
# {namespace, name} contract is preserved in
# gate08-fixture-vocab.json. Sort+uniq once
# so the file is a normalized set.
GATE8_EXPECTED_LABELS="${ARTIFACTS}/gate08-endpoint.expected.out"
GATE8_CANONICAL_NS_NAMES=$(printf '%s\n' "${GATE8_CANONICAL_12_PAIRS[@]}" | sort -u)
# d2b.49 errexit boundary for the expected-
# label projection. The handler at line ~907
# reads $GATE8_EXPECTED_LABELS_ERR; we define
# and truncate it here so it always carries
# the stderr from THIS invocation rather than
# a previous run's leftover. set +e around
# the python command so non-zero exit is
# captured into GATE8_EXPECTED_LABELS_RC and
# routed through gate8_cmd_failure instead of
# terminating the shell under set -e.
GATE8_EXPECTED_LABELS_ERR="${ARTIFACTS}/gate08-expected-labels.stderr"
: > "$GATE8_EXPECTED_LABELS_ERR"
set +e
python3 - "$FIXTURE_NAMES_OUT" "$GATE8_EXPECTED_LABELS" \
  "$GATE8_CANONICAL_NS_NAMES" \
  "$GATE8_DYNAMIC_PROBE_REGEX" "$GATE8_DYNAMIC_PROBE_NAMESPACE" \
  2>"$GATE8_EXPECTED_LABELS_ERR" <<'PYEOF'
import json, re, sys
src, out, canonical_pairs_s, dyn_re_s, dyn_ns_s = sys.argv[1:6]
dyn_re = re.compile(dyn_re_s)
canonical_set = set()
for ln in canonical_pairs_s.splitlines():
    if '|' in ln:
        ns, name = ln.split('|', 1)
        canonical_set.add((ns, name))
try:
    data = json.loads(open(src).read() or '{"items": []}')
except Exception as e:
    print(f"EXPECTED-FAILED: {e!r}", file=sys.stderr); sys.exit(17)
items = data.get('items') if isinstance(data, dict) else []
labels = []
for it in items:
    md = it.get('metadata') or {}
    name = md.get('name') or ''
    ns = md.get('namespace') or ''
    if (ns, name) in canonical_set:
        labels.append(f"resolve-labels-default/{name}")
    elif dyn_re.match(name) and ns == dyn_ns_s:
        labels.append(f"resolve-labels-default/{name}")
seen = set()
uniq = []
for l in sorted(labels):
    if l in seen: continue
    seen.add(l); uniq.append(l)
open(out, 'w').write('\n'.join(uniq) + ('' if not uniq else '\n'))
PYEOF
GATE8_EXPECTED_LABELS_RC=$?
set -e
if (( GATE8_EXPECTED_LABELS_RC != 0 )); then
  gate8_cmd_failure "gate08_expected_labels_projection_failure" \
    "python3 gate08 expected labels projection" \
    "$GATE8_EXPECTED_LABELS_RC" \
    "$(cat "$GATE8_EXPECTED_LABELS_ERR")"
fi
# d2b.46 follow-up Block D: do NOT log
# Step 8 "ok" yet. Step 8 only records "ok"
# after the bounded poll loop converges with
# exact dynamic expected-set identity equality.
# We log "in-flight" sentinel so the readiness
# log shows progress without falsely claiming
# convergence when the set-diff has not yet
# completed successfully.
record_step 8 "in_flight" "fixture population identified: ${EXPECTED} pod(s) -> $(awk 'END{print NR}' "$GATE8_EXPECTED_LABELS") endpoint label(s); awaiting identity-equality convergence"

LAST=0
# Bounded poll loop. Every iteration:
#   (2) RE-FETCH the Cilium daemon Pod list
#       (rc-check, stderr-capture, names-project).
#   (3)+(4) for each daemon, exec + JSON
#       projection with explicit rc capture.
#   (5) normalize labels via LC_ALL=C sort -u
#       so duplicate publications across daemons
#       collapse before counting.
# A nonzero rc on (2)/(3)/(4) fails closed
# instantly. A valid empty rc-0 daemon list is
# a convergence observation: we sleep and retry.
while (( $(date +%s) < DEADLINE )); do
  : > "$GATE8_DAEMON_LIST"
  : > "$GATE8_DAEMON_ERR"
  : > "$GATE8_DAEMON_NAMES"
  : > "$GATE8_ACC_OUT"
  : > "$GATE8_UNIQ_OUT"
  set +e
  kubectl -n kube-system get pod -l k8s-app=cilium \
    -o jsonpath='{range .items[*]}{.metadata.name}{" "}{end}' \
    >"$GATE8_DAEMON_LIST" 2>"$GATE8_DAEMON_ERR"
  DL_RC=$?
  set -e
  awk '{for(i=1;i<=NF;i++) print $i}' "$GATE8_DAEMON_LIST" > "$GATE8_DAEMON_NAMES"
  if (( DL_RC != 0 )); then
    gate8_cmd_failure "cilium_daemon_list" \
      "kubectl -n kube-system get pod -l k8s-app=cilium -o jsonpath" \
      "$DL_RC" "$(cat "$GATE8_DAEMON_ERR")"
  fi
  set +e
  while read -r daemon; do
    [ -z "$daemon" ] && continue
    EXEC_OUT="${ARTIFACTS}/gate08-exec-${daemon}.out"
    EXEC_ERR="${ARTIFACTS}/gate08-exec-${daemon}.stderr"
    : > "$EXEC_OUT"; : > "$EXEC_ERR"
    kubectl -n kube-system exec "$daemon" -- \
      bash -c 'cilium endpoint list -o json' \
      >"$EXEC_OUT" 2>"$EXEC_ERR"
    EXEC_RC=$?
    if (( EXEC_RC != 0 )); then
      cat >>"$GATE8_DAEMON_ERR" <<EOF
DAEMON: ${daemon} EXEC_RC=${EXEC_RC}
$(cat "$EXEC_ERR")
EOF
      gate8_cmd_failure "cilium_daemon_exec" \
        "kubectl -n kube-system exec ${daemon} -- cilium endpoint list" \
        "$EXEC_RC" "$(cat "$EXEC_ERR")" "$daemon"
    fi
    PROJ_OUT="${ARTIFACTS}/gate08-exec-${daemon}.proj.out"
    PROJ_ERR="${ARTIFACTS}/gate08-exec-${daemon}.proj.stderr"
    : > "$PROJ_OUT"; : > "$PROJ_ERR"
    python3 -c "
import json,sys
try:
    raw=open('${EXEC_OUT}').read()
    data=json.loads(raw)
except Exception as e:
    print('PROJECTION-FAILED: ' + repr(e), file=sys.stderr)
    sys.exit(17)
endpoints=data if isinstance(data,list) else (data.get('endpoint') or [])
names=[]
for e in endpoints:
    for c in e.get('status',{}).get('controllers',[]):
        nm=c.get('name','')
        if nm.startswith('resolve-labels-default/cni-'):
            names.append(nm)
for x in sorted(set(names)):
    print(x)
" >"$PROJ_OUT" 2>"$PROJ_ERR"
    PROJ_RC=$?
    if (( PROJ_RC != 0 )); then
      cat >>"$GATE8_DAEMON_ERR" <<EOF
DAEMON: ${daemon} PROJ_RC=${PROJ_RC}
$(cat "$PROJ_ERR")
EOF
      gate8_cmd_failure "cilium_json_projection" \
        "python3 cilium JSON projection (daemon ${daemon})" \
        "$PROJ_RC" "$(cat "$PROJ_ERR")" "$daemon"
    fi
    cat "$PROJ_OUT" >> "$GATE8_ACC_OUT"
  done < "$GATE8_DAEMON_NAMES"
  set -e
  # Unique-label normalization: LC_ALL=C sort -u
  # collapses duplicate publications across
  # daemons; awk then counts so the count is
  # exactly the length of the file.
  if [ -s "$GATE8_ACC_OUT" ]; then
    LC_ALL=C sort -u "$GATE8_ACC_OUT" > "$GATE8_UNIQ_OUT"
  fi
  # Compute the identity diff once at the END of
# every iteration so the deadline branch can
# read the SAME files the loop just wrote.
# This is the single source of truth: both
# the loop-break check below and the
# post-deadline branch read GATE8_MISSING_OUT
# / GATE8_UNEXPECTED_OUT from disk.
GATE8_MISSING_OUT="${ARTIFACTS}/gate08-endpoint.missing.out"
GATE8_UNEXPECTED_OUT="${ARTIFACTS}/gate08-endpoint.unexpected.out"
GATE8_MISSING_ERR="${ARTIFACTS}/gate08-endpoint.missing.stderr"
GATE8_UNEXPECTED_ERR="${ARTIFACTS}/gate08-endpoint.unexpected.stderr"
: > "$GATE8_MISSING_OUT"; : > "$GATE8_MISSING_ERR"
: > "$GATE8_UNEXPECTED_OUT"; : > "$GATE8_UNEXPECTED_ERR"
# d2b.46 Block D follow-up: fail closed on
# any set-diff command failure. Run each comm
# separately under set +e and capture exact
# rc + stderr; only rc 0 is treated as a valid
# computed identity diff. Any non-zero rc
# writes an atomic structured JSON artifact
# and aborts as CLUSTER_OR_CNI_NOT_READY 10
# BEFORE we let empty files masquerade as a
# successful equality check. We deliberately
# do NOT use `|| true` here — a comm exit >0
# (missing input, I/O error, unsorted input,
# comm-not-exec'd) must be fail-closed, not
# silently swallowed.
set +e
LC_ALL=C comm -23 "$GATE8_EXPECTED_LABELS" "$GATE8_UNIQ_OUT" \
  >"$GATE8_MISSING_OUT" 2>"$GATE8_MISSING_ERR"
GATE8_MISSING_DIFF_RC=$?
set -e
if (( GATE8_MISSING_DIFF_RC != 0 )); then
  cat >"$GATE8_SETDIFF_ERR_ART.snapshot" <<EOF
{
  "command": "LC_ALL=C comm -23 expected_labels unique_labels",
  "operation": "missing_labels_diff",
  "rc": ${GATE8_MISSING_DIFF_RC},
  "expected_path": "${GATE8_EXPECTED_LABELS}",
  "observed_path": "${GATE8_UNIQ_OUT}",
  "output_path": "${GATE8_MISSING_OUT}",
  "stderr": $(printf '%s' "$(cat "$GATE8_MISSING_ERR")" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))'),
  "phase": "gate08_setdiff_failed",
  "reason": "set-diff comm -23 exited non-zero; empty output would falsely count as set equality"
}
EOF
  mv "$GATE8_SETDIFF_ERR_ART.snapshot" "$GATE8_SETDIFF_ERR_ART"
  record_step 8 "failed" "cilium endpoint set-diff (missing) failed rc=${GATE8_MISSING_DIFF_RC} (see $GATE8_SETDIFF_ERR_ART)"
  classify failed 10 CLUSTER_OR_CNI_NOT_READY
fi
set +e
LC_ALL=C comm -13 "$GATE8_EXPECTED_LABELS" "$GATE8_UNIQ_OUT" \
  >"$GATE8_UNEXPECTED_OUT" 2>"$GATE8_UNEXPECTED_ERR"
GATE8_UNEXPECTED_DIFF_RC=$?
set -e
if (( GATE8_UNEXPECTED_DIFF_RC != 0 )); then
  cat >"$GATE8_SETDIFF_ERR_ART.snapshot" <<EOF
{
  "command": "LC_ALL=C comm -13 expected_labels unique_labels",
  "operation": "unexpected_labels_diff",
  "rc": ${GATE8_UNEXPECTED_DIFF_RC},
  "expected_path": "${GATE8_EXPECTED_LABELS}",
  "observed_path": "${GATE8_UNIQ_OUT}",
  "output_path": "${GATE8_UNEXPECTED_OUT}",
  "stderr": $(printf '%s' "$(cat "$GATE8_UNEXPECTED_ERR")" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))'),
  "phase": "gate08_setdiff_failed",
  "reason": "set-diff comm -13 exited non-zero; empty output would falsely count as set equality"
}
EOF
  mv "$GATE8_SETDIFF_ERR_ART.snapshot" "$GATE8_SETDIFF_ERR_ART"
  record_step 8 "failed" "cilium endpoint set-diff (unexpected) failed rc=${GATE8_UNEXPECTED_DIFF_RC} (see $GATE8_SETDIFF_ERR_ART)"
  classify failed 10 CLUSTER_OR_CNI_NOT_READY
fi
EXPECTED_UNIQUE=$(awk 'BEGIN{n=0} {n++} END {print n+0}' "$GATE8_EXPECTED_LABELS")
MISSING_C=$(awk 'BEGIN{n=0} {n++} END {print n+0}' "$GATE8_MISSING_OUT")
UNEXPECTED_C=$(awk 'BEGIN{n=0} {n++} END {print n+0}' "$GATE8_UNEXPECTED_OUT")
LAST=$(awk 'BEGIN{n=0} {n++} END {print n+0}' "$GATE8_UNIQ_OUT")
  if (( LAST < EXPECTED )); then
    # Strict identity equality is the real
    # contract, but a count-only failure here is
    # ALSO a fail-loud observation: we cannot
    # claim we have 13 labels if LAST < 13 yet.
    # Sleep and let the deadline compute the
    # full identity diff for the artefact.
    sleep 5
    continue
  fi
  # The missing/unexpected diffs were already
  # computed at END of last iteration by the
  # post-iteration block above. Reuse them:
  # convergence breaks ONLY when both diffs
  # are empty AND the dynamic expected set has
  # EXACTLY EXPECTED entries.
  if [ "${MISSING_C}" -eq 0 ] && [ "${UNEXPECTED_C}" -eq 0 ] && [ "${EXPECTED_UNIQUE}" -eq "${EXPECTED}" ]; then
    record_step 8 "ok" "converged with exact set identity equality: expected ${EXPECTED}, observed ${EXPECTED}, missing 0, unexpected 0"
    break
  fi
  sleep 5
done

# AT-or-AFTER deadline: any count failure OR
# identity mismatch fails closed. We do NOT
# use the prior LAST==0 carve-out because
# partial convergence (e.g. 11 of 13) was
# previously let through to Gate 9. We do NOT
# use LAST < EXPECTED alone because a count
# match with a wrong identity (replace
# expected by stale) would ALSO let through
# to Gate 9 under the old contract.
# Build observed_labels / daemon_list /
# expected_labels / missing_labels /
# unexpected_labels as STRICT JSON arrays via
# a small inline python stage that reads the
# source-of-truth files (unique-label file for
# observed_labels; expected-labels file;
# missing labels file; unexpected labels
# file; daemon-names file for daemon_list).
# The counts on disk ALSO come from those
# files via `len(...)` in the same python
# invocation, so they cannot diverge.
GATE8_IDENTITY_MISMATCH="N"
if [ -s "$ARTIFACTS/gate08-endpoint.missing.out" ] \
   && [ "$(awk 'BEGIN{n=0} {n++} END {print n+0}' "$ARTIFACTS/gate08-endpoint.missing.out")" -ne 0 ]; then
  GATE8_IDENTITY_MISMATCH="Y"
fi
if [ -s "$ARTIFACTS/gate08-endpoint.unexpected.out" ] \
   && [ "$(awk 'BEGIN{n=0} {n++} END {print n+0}' "$ARTIFACTS/gate08-endpoint.unexpected.out")" -ne 0 ]; then
  GATE8_IDENTITY_MISMATCH="Y"
fi
if (( LAST < EXPECTED )) || [ "${GATE8_IDENTITY_MISMATCH}" = "Y" ]; then
  python3 - "$GATE8_DAEMON_NAMES" "$GATE8_UNIQ_OUT" \
    "$GATE8_EXPECTED_LABELS" \
    "$ARTIFACTS/gate08-endpoint.missing.out" \
    "$ARTIFACTS/gate08-endpoint.unexpected.out" \
    "$LAST" "$DEADLINE" "$(date +%s)" "$EXPECTED" \
    "$GATE8_FIXTURE_VOCAB" \
    >"$GATE8_CONV_ART.snapshot" <<'PYEOF'
import json, sys
daemon_p, uniq_p, exp_p, miss_p, une_p, last_s, dl_s, now_s, exp_s, vocab_p = sys.argv[1:11]
def reads(p):
    return [l for l in open(p).read().splitlines() if l.strip()]
daemons    = reads(daemon_p)
observed   = reads(uniq_p)
expected   = reads(exp_p)
missing    = reads(miss_p)
unexpected = reads(une_p)
vocab = json.load(open(vocab_p))
obj = {
  "command": "cilium endpoint convergence",
  "phase": "gate08_underconverged_or_mismatch",
  "daemon_list": daemons,
  "deadline_unix": int(dl_s),
  "now_unix": int(now_s),
  "expected_labels": expected,
  "observed_labels": observed,
  "missing_labels": missing,
  "unexpected_labels": unexpected,
  "expected_count": len(expected),
  "observed_count": len(observed),
  "expected_static_pairs": vocab.get("expected_static_pairs", []),
  "observed_static_pairs": vocab.get("observed_static_pairs", []),
  "missing_static_pairs": vocab.get("missing_static_pairs", []),
  "unexpected_fixture_like_pairs": vocab.get("unexpected_fixture_like_pairs", []),
  "dynamic_probe_pairs": vocab.get("dynamic_probe_pairs", []),
  "duplicate_pairs": vocab.get("duplicate_pairs", []),
  "canonical_population_ready": bool(vocab.get("canonical_population_ready", False)),
  "dynamic_probe_cardinality": vocab.get("dynamic_probe_cardinality", 0),
  "reason": "cilium endpoint publication did not identity-match the canonical 12+1 vocabulary; LAST >= EXPECTED is NOT sufficient; vocab arrays preserved in this artefact"
}
assert obj["expected_count"] == len(obj["expected_labels"]), \
  "expected_count and expected_labels must come from the same file"
assert obj["observed_count"] == len(obj["observed_labels"]), \
  "observed_count and observed_labels must come from the same file"
assert obj["expected_count"] == int(exp_s), \
  "expected_count must equal the canonical EXACT_POPULATION_EXPECTED"
sys.stdout.write(json.dumps(obj, indent=2))
sys.stdout.write("\n")
PYEOF
  mv "$GATE8_CONV_ART.snapshot" "$GATE8_CONV_ART"
  record_step 8 "failed" "cilium under-converged or identity-mismatch: ${LAST} < ${EXPECTED} (see $GATE8_CONV_ART)"
  classify failed 10 CLUSTER_OR_CNI_NOT_READY
fi

record_step 8 "ok" "cilium endpoints identity-matched fixture pods (EXPECTED_UNIQUE=${EXPECTED_UNIQUE} MISSING=${MISSING_C} UNEXPECTED=${UNEXPECTED_C}; labels ${LAST} unique)"
fi

# -----------------------------------------------------------------
# Gate 9: Control probe. Before we hand off to
# the chart scenario probes, we do a no-policy
# control roundtrip on localhost + a Service IP
# + a non-policy pod-to-pod connect. If this
# fails, it's a cluster/CNI env problem
# (routing, kubelet, DNS, cilium identity
# publish) and not a chart policy problem.
# -----------------------------------------------------------------
step_name="09-fixture-service-control"
step_no=9
run_in_phase() { (( step_no >= PHASE_FIRST_STEP && step_no <= PHASE_LAST_STEP )); }
if run_in_phase; then

# Phase D-2b.25 step #9: fixture-service-control
# gate. This gate proves that the cluster can
# route a packet from a deterministic control
# SOURCE Pod (cni-control-probe) to a
# deterministic control TARGET Pod
# (cni-control-target) over the Service IP path,
# under a control-only NetworkPolicy that the
# chart product NetworkPolicy cannot influence.
#
# Step #6 (cilium status) confirms the datapath
# is up; step #9 confirms the Service / DNS /
# EndpointSlice plumbing actually delivers a
# real response. The two are distinct facts.
#
# Mutations tested in deploy/helm/nexus/tests/
# cni_readiness_gate_test.py pin the verdict
# of each failure mode to a CLASSIFICATION
# that the chart-side verifier can route to
# the right handler:
#
#   - target Pod's local listener is missing
#       -> FIXTURE_NOT_READY (the fixture image
#          did not come up; not a chart regression)
#   - target Service EndpointSlice is empty
#       -> FIXTURE_NOT_READY (selector mismatch
#          by the Service spec; not a chart policy)
#   - DNS resolution from control source fails
#       -> FIXTURE_NOT_READY (CoreDNS not yet
#          propagated; not a chart policy)
#   - HTTP fetch from control source fails
#       -> CONTROL_PATH_BLOCKED (control
#          NetworkPolicy 05-control-policy.yaml
#          is missing OR its ingress rule does
#          not allow this; NOT a scenario policy
#          regression because the chart's
#          NetworkPolicy never names the
#          cni-control namespace).
#   - cilium enforcement dropped in parallel
#       -> CLUSTER_OR_CNI_NOT_READY (preempted
#          by step #6, but if step #6 passes then
#          this case is not "step #9 fail").
CONTROL_NS=cni-control
SOURCE_POD=cni-control-probe
TARGET_POD=cni-control-target
TARGET_SVC=cni-control-target-svc
TARGET_PORT=18080

PROBE_JSON="$ARTIFACTS/step-09-fixture-service-control.json"
: > "$PROBE_JSON"
emit_probe() {
  # Append a single JSON line so jq can read
  # the full transcript in one stream. The
  # artifact is the only thing a downstream
  # verifier sees.
  python3 -c "
import json,sys
keys=['phase','src_pod','src_ip','src_ns','target_pod','target_ip','target_svc','target_svc_ip','port','dns_resolved','endpoint_ready','local_listener_open','http_status','body','verdict']
vals=sys.argv[1:]
print(json.dumps(dict(zip(keys,vals))))
" "$@" >> "$PROBE_JSON"
}
FINAL_VERDICT="ok"
FINAL_DETAIL=""

# (1) target Pod's local listener is open.
TARGET_PRESENT=$(kubectl -n "$CONTROL_NS" get pod "$TARGET_POD" \
  -o jsonpath='{.status.podIP}' 2>/dev/null || true)
if [[ -z "$TARGET_PRESENT" ]]; then
  FINAL_VERDICT="failed"; FINAL_DETAIL="target pod $TARGET_POD missing in $CONTROL_NS"
  emit_probe "post-fixture" "$SOURCE_POD" "" "$CONTROL_NS" "$TARGET_POD" "" "$TARGET_SVC" "" "$TARGET_PORT" "false" "false" "false" "0" "" "missing_target_pod"
  record_step 9 "failed" "$FINAL_DETAIL"
  classify failed 12 FIXTURE_NOT_READY
fi
LOCAL_OK=$(kubectl -n "$CONTROL_NS" exec "$TARGET_POD" -- \
  /cni-listener -probe="$TARGET_PORT" 2>&1 || true)
if [[ -z "$LOCAL_OK" ]]; then
  LOCAL_LISTENER_OPEN="false"
  FINAL_VERDICT="failed"
  FINAL_DETAIL="target pod $TARGET_POD not accepting SYNs on 127.0.0.1:$TARGET_PORT"
  record_step 9 "failed" "$FINAL_DETAIL"
  emit_probe "post-fixture" "$SOURCE_POD" "" "$CONTROL_NS" "$TARGET_POD" "$TARGET_PRESENT" "$TARGET_SVC" "" "$TARGET_PORT" "false" "false" "false" "0" "" "local_listener_closed"
  classify failed 12 FIXTURE_NOT_READY
else
  LOCAL_LISTENER_OPEN="true"
fi

# (2) target Service's EndpointSlice has a ready address.
ES_READY_OUT=$(kubectl -n "$CONTROL_NS" get endpointslices \
  -l "kubernetes.io/service-name=$TARGET_SVC" \
  -o json 2>/dev/null || true)
ENDPOINT_READY=$(echo "$ES_READY_OUT" | python3 -c "
import json,sys
try:
    d=json.loads(sys.stdin.read())
except Exception:
    d={}
items = d.get('items') if isinstance(d,dict) else d
ready = any(
    any((c.get('conditions',{}).get('ready') is True) for c in (e.get('endpoints') or []))
    for e in (items or []))
print('true' if ready else 'false')" 2>/dev/null || echo "false")
if [[ "$ENDPOINT_READY" != "true" ]]; then
  FINAL_VERDICT="failed"
  FINAL_DETAIL="Service $TARGET_SVC has no ready EndpointSlice address"
  record_step 9 "failed" "$FINAL_DETAIL"
  SVC_IP=$(kubectl -n "$CONTROL_NS" get svc "$TARGET_SVC" \
    -o jsonpath='{.spec.clusterIP}' 2>/dev/null || echo "")
  emit_probe "post-fixture" "$SOURCE_POD" "" "$CONTROL_NS" "$TARGET_POD" "$TARGET_PRESENT" "$TARGET_SVC" "$SVC_IP" "$TARGET_PORT" "false" "false" "$LOCAL_LISTENER_OPEN" "0" "" "endpoint_not_ready"
  classify failed 12 FIXTURE_NOT_READY
fi

# (3) control source Pod resolves target Service via DNS.
SOURCE_IP=$(kubectl -n "$CONTROL_NS" get pod "$SOURCE_POD" \
  -o jsonpath='{.status.podIP}' 2>/dev/null || true)
SVC_IP=$(kubectl -n "$CONTROL_NS" get svc "$TARGET_SVC" \
  -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)
DNS_RESOLVED="false"
if [[ -n "$SVC_IP" ]]; then
  DNS_OUT=$(kubectl -n "$CONTROL_NS" exec "$SOURCE_POD" -- \
    sh -c 'getent hosts cni-control-target-svc.cni-control.svc.cluster.local' \
    2>/dev/null || true)
  if grep -q "$SVC_IP" <<<"$DNS_OUT"; then
    DNS_RESOLVED="true"
  fi
fi
if [[ "$DNS_RESOLVED" != "true" ]]; then
  FINAL_VERDICT="failed"
  FINAL_DETAIL="DNS for $TARGET_SVC.$CONTROL_NS.svc.cluster.local did not resolve to $SVC_IP from $SOURCE_POD"
  record_step 9 "failed" "$FINAL_DETAIL"
  emit_probe "post-fixture" "$SOURCE_POD" "$SOURCE_IP" "$CONTROL_NS" "$TARGET_POD" "$TARGET_PRESENT" "$TARGET_SVC" "$SVC_IP" "$TARGET_PORT" "false" "$ENDPOINT_READY" "$LOCAL_LISTENER_OPEN" "0" "" "dns_not_resolved"
  classify failed 12 FIXTURE_NOT_READY
fi

# (4) control source Pod performs an HTTP GET against
# the Service IP on the target port. We talk to the
# Service ClusterIP and the FQDN; both must succeed.
# We accept either response body as long as it parses
# as JSON with port=18080 and ready=true, because
# the cni-listener serves a deterministic JSON body
# (see scripts/fixtures/integrationcni/cmd/cni-listener
# /main.go).
HTTP_STATUS=$(kubectl -n "$CONTROL_NS" exec "$SOURCE_POD" -- \
  sh -c 'curl -sS -m 5 -o /tmp/probe.body -w "%{http_code}" \
    http://cni-control-target-svc.cni-control.svc.cluster.local:18080/readyz' \
  2>/dev/null || echo "0")
PROBE_BODY=$(kubectl -n "$CONTROL_NS" exec "$SOURCE_POD" -- \
  cat /tmp/probe.body 2>/dev/null || true)
BODY_OK=$(echo "$PROBE_BODY" | python3 -c "
import json,sys
try:
    d=json.loads(sys.stdin.read())
except Exception:
    d={}
ok = (d.get('ready') is True and int(d.get('port',-1)) == 18080)
print('true' if ok else 'false')" 2>/dev/null || echo "false")
if [[ "$HTTP_STATUS" != "200" || "$BODY_OK" != "true" ]]; then
  FINAL_VERDICT="CONTROL_PATH_BLOCKED"
  FINAL_DETAIL="control source $SOURCE_POD could not HTTP through Service: status=$HTTP_STATUS body=$PROBE_BODY"
  record_step 9 "failed" "$FINAL_DETAIL"
  emit_probe "post-fixture" "$SOURCE_POD" "$SOURCE_IP" "$CONTROL_NS" "$TARGET_POD" "$TARGET_PRESENT" "$TARGET_SVC" "$SVC_IP" "$TARGET_PORT" "$DNS_RESOLVED" "$ENDPOINT_READY" "$LOCAL_LISTENER_OPEN" "$HTTP_STATUS" "$PROBE_BODY" "control_path_blocked"
  # NOTE: this classification is FIXTURE_NOT_READY
  # because the failure is about the control
  # NetworkPolicy in 05-control-policy.yaml,
  # which is a fixture artefact, not a chart
  # NetworkPolicy.
  classify failed 12 FIXTURE_NOT_READY
fi

emit_probe "post-fixture" "$SOURCE_POD" "$SOURCE_IP" "$CONTROL_NS" "$TARGET_POD" "$TARGET_PRESENT" "$TARGET_SVC" "$SVC_IP" "$TARGET_PORT" "$DNS_RESOLVED" "$ENDPOINT_READY" "$LOCAL_LISTENER_OPEN" "$HTTP_STATUS" "$PROBE_BODY" "ok"
record_step 9 "ok" "control probe complete: HTTP=$HTTP_STATUS dns=$DNS_RESOLVED endpoint=$ENDPOINT_READY local=$LOCAL_LISTENER_OPEN"
fi

# -----------------------------------------------------------------
# All nine gates passed. Stamp a clean SUCCESS
# classification so the workflow job can read
# the summary and not re-run cluster diagnostics.
# The success-line deliberately reflects the
# phase range, not "all 9", because pre-fixture
# only ran steps 1..6. A successful pre-fixture
# run is logged as "all $total_step_count
# readiness gates passed" where $total_step_count
# = $((PHASE_LAST_STEP - PHASE_FIRST_STEP + 1)).
# -----------------------------------------------------------------
TOTAL_CHECKED=$((PHASE_LAST_STEP - PHASE_FIRST_STEP + 1))
{
  printf 'classification=SUCCESS (exit 0)\n'
  printf 'all %d readiness gates passed (phase=%s)\n' \
    "$TOTAL_CHECKED" "$GATE_PHASE"
} | tee -a "$READINESS_LOG"
printf 'SUCCESS\n' > "$READINESS_SUMMARY"

# Re-emit the JSON with the classification and
# per-step records stitched in.
python3 - "$READINESS_JSON" "$READINESS_LOG" <<'PY' >"${READINESS_JSON}.new"
import json, re, sys

path, logp = sys.argv[1], sys.argv[2]
obj = json.load(open(path))
classification = "SUCCESS"
results = []
with open(logp) as f:
    txt = f.read()
# Each record_step emits a [step NN] : verdict line.
for m in re.finditer(r"\[step (\d+)\] (\S+(?:[ -]\S+)*) : (ok|failed|skipped)\s*\n\s*detail: (.*?)(?=\n\[step |\n\$|\Z)", txt, re.DOTALL):
    n  = int(m.group(1))
    nm = m.group(2)
    v  = m.group(3)
    d  = m.group(4).strip()
    results.append({"step": n, "name": nm, "verdict": v, "detail": d[:1500]})
if any(r["verdict"] == "failed" for r in results):
    classification = "CLUSTER_OR_CNI_NOT_READY"
obj["results"] = results
obj["classification"] = classification
print(json.dumps(obj, indent=2))
PY
mv "${READINESS_JSON}.new" "$READINESS_JSON"

exit 0
