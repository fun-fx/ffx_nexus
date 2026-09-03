#!/usr/bin/env bash
# scripts/test-upgrade-rehearsal-up.sh
#
# Phase D-2b.21: Helm upgrade rehearsal from
# networkPolicy.mode=disabled to enforce, then
# an intentionally-invalid upgrade that must
# fail-closed with the enforced release state
# preserved. Uses a profile=development release
# because the chart fail-closes "profile=enterprise
# + mode=disabled" inside templates.
#
# Steps:
#   1. helm install --set networkPolicy.mode=disabled
#      --set networkPolicy.profile=development
#      Expected: helm exit 0
#      Sentinels (only on rc=0):
#        upgrade-step1.rc    = 0
#        upgrade-step1.txt   = install-disabled-ok
#        upgrade-step1.log   = helm stdout/stderr
#      Non-zero helm install ⇒ fail-closed; NO
#      "install-disabled-ok" sentinel; later steps
#      not invoked.
#
#   2. helm upgrade --set networkPolicy.mode=enforce
#      --set networkPolicy.profile=enterprise
#      Expected: helm exit 0 (the enforced release
#      is applied; NetworkPolicies rendered)
#      Sentinels (only on rc=0):
#        upgrade-step2.rc      = 0
#        upgrade-step2.txt     = upgrade-enforce-ok
#        upgrade-step2.log     = helm stdout/stderr
#        upgrade-revision-before.txt  = initial revision
#        upgrade-revision-after.txt   = incremented revision
#        upgrade-enforce-np.json      = kubectl get netpol
#        upgrade-step2-values-projection.txt  = current values
#      Non-zero helm upgrade ⇒ fail-closed; NO
#      "upgrade-enforce-ok" sentinel.
#
#   3. helm upgrade with deliberately-invalid input
#      (an empty ingressController.namespaces list
#      renders a NetworkPolicy whose peer reference
#      ends up malformed inside the chart, so the
#      chart-template path fails closed before any
#      release mutation).
#      Expected: helm exit code != 0 (rejected)
#      Sentinels (only on rc != 0):
#        upgrade-step3.rc     = non-zero captured value
#        upgrade-step3.txt    = rejected-invalid-upgrade-ok
#        upgrade-step3.log    = helm stdout/stderr
#        upgrade-revision-after-rejected.txt   = must be unchanged
#      If helm exit code is 0 here, this is a
#      CONTRACT FAILURE — a chart that "happily
#      rendered" an invalid upgrade means the
#      fail-closed guard is broken. Emit no
#      "rejected-invalid-upgrade-ok" sentinel;
#      exit non-zero.
#
#   4. Verify enforced release remains deployed at
#      the SAME revision as after Step 2 (server-
#      side state preservation across rejection).
#      Expected: revision still equals
#      upgrade-revision-after.txt; values still match
#      upgrade-step2-values-projection.txt; the
#      captured manifest identity (selectors + ports
#      namespace) is unchanged.
#      Sentinels (only on identity match):
#        upgrade-step4.txt   = state-preserved-after-rejected-upgrade
#      Mismatch ⇒ fail-closed; NO state-preserved
#      sentinel.
#
# Failure model:
#   - Every asserted helm/kubectl interaction runs through
#     run_helm_capture / run_kc_capture which preserves the actual
#     PIPESTATUS[0] or $? exit code without ending the script with
#     || true.
#   - Sentinels encode VERIFIED OUTCOMES — they are written only when
#     the captured exit code satisfies the contract for that step. CI
#     parses the sentinels; the absence of a sentinel is therefore an
#     immediate failure, not a sentinel-of-no-evidence.
#   - State-observation commands (helm list, helm get values, helm get
#     manifest, kubectl get netpol) are ASSERTED inputs: their rc is
#     checked before downstream values / manifest identity / sentinels
#     are trusted. The previous best-effort framing was a fail-open in
#     disguise.
#   - GET-vs-SUBRESOURCE positions: real helm places the subresource
#     element as $2 (`helm get values <release>` → values is $2, the
#     release name is $2; `helm get manifest <release>` → manifest is
#     $2). The contract-test stub uses $2 as subresource, not $3.
#   - This script proves "rejected invalid upgrade with enforced-state
#     preservation". It does NOT prove observed runtime atomic rollback.
#     Step 3 deliberately fails at the chart-render / client-validation
#     layer, before any release mutation reaches the cluster. Naming
#     state preservation as evidence of server-side atomic rollback
#     would be a misclassification.

set -euo pipefail

SCRIPT_DIR="$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"
VALUES_EXTRA="${VALUES_EXTRA:-$SCRIPT_DIR/fixtures/integrationcni/values-extra-cni.yaml}"
ARTIFACTS="${ARTIFACTS:-${PWD}/artifacts/integrationcni}"
CHART_PATH="${CHART_PATH:-${PWD}/deploy/helm/nexus}"
RELEASE="${RELEASE:-nexus-cni-upgrade}"

# --------------------------------------------------------------------------
# d2b.53 — bounded Helm client transport.
#
# Heavy run 33642318757 failed Step 1 with
#   Error: INSTALLATION FAILED: client rate limiter Wait returned an error:
#   context deadline exceeded
# before install-disabled-ok was written. That is a CLIENT-SIDE transport
# deadline: Helm's default client throttle (QPS 5 / burst 100) plus the
# default --wait timeout of 5m could not push the enterprise-profile object
# graph through the discovery/apply path in time on a three-node kind
# cluster. It is NOT evidence of a chart contract failure — the chart had
# already rendered and Step 09 had already passed on the same SHA.
#
# The repair is to declare the transport explicitly and apply it to every
# MUTATING helm install / helm upgrade in Steps 1-3:
#
#   --wait --timeout <T> --qps <Q> --burst-limit <B>
#
# This is NOT a retry loop, NOT a rerun, and NOT a weakened release-state
# assertion. Each Helm invocation still runs EXACTLY ONCE, its original exit
# code is still captured verbatim by run_helm_capture, and every sentinel is
# still written only after its contract is satisfied. Raising the client
# throttle changes how fast the single attempt may talk to the API server;
# it does not change what counts as success.
#
# Values are validated BEFORE the first Helm command so an invalid local
# override fails with zero Helm calls and no sentinel.
# --------------------------------------------------------------------------
D2B_HELM_TIMEOUT="${D2B_HELM_TIMEOUT:-10m}"
D2B_HELM_QPS="${D2B_HELM_QPS:-50}"
D2B_HELM_BURST_LIMIT="${D2B_HELM_BURST_LIMIT:-100}"

# Explicit reasonable upper bounds. A local override outside these bounds is
# a configuration error, not something to clamp silently.
D2B_HELM_TIMEOUT_MAX_MINUTES=60
D2B_HELM_QPS_MAX=500
D2B_HELM_BURST_LIMIT_MAX=2000

mkdir -p "$ARTIFACTS"

if [[ ! -f "${ARTIFACTS}/cluster-up.txt" ]]; then
  echo "[upgrade] ERROR: cluster not up; run test-cluster-up.sh first"
  exit 2
fi

# --------------------------------------------------------------------------
# Transport validation — runs before ANY Helm invocation.
#
#   timeout : a positive whole-minute duration, i.e. /^[1-9][0-9]*m$/, at
#             most D2B_HELM_TIMEOUT_MAX_MINUTES. Seconds/hours forms and a
#             bare integer are rejected so the value is unambiguous in the
#             captured argv.
#   qps     : positive decimal integer, no sign, no zero padding, <= max.
#   burst   : positive decimal integer, no sign, no zero padding, <= max.
#             Must be >= qps; a burst below the sustained rate cannot be
#             honoured and would silently re-throttle the single attempt.
#
# Any violation exits non-zero with NO sentinel written, before Helm runs.
# --------------------------------------------------------------------------
transport_die() {
  echo "FAIL: invalid Helm transport configuration: $*" >&2
  echo "      no Helm command was issued and no sentinel was written" >&2
  exit 21
}

if [[ ! "${D2B_HELM_TIMEOUT}" =~ ^[1-9][0-9]*m$ ]]; then
  transport_die "D2B_HELM_TIMEOUT='${D2B_HELM_TIMEOUT}' must be a positive whole-minute duration such as 10m"
fi
_d2b_timeout_minutes="${D2B_HELM_TIMEOUT%m}"
if [[ "${_d2b_timeout_minutes}" -gt "${D2B_HELM_TIMEOUT_MAX_MINUTES}" ]]; then
  transport_die "D2B_HELM_TIMEOUT='${D2B_HELM_TIMEOUT}' exceeds the ${D2B_HELM_TIMEOUT_MAX_MINUTES}m upper bound"
fi
if [[ ! "${D2B_HELM_QPS}" =~ ^[1-9][0-9]*$ ]]; then
  transport_die "D2B_HELM_QPS='${D2B_HELM_QPS}' must be a positive decimal integer with no sign or zero padding"
fi
if [[ "${D2B_HELM_QPS}" -gt "${D2B_HELM_QPS_MAX}" ]]; then
  transport_die "D2B_HELM_QPS='${D2B_HELM_QPS}' exceeds the ${D2B_HELM_QPS_MAX} upper bound"
fi
if [[ ! "${D2B_HELM_BURST_LIMIT}" =~ ^[1-9][0-9]*$ ]]; then
  transport_die "D2B_HELM_BURST_LIMIT='${D2B_HELM_BURST_LIMIT}' must be a positive decimal integer with no sign or zero padding"
fi
if [[ "${D2B_HELM_BURST_LIMIT}" -gt "${D2B_HELM_BURST_LIMIT_MAX}" ]]; then
  transport_die "D2B_HELM_BURST_LIMIT='${D2B_HELM_BURST_LIMIT}' exceeds the ${D2B_HELM_BURST_LIMIT_MAX} upper bound"
fi
if [[ "${D2B_HELM_BURST_LIMIT}" -lt "${D2B_HELM_QPS}" ]]; then
  transport_die "D2B_HELM_BURST_LIMIT='${D2B_HELM_BURST_LIMIT}' must be >= D2B_HELM_QPS='${D2B_HELM_QPS}'"
fi

# The validated transport is recorded so a heavy-run artifact reviewer can
# see exactly which client bounds the single attempt used.
printf 'timeout=%s\nqps=%s\nburst_limit=%s\ntimeout_max_minutes=%s\nqps_max=%s\nburst_limit_max=%s\nretry_loop=none\nattempts_per_step=1\n' \
  "${D2B_HELM_TIMEOUT}" "${D2B_HELM_QPS}" "${D2B_HELM_BURST_LIMIT}" \
  "${D2B_HELM_TIMEOUT_MAX_MINUTES}" "${D2B_HELM_QPS_MAX}" "${D2B_HELM_BURST_LIMIT_MAX}" \
  > "${ARTIFACTS}/upgrade-helm-transport.txt"
echo "[upgrade] helm client transport: --wait --timeout ${D2B_HELM_TIMEOUT} --qps ${D2B_HELM_QPS} --burst-limit ${D2B_HELM_BURST_LIMIT} (single attempt per step, no retry)"

# --------------------------------------------------------------------------
# Capture helpers — preserve the actual exit code of the underlying command
# regardless of the `set -e` framing of this script. Logs remain in
# "$log"; the captured return code is what we base the contract on.
# --------------------------------------------------------------------------
run_helm_capture() {
  local log="$1"; shift
  set +e
  "$@" 2>&1 | tee "$log"
  local rc=${PIPESTATUS[0]}
  set -e
  printf '%s\n' "$rc" > "${log%.log}.rc"
}
run_kc_capture() {
  local log="$1"; shift
  set +e
  "$@" > "$log" 2>&1
  local rc=$?
  set -e
  printf '%s\n' "$rc" > "${log%.log}.rc"
  return "$rc"
}
die() {
  echo "FAIL: $*" >&2
  exit 22
}

# --------------------------------------------------------------------------
# Asserted state-capture helpers.
#
# Every helper below performs a real CLI invocation, validates the captured
# exit code via run_kc_capture, and refuses to continue on failure. These
# are NOT best-effort: their rc drives the contract.
#
# - get_rev <log-path>
#     Calls helm list, parses the JSON, returns the first matching release's
#     revision as a plain non-empty numeric string. Empty JSON, parser
#     failure, missing release, or non-numeric output → die.
# - assert_numeric_rev <label> <rev>
#     die()s if the revision string is not a non-empty decimal.
# - capture_values_asserted <out-path>
#     Captures `helm get values <release> -o yaml`; die on non-zero.
# - capture_manifest_asserted <out-manifest-path> <out-sha-path>
#     Captures `helm get manifest <release>` and writes sha256; die on
#     non-zero or empty manifest.
# - capture_np_asserted <raw-json-out> <semantic-identity-out>
#     Captures `kubectl get netpol -A -o json`; verifies the response is a
#     well-formed Kubernetes List document with an items ARRAY; writes the
#     unchanged raw JSON to <raw-json-out> and projects a deterministic
#     semantic identity to <semantic-identity-out>. The identity excludes
#     volatile metadata (resourceVersion, managedFields, creationTimestamp,
#     generation, uid, selfLink) and is sorted by (namespace,name). Empty
#     items=[] is allowed and produces a stable identity. Any parse /
#     projection / sha256 failure → die immediately — we refuse to claim a
#     NetworkPolicy surface that we cannot fingerprint.
PYTHON3="${PYTHON3:-$(command -v python3 || true)}"
if [[ -z "${PYTHON3}" ]]; then
  die "python3 not present on PATH; capture_np_asserted requires real python for json + sha256"
fi
# --------------------------------------------------------------------------
get_rev() {
  local log="$1"
  run_kc_capture "${log}" helm list -A --filter "^${RELEASE}$" -o json
  local rc; rc=$(cat "${log%.log}.rc")
  if [[ "$rc" -ne 0 ]]; then
    die "${log}: helm list returned rc=${rc}; revision capture asserted input failed"
  fi
  python3 -c '
import sys, json, re
data = sys.stdin.read().strip()
if not data:
    print(""); sys.exit(0)
try:
    parsed = json.loads(data)
except Exception:
    if re.fullmatch(r"\d+", data):
        print(data); sys.exit(0)
    print("?PARSE_ERROR"); sys.exit(0)
if not isinstance(parsed, list) or not parsed:
    print(""); sys.exit(0)
rev = parsed[0].get("revision", "")
if not re.fullmatch(r"\d+", str(rev)):
    print("?NOT_NUMERIC"); sys.exit(0)
print(rev)
' < "${log}"
}
assert_numeric_rev() {
  local label="$1" rev="$2"
  if [[ -z "${rev}" || ! "${rev}" =~ ^[0-9]+$ ]]; then
    die "${label}: revision '${rev}' is not a non-empty decimal; refusing to trust downstream comparisons"
  fi
}
capture_values_asserted() {
  local out="$1"
  local log="${out}.log"
  run_kc_capture "${log}" helm get values "${RELEASE}" -o yaml
  local rc; rc=$(cat "${log%.log}.rc")
  cp "${log}" "${out}"
  if [[ "$rc" -ne 0 || ! -s "${out}" ]]; then
    die "values capture: helm get values rc=${rc} out_size=$(wc -c < "${out}" 2>/dev/null | tr -d ' '); refusing to trust values"
  fi
}
# Best-effort cluster snapshot for a failed mutating helm step. A `--wait`
# timeout otherwise leaves nothing but helm's one-line error, which names
# the client rate limiter rather than the workload that never went Ready;
# run 33736159105 spent ten minutes producing a 98-byte artifact. Every
# command here is unasserted on purpose: this runs on the way to `die`, so
# a capture fault must not replace the original verdict.
capture_install_failure_evidence() {
  local step="$1"
  local prefix="${ARTIFACTS}/upgrade-step${step}-failure"
  (
    set +e
    echo "=== helm status ==="
    helm status "${RELEASE}" 2>&1
    echo "=== pods (release selector, all namespaces) ==="
    kubectl get pods -A -l "app.kubernetes.io/instance=${RELEASE}" -o wide 2>&1
    echo "=== deployments (release selector, all namespaces) ==="
    kubectl get deploy -A -l "app.kubernetes.io/instance=${RELEASE}" -o wide 2>&1
    echo "=== pod describe + container state ==="
    kubectl describe pods -A -l "app.kubernetes.io/instance=${RELEASE}" 2>&1
    echo "=== recent events (all namespaces, by time) ==="
    kubectl get events -A --sort-by=.lastTimestamp 2>&1 | tail -60
  ) > "${prefix}.txt" 2>&1
  echo "[upgrade] step ${step} failed; cluster snapshot written to ${prefix}.txt ($(wc -c < "${prefix}.txt" 2>/dev/null | tr -d ' ') bytes)"
}
capture_manifest_asserted() {
  local out_manifest="$1" out_sha="$2"
  local log="${out_manifest}.log"
  run_kc_capture "${log}" helm get manifest "${RELEASE}"
  local rc; rc=$(cat "${log%.log}.rc")
  cp "${log}" "${out_manifest}"
  if [[ "$rc" -ne 0 || ! -s "${out_manifest}" ]]; then
    die "manifest capture: helm get manifest rc=${rc} out_size=$(wc -c < "${out_manifest}" 2>/dev/null | tr -d ' '); refusing to trust manifest"
  fi
  shasum -a 256 "${out_manifest}" | head -n1 | cut -d' ' -f1 > "${out_sha}"
}
capture_np_asserted() {
  local raw_out="$1" id_out="$2"
  local log="${raw_out}.log"
  run_kc_capture "${log}" kubectl get netpol -A -o json
  local rc; rc=$(cat "${log%.log}.rc")
  if [[ "$rc" -ne 0 ]]; then
    die "kubectl get netpol -A rc=${rc}; refused to claim NetworkPolicy surface is verified"
  fi
  if [[ ! -s "${log}" ]]; then
    die "kubectl get netpol -A log empty (rc=${rc}); refused to claim NetworkPolicy surface is verified"
  fi
  cp "${log}" "${raw_out}"
  "${PYTHON3}" - "${log}" "${id_out}" <<'EOF_PY_NP_IDENTITY'
import sys, json, hashlib
log_path, id_path = sys.argv[1], sys.argv[2]
try:
    with open(log_path, "r", encoding="utf-8") as f:
        raw = f.read()
except Exception as e:
    print("NP:READ_FAIL:" + repr(e), file=sys.stderr); sys.exit(95)
raw_stripped = raw.strip()
if not raw_stripped:
    print("NP:EMPTY_DOCUMENT", file=sys.stderr); sys.exit(95)
try:
    parsed = json.loads(raw_stripped)
except Exception as e:
    print("NP:NOT_JSON:" + repr(e), file=sys.stderr); sys.exit(95)
if not isinstance(parsed, dict):
    print("NP:NOT_OBJECT", file=sys.stderr); sys.exit(95)
items = parsed.get("items")
if not isinstance(items, list):
    print("NP:ITEMS_NOT_LIST", file=sys.stderr); sys.exit(95)
VOLATILE = ("resourceVersion", "managedFields", "creationTimestamp", "generation", "uid", "selfLink")
def strip_volatile(obj):
    if isinstance(obj, dict):
        return {k: strip_volatile(v) for k, v in obj.items() if k not in VOLATILE}
    if isinstance(obj, list):
        return [strip_volatile(v) for v in obj]
    return obj
projected = []
for it in items:
    if not isinstance(it, dict):
        print("NP:ITEM_NOT_OBJECT", file=sys.stderr); sys.exit(95)
    md = it.get("metadata", {})
    if not isinstance(md, dict):
        print("NP:METADATA_NOT_OBJECT", file=sys.stderr); sys.exit(95)
    ns = md.get("namespace", "")
    nm = md.get("name", "")
    if not isinstance(ns, str) or not isinstance(nm, str):
        print("NP:NS_OR_NAME_NOT_STR", file=sys.stderr); sys.exit(95)
    rec = {
        "metadata": {
            "namespace": ns,
            "name": nm,
        },
        "spec": it.get("spec", {}),
    }
    labels = md.get("labels")
    if isinstance(labels, dict):
        rec["metadata"]["labels"] = labels
    projected.append(strip_volatile(rec))
projected_sorted = sorted(projected, key=lambda r: (r["metadata"].get("namespace", ""), r["metadata"].get("name", "")))
try:
    canon = json.dumps(projected_sorted, sort_keys=True, separators=(",", ":"))
except Exception as e:
    print("NP:CANON_FAIL:" + repr(e), file=sys.stderr); sys.exit(95)
sha = hashlib.sha256(canon.encode("utf-8")).hexdigest()
try:
    with open(id_path, "w", encoding="utf-8") as f:
        f.write(sha + "\n")
except Exception as e:
    print("NP:WRITE_FAIL:" + repr(e), file=sys.stderr); sys.exit(95)
sys.exit(0)
EOF_PY_NP_IDENTITY
  local py_rc=$?
  if [[ "${py_rc}" -ne 0 ]]; then
    die "capture_np_asserted: identity projection failed (py_rc=${py_rc}); refusing to trust NetworkPolicy surface"
  fi
  if [[ ! -s "${id_out}" ]]; then
    die "capture_np_asserted: identity file empty (${id_out}); refusing to trust NetworkPolicy surface"
  fi
}
# --------------------------------------------------------------------------
# Asserted release-status helper.
#
# Runs `helm status <release> -o json` via run_kc_capture, parses the JSON
# with the real system python3 -c '<script>', and requires
# .info.status == "deployed" EXACTLY (no fuzzy match, no default fallback).
# Any of: nonzero rc, empty/invalid JSON, missing .info, non-deployed status
# → die immediately. The parsed status is recorded to <log>.status for
# downstream inspection.
# --------------------------------------------------------------------------
assert_release_deployed() {
  local label="$1" log="$2"
  run_kc_capture "${log}" helm status "${RELEASE}" -o json
  local rc; rc=$(cat "${log%.log}.rc")
  if [[ "$rc" -ne 0 ]]; then
    die "${label}: helm status rc=${rc}; refused to trust release state"
  fi
  local parsed_status
  parsed_status=$(python3 -c '
import sys, json
data = sys.stdin.read().strip()
if not data:
    print("?EMPTY"); sys.exit(0)
try:
    parsed = json.loads(data)
except Exception:
    print("?NOT_JSON"); sys.exit(0)
info = parsed.get("info") if isinstance(parsed, dict) else None
if not isinstance(info, dict):
    print("?INFO_MISSING"); sys.exit(0)
status = info.get("status", "")
if status != "deployed":
    print("?NOT_DEPLOYED:" + str(status)); sys.exit(0)
print(status)
' < "${log}")
  printf '%s\n' "${parsed_status}" > "${log}.status"
  if [[ "${parsed_status}" != "deployed" ]]; then
    die "${label}: release status assertion FAILED; got=${parsed_status}; refusing to claim deployed"
  fi
}

# --------------------------------------------------------------------------
# NetworkPolicy transition helper.
#
# Proves the disabled→enforced transition itself, not just stability. Required
# by callers between the Step 2 capture and the upgrade-step2 sentinel write.
#
# args:
#   $1 — path to the disabled-state identity file (e.g. upgrade-step1-np-id)
#   $2 — path to the enforced-state identity file (e.g. upgrade-step2-np-id)
#   $3 — path to the enforced-state raw JSON list (upgrade-step2-np.json)
#
# Required assertions before continuing:
#   - both identity files exist and have a non-empty single-line content
#   - the enforced raw JSON is parseable, an object, with items list len >= 1
#   - parsed items[0] has stable policy semantics (namespace/name/labels/spec)
#   - enforced identity != disabled identity (semantic transition occurred)
#
# Any failure dies with a diagnostic that names the missing piece.
# This is intentionally separate from capture_np_asserted so the transition
# itself is a contract assertion, not an implicit by-product.
# --------------------------------------------------------------------------
assert_np_transition() {
  local disabled_id_path="$1" enforced_id_path="$2" enforced_raw_path="$3"

  if [[ ! -s "${disabled_id_path}" ]]; then
    die "NetworkPolicy enforcement transition missing (disabled identity file absent or empty: ${disabled_id_path}); refusing to write upgrade-enforce-ok sentinel"
  fi
  if [[ ! -s "${enforced_id_path}" ]]; then
    die "NetworkPolicy enforcement transition missing (enforced identity file absent or empty: ${enforced_id_path}); refusing to write upgrade-enforce-ok sentinel"
  fi
  if [[ ! -s "${enforced_raw_path}" ]]; then
    die "NetworkPolicy enforcement transition missing (enforced raw JSON absent or empty: ${enforced_raw_path}); refusing to write upgrade-enforce-ok sentinel"
  fi

  local disabled_id enforced_id
  disabled_id="$(head -n1 "${disabled_id_path}" 2>/dev/null || true)"
  enforced_id="$(head -n1 "${enforced_id_path}" 2>/dev/null || true)"
  if [[ -z "${disabled_id}" || "${#disabled_id}" -ne 64 ]]; then
    die "NetworkPolicy enforcement transition missing (disabled identity not a 64-hex sha256: got '${disabled_id}'); refusing to write upgrade-enforce-ok sentinel"
  fi
  if [[ -z "${enforced_id}" || "${#enforced_id}" -ne 64 ]]; then
    die "NetworkPolicy enforcement transition missing (enforced identity not a 64-hex sha256: got '${enforced_id}'); refusing to write upgrade-enforce-ok sentinel"
  fi
  if [[ "${disabled_id}" == "${enforced_id}" ]]; then
    die "NetworkPolicy enforcement transition missing (step2-NetworkPolicy semantic identity equal to step1; disabled state not transitioned to enforced); refusing to write upgrade-enforce-ok sentinel"
  fi

  local items_len
  items_len=$("${PYTHON3}" - "${enforced_raw_path}" "${disabled_id_path}" "${enforced_id_path}" <<'EOF_PY_NP_TRANSITION'
import sys, json
raw_path = sys.argv[1]
disabled_path = sys.argv[2]
enforced_path = sys.argv[3]
try:
    with open(raw_path, "r", encoding="utf-8") as f:
        raw = f.read()
except Exception as e:
    print("NPX:READ_FAIL:" + repr(e), file=sys.stderr); sys.exit(96)
try:
    doc = json.loads(raw)
except Exception as e:
    print("NPX:NOT_JSON:" + repr(e), file=sys.stderr); sys.exit(96)
if not isinstance(doc, dict):
    print("NPX:NOT_OBJECT", file=sys.stderr); sys.exit(96)
items = doc.get("items")
if not isinstance(items, list):
    print("NPX:ITEMS_NOT_LIST", file=sys.stderr); sys.exit(96)
if len(items) < 1:
    print("NPX:SURFACE_EMPTY:" + str(len(items)), file=sys.stderr); sys.exit(96)
# Confirm at least one item exposes stable policy semantics.
for it in items:
    if not isinstance(it, dict):
        print("NPX:ITEM_NOT_OBJECT", file=sys.stderr); sys.exit(96)
    md = it.get("metadata", {})
    if not isinstance(md, dict):
        print("NPX:METADATA_NOT_OBJECT", file=sys.stderr); sys.exit(96)
    ns = md.get("namespace", "")
    nm = md.get("name", "")
    if not isinstance(ns, str) or not isinstance(nm, str) or ns == "" or nm == "":
        print("NPX:NS_OR_NAME_NOT_STR", file=sys.stderr); sys.exit(96)
    spec = it.get("spec", {})
    if not isinstance(spec, dict):
        print("NPX:SPEC_NOT_OBJECT", file=sys.stderr); sys.exit(96)
print(str(len(items)))
EOF_PY_NP_TRANSITION
  )
  local py_rc=$?
  if [[ "${py_rc}" -ne 0 ]]; then
    die "enforced NetworkPolicy surface empty (raw JSON parse unsigned the enforced list: ${enforced_raw_path}; py_rc=${py_rc}); refusing to write upgrade-enforce-ok sentinel"
  fi
  if [[ -z "${items_len}" || "${items_len}" -lt 1 ]]; then
    die "enforced NetworkPolicy surface empty (parsed items length '${items_len}'); refusing to write upgrade-enforce-ok sentinel"
  fi
}

# --------------------------------------------------------------------------
# Step 1/4 — install mode=disabled (development profile)
# --------------------------------------------------------------------------
echo "[upgrade] step 1/4: install mode=disabled (development profile)"
# Cleanup of a prior run; helm uninstall --ignore-not-found is contractually
# rc=0 in every everyday scenario. If something catastrophic happens here the
# `} || true` envelope keeps the install attempt alive so C2 exercises the
# install-failure branch (install asserts its own rc).
{
  set +e
  helm uninstall "${RELEASE}" --ignore-not-found >/dev/null 2>&1
  set -e
} || true
# The placeholder image must satisfy the probes this chart declares, not
# merely exist. values.yaml pins readinessProbe httpGet /readyz and
# livenessProbe httpGet /healthz on the gateway port, so `--wait` can only
# succeed against an image that serves both. busybox serves neither and its
# default entrypoint exits immediately, which is the exact defect
# control-netpol-gate.Dockerfile was written to eliminate; using it here
# guarantees a 10-minute `--wait` burn surfacing as a client-side rate
# limiter deadline. The fixture listener already loaded into this cluster
# answers /readyz and /healthz on every -ports value.
#
# migrations.enabled=false is a declared scope boundary, not a relaxation
# of what this rehearsal asserts. The migration Job is a helm
# pre-install/pre-upgrade hook whose container runs `migrate --engine=all`
# against a live schema owner. This cluster has neither the real Nexus
# image nor a real Postgres — install-nexus-test.sh says so in its own
# header — so hook completion is UNAVAILABLE here and must never be
# reported as green. What the customer actually depends on, that the
# migration role can still reach the schema owner once enforcement is on,
# is proven as datapath evidence by scenario s14
# (cni-mock-nexus-migration -> cni-postgres.database:5432, expect ALLOW)
# rather than inferred from a hook that cannot run. Disabling the hook
# also leaves the rendered NetworkPolicy set byte-identical, so the
# revision, values, and policy-identity assertions below are untouched.
S1RC=$(run_helm_capture "${ARTIFACTS}/upgrade-step1.log" \
  helm install "${RELEASE}" "${CHART_PATH}" \
    --set networkPolicy.mode=disabled \
    --set networkPolicy.profile=development \
    --set networkPolicy.enforcementAcknowledged=true \
    --set image.repository=cni-listener \
    --set image.tag=local \
    --set image.pullPolicy=Never \
    --set-json 'args=["-ports=8080,8081","-role=rehearsal","-target=upgrade-rehearsal"]' \
    --set migrations.enabled=false \
    --set dependencies.postgres.url="postgres://nexus:nopassword@postgres.default.svc.cluster.local:5432/nexus" \
    --wait \
    --timeout "$D2B_HELM_TIMEOUT" \
    --qps "$D2B_HELM_QPS" \
    --burst-limit "$D2B_HELM_BURST_LIMIT")
S1RC=$(cat "${ARTIFACTS}/upgrade-step1.rc")
S1RC=${S1RC:-1}

if [[ "$S1RC" -ne 0 ]]; then
  capture_install_failure_evidence 1
  die "step 1 (disabled install) helm exit code = ${S1RC}; not 0; refusing to write install-disabled-ok sentinel"
fi
# Step 1 sentinel-last ordering:
#   1) assert status=deployed via assert_release_deployed (own log/status file),
#   2) capture raw NetPol JSON plus a deterministic semantic identity,
#   3) capture values,
#   4) only then write upgrade-step1.txt.
# A failed status observation, NetPol observation, or values observation
# leaves upgrade-step1.txt absent; CI parses absence as a failure.
assert_release_deployed "[upgrade] S1 status" "${ARTIFACTS}/upgrade-step1-status.log"
capture_np_asserted "${ARTIFACTS}/upgrade-step1-np.json" "${ARTIFACTS}/upgrade-step1-np-id"
capture_values_asserted "${ARTIFACTS}/upgrade-step1-values.yaml"
printf 'install-disabled-ok\n' > "${ARTIFACTS}/upgrade-step1.txt"

# --------------------------------------------------------------------------
# Step 2/4 — upgrade to mode=enforce (enterprise profile)
# --------------------------------------------------------------------------
echo "[upgrade] step 2/4: upgrade to mode=enforce (enterprise profile)"
# Step 2 ordering (R1): capture numeric S2_REV_BEFORE, run enforce upgrade,
# require rc=0, capture S2_REV_AFTER (> S2_REV_BEFORE), assert status=deployed,
# assert captured values/manifest/NP state, ONLY THEN write upgrade-step2.txt.
S2_REV_BEFORE=$(get_rev "${ARTIFACTS}/upgrade-step2-rev-before.log")
printf '%s\n' "${S2_REV_BEFORE}" > "${ARTIFACTS}/upgrade-revision-before.txt"
assert_numeric_rev "[upgrade] S2_REV_BEFORE" "${S2_REV_BEFORE}"
get_rev "${ARTIFACTS}/upgrade-step2-rev-before.log" >/dev/null

S2RC=$(run_helm_capture "${ARTIFACTS}/upgrade-step2.log" \
  helm upgrade "${RELEASE}" "${CHART_PATH}" \
    --values "${VALUES_EXTRA}" \
    --set networkPolicy.mode=enforce \
    --set networkPolicy.profile=enterprise \
    --set networkPolicy.enforcementAcknowledged=true \
    --set image.repository=cni-listener \
    --set image.tag=local \
    --set image.pullPolicy=Never \
    --set-json 'args=["-ports=8080,8081","-role=rehearsal","-target=upgrade-rehearsal"]' \
    --set migrations.enabled=false \
    --set dependencies.postgres.url="postgres://nexus:nopassword@postgres.default.svc.cluster.local:5432/nexus" \
    --atomic \
    --wait \
    --timeout "$D2B_HELM_TIMEOUT" \
    --qps "$D2B_HELM_QPS" \
    --burst-limit "$D2B_HELM_BURST_LIMIT")
S2RC=$(cat "${ARTIFACTS}/upgrade-step2.rc")
S2RC=${S2RC:-1}

if [[ "$S2RC" -ne 0 ]]; then
  capture_install_failure_evidence 2
  die "step 2 (enforce helm upgrade --atomic) exit code = ${S2RC}; not 0; refusing to write upgrade-enforce-ok sentinel"
fi

# Capture post-upgrade revision AFTER the rc=0 check. Any non-die here is
# the last chance to die before writing the upgrade-step2 sentinel.
S2_REV_AFTER=$(get_rev "${ARTIFACTS}/upgrade-step2-rev-after.log")
printf '%s\n' "${S2_REV_AFTER}" > "${ARTIFACTS}/upgrade-revision-after.txt"
assert_numeric_rev "[upgrade] S2_REV_AFTER" "${S2_REV_AFTER}"
# Step 2 must have actually advanced the revision.
if [[ "${S2_REV_AFTER}" -le "${S2_REV_BEFORE}" ]]; then
  die "step 2: revision did not increment after --atomic enforce upgrade; before=${S2_REV_BEFORE} after=${S2_REV_AFTER}; refusing to write upgrade-enforce-ok sentinel"
fi

# Release must be in status=deployed after the enforce upgrade (R4).
assert_release_deployed "[upgrade] S2 status" "${ARTIFACTS}/upgrade-step2-status.log"

# Asserted values / manifest / NetworkPolicy captures are LAST so the
# success sentinel encodes verified transition + identity + deployed state.
capture_values_asserted "${ARTIFACTS}/upgrade-step2-values.yaml"
capture_manifest_asserted "${ARTIFACTS}/upgrade-step2-manifest.txt" \
  "${ARTIFACTS}/upgrade-step2-manifest-id"
capture_np_asserted "${ARTIFACTS}/upgrade-step2-np.json" "${ARTIFACTS}/upgrade-step2-np-id"
# NetworkPolicy transition from disabled (Step 1) to enforced (Step 2) MUST
# be a real, observable change — not just stability. The transition helper
# reads Step 1 disabled identity, Step 2 enforced identity, and the enforced
# raw JSON list to assert identity changed and the enforced list is nonempty.
assert_np_transition "${ARTIFACTS}/upgrade-step1-np-id" \
  "${ARTIFACTS}/upgrade-step2-np-id" \
  "${ARTIFACTS}/upgrade-step2-np.json"

# Final: write the upgrade-step2 sentinel LAST, only after every required
# state assertion has passed. Absence of upgrade-step2.txt is the outcome
# for any failure in the prior assertions.
printf 'upgrade-enforce-ok\n' > "${ARTIFACTS}/upgrade-step2.txt"

# --------------------------------------------------------------------------
# Step 3/4 — invalid upgrade must be REJECTED
#
# The chart's NetworkPolicy template render path
# closes on an empty ingressController.namespaces
# list (peer references become undefined). Helm's
# exit code is the only contract for rejection;
# a zero exit is a CONTRACT FAILURE.
# --------------------------------------------------------------------------
echo "[upgrade] step 3/4: invalid upgrade must be REJECTED"
# Step 3 ordering (R2): capture numeric S3_REV_BEFORE, REQUIRE == S2_REV_AFTER
# (the enforced snapshot), run invalid Helm upgrade, REQUIRE rc != 0, capture
# numeric S3_REV_AFTER (must equal S2_REV_AFTER / S3_REV_BEFORE), assert
# status=deployed, capture post-rejection values/manifest identity / NetPol
# surface as evidence, ONLY THEN write upgrade-step3.txt = rejected.

S3_REV_BEFORE=$(get_rev "${ARTIFACTS}/upgrade-step3-rev-before.log")
printf '%s\n' "${S3_REV_BEFORE}" > "${ARTIFACTS}/upgrade-rev-step3-before.txt"
assert_numeric_rev "[upgrade] S3_REV_BEFORE" "${S3_REV_BEFORE}"
# R3: inter-step revision equality enforced BEFORE the invalid upgrade runs.
if [[ "${S3_REV_BEFORE}" != "${S2_REV_AFTER}" ]]; then
  die "step 3: S3_REV_BEFORE=${S3_REV_BEFORE} must equal S2_REV_AFTER=${S2_REV_AFTER} before the invalid upgrade runs; refused"
fi

S3RC=$(run_helm_capture "${ARTIFACTS}/upgrade-step3.log" \
  helm upgrade "${RELEASE}" "${CHART_PATH}" \
    --values "${VALUES_EXTRA}" \
    --set networkPolicy.mode=enforce \
    --set networkPolicy.profile=enterprise \
    --set networkPolicy.enforcementAcknowledged=true \
    --set 'networkPolicy.ingressController.namespaces[0]=' \
    --set 'networkPolicy.ingressController.matchPorts[0]=8080' \
    --set image.repository=cni-listener \
    --set image.tag=local \
    --set image.pullPolicy=Never \
    --set-json 'args=["-ports=8080,8081","-role=rehearsal","-target=upgrade-rehearsal"]' \
    --set migrations.enabled=false \
    --set dependencies.postgres.url="postgres://nexus:nopassword@postgres.default.svc.cluster.local:5432/nexus" \
    --atomic \
    --wait \
    --timeout "$D2B_HELM_TIMEOUT" \
    --qps "$D2B_HELM_QPS" \
    --burst-limit "$D2B_HELM_BURST_LIMIT")
S3RC=$(cat "${ARTIFACTS}/upgrade-step3.rc")
S3RC=${S3RC:-0}

# Require non-zero AT THIS POINT, before any post-rejection state capture.
# A zero exit is a CONTRACT FAILURE — the chart accepted a broken peer list.
if [[ "$S3RC" -eq 0 ]]; then
  # The chart just applied a peer list it should have refused, so the live
  # NetworkPolicy is the only place that says what the API actually stored
  # for the unusable peer — an empty label value (matches nothing, silently
  # drops an authorized peer) and a dropped selector key (matches every
  # namespace) are the same helm exit code and opposite security outcomes.
  # Run 33743750984 hit this branch with no such capture, so the direction
  # had to be inferred rather than read. Unasserted: it cannot soften the
  # contract failure below, it only makes the next one diagnosable.
  (
    set +e
    echo "=== live gateway NetworkPolicy (what the API stored for the invalid peer) ==="
    kubectl get networkpolicy -A -l "app.kubernetes.io/instance=${RELEASE}" -o json 2>&1
    echo "=== helm values as accepted ==="
    helm get values "${RELEASE}" --all 2>&1
  ) > "${ARTIFACTS}/upgrade-step3-accepted-invalid.json" 2>&1
  die "step 3 (invalid upgrade) helm exit code = 0; the chart accepted broken NetworkPolicy peer references — this is a contract failure; refusing to write rejected-invalid-upgrade-ok sentinel"
fi
printf '%s\n' "${S3RC}" | tr -dc '0-9\n' > "${ARTIFACTS}/upgrade-step3.rc"

# Capture post-rejection revision AS EVIDENCE: it must equal S2_REV_AFTER.
S3_REV_AFTER=$(get_rev "${ARTIFACTS}/upgrade-step3-rev-after.log")
printf '%s\n' "${S3_REV_AFTER}" > "${ARTIFACTS}/upgrade-rev-step3-after.txt"
assert_numeric_rev "[upgrade] S3_REV_AFTER" "${S3_REV_AFTER}"
# R3: post-rejection revision must STILL equal the enforced snapshot.
if [[ "${S3_REV_AFTER}" != "${S2_REV_AFTER}" ]]; then
  die "step 3: S3_REV_AFTER=${S3_REV_AFTER} must equal S2_REV_AFTER=${S2_REV_AFTER} after the rejected upgrade; refusing"
fi

# R4: release must still be in status=deployed after the rejected upgrade.
assert_release_deployed "[upgrade] S3 status" "${ARTIFACTS}/upgrade-step3-status.log"

# Post-rejection values / manifest identity / NetPol surface captures are
# EVIDENCE only — used by Step 4 for byte-equality / sha256-equality assertions.
capture_values_asserted "${ARTIFACTS}/upgrade-step3-values-post.yaml"
capture_manifest_asserted "${ARTIFACTS}/upgrade-step3-manifest-post.txt" \
  "${ARTIFACTS}/upgrade-step3-manifest-post-id"
capture_np_asserted "${ARTIFACTS}/upgrade-step3-np-post.json" "${ARTIFACTS}/upgrade-step3-np-post-id"
# Enforced/rejected NetworkPolicy semantic identity MUST match: the rejected
# upgrade must not have mutated the live NetPolicy surface. Mismatch is a
# NetworkPolicy identity drift across the rejection window.
if ! diff -q "${ARTIFACTS}/upgrade-step2-np-id" "${ARTIFACTS}/upgrade-step3-np-post-id" >/dev/null 2>&1; then
  die "step 3: NetworkPolicy identity drifted (step3-np-post-id != step2-np-id); refusing to write rejected-invalid-upgrade-ok sentinel"
fi

# Final: write the rejected sentinel LAST, only after every asserted state
# check has passed (rc != 0, revisions equal to S2_REV_AFTER, status=deployed,
# values/manifest/NP observable, NetPol semantic identity stable).
printf 'rejected-invalid-upgrade-ok\n' > "${ARTIFACTS}/upgrade-step3.txt"

# --------------------------------------------------------------------------
# Step 4/4 — verify enforced-state preservation across the rejected upgrade
#
# The release MUST still:
#   - be deployed at the SAME revision it was
#     after Step 2 succeeded;
#   - return the SAME values.yaml;
#   - render the SAME manifest (matching the
#     manifest identity captured between Step 2
#     success and Step 3 invocation).
#
# The contract verified here is "rejected invalid
# upgrade with enforced-state preservation".
# --------------------------------------------------------------------------
echo "[upgrade] step 4/4: enforced-state preservation across rejected upgrade"
S4_REV=$(get_rev "${ARTIFACTS}/upgrade-step4-rev.log")
assert_numeric_rev "[upgrade] S4_REV" "${S4_REV}"

# 4a. The release must still be present at all (helm list non-empty) and
#     the parser must have yielded a verified numeric revision.
# 4b. R3 re-check: S3_REV_BEFORE and S3_REV_AFTER must be exact-equal
#     to S2_REV_AFTER — the enforced snapshot — else the rejection was not
#     state-preserving. We re-validate numerically here before any later
#     comparison.
assert_numeric_rev "[upgrade] S3_REV_BEFORE @4b" "${S3_REV_BEFORE}"
assert_numeric_rev "[upgrade] S3_REV_AFTER @4b" "${S3_REV_AFTER}"
if [[ "${S3_REV_BEFORE}" != "${S2_REV_AFTER}" ]]; then
  die "step 4: S3_REV_BEFORE=${S3_REV_BEFORE} must equal S2_REV_AFTER=${S2_REV_AFTER}; enforced-state preservation violated"
fi
if [[ "${S3_REV_AFTER}" != "${S2_REV_AFTER}" ]]; then
  die "step 4: S3_REV_AFTER=${S3_REV_AFTER} must equal S2_REV_AFTER=${S2_REV_AFTER}; enforced-state preservation violated"
fi

# 4c. Post-rejection release must remain in status=deployed (R4).
assert_release_deployed "[upgrade] S4 status" "${ARTIFACTS}/upgrade-step4-status.log"

# 4d. The current revision must equal the verified numeric S2_REV_AFTER —
#     the enforced snapshot right after step 2 succeeded. Numeric compare
#     rules out any string-only path that chart-render stdio could pollute.
if [[ "${S4_REV}" != "${S2_REV_AFTER}" ]]; then
  die "step 4: revision mismatch across the rejected upgrade; expected verified S2_REV_AFTER=${S2_REV_AFTER}, now=${S4_REV}; enforced-state preservation violated"
fi
printf '%s\n' "${S4_REV}" > "${ARTIFACTS}/upgrade-step4-revision.txt"

# 4e. Values identity must match the values captured after Step 2. Use
#     byte-equality (diff -q). Also assert bytestand between the post-
#     rejection S3 values capture and the post-Step-4 re-capture (the
#     post-rejection snapshot MUST equal the post-Step-4 re-capture to
#     prove no further drift across the rejection-window to sentinel-time).
capture_values_asserted "${ARTIFACTS}/upgrade-step4-values-post.yaml"
if ! diff -q "${ARTIFACTS}/upgrade-step2-values.yaml" "${ARTIFACTS}/upgrade-step4-values-post.yaml" >/dev/null 2>&1; then
  die "step 4: values identity drifted across the rejected upgrade; diff shows a real delta"
fi
if ! diff -q "${ARTIFACTS}/upgrade-step3-values-post.yaml" "${ARTIFACTS}/upgrade-step4-values-post.yaml" >/dev/null 2>&1; then
  die "step 4: post-rejection values !== post-step4 re-capture; values drifted between rejection and sentinel"
fi

# 4f. Manifest identity must match the manifest captured after Step 2.
capture_manifest_asserted "${ARTIFACTS}/upgrade-step4-manifest-post.txt" \
  "${ARTIFACTS}/upgrade-step4-manifest-id"
if ! diff -q "${ARTIFACTS}/upgrade-step2-manifest-id" "${ARTIFACTS}/upgrade-step4-manifest-id" >/dev/null 2>&1; then
  die "step 4: manifest identity drifted across the rejected upgrade; sha256 differs"
fi

# 4g. NetworkPolicy semantic identity must equal the enforced Step 2 snapshot
#     AND the post-rejection Step 3 snapshot. Both comparisons are required —
#     a drift between Step 3 and Step 4 is also a contract violation.
capture_np_asserted "${ARTIFACTS}/upgrade-step4-np-post.json" "${ARTIFACTS}/upgrade-step4-np-post-id"
if ! diff -q "${ARTIFACTS}/upgrade-step2-np-id" "${ARTIFACTS}/upgrade-step4-np-post-id" >/dev/null 2>&1; then
  die "step 4: NetworkPolicy identity drifted across the rejected upgrade (step4-np-post-id != step2-np-id)"
fi
if ! diff -q "${ARTIFACTS}/upgrade-step3-np-post-id" "${ARTIFACTS}/upgrade-step4-np-post-id" >/dev/null 2>&1; then
  die "step 4: NetworkPolicy identity drifted between rejection and sentinel (step4-np-post-id != step3-np-post-id)"
fi

# All four contract checks above have passed. Emit the state-preserved
# sentinel — only when ALL evidence (revision, values, manifest identity,
# NetworkPolicy semantic identity) matches the asserted enforced snapshot
# from Step 2 and the post-rejection snapshot from Step 3.
printf 'state-preserved-after-rejected-upgrade\n' > "${ARTIFACTS}/upgrade-step4.txt"

# --------------------------------------------------------------------------
# Final summary of verified outcome sentinels
# --------------------------------------------------------------------------
echo "[upgrade] rehearsal complete with fail-closed verified sentinels:"
for f in upgrade-step1.txt upgrade-step2.txt upgrade-step3.txt upgrade-step4.txt; do
  printf '  %s  %s\n' "${ARTIFACTS}/${f}" "$(cat "${ARTIFACTS}/${f}")"
done
exit 0
