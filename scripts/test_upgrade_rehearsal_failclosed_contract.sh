#!/usr/bin/env bash
# scripts/test_upgrade_rehearsal_failclosed_contract.sh
#
# Deterministic offline regression that proves
# scripts/test-upgrade-rehearsal-up.sh's fail-closed
# contract. The test stubs `helm` and `kubectl` in a
# private PATH using ONE quoted heredoc stub body per
# stub (heredoc is FULLY quoted so bash never expands
# $, ;, (, ), { inside the body). The real system
# python3 is preserved so the target's `python3 -c '…'`
# parser consumes valid JSON emitted by the stub helm.
#
# Stub state model:
#   - helm install <r>          → rc = STUB_RC_STEP1
#   - helm upgrade <r>          → 1st call rc = STUB_RC_STEP2; 2nd call rc = STUB_RC_STEP3
#   - helm uninstall <r>        → rc = 0
#   - helm list … -o json       → JSON revision derived from STUB_LIST_REV_PATH.
#                                 The stub bumps `after=…` post-step-2 success and
#                                 mutates to STUB_DRIFT_REV_TO (or +93 default)
#                                 post-step-3 if STUB_DRIFT_REV=1.
#   - helm status <r> -o json   → JSON with .info.status.
#                                 STUB_STATUS_FAIL_FROM / STUB_STATUS_FAIL_TO ranges
#                                 return STUB_STATUS_FAIL_VALUE (default "failed").
#                                 STUB_STATUS_FAIL_RC overrides exit code.
#   - helm get values <r>       → STUB_GET_VALUES_FAIL_FROM forces rc=STUB_GET_VALUES_FAIL_RC
#                                 and exits before reading content; otherwise baseline
#                                 vs STUB_VALUE_DRIFT_PATH based on STUB_DRIFT_VALUES
#                                 state-transition rule.
#   - helm get manifest <r>     → similar to values with STUB_GET_MANIFEST_FAIL_FROM and
#                                 STUB_DRIFT_MANIFEST.
#   - kubectl get netpol -A     → STUB_KC_NETPOL_FAIL_FROM forces rc=STUB_KC_NETPOL_FAIL_RC;
#                                 else cats STUB_NP_PATH.
#
# Each control asserts the real target script's exit code, on-disk sentinel
# contents, revision numbers, status, and value/manifest byte identity. No
# re-implementation of target business logic is performed.

set -euo pipefail

SCRIPT_DIR="$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"
REPO_ROOT="$( cd "$SCRIPT_DIR/.." && pwd )"
TARGET="${REPO_ROOT}/scripts/test-upgrade-rehearsal-up.sh"

if [[ ! -f "$TARGET" ]]; then
    echo "FATAL: target script not found: $TARGET" >&2
    exit 2
fi

pass() { printf "  [OK]   %s\n" "$1"; }
declare -a _FAIL_LIST
fail() {
  printf "  [FAIL] %s\n" "$1"
  _FAIL_LIST+=( "$1" )
}
errs=()

# ============================================================================
# Static mask checks.
# ============================================================================
echo "== static mask checks =="

# (1) No `|| true` immediately on the asserted helm install / helm upgrade
#     line (look at the next 12 lines after each helm install/upgrade).
FOUND=
while IFS=: read -r ln _ src; do
    [[ "$src" == "$TARGET" ]] || continue
    ctx=$(sed -n "$((ln)),$((ln+12))p" "$TARGET")
    case "$ctx" in
        *'|| true'*) FOUND="${FOUND} ${ln}" ;;
    esac
done < <(grep -nE '^\s*\{\s*set\s\+-e$' "$TARGET")
if [[ -n "$FOUND" ]]; then
    fail "static (1): scrubbed-helm / set+-e envelope adjacent to helm install/upgrade at line(s): ${FOUND}"
fi
pass "static (1): no '|| true' adjacent to asserted helm install/upgrade"

# (2) Target must use the asserted-input helpers and required die-sites.
for token in "run_helm_capture" "S1RC=" "S2RC=" "S3RC=" "die " \
              "get_rev" "assert_numeric_rev" "capture_values_asserted" \
              "capture_manifest_asserted" "capture_np_asserted" \
              "assert_release_deployed"; do
    if ! grep -qF "$token" "$TARGET"; then
        fail "static (2): required token missing in target: $token"
    fi
done
pass "static (2): target uses run helpers / \$NRC / get_rev / assert_numeric_rev / capture_* / status helper"

# (3) Forbid `} || true` adjacency near an asserted helm install / upgrade.
while IFS=: read -r ln _; do
    ctx=$(sed -n "$((ln-9)),$((ln-1))p" "$TARGET")
    case "$ctx" in
        *helm\ install*|*helm\ upgrade*) fail "static (3): '|| true' near an asserted helm (line $ln)" ;;
    esac
done < <(grep -nE '^\s*\}\s*\|\|\s*true\s*$' "$TARGET")
pass "static (3): no } || true adjacent to asserted helm invocations"

# (4) Malformed command delimiter. If the literal token "----------echo"
#     appears anywhere in the target, fail (the original B1 regression was a
#     comment header accidentally glued to an echo command).
if grep -nF -- '----------echo' "$TARGET" >/dev/null 2>&1; then
    fail "static (4): malformed ----------echo command present in target"
fi
pass "static (4): no malformed ----------echo command in target"

# ============================================================================
# Shared stub generator (one quoted heredoc per body).
# ============================================================================
write_stubs() {
    local tmp="$1"
    mkdir -p "$tmp/stub_path" "$tmp/cluster_state"
    cat > "$tmp/stub_path/helm" <<'STUB_HELM_EOF'
#!/usr/bin/env bash
ts() { printf '[stub-helm %s] %s\n' "$(date +%s)" "$1" >> "${STUB_LOG_PATH:-/dev/null}"; }
ts "args: $*"
case "$1" in
    install)
        rc="${STUB_RC_STEP1:-0}"
        ts "install rc=$rc"
        exit "$rc"
        ;;
    upgrade)
        STUB_STATE="${STUB_STATE_PATH:-/dev/null}"
        idx=0
        if [[ -f "$STUB_STATE" ]]; then
            idx=$(awk -F= '/^idx=/{print $2; exit}' "$STUB_STATE" 2>/dev/null || echo 0)
        fi
        idx=$((idx+1))
        if [[ "$idx" -eq 1 ]]; then
            rc="${STUB_RC_STEP2:-0}"
            ts "upgrade1 enforce rc=$rc"
            printf 'idx=%d\n' "$idx" > "$STUB_STATE"
            cur_revision=5
            if [[ -f "${STUB_LIST_REV_PATH:-/dev/null}" ]]; then
                v=$(awk -F= '/^revision=/{print $2; exit}' "${STUB_LIST_REV_PATH}" 2>/dev/null)
                [[ -n "$v" ]] && cur_revision="$v"
            fi
            if [[ "$rc" -eq 0 ]]; then
                cur_revision=$((cur_revision+1))
            fi
            printf 'revision=%s\n' "$cur_revision" > "${STUB_LIST_REV_PATH}"
            exit "$rc"
        else
            rc="${STUB_RC_STEP3:-0}"
            ts "upgrade2 invalid rc=$rc"
            printf 'idx=%d\n' "$idx" > "$STUB_STATE"
            if [[ "${STUB_DRIFT_REV:-0}" == "1" ]]; then
                cur=$(awk -F= '/^revision=/{print $2; exit}' "${STUB_LIST_REV_PATH}" 2>/dev/null || echo 6)
                target_t="${STUB_DRIFT_REV_TO:-}"
                if [[ -n "$target_t" && "$target_t" =~ ^[0-9]+$ ]]; then
                    printf 'revision=%s\n' "$target_t" > "${STUB_LIST_REV_PATH}"
                else
                    cur_bumped=$((cur+93))
                    printf 'revision=%s\n' "$cur_bumped" > "${STUB_LIST_REV_PATH}"
                fi
            fi
            exit "$rc"
        fi
        ;;
    uninstall)
        ts "uninstall rc=0 best-effort"
        exit 0
        ;;
    list)
        cnt_list="${STUB_LIST_CALLS_PATH:-/dev/null}"
        nl=0
        if [[ -f "$cnt_list" ]]; then
            nl=$(awk -F= '/^nl=/{print $2; exit}' "$cnt_list" 2>/dev/null || echo 0)
        fi
        nl=$((nl+1))
        printf 'nl=%d\n' "$nl" > "$cnt_list"
        revision=5
        if [[ -f "${STUB_LIST_REV_PATH:-/dev/null}" ]]; then
            v=$(awk -F= '/^revision=/{print $2; exit}' "${STUB_LIST_REV_PATH}" 2>/dev/null)
            [[ -n "$v" ]] && revision="$v"
        fi
        echo '[{"name":"'"${STUB_RELEASE_NAME:-nexus-cni-upgrade}"'","revision":"'"$revision"'","status":"deployed","chart":"nexus-9.9.9","namespace":"default"}]'
        exit 0
        ;;
    status)
        cnt_status="${STUB_STATUS_CALLS_PATH:-/dev/null}"
        ns=0
        if [[ -f "$cnt_status" ]]; then
            ns=$(awk -F= '/^ns=/{print $2; exit}' "$cnt_status" 2>/dev/null || echo 0)
        fi
        ns=$((ns+1))
        printf 'ns=%s\n' "$ns" > "$cnt_status"
        fail_from="${STUB_STATUS_FAIL_FROM:-999999}"
        fail_to="${STUB_STATUS_FAIL_TO:-999999}"
        if [[ "$ns" -ge "$fail_from" && "$ns" -le "$fail_to" ]]; then
            echo '{"name":"'"${STUB_RELEASE_NAME:-nexus-cni-upgrade}"'","info":{"status":"'"${STUB_STATUS_FAIL_VALUE:-failed}"'"}}'
            exit "${STUB_STATUS_FAIL_RC:-0}"
        fi
        echo '{"name":"'"${STUB_RELEASE_NAME:-nexus-cni-upgrade}"'","info":{"status":"deployed"}}'
        exit 0
        ;;
    get)
        sub="$2"
        case "$sub" in
            values)
                cnt_file="${STUB_GET_VALUES_CALLS_PATH:-/dev/null}"
                n=0
                if [[ -f "$cnt_file" ]]; then
                    n=$(awk -F= '/^n=/{print $2; exit}' "$cnt_file" 2>/dev/null || echo 0)
                fi
                n=$((n+1))
                printf 'idx=1\nn=%d\n' "$n" > "$cnt_file"
                vf_from="${STUB_GET_VALUES_FAIL_FROM:-999999}"
                if [[ "$n" -ge "$vf_from" ]]; then
                    echo "stub-helm: forced values failure on call $n" >&2
                    exit "${STUB_GET_VALUES_FAIL_RC:-1}"
                fi
                if [[ "${STUB_DRIFT_VALUES:-0}" == "1" && "$n" -ge 4 ]]; then
                    cat "${STUB_VALUE_DRIFT_PATH:-/dev/null}"
                else
                    cat "${STUB_VALUE_BASELINE_PATH:-/dev/null}"
                fi
                exit 0
                ;;
            manifest)
                cnt_file="${STUB_GET_MANIFEST_CALLS_PATH:-/dev/null}"
                n=0
                if [[ -f "$cnt_file" ]]; then
                    n=$(awk -F= '/^n=/{print $2; exit}' "$cnt_file" 2>/dev/null || echo 0)
                fi
                n=$((n+1))
                printf 'idx=1\nn=%d\n' "$n" > "$cnt_file"
                mf_from="${STUB_GET_MANIFEST_FAIL_FROM:-999999}"
                if [[ "$n" -ge "$mf_from" ]]; then
                    echo "stub-helm: forced manifest failure on call $n" >&2
                    exit "${STUB_GET_MANIFEST_FAIL_RC:-1}"
                fi
                if [[ "${STUB_DRIFT_MANIFEST:-0}" == "1" && "$n" -ge 3 ]]; then
                    cat "${STUB_MANIFEST_DRIFT_PATH:-/dev/null}"
                else
                    cat "${STUB_MANIFEST_BASELINE_PATH:-/dev/null}"
                fi
                exit 0
                ;;
        esac
        exit 0
        ;;
esac
ts "unknown helm subcommand $1"
exit 0
STUB_HELM_EOF
    chmod 0755 "$tmp/stub_path/helm"

    cat > "$tmp/stub_path/kubectl" <<'STUB_KC_EOF'
#!/usr/bin/env bash
case "$1" in
    get)
        if [[ "$2" == "netpol" ]]; then
            cnt_file="${STUB_KC_NETPOL_CALLS_PATH:-/dev/null}"
            n=0
            if [[ -f "$cnt_file" ]]; then
                n=$(awk -F= '/^n=/{print $2; exit}' "$cnt_file" 2>/dev/null || echo 0)
            fi
            n=$((n+1))
            printf 'n=%d\n' "$n" > "$cnt_file"
            npf_from="${STUB_KC_NETPOL_FAIL_FROM:-999999}"
            if [[ "$n" -ge "$npf_from" ]]; then
                echo "stub-kc: forced netpol failure on call $n" >&2
                exit "${STUB_KC_NETPOL_FAIL_RC:-1}"
            fi
            invalid_kinds="${STUB_NP_INVALID_FROM:-}"
            if [[ -n "${invalid_kinds}" ]]; then
                IFS=',' read -ra PAIRS_IV <<< "${invalid_kinds}"
                for pair in "${PAIRS_IV[@]}"; do
                    IFS=':' read iv_from iv_kind <<< "$pair"
                    if [[ "${n}" == "${iv_from}" ]]; then
                        case "${iv_kind}" in
                            empty_doc) printf ''; printf ''; exit 0 ;;
                            bad_json) printf 'this is not { valid JSON {' ; printf ''; exit 0 ;;
                            scalar_top) printf '"just a string"' ; printf ''; exit 0 ;;
                            array_top) printf '[]' ; printf ''; exit 0 ;;
                            object_no_items) printf '{"kind":"List","apiVersion":"v1"}' ; exit 0 ;;
                            items_not_list) printf '{"items":"not-a-list"}' ; exit 0 ;;
                            *) ;;
                        esac
                    fi
                done
            fi
            step3_drift="${STUB_NP_STEP3_DRIFT_FROM:-999999}"
            step4_drift="${STUB_NP_STEP4_DRIFT_FROM:-999999}"
            if [[ "${n}" -ge "${step3_drift}" && "${n}" -lt "${step4_drift}" ]]; then
                drift_doc="$(cat "${STUB_NP_STEP3_DRIFT_PATH:-${STUB_NP_BASELINE_PATH:-/dev/null}}" 2>/dev/null || printf '')"
                if [[ -n "${drift_doc}" ]]; then
                    printf '%s' "${drift_doc}"
                    exit 0
                fi
            fi
            if [[ "${n}" -ge "${step4_drift}" ]]; then
                drift_doc="$(cat "${STUB_NP_STEP4_DRIFT_PATH:-${STUB_NP_BASELINE_PATH:-/dev/null}}" 2>/dev/null || printf '')"
                if [[ -n "${drift_doc}" ]]; then
                    printf '%s' "${drift_doc}"
                    exit 0
                fi
            fi
            # Phase model: n=1 (Step 1) responds with the disabled baseline.
            # n>=2 (Step 2/3/4) responds with the enforced baseline; the
            # optional drift overlays above override when configured.
            if [[ "${n}" == "1" && -n "${STUB_NP_STEP1_PATH:-}" ]]; then
                cat "${STUB_NP_STEP1_PATH}" 2>/dev/null
                exit 0
            fi
            base_path="${STUB_NP_BASELINE_PATH:-${STUB_NP_PATH:-/dev/null}}"
            base_doc="$(cat "${base_path}" 2>/dev/null || printf '{"kind":"List","apiVersion":"v1","items":[]}')"
            printf '%s' "${base_doc}"
            exit 0
        fi
        if [[ "$2" == "version" ]]; then
            echo '{"clientVersion":{"major":"1","minor":"30"}}'
            exit 0
        fi
        ;;
esac
echo '[]'
exit 0
STUB_KC_EOF
    chmod 0755 "$tmp/stub_path/kubectl"
}

# Seed baseline + (optional) drift cluster-state files.
seed_cluster_state() {
    local clusterdir="$1" post_state="$2"
    cat > "$clusterdir/np.json" <<'EOF_LIST_JSON'
{"kind":"List","apiVersion":"v1","metadata":{"resourceVersion":"1"},"items":[]}
EOF_LIST_JSON
    printf '[]\n' > "$clusterdir/np_legacy.json"
    # Disabled surface captured at Step 1.
    cat > "$clusterdir/np_step1_disabled.json" <<'EOF_LIST_DISABLED'
{"kind":"List","apiVersion":"v1","metadata":{"resourceVersion":"2"},"items":[]}
EOF_LIST_DISABLED
    # Enforced surface captured at Step 2 onward.
    cat > "$clusterdir/np_baseline.json" <<'EOF_LIST_ENFORCED'
{"kind":"List","apiVersion":"v1","metadata":{"resourceVersion":"3"},"items":[{"apiVersion":"networking.k8s.io/v1","kind":"NetworkPolicy","metadata":{"name":"nexus","namespace":"default","labels":{"app":"nexus"}},"spec":{"podSelector":{"matchLabels":{"role":"nexus"}},"ingress":[{"from":[{"namespaceSelector":{"matchLabels":{"name":"kube-system"}}}],"ports":[{"protocol":"TCP","port":8080}]}]},"DRIFT":"enforced"}]}
EOF_LIST_ENFORCED
    cat > "$clusterdir/np_step3_drift.json" <<'EOF_LIST_STEP3'
{"kind":"List","apiVersion":"v1","metadata":{"resourceVersion":"4"},"items":[{"apiVersion":"networking.k8s.io/v1","kind":"NetworkPolicy","metadata":{"name":"nexus","namespace":"default","labels":{"app":"nexus"}},"spec":{"podSelector":{"matchLabels":{"role":"nexus"}},"ingress":[{"from":[{"namespaceSelector":{"matchLabels":{"name":"kube-system"}}}],"ports":[{"protocol":"TCP","port":9090}]}]},"DRIFT":"step3"}]}
EOF_LIST_STEP3
    cat > "$clusterdir/np_step4_drift.json" <<'EOF_LIST_STEP4'
{"kind":"List","apiVersion":"v1","metadata":{"resourceVersion":"5"},"items":[{"apiVersion":"networking.k8s.io/v1","kind":"NetworkPolicy","metadata":{"name":"nexus","namespace":"default","labels":{"app":"nexus"}},"spec":{"podSelector":{"matchLabels":{"role":"nexus"}},"ingress":[{"from":[{"namespaceSelector":{"matchLabels":{"name":"kube-system"}}}],"ports":[{"protocol":"TCP","port":9091}]}]},"DRIFT":"step4"}]}
EOF_LIST_STEP4
    printf 'networkPolicy:\n  mode: enforce\n  profile: enterprise\n  allowedPeerPorts: []\n' > "$clusterdir/values_baseline.yaml"
    printf 'networkPolicy:\n  mode: enforce\n  profile: enterprise\n  allowedPeerPorts: []\n  DRIFT: true\n' > "$clusterdir/values_drift.yaml"
    printf '# NetworkPolicy manifest after step 2 enforce\nkind: NetworkPolicy\nspec:\n  podSelector: {}\n' > "$clusterdir/manifest_baseline.txt"
    printf '# NetworkPolicy manifest after step 2 enforce\nkind: NetworkPolicy\nspec:\n  podSelector: {}\n  DRIFT: true\n' > "$clusterdir/manifest_drift.txt"
}

# Configure knobs for one control invocation, then run the target once.
# Args: label rc_step1 rc_step2 rc_step3 drift_kind
#   drift_kind ∈ {same, revision-drift, values-drift, manifest-drift}
run_control() {
    local label="$1"
    local rc_step1="$2" rc_step2="$3" rc_step3="$4"
    local drift_kind="$5"

    local tmp artdir clusterdir
    tmp="$(mktemp -d -t d2b-up-c-XXXXXX)"
    artdir="$tmp/external_artifacts"
    clusterdir="$tmp/cluster_state"
    mkdir -p "$artdir" "$clusterdir"
    write_stubs "$tmp"
    seed_cluster_state "$clusterdir" "$drift_kind"
    printf 'before=5\nafter=6\n' > "$tmp/list_rev"
    printf 'cluster-up-ok\n' > "$tmp/cluster-up.txt"

    export STUB_RELEASE_NAME="nexus-cni-upgrade"
    export STUB_RC_STEP1="$rc_step1"
    export STUB_RC_STEP2="$rc_step2"
    export STUB_RC_STEP3="$rc_step3"
    export STUB_STATE_PATH="$tmp/state"
    : > "$tmp/state"
    export STUB_LIST_REV_PATH="$tmp/list_rev"
    export STUB_NP_PATH="$clusterdir/np.json"
    export STUB_NP_BASELINE_PATH="$clusterdir/np_baseline.json"
    export STUB_NP_STEP1_PATH="$clusterdir/np_step1_disabled.json"
    export STUB_NP_STEP3_DRIFT_FROM="${STUB_NP_STEP3_DRIFT_FROM:-999999}"
    export STUB_NP_STEP4_DRIFT_FROM="${STUB_NP_STEP4_DRIFT_FROM:-999999}"
    export STUB_NP_STEP3_DRIFT_PATH="${STUB_NP_STEP3_DRIFT_PATH:-$clusterdir/np_step3_drift.json}"
    export STUB_NP_STEP4_DRIFT_PATH="${STUB_NP_STEP4_DRIFT_PATH:-$clusterdir/np_step4_drift.json}"
    export STUB_NP_INVALID_FROM=""
    export STUB_LOG_PATH="$tmp/stub.log"
    export STUB_GET_VALUES_CALLS_PATH="$tmp/get_values_calls"
    export STUB_GET_MANIFEST_CALLS_PATH="$tmp/get_manifest_calls"
    export STUB_VALUE_BASELINE_PATH="$clusterdir/values_baseline.yaml"
    export STUB_VALUE_DRIFT_PATH="$clusterdir/values_drift.yaml"
    export STUB_MANIFEST_BASELINE_PATH="$clusterdir/manifest_baseline.txt"
    export STUB_MANIFEST_DRIFT_PATH="$clusterdir/manifest_drift.txt"
    export STUB_DRIFT_REV=0
    export STUB_DRIFT_VALUES=0
    export STUB_DRIFT_MANIFEST=0
    export STUB_DRIFT_REV_TO=""
    export STUB_STATUS_CALLS_PATH="$tmp/status_calls"
    : > "$tmp/status_calls"
    export STUB_LIST_CALLS_PATH="$tmp/list_calls"
    : > "$tmp/list_calls"
    export STUB_KC_NETPOL_CALLS_PATH="$tmp/kc_netpol_calls"
    : > "$tmp/kc_netpol_calls"
    export STUB_STATUS_FAIL_FROM="999999"
    export STUB_STATUS_FAIL_TO="999999"
    export STUB_STATUS_FAIL_VALUE="failed"
    export STUB_STATUS_FAIL_RC="0"
    export STUB_GET_VALUES_FAIL_FROM="999999"
    export STUB_GET_VALUES_FAIL_RC="1"
    export STUB_GET_MANIFEST_FAIL_FROM="999999"
    export STUB_GET_MANIFEST_FAIL_RC="1"
    export STUB_KC_NETPOL_FAIL_FROM="999999"
    export STUB_KC_NETPOL_FAIL_RC="1"
    case "$drift_kind" in
        revision-drift) export STUB_DRIFT_REV=1 ;;
        values-drift)   export STUB_DRIFT_VALUES=1 ;;
        manifest-drift) export STUB_DRIFT_MANIFEST=1 ;;
    esac

    export PATH="$tmp/stub_path:$PATH"
    mkdir -p "$tmp/fake_chart" "$tmp/fixtures/integrationcni"
    printf 'networkPolicy:\n  allowedPeerPorts: []\n' > "$tmp/fixtures/integrationcni/values-extra-cni.yaml"
    export VALUES_EXTRA="$tmp/fixtures/integrationcni/values-extra-cni.yaml"
    export ARTIFACTS="$artdir"
    export CHART_PATH="$tmp/fake_chart"
    export RELEASE="nexus-cni-upgrade"
    cp "$tmp/cluster-up.txt" "$artdir/cluster-up.txt"

    HELM_STUB_PATH_OUT="$(command -v helm)"
    KC_STUB_PATH_OUT="$(command -v kubectl)"
    PY_REAL_PATH_OUT="$(command -v python3)"
    printf '  [%s] command -v helm -> %s\n' "$label" "$HELM_STUB_PATH_OUT"
    printf '  [%s] command -v kubectl -> %s\n' "$label" "$KC_STUB_PATH_OUT"
    printf '  [%s] command -v python3 (real) -> %s\n' "$label" "$PY_REAL_PATH_OUT"

    local rc=0
    bash "$TARGET" 2>"$tmp/control.err" 1>"$tmp/control.out" || rc=$?

    unset STUB_RC_STEP1 STUB_RC_STEP2 STUB_RC_STEP3 STUB_STATE_PATH \
          STUB_LIST_REV_PATH STUB_NP_PATH STUB_LOG_PATH \
          STUB_GET_VALUES_CALLS_PATH STUB_GET_MANIFEST_CALLS_PATH \
          STUB_VALUE_BASELINE_PATH STUB_VALUE_DRIFT_PATH \
          STUB_MANIFEST_BASELINE_PATH STUB_MANIFEST_DRIFT_PATH \
          STUB_DRIFT_REV STUB_DRIFT_VALUES STUB_DRIFT_MANIFEST \
          STUB_DRIFT_REV_TO STUB_STATUS_CALLS_PATH STUB_LIST_CALLS_PATH \
          STUB_KC_NETPOL_CALLS_PATH \
          STUB_NP_BASELINE_PATH STUB_NP_STEP1_PATH \
          STUB_NP_STEP3_DRIFT_FROM STUB_NP_STEP4_DRIFT_FROM \
          STUB_NP_STEP3_DRIFT_PATH STUB_NP_STEP4_DRIFT_PATH STUB_NP_INVALID_FROM \
          STUB_STATUS_FAIL_FROM STUB_STATUS_FAIL_TO STUB_STATUS_FAIL_VALUE STUB_STATUS_FAIL_RC \
          STUB_GET_VALUES_FAIL_FROM STUB_GET_VALUES_FAIL_RC \
          STUB_GET_MANIFEST_FAIL_FROM STUB_GET_MANIFEST_FAIL_RC \
          STUB_KC_NETPOL_FAIL_FROM STUB_KC_NETPOL_FAIL_RC \
          STUB_RELEASE_NAME

    printf "  [%s] target rc=%d\n" "$label" "$rc"
    for s in upgrade-step1.txt upgrade-step2.txt upgrade-step3.txt upgrade-step4.txt; do
        if [[ -f "$artdir/$s" ]]; then
            printf "      %s -> %s\n" "$s" "$(cat "$artdir/$s")"
        else
            printf "      %s -> sentinel-absent\n" "$s"
        fi
    done

    printf '%s\n' "$rc" > "$tmp/result.rc"
    printf '%s\n' "$artdir" > "$tmp/result.artdir"
    cp "$tmp/control.err" "$tmp/result.err.txt"
    echo "$tmp" > /tmp/.d2b-up-last-tmp
}

# ============================================================================
# Control 1 — happy path.
# ============================================================================
echo "== Control 1: happy ==="
run_control "C1-happy" 0 0 7 same
T1=$(cat /tmp/.d2b-up-last-tmp)
RC1=$(cat "$T1/result.rc")
ADIR1=$(cat "$T1/result.artdir")
ERR1=$(cat "$T1/result.err.txt")
[[ "$RC1" -eq 0 ]] && pass "control 1: script rc=0" || fail "control 1: expected rc=0, got $RC1; stderr=${ERR1}"
S1=$(cat "$ADIR1/upgrade-step1.txt" 2>/dev/null || true)
S2=$(cat "$ADIR1/upgrade-step2.txt" 2>/dev/null || true)
S3=$(cat "$ADIR1/upgrade-step3.txt" 2>/dev/null || true)
S4=$(cat "$ADIR1/upgrade-step4.txt" 2>/dev/null || true)
[[ "$S1" == "install-disabled-ok" ]] && pass "control 1: step1 sentinel" || fail "control 1: step1 sentinel wrong: '${S1}'"
[[ "$S2" == "upgrade-enforce-ok" ]] && pass "control 1: step2 sentinel" || fail "control 1: step2 sentinel wrong: '${S2}'"
[[ "$S3" == "rejected-invalid-upgrade-ok" ]] && pass "control 1: step3 sentinel" || fail "control 1: step3 sentinel wrong: '${S3}'"
[[ "$S4" == "state-preserved-after-rejected-upgrade" ]] && pass "control 1: step4 sentinel" || fail "control 1: step4 sentinel wrong: '${S4}'"
RC_FILE_1=$(cat "$ADIR1/upgrade-step1.rc" 2>/dev/null || echo missing)
RC_FILE_3=$(cat "$ADIR1/upgrade-step3.rc" 2>/dev/null || echo missing)
[[ "$RC_FILE_1" == "0" ]] && pass "control 1: upgrade-step1.rc=0" || fail "control 1: upgrade-step1.rc='${RC_FILE_1}'"
[[ "$RC_FILE_3" =~ ^[0-9]+$ && "$RC_FILE_3" -ne 0 ]] && pass "control 1: upgrade-step3.rc=${RC_FILE_3} (non-zero)" || fail "control 1: upgrade-step3.rc='${RC_FILE_3}' (must be non-zero)"
RB=$(cat "$ADIR1/upgrade-revision-before.txt" 2>/dev/null || echo missing)
RA=$(cat "$ADIR1/upgrade-revision-after.txt" 2>/dev/null || echo missing)
RR=$(cat "$ADIR1/upgrade-rev-step3-after.txt" 2>/dev/null || echo missing)
RSB3=$(cat "$ADIR1/upgrade-rev-step3-before.txt" 2>/dev/null || echo missing)
R4=$(cat "$ADIR1/upgrade-step4-revision.txt" 2>/dev/null || echo missing)
[[ "$RB" =~ ^[0-9]+$ ]] && pass "control 1: S2_REV_BEFORE='${RB}' numeric" || fail "control 1: S2_REV_BEFORE='${RB}' not numeric (B6)"
[[ "$RA" =~ ^[0-9]+$ ]] && pass "control 1: S2_REV_AFTER='${RA}' numeric" || fail "control 1: S2_REV_AFTER='${RA}' not numeric (B6)"
[[ "$RA" -gt "$RB" ]] && pass "control 1: S2_REV_AFTER(${RA}) > S2_REV_BEFORE(${RB}) (B6)" || fail "control 1: revision did not increment (B6): before=${RB} after=${RA}"
[[ "$RR" =~ ^[0-9]+$ ]] && pass "control 1: S3_REV_AFTER='${RR}' numeric" || fail "control 1: S3_REV_AFTER='${RR}' not numeric"
[[ "$RSB3" =~ ^[0-9]+$ ]] && pass "control 1: S3_REV_BEFORE='${RSB3}' numeric" || fail "control 1: S3_REV_BEFORE='${RSB3}' not numeric"
[[ "$R4" =~ ^[0-9]+$ ]] && pass "control 1: S4_REV='${R4}' numeric" || fail "control 1: S4_REV='${R4}' not numeric"
[[ "$R4" == "$RR" ]] && pass "control 1: S4_REV(${R4}) == S3_REV_AFTER(${RR})" || fail "control 1: S4_REV(${R4}) != S3_REV_AFTER(${RR})"
[[ "$RSB3" == "$RA" ]] && pass "control 1: S3_REV_BEFORE(${RSB3}) == S2_REV_AFTER(${RA})" || fail "control 1: S3_REV_BEFORE(${RSB3}) != S2_REV_AFTER(${RA})"
[[ "$RR" == "$RA" ]] && pass "control 1: S3_REV_AFTER(${RR}) == S2_REV_AFTER(${RA})" || fail "control 1: S3_REV_AFTER(${RR}) != S2_REV_AFTER(${RA})"
SHA2=$(cat "$ADIR1/upgrade-step2-manifest-id" 2>/dev/null || echo missing)
SHA4=$(cat "$ADIR1/upgrade-step4-manifest-id" 2>/dev/null || echo missing)
[[ -n "$SHA2" && "$SHA2" == "$SHA4" ]] && pass "control 1: manifest sha256 stable (${SHA2:0:12})" || fail "control 1: manifest sha256 drifted: ${SHA2} vs ${SHA4}"
[[ -s "$ADIR1/upgrade-step2-values.yaml" ]] && pass "control 1: upgrade-step2-values.yaml non-empty" || fail "control 1: upgrade-step2-values.yaml empty/absent"
[[ -s "$ADIR1/upgrade-step4-values-post.yaml" ]] && pass "control 1: upgrade-step4-values-post.yaml non-empty" || fail "control 1: upgrade-step4-values-post.yaml empty/absent"
# R4 happy: status log files written and contain "deployed".
for s in S1 S2 S3 S4; do
    case "$s" in
        S1) sf="$ADIR1/upgrade-step1-status.log.status" ;;
        S2) sf="$ADIR1/upgrade-step2-status.log.status" ;;
        S3) sf="$ADIR1/upgrade-step3-status.log.status" ;;
        S4) sf="$ADIR1/upgrade-step4-status.log.status" ;;
    esac
    [[ -s "$sf" && "$(cat "$sf")" == "deployed" ]] && pass "control 1: ${s} status= deployed (file ${sf##*/})" || fail "control 1: ${s} status file missing/non-deployed: '$sf'/"
done
# Raw NetPol artifacts must be valid Kubernetes List documents.
for n in upgrade-step1-np.json upgrade-step2-np.json upgrade-step3-np-post.json upgrade-step4-np-post.json; do
    f="$ADIR1/$n"
    rc_v=$("${PYTHON3_BIN:-python3}" - <<PYEOF
import json, sys
try:
    with open("$f", "r") as fh:
        doc = json.loads(fh.read())
except Exception as e:
    print("PARSE_FAIL:"+repr(e)); sys.exit(2)
if not isinstance(doc, dict):
    print("NOT_OBJECT"); sys.exit(2)
items = doc.get("items")
if not isinstance(items, list):
    print("ITEMS_NOT_LIST"); sys.exit(2)
print("OK")
PYEOF
    )
    [[ "$rc_v" == "OK" ]] && pass "control 1: ${n} is a Kubernetes List document" || fail "control 1: ${n} shape parse '${rc_v}'"
done
# Semantic NetPol identity must be equal across the enforced snapshot,
# the post-rejection snapshot, and the final check. The pre-Step-3
# comparison is enforced inside the target script and surfaced here.
# Required Step-1 disabled capture + Step-1 != Step-2 transition + Step-2
# raw items length >= 1 ensure the rehearsal actually produces an enforced
# NetworkPolicy surface, not merely a stable disabled one.
NP1=$(cat "$ADIR1/upgrade-step1-np-id" 2>/dev/null || echo missing)
NP2=$(cat "$ADIR1/upgrade-step2-np-id" 2>/dev/null || echo missing)
NP3=$(cat "$ADIR1/upgrade-step3-np-post-id" 2>/dev/null || echo missing)
NP4=$(cat "$ADIR1/upgrade-step4-np-post-id" 2>/dev/null || echo missing)
[[ -n "$NP1" && "$NP1" != "missing" ]] && pass "control 1: step1 NetPol identity present (${NP1:0:12})" || fail "control 1: step1 NetPol identity missing (assert_np_transition was bypassed)"
# Step-2 raw JSON must parse and have at least one NetworkPolicy item.
NP2_RAW="$ADIR1/upgrade-step2-np.json"
NP2_ITEMS_LEN="$("${PYTHON3_BIN:-python3}" - "$NP2_RAW" <<'PY_LEN'
import sys, json
with open(sys.argv[1], "r", encoding="utf-8") as f:
    raw = f.read()
doc = json.loads(raw)
items = doc.get("items")
if not isinstance(items, list):
    print("ITEMS_NOT_LIST"); sys.exit(0)
print(len(items))
PY_LEN
)"
[[ "${NP2_ITEMS_LEN}" =~ ^[0-9]+$ && "${NP2_ITEMS_LEN}" -ge 1 ]] && pass "control 1: step2 raw items length ${NP2_ITEMS_LEN} >= 1" || fail "control 1: step2 raw items length invalid (got '${NP2_ITEMS_LEN}')"
[[ -n "$NP2" && "$NP2" != "missing" ]] && pass "control 1: step2 NetPol identity present (${NP2:0:12})" || fail "control 1: step2 NetPol identity missing"
[[ -n "$NP1" && -n "$NP2" && "$NP1" != missing && "$NP2" != missing && "$NP1" != "$NP2" ]] && pass "control 1: step1 NetPol identity (${NP1:0:12}) != step2 (${NP2:0:12}) (transition present)" || fail "control 1: step1 NetPol identity equal to step2 (NP1=${NP1:0:12} NP2=${NP2:0:12})"
[[ "$NP3" == "$NP2" ]] && pass "control 1: step3 NetPol identity == step2" || fail "control 1: step3 NetPol identity drifted (step2=${NP2:0:12} step3=${NP3:0:12})"
[[ "$NP4" == "$NP2" ]] && pass "control 1: step4 NetPol identity == step2" || fail "control 1: step4 NetPol identity drifted (step2=${NP2:0:12} step4=${NP4:0:12})"
[[ "$NP4" == "$NP3" ]] && pass "control 1: step4 NetPol identity == step3" || fail "control 1: step4 NetPol identity drifted (step3=${NP3:0:12} step4=${NP4:0:12})"
# Exact sentinel strings.
for s1_desired in "install-disabled-ok" "upgrade-enforce-ok" "rejected-invalid-upgrade-ok" "state-preserved-after-rejected-upgrade"; do
    case "$s1_desired" in
        install-disabled-ok) f="$ADIR1/upgrade-step1.txt" ;;
        upgrade-enforce-ok) f="$ADIR1/upgrade-step2.txt" ;;
        rejected-invalid-upgrade-ok) f="$ADIR1/upgrade-step3.txt" ;;
        state-preserved-after-rejected-upgrade) f="$ADIR1/upgrade-step4.txt" ;;
    esac
    [[ "$(cat "$f" 2>/dev/null)" == "$s1_desired" ]] && pass "control 1: exact sentinel '${s1_desired}'" || fail "control 1: sentinel file $f content not equal to '${s1_desired}' (got '$(cat "$f" 2>/dev/null)')"
done

# ============================================================================
# Control 2 — install failure.
# ============================================================================
echo "== Control 2: install failure ==="
run_control "C2-install-fail" 2 0 7 same
T2=$(cat /tmp/.d2b-up-last-tmp)
RC2=$(cat "$T2/result.rc")
ADIR2=$(cat "$T2/result.artdir")
[[ "$RC2" -ne 0 ]] && pass "control 2: script non-zero (rc=$RC2)" || fail "control 2: expected non-zero, got $RC2"
# All four expected sentinels must be absent on install failure.
for s in upgrade-step1.txt upgrade-step2.txt upgrade-step3.txt upgrade-step4.txt; do
    SV=$(cat "$ADIR2/$s" 2>/dev/null || true)
    [[ ! -f "$ADIR2/$s" && -z "$SV" ]] && pass "control 2: $s sentinel-absent" || fail "control 2: $s present ('$SV')"
done

# ============================================================================
# Control 3 — enforce upgrade fails.
# ============================================================================
echo "== Control 3: enforce upgrade fails ==="
run_control "C3-enforce-fail" 0 3 7 same
T3=$(cat /tmp/.d2b-up-last-tmp)
RC3=$(cat "$T3/result.rc")
ADIR3=$(cat "$T3/result.artdir")
[[ "$RC3" -ne 0 ]] && pass "control 3: script non-zero (rc=$RC3)" || fail "control 3: expected non-zero, got $RC3"
S1=$(cat "$ADIR3/upgrade-step1.txt" 2>/dev/null || true)
[[ "$S1" == "install-disabled-ok" ]] && pass "control 3: step1 sentinel recorded" || fail "control 3: step1 sentinel '${S1}' wrong/missing"
for s in upgrade-step2.txt upgrade-step3.txt upgrade-step4.txt; do
    SV=$(cat "$ADIR3/$s" 2>/dev/null || true)
    [[ ! -f "$ADIR3/$s" && -z "$SV" ]] && pass "control 3: $s sentinel-absent" || fail "control 3: $s present ('$SV')"
done

# ============================================================================
# Control 4 — invalid upgrade ACCEPTED is a CONTRACT FAILURE.
# ============================================================================
echo "== Control 4: invalid upgrade unexpectedly accepted ==="
run_control "C4-step3-zero" 0 0 0 same
T4=$(cat /tmp/.d2b-up-last-tmp)
RC4=$(cat "$T4/result.rc")
ADIR4=$(cat "$T4/result.artdir")
ERR4=$(cat "$T4/result.err.txt")
[[ "$RC4" -ne 0 ]] && pass "control 4: script non-zero (rc=$RC4)" || fail "control 4: expected non-zero, got $RC4"
# Step 1/2 sentinels may legitimately be present (install succeeded, enforce
# upgrade succeeded); Step 3 acceptance is the contract failure and the
# subsequent expected outcome is BOTH step3 AND step4 absent.
for s in upgrade-step3.txt upgrade-step4.txt; do
    SV=$(cat "$ADIR4/$s" 2>/dev/null || true)
    [[ ! -f "$ADIR4/$s" && -z "$SV" ]] && pass "control 4: $s sentinel-absent" || fail "control 4: $s present ('$SV')"
done

# ============================================================================
# Control 5 — revision drift after rejected upgrade.
# ============================================================================
echo "== Control 5: revision drift ==="
run_control "C5-revision-drift" 0 0 7 revision-drift
T5=$(cat /tmp/.d2b-up-last-tmp)
RC5=$(cat "$T5/result.rc")
ADIR5=$(cat "$T5/result.artdir")
ERR5=$(cat "$T5/result.err.txt")
S4=$(cat "$ADIR5/upgrade-step4.txt" 2>/dev/null || true)
S3=$(cat "$ADIR5/upgrade-step3.txt" 2>/dev/null || true)
[[ "$RC5" -ne 0 ]] && pass "control 5: target non-zero on revision drift (rc=$RC5)" || fail "control 5: target rc=0 on revision drift"
[[ -z "$S4" || "$S4" != "state-preserved-after-rejected-upgrade" ]] && pass "control 5: no false-positive state-preserved sentinel" || fail "control 5: state-preserved emitted despite drift"
# R3 contract: revision drift in step 3 means S3_REV_AFTER != S2_REV_AFTER and
# the target must die BEFORE writing upgrade-step3.txt; absence is the
# correct outcome.
[[ -z "$S3" || "$S3" != "rejected-invalid-upgrade-ok" ]] && pass "control 5: rejected-invalid-upgrade-ok absent (die before rejection sentinel on rev drift)" || fail "control 5: rejected-invalid-upgrade-ok emitted despite revision drift"
[[ -n "$ERR5" && ( "$ERR5" == *"revision mismatch"* || "$ERR5" == *"S3_REV_AFTER"* || "$ERR5" == *"enforced-state preservation violated"* ) ]] && pass "control 5: stderr identifies revision mismatch" || fail "control 5: stderr did not identify revision mismatch: '${ERR5}'"

# ============================================================================
# Control 6 — values drift after rejected upgrade.
# ============================================================================
echo "== Control 6: values drift ==="
run_control "C6-values-drift" 0 0 7 values-drift
T6=$(cat /tmp/.d2b-up-last-tmp)
RC6=$(cat "$T6/result.rc")
ADIR6=$(cat "$T6/result.artdir")
ERR6=$(cat "$T6/result.err.txt")
S4=$(cat "$ADIR6/upgrade-step4.txt" 2>/dev/null || true)
[[ "$RC6" -ne 0 ]] && pass "control 6: target non-zero on values drift (rc=$RC6)" || fail "control 6: target rc=0 on values drift"
[[ -z "$S4" || "$S4" != "state-preserved-after-rejected-upgrade" ]] && pass "control 6: no false-positive state-preserved sentinel" || fail "control 6: state-preserved emitted despite values drift"
[[ -n "$ERR6" && "$ERR6" == *"values identity drifted"* ]] && pass "control 6: stderr identifies values drift" || fail "control 6: stderr did not identify values drift: '${ERR6}'"

# ============================================================================
# Control 7 — manifest drift after rejected upgrade.
# ============================================================================
echo "== Control 7: manifest drift ==="
run_control "C7-manifest-drift" 0 0 7 manifest-drift
T7=$(cat /tmp/.d2b-up-last-tmp)
RC7=$(cat "$T7/result.rc")
ADIR7=$(cat "$T7/result.artdir")
ERR7=$(cat "$T7/result.err.txt")
S4=$(cat "$ADIR7/upgrade-step4.txt" 2>/dev/null || true)
[[ "$RC7" -ne 0 ]] && pass "control 7: target non-zero on manifest drift (rc=$RC7)" || fail "control 7: target rc=0 on manifest drift"
[[ -z "$S4" || "$S4" != "state-preserved-after-rejected-upgrade" ]] && pass "control 7: no false-positive state-preserved sentinel" || fail "control 7: state-preserved emitted despite manifest drift"
[[ -n "$ERR7" && ( "$ERR7" == *"manifest identity drifted"* || "$ERR7" == *"sha256 differs"* ) ]] && pass "control 7: stderr identifies manifest drift" || fail "control 7: stderr did not identify manifest drift: '${ERR7}'"

# ============================================================================
# Control 8 — malformed-command regression.
# ============================================================================
echo "== Control 8: malformed-command regression ==="
MUTATED_TARGET="/tmp/d2b-up-mutated-target-$$.sh"
sed 's|^echo "\[upgrade\] step 4/4: enforced-state preservation across rejected upgrade"$|----------echo "[upgrade] step 4/4: enforced-state preservation across rejected upgrade"|' \
    "$TARGET" > "$MUTATED_TARGET"
chmod 0755 "$MUTATED_TARGET"
if grep -nF -- '----------echo' "$MUTATED_TARGET" >/dev/null 2>&1; then
    pass "control 8: mutated target contains B1 regression literal"
else
    fail "control 8: mutated target does not contain B1 regression literal"
fi
TMP8="$(mktemp -d -t d2b-up-c-XXXXXX)"
ARTDIR8="$TMP8/external_artifacts"
CLUSDIR8="$TMP8/cluster_state"
mkdir -p "$ARTDIR8" "$CLUSDIR8"
write_stubs "$TMP8"
seed_cluster_state "$CLUSDIR8" "same"
printf 'before=5\nafter=6\n' > "$TMP8/list_rev"
printf 'cluster-up-ok\n' > "$TMP8/cluster-up.txt"
export STUB_RELEASE_NAME="nexus-cni-upgrade"
export STUB_RC_STEP1=0
export STUB_RC_STEP2=0
export STUB_RC_STEP3=7
export STUB_STATE_PATH="$TMP8/state"; : > "$TMP8/state"
export STUB_LIST_REV_PATH="$TMP8/list_rev"
export STUB_NP_PATH="$CLUSDIR8/np.json"
export STUB_NP_BASELINE_PATH="$CLUSDIR8/np_baseline.json"
export STUB_NP_STEP1_PATH="$CLUSDIR8/np_step1_disabled.json"
export STUB_LOG_PATH="$TMP8/stub.log"
export STUB_GET_VALUES_CALLS_PATH="$TMP8/get_values_calls"
export STUB_GET_MANIFEST_CALLS_PATH="$TMP8/get_manifest_calls"
export STUB_VALUE_BASELINE_PATH="$CLUSDIR8/values_baseline.yaml"
export STUB_VALUE_DRIFT_PATH="$CLUSDIR8/values_baseline.yaml"
export STUB_MANIFEST_BASELINE_PATH="$CLUSDIR8/manifest_baseline.txt"
export STUB_MANIFEST_DRIFT_PATH="$CLUSDIR8/manifest_baseline.txt"
export STUB_DRIFT_REV=0 STUB_DRIFT_VALUES=0 STUB_DRIFT_MANIFEST=0
export STUB_STATUS_CALLS_PATH="$TMP8/status_calls"; : > "$TMP8/status_calls"
export PATH="$TMP8/stub_path:$PATH"
mkdir -p "$TMP8/fake_chart" "$TMP8/fixtures/integrationcni"
printf 'networkPolicy:\n  allowedPeerPorts: []\n' > "$TMP8/fixtures/integrationcni/values-extra-cni.yaml"
export VALUES_EXTRA="$TMP8/fixtures/integrationcni/values-extra-cni.yaml"
export ARTIFACTS="$ARTDIR8"
export CHART_PATH="$TMP8/fake_chart"
export RELEASE="nexus-cni-upgrade"
cp "$TMP8/cluster-up.txt" "$ARTDIR8/cluster-up.txt"
RC_MUT=0
bash "$MUTATED_TARGET" 2>"$TMP8/mut.err" 1>"$TMP8/mut.out" || RC_MUT=$?
unset STUB_RC_STEP1 STUB_RC_STEP2 STUB_RC_STEP3 STUB_STATE_PATH STUB_LIST_REV_PATH \
      STUB_NP_PATH STUB_LOG_PATH STUB_GET_VALUES_CALLS_PATH \
      STUB_GET_MANIFEST_CALLS_PATH STUB_VALUE_BASELINE_PATH STUB_VALUE_DRIFT_PATH \
      STUB_MANIFEST_BASELINE_PATH STUB_MANIFEST_DRIFT_PATH \
      STUB_DRIFT_REV STUB_DRIFT_VALUES STUB_DRIFT_MANIFEST STUB_DRIFT_REV_TO \
      STUB_STATUS_CALLS_PATH STUB_LIST_CALLS_PATH \
      STUB_GET_VALUES_FAIL_FROM STUB_GET_VALUES_FAIL_RC \
      STUB_GET_MANIFEST_FAIL_FROM STUB_GET_MANIFEST_FAIL_RC \
      STUB_KC_NETPOL_FAIL_FROM STUB_KC_NETPOL_FAIL_RC \
      STUB_NP_BASELINE_PATH STUB_NP_STEP1_PATH \
      STUB_NP_STEP3_DRIFT_FROM STUB_NP_STEP4_DRIFT_FROM \
      STUB_NP_STEP3_DRIFT_PATH STUB_NP_STEP4_DRIFT_PATH STUB_NP_INVALID_FROM \
            STUB_STATUS_FAIL_FROM STUB_STATUS_FAIL_TO STUB_STATUS_FAIL_VALUE STUB_STATUS_FAIL_RC \
      STUB_RELEASE_NAME
[[ "$RC_MUT" -ne 0 ]] && pass "control 8: mutated target rc=$RC_MUT (B1 regression injection broke happy)" || fail "control 8: mutated target rc=0 — B1 regression did not break happy (B1)"
rm -f "$MUTATED_TARGET"

# ============================================================================
# Control 9 — artifact routing.
# ============================================================================
echo "== Control 9: artifact routing ==="
HAPPY_TMP="$(mktemp -d -t d2b-up-9-XXXXXX)"
ARTDIR9="$HAPPY_TMP/external_artifacts"
CLUSDIR9="$HAPPY_TMP/ext_cluster_state"
mkdir -p "$ARTDIR9" "$CLUSDIR9"
write_stubs "$HAPPY_TMP"
seed_cluster_state "$CLUSDIR9" "same"
printf 'before=5\nafter=6\n' > "$HAPPY_TMP/list_rev"
printf 'cluster-up-ok\n' > "$HAPPY_TMP/cluster-up.txt"
export STUB_RELEASE_NAME="nexus-cni-upgrade"
export STUB_RC_STEP1=0
export STUB_RC_STEP2=0
export STUB_RC_STEP3=7
export STUB_STATE_PATH="$HAPPY_TMP/state"; : > "$HAPPY_TMP/state"
export STUB_LIST_REV_PATH="$HAPPY_TMP/list_rev"
export STUB_NP_PATH="$CLUSDIR9/np.json"
export STUB_NP_BASELINE_PATH="$CLUSDIR9/np_baseline.json"
export STUB_NP_STEP1_PATH="$CLUSDIR9/np_step1_disabled.json"
export STUB_KC_NETPOL_CALLS_PATH="$HAPPY_TMP/kc_netpol_calls"; : > "$HAPPY_TMP/kc_netpol_calls"
export STUB_LOG_PATH="$HAPPY_TMP/stub.log"
export STUB_GET_VALUES_CALLS_PATH="$HAPPY_TMP/get_values_calls"
export STUB_GET_MANIFEST_CALLS_PATH="$HAPPY_TMP/get_manifest_calls"
export STUB_VALUE_BASELINE_PATH="$CLUSDIR9/values_baseline.yaml"
export STUB_VALUE_DRIFT_PATH="$CLUSDIR9/values_baseline.yaml"
export STUB_MANIFEST_BASELINE_PATH="$CLUSDIR9/manifest_baseline.txt"
export STUB_MANIFEST_DRIFT_PATH="$CLUSDIR9/manifest_baseline.txt"
export STUB_DRIFT_REV=0 STUB_DRIFT_VALUES=0 STUB_DRIFT_MANIFEST=0
export STUB_STATUS_CALLS_PATH="$HAPPY_TMP/status_calls"; : > "$HAPPY_TMP/status_calls"
export PATH="$HAPPY_TMP/stub_path:$PATH"
mkdir -p "$HAPPY_TMP/fake_chart" "$HAPPY_TMP/fixtures/integrationcni"
printf 'networkPolicy:\n  allowedPeerPorts: []\n' > "$HAPPY_TMP/fixtures/integrationcni/values-extra-cni.yaml"
export VALUES_EXTRA="$HAPPY_TMP/fixtures/integrationcni/values-extra-cni.yaml"
export ARTIFACTS="$ARTDIR9"
export CHART_PATH="$HAPPY_TMP/fake_chart"
export RELEASE="nexus-cni-upgrade"
cp "$HAPPY_TMP/cluster-up.txt" "$ARTDIR9/cluster-up.txt"
PWD_INV="$(mktemp -d -t d2b-up-repopwd-XXXXXX)"
RC_C9_FINAL=0
( cd "$PWD_INV"; bash "$TARGET" ) > "$HAPPY_TMP/c9.out" 2> "$HAPPY_TMP/c9.err" || RC_C9_FINAL=$?
PROBE="$(cd "$PWD_INV" && pwd -P)"
WROTE=$(find "$PROBE" -path "$PROBE/artifacts/integrationcni" -type f 2>/dev/null | head -3 || true)
printf '  [C9-artifact-routing] PROBE=%s\n' "$PROBE"
[[ "$RC_C9_FINAL" -eq 0 ]] && pass "control 9: real target rc=0 (rc=$RC_C9_FINAL)" || fail "control 9: expected rc=0, got rc=$RC_C9_FINAL; stderr='$(head -50 "$HAPPY_TMP/c9.err")'"
[[ -z "$WROTE" ]] && pass "control 9: no \$PWD/artifacts/integrationcni write under PWD during execution" || fail "control 9: unwanted write under PWD"
for s1_desired in "install-disabled-ok" "upgrade-enforce-ok" "rejected-invalid-upgrade-ok" "state-preserved-after-rejected-upgrade"; do
    case "$s1_desired" in
        install-disabled-ok) f="$ARTDIR9/upgrade-step1.txt" ;;
        upgrade-enforce-ok) f="$ARTDIR9/upgrade-step2.txt" ;;
        rejected-invalid-upgrade-ok) f="$ARTDIR9/upgrade-step3.txt" ;;
        state-preserved-after-rejected-upgrade) f="$ARTDIR9/upgrade-step4.txt" ;;
    esac
    [[ "$(cat "$f" 2>/dev/null)" == "$s1_desired" ]] && pass "control 9: supplied ARTIFACTS contains exact sentinel '${s1_desired}'" || fail "control 9: missing sentinel $f ('$(cat "$f" 2>/dev/null)')"
done
unset STUB_RC_STEP1 STUB_RC_STEP2 STUB_RC_STEP3 STUB_STATE_PATH STUB_LIST_REV_PATH \
      STUB_NP_PATH STUB_LOG_PATH STUB_GET_VALUES_CALLS_PATH \
      STUB_GET_MANIFEST_CALLS_PATH STUB_VALUE_BASELINE_PATH STUB_VALUE_DRIFT_PATH \
      STUB_MANIFEST_BASELINE_PATH STUB_MANIFEST_DRIFT_PATH \
      STUB_DRIFT_REV STUB_DRIFT_VALUES STUB_DRIFT_MANIFEST STUB_DRIFT_REV_TO \
      STUB_STATUS_CALLS_PATH STUB_LIST_CALLS_PATH \
      STUB_GET_VALUES_FAIL_FROM STUB_GET_VALUES_FAIL_RC \
      STUB_GET_MANIFEST_FAIL_FROM STUB_GET_MANIFEST_FAIL_RC \
      STUB_KC_NETPOL_FAIL_FROM STUB_KC_NETPOL_FAIL_RC \
      STUB_NP_BASELINE_PATH STUB_NP_STEP1_PATH \
      STUB_NP_STEP3_DRIFT_FROM STUB_NP_STEP4_DRIFT_FROM \
      STUB_NP_STEP3_DRIFT_PATH STUB_NP_STEP4_DRIFT_PATH STUB_NP_INVALID_FROM \
            STUB_STATUS_FAIL_FROM STUB_STATUS_FAIL_TO STUB_STATUS_FAIL_VALUE STUB_STATUS_FAIL_RC \
      STUB_RELEASE_NAME

# ============================================================================
# Control 10 — Step 2 status observation fails.
# ============================================================================
echo "== Control 10: step2 status observation failure ==="
TMP10="$(mktemp -d -t d2b-up-c10-XXXXXX)"
ARTDIR_C10="$TMP10/external_artifacts"
CLUSDIR_C10="$TMP10/cluster_state"
mkdir -p "$ARTDIR_C10" "$CLUSDIR_C10"
write_stubs "$TMP10"
seed_cluster_state "$CLUSDIR_C10" same
printf 'before=5\nafter=6\n' > "$TMP10/list_rev"
printf 'cluster-up-ok\n' > "$TMP10/cluster-up.txt"
export STUB_RELEASE_NAME="nexus-cni-upgrade"
export STUB_RC_STEP1=0 STUB_RC_STEP2=0 STUB_RC_STEP3=7
export STUB_STATE_PATH="$TMP10/state"; : > "$TMP10/state"
export STUB_LIST_REV_PATH="$TMP10/list_rev"
export STUB_NP_PATH="$CLUSDIR_C10/np.json"
export STUB_NP_BASELINE_PATH="$CLUSDIR_C10/np_baseline.json"
export STUB_NP_STEP1_PATH="$CLUSDIR_C10/np_step1_disabled.json"
export STUB_KC_NETPOL_CALLS_PATH="$TMP10/kc_netpol_calls"; : > "$TMP10/kc_netpol_calls"
export STUB_LOG_PATH="$TMP10/stub.log"
export STUB_GET_VALUES_CALLS_PATH="$TMP10/get_values_calls"
export STUB_GET_MANIFEST_CALLS_PATH="$TMP10/get_manifest_calls"
export STUB_VALUE_BASELINE_PATH="$CLUSDIR_C10/values_baseline.yaml"
export STUB_VALUE_DRIFT_PATH="$CLUSDIR_C10/values_baseline.yaml"
export STUB_MANIFEST_BASELINE_PATH="$CLUSDIR_C10/manifest_baseline.txt"
export STUB_MANIFEST_DRIFT_PATH="$CLUSDIR_C10/manifest_baseline.txt"
export STUB_DRIFT_REV=0 STUB_DRIFT_VALUES=0 STUB_DRIFT_MANIFEST=0
export STUB_STATUS_CALLS_PATH="$TMP10/status_calls"; : > "$TMP10/status_calls"
# Step 1 calls status once; Step 2 status is the 2nd call.
export STUB_STATUS_FAIL_FROM=2
export STUB_STATUS_FAIL_TO=2
export STUB_STATUS_FAIL_VALUE="pending"
export STUB_STATUS_FAIL_RC=0
export PATH="$TMP10/stub_path:$PATH"
mkdir -p "$TMP10/fake_chart" "$TMP10/fixtures/integrationcni"
printf 'networkPolicy:\n  allowedPeerPorts: []\n' > "$TMP10/fixtures/integrationcni/values-extra-cni.yaml"
export VALUES_EXTRA="$TMP10/fixtures/integrationcni/values-extra-cni.yaml"
export ARTIFACTS="$ARTDIR_C10"
export CHART_PATH="$TMP10/fake_chart"
export RELEASE="nexus-cni-upgrade"
cp "$TMP10/cluster-up.txt" "$ARTDIR_C10/cluster-up.txt"
RC_C10=0
bash "$TARGET" 2>"$TMP10/c10.err" 1>"$TMP10/c10.out" || RC_C10=$?
unset STUB_RC_STEP1 STUB_RC_STEP2 STUB_RC_STEP3 STUB_STATE_PATH STUB_LIST_REV_PATH STUB_NP_PATH \
      STUB_LOG_PATH STUB_GET_VALUES_CALLS_PATH STUB_GET_MANIFEST_CALLS_PATH \
      STUB_VALUE_BASELINE_PATH STUB_VALUE_DRIFT_PATH \
      STUB_MANIFEST_BASELINE_PATH STUB_MANIFEST_DRIFT_PATH \
      STUB_DRIFT_REV STUB_DRIFT_VALUES STUB_DRIFT_MANIFEST STUB_DRIFT_REV_TO \
      STUB_STATUS_CALLS_PATH STUB_LIST_CALLS_PATH \
      STUB_STATUS_FAIL_FROM STUB_STATUS_FAIL_TO STUB_STATUS_FAIL_VALUE STUB_STATUS_FAIL_RC \
      STUB_GET_VALUES_FAIL_FROM STUB_GET_VALUES_FAIL_RC \
      STUB_GET_MANIFEST_FAIL_FROM STUB_GET_MANIFEST_FAIL_RC \
      STUB_KC_NETPOL_FAIL_FROM STUB_KC_NETPOL_FAIL_RC \
      STUB_NP_BASELINE_PATH STUB_NP_STEP1_PATH \
      STUB_NP_STEP3_DRIFT_FROM STUB_NP_STEP4_DRIFT_FROM \
      STUB_NP_STEP3_DRIFT_PATH STUB_NP_STEP4_DRIFT_PATH STUB_NP_INVALID_FROM \
            STUB_RELEASE_NAME
ERR_C10="$(cat "$TMP10/c10.err" 2>/dev/null)"
[[ "$RC_C10" -ne 0 ]] && pass "control 10: rc != 0 (rc=$RC_C10)" || fail "control 10: expected rc != 0, got rc=0; stderr='$ERR_C10'"
[[ ! -f "$ARTDIR_C10/upgrade-step2.txt" ]] && pass "control 10: no upgrade-step2 sentinel" || fail "control 10: unwanted upgrade-step2 sentinel"
[[ ! -f "$ARTDIR_C10/upgrade-step3.txt" ]] && pass "control 10: no upgrade-step3 sentinel" || fail "control 10: upgrade-step3 sentinel emitted despite S2 status drift"
[[ ! -f "$ARTDIR_C10/upgrade-step4.txt" ]] && pass "control 10: no upgrade-step4 sentinel" || fail "control 10: upgrade-step4 sentinel emitted despite S2 status drift"

# ============================================================================
# Control 11 — Step 3 revision mismatch (post-rejection list reports 99).
# ============================================================================
echo "== Control 11: step3 revision mismatch ==="
TMP11="$(mktemp -d -t d2b-up-c11-XXXXXX)"
ARTDIR_C11="$TMP11/external_artifacts"
CLUSDIR_C11="$TMP11/cluster_state"
mkdir -p "$ARTDIR_C11" "$CLUSDIR_C11"
write_stubs "$TMP11"
seed_cluster_state "$CLUSDIR_C11" same
printf 'before=5\nafter=6\n' > "$TMP11/list_rev"
printf 'cluster-up-ok\n' > "$TMP11/cluster-up.txt"
export STUB_RELEASE_NAME="nexus-cni-upgrade"
export STUB_RC_STEP1=0 STUB_RC_STEP2=0 STUB_RC_STEP3=7
export STUB_STATE_PATH="$TMP11/state"; : > "$TMP11/state"
export STUB_LIST_REV_PATH="$TMP11/list_rev"
export STUB_NP_PATH="$CLUSDIR_C11/np.json"
export STUB_NP_BASELINE_PATH="$CLUSDIR_C11/np_baseline.json"
export STUB_NP_STEP1_PATH="$CLUSDIR_C11/np_step1_disabled.json"
export STUB_KC_NETPOL_CALLS_PATH="$TMP11/kc_netpol_calls"; : > "$TMP11/kc_netpol_calls"
export STUB_LOG_PATH="$TMP11/stub.log"
export STUB_GET_VALUES_CALLS_PATH="$TMP11/get_values_calls"
export STUB_GET_MANIFEST_CALLS_PATH="$TMP11/get_manifest_calls"
export STUB_VALUE_BASELINE_PATH="$CLUSDIR_C11/values_baseline.yaml"
export STUB_VALUE_DRIFT_PATH="$CLUSDIR_C11/values_baseline.yaml"
export STUB_MANIFEST_BASELINE_PATH="$CLUSDIR_C11/manifest_baseline.txt"
export STUB_MANIFEST_DRIFT_PATH="$CLUSDIR_C11/manifest_baseline.txt"
export STUB_DRIFT_REV=1
export STUB_DRIFT_REV_TO="99"
export STUB_DRIFT_VALUES=0
export STUB_DRIFT_MANIFEST=0
export STUB_STATUS_CALLS_PATH="$TMP11/status_calls"; : > "$TMP11/status_calls"
export PATH="$TMP11/stub_path:$PATH"
mkdir -p "$TMP11/fake_chart" "$TMP11/fixtures/integrationcni"
printf 'networkPolicy:\n  allowedPeerPorts: []\n' > "$TMP11/fixtures/integrationcni/values-extra-cni.yaml"
export VALUES_EXTRA="$TMP11/fixtures/integrationcni/values-extra-cni.yaml"
export ARTIFACTS="$ARTDIR_C11"
export CHART_PATH="$TMP11/fake_chart"
export RELEASE="nexus-cni-upgrade"
cp "$TMP11/cluster-up.txt" "$ARTDIR_C11/cluster-up.txt"
RC_C11=0
bash "$TARGET" 2>"$TMP11/c11.err" 1>"$TMP11/c11.out" || RC_C11=$?
unset STUB_RC_STEP1 STUB_RC_STEP2 STUB_RC_STEP3 STUB_STATE_PATH STUB_LIST_REV_PATH STUB_NP_PATH \
      STUB_LOG_PATH STUB_GET_VALUES_CALLS_PATH STUB_GET_MANIFEST_CALLS_PATH \
      STUB_VALUE_BASELINE_PATH STUB_VALUE_DRIFT_PATH \
      STUB_MANIFEST_BASELINE_PATH STUB_MANIFEST_DRIFT_PATH \
      STUB_DRIFT_REV STUB_DRIFT_VALUES STUB_DRIFT_MANIFEST STUB_DRIFT_REV_TO \
      STUB_STATUS_CALLS_PATH STUB_LIST_CALLS_PATH \
      STUB_STATUS_FAIL_FROM STUB_STATUS_FAIL_TO STUB_STATUS_FAIL_VALUE STUB_STATUS_FAIL_RC \
      STUB_GET_VALUES_FAIL_FROM STUB_GET_VALUES_FAIL_RC \
      STUB_GET_MANIFEST_FAIL_FROM STUB_GET_MANIFEST_FAIL_RC \
      STUB_KC_NETPOL_FAIL_FROM STUB_KC_NETPOL_FAIL_RC \
      STUB_NP_BASELINE_PATH STUB_NP_STEP1_PATH \
      STUB_NP_STEP3_DRIFT_FROM STUB_NP_STEP4_DRIFT_FROM \
      STUB_NP_STEP3_DRIFT_PATH STUB_NP_STEP4_DRIFT_PATH STUB_NP_INVALID_FROM \
            STUB_RELEASE_NAME
ERR_C11="$(cat "$TMP11/c11.err" 2>/dev/null)"
[[ "$RC_C11" -ne 0 ]] && pass "control 11: rc != 0 (rc=$RC_C11)" || fail "control 11: expected rc != 0, got rc=0; stderr='$ERR_C11'"
[[ ! -f "$ARTDIR_C11/upgrade-step3.txt" ]] && pass "control 11: no upgrade-step3 sentinel" || fail "control 11: unwanted upgrade-step3 sentinel"
[[ ! -f "$ARTDIR_C11/upgrade-step4.txt" ]] && pass "control 11: no upgrade-step4 sentinel" || fail "control 11: upgrade-step4 sentinel emitted despite revision mismatch"
[[ -n "$ERR_C11" && ( "$ERR_C11" == *"S3_REV_AFTER"* || "$ERR_C11" == *"revision mismatch"* || "$ERR_C11" == *"enforced-state preservation violated"* ) ]] && pass "control 11: stderr identifies S3 revision mismatch" || fail "control 11: stderr did not identify S3 revision mismatch: '$ERR_C11'"

# ============================================================================
# Control 12 — Step 3 release status drift (status returns non-deployed
# after rejected upgrade).
# ============================================================================
echo "== Control 12: step3 status drift ==="
TMP12="$(mktemp -d -t d2b-up-c12-XXXXXX)"
ARTDIR_C12="$TMP12/external_artifacts"
CLUSDIR_C12="$TMP12/cluster_state"
mkdir -p "$ARTDIR_C12" "$CLUSDIR_C12"
write_stubs "$TMP12"
seed_cluster_state "$CLUSDIR_C12" same
printf 'before=5\nafter=6\n' > "$TMP12/list_rev"
printf 'cluster-up-ok\n' > "$TMP12/cluster-up.txt"
export STUB_RELEASE_NAME="nexus-cni-upgrade"
export STUB_RC_STEP1=0 STUB_RC_STEP2=0 STUB_RC_STEP3=7
export STUB_STATE_PATH="$TMP12/state"; : > "$TMP12/state"
export STUB_LIST_REV_PATH="$TMP12/list_rev"
export STUB_NP_PATH="$CLUSDIR_C12/np.json"
export STUB_NP_BASELINE_PATH="$CLUSDIR_C12/np_baseline.json"
export STUB_NP_STEP1_PATH="$CLUSDIR_C12/np_step1_disabled.json"
export STUB_KC_NETPOL_CALLS_PATH="$TMP12/kc_netpol_calls"; : > "$TMP12/kc_netpol_calls"
export STUB_LOG_PATH="$TMP12/stub.log"
export STUB_GET_VALUES_CALLS_PATH="$TMP12/get_values_calls"
export STUB_GET_MANIFEST_CALLS_PATH="$TMP12/get_manifest_calls"
export STUB_VALUE_BASELINE_PATH="$CLUSDIR_C12/values_baseline.yaml"
export STUB_VALUE_DRIFT_PATH="$CLUSDIR_C12/values_baseline.yaml"
export STUB_MANIFEST_BASELINE_PATH="$CLUSDIR_C12/manifest_baseline.txt"
export STUB_MANIFEST_DRIFT_PATH="$CLUSDIR_C12/manifest_baseline.txt"
export STUB_DRIFT_REV=0 STUB_DRIFT_VALUES=0 STUB_DRIFT_MANIFEST=0
export STUB_STATUS_CALLS_PATH="$TMP12/status_calls"; : > "$TMP12/status_calls"
export STUB_STATUS_FAIL_FROM=3
export STUB_STATUS_FAIL_TO=3
export STUB_STATUS_FAIL_VALUE="failed"
export STUB_STATUS_FAIL_RC=0
export PATH="$TMP12/stub_path:$PATH"
mkdir -p "$TMP12/fake_chart" "$TMP12/fixtures/integrationcni"
printf 'networkPolicy:\n  allowedPeerPorts: []\n' > "$TMP12/fixtures/integrationcni/values-extra-cni.yaml"
export VALUES_EXTRA="$TMP12/fixtures/integrationcni/values-extra-cni.yaml"
export ARTIFACTS="$ARTDIR_C12"
export CHART_PATH="$TMP12/fake_chart"
export RELEASE="nexus-cni-upgrade"
cp "$TMP12/cluster-up.txt" "$ARTDIR_C12/cluster-up.txt"
RC_C12=0
bash "$TARGET" 2>"$TMP12/c12.err" 1>"$TMP12/c12.out" || RC_C12=$?
unset STUB_RC_STEP1 STUB_RC_STEP2 STUB_RC_STEP3 STUB_STATE_PATH STUB_LIST_REV_PATH STUB_NP_PATH \
      STUB_LOG_PATH STUB_GET_VALUES_CALLS_PATH STUB_GET_MANIFEST_CALLS_PATH \
      STUB_VALUE_BASELINE_PATH STUB_VALUE_DRIFT_PATH \
      STUB_MANIFEST_BASELINE_PATH STUB_MANIFEST_DRIFT_PATH \
      STUB_DRIFT_REV STUB_DRIFT_VALUES STUB_DRIFT_MANIFEST STUB_DRIFT_REV_TO \
      STUB_STATUS_CALLS_PATH STUB_LIST_CALLS_PATH \
      STUB_STATUS_FAIL_FROM STUB_STATUS_FAIL_TO STUB_STATUS_FAIL_VALUE STUB_STATUS_FAIL_RC \
      STUB_GET_VALUES_FAIL_FROM STUB_GET_VALUES_FAIL_RC \
      STUB_GET_MANIFEST_FAIL_FROM STUB_GET_MANIFEST_FAIL_RC \
      STUB_KC_NETPOL_FAIL_FROM STUB_KC_NETPOL_FAIL_RC \
      STUB_NP_BASELINE_PATH STUB_NP_STEP1_PATH \
      STUB_NP_STEP3_DRIFT_FROM STUB_NP_STEP4_DRIFT_FROM \
      STUB_NP_STEP3_DRIFT_PATH STUB_NP_STEP4_DRIFT_PATH STUB_NP_INVALID_FROM \
            STUB_RELEASE_NAME
ERR_C12="$(cat "$TMP12/c12.err" 2>/dev/null)"
[[ "$RC_C12" -ne 0 ]] && pass "control 12: rc != 0 (rc=$RC_C12)" || fail "control 12: expected rc != 0, got rc=0; stderr='$ERR_C12'"
[[ ! -f "$ARTDIR_C12/upgrade-step3.txt" ]] && pass "control 12: no upgrade-step3 sentinel" || fail "control 12: upgrade-step3 sentinel emitted despite S3 status drift"
[[ ! -f "$ARTDIR_C12/upgrade-step4.txt" ]] && pass "control 12: no upgrade-step4 sentinel" || fail "control 12: upgrade-step4 sentinel emitted despite S3 status drift"
[[ -n "$ERR_C12" && "$ERR_C12" == *"NOT_DEPLOYED"* ]] && pass "control 12: stderr identifies non-deployed status" || fail "control 12: stderr did not identify status drift: '$ERR_C12'"

# ============================================================================
# Control 13 — Step 2 values observation fails.
# ============================================================================
echo "== Control 13: step2 values observation failure ==="
TMP13="$(mktemp -d -t d2b-up-c13-XXXXXX)"
ARTDIR_C13="$TMP13/external_artifacts"
CLUSDIR_C13="$TMP13/cluster_state"
mkdir -p "$ARTDIR_C13" "$CLUSDIR_C13"
write_stubs "$TMP13"
seed_cluster_state "$CLUSDIR_C13" same
printf 'before=5\nafter=6\n' > "$TMP13/list_rev"
printf 'cluster-up-ok\n' > "$TMP13/cluster-up.txt"
export STUB_RELEASE_NAME="nexus-cni-upgrade"
export STUB_RC_STEP1=0 STUB_RC_STEP2=0 STUB_RC_STEP3=7
export STUB_STATE_PATH="$TMP13/state"; : > "$TMP13/state"
export STUB_LIST_REV_PATH="$TMP13/list_rev"
export STUB_NP_PATH="$CLUSDIR_C13/np.json"
export STUB_NP_BASELINE_PATH="$CLUSDIR_C13/np_baseline.json"
export STUB_NP_STEP1_PATH="$CLUSDIR_C13/np_step1_disabled.json"
export STUB_KC_NETPOL_CALLS_PATH="$TMP13/kc_netpol_calls"; : > "$TMP13/kc_netpol_calls"
export STUB_LOG_PATH="$TMP13/stub.log"
export STUB_GET_VALUES_CALLS_PATH="$TMP13/get_values_calls"
export STUB_GET_MANIFEST_CALLS_PATH="$TMP13/get_manifest_calls"
export STUB_VALUE_BASELINE_PATH="$CLUSDIR_C13/values_baseline.yaml"
export STUB_VALUE_DRIFT_PATH="$CLUSDIR_C13/values_baseline.yaml"
export STUB_MANIFEST_BASELINE_PATH="$CLUSDIR_C13/manifest_baseline.txt"
export STUB_MANIFEST_DRIFT_PATH="$CLUSDIR_C13/manifest_baseline.txt"
export STUB_DRIFT_REV=0 STUB_DRIFT_VALUES=0 STUB_DRIFT_MANIFEST=0
export STUB_STATUS_CALLS_PATH="$TMP13/status_calls"; : > "$TMP13/status_calls"
# Force STEP-2 values capture (call #2 of get values: first is Step 1, second is Step 2)
export STUB_GET_VALUES_FAIL_FROM="2"
export STUB_GET_VALUES_FAIL_RC="1"
export PATH="$TMP13/stub_path:$PATH"
mkdir -p "$TMP13/fake_chart" "$TMP13/fixtures/integrationcni"
printf 'networkPolicy:\n  allowedPeerPorts: []\n' > "$TMP13/fixtures/integrationcni/values-extra-cni.yaml"
export VALUES_EXTRA="$TMP13/fixtures/integrationcni/values-extra-cni.yaml"
export ARTIFACTS="$ARTDIR_C13"
export CHART_PATH="$TMP13/fake_chart"
export RELEASE="nexus-cni-upgrade"
cp "$TMP13/cluster-up.txt" "$ARTDIR_C13/cluster-up.txt"
RC_C13=0
bash "$TARGET" 2>"$TMP13/c13.err" 1>"$TMP13/c13.out" || RC_C13=$?
unset STUB_RC_STEP1 STUB_RC_STEP2 STUB_RC_STEP3 STUB_STATE_PATH STUB_LIST_REV_PATH STUB_NP_PATH \
      STUB_LOG_PATH STUB_GET_VALUES_CALLS_PATH STUB_GET_MANIFEST_CALLS_PATH \
      STUB_VALUE_BASELINE_PATH STUB_VALUE_DRIFT_PATH \
      STUB_MANIFEST_BASELINE_PATH STUB_MANIFEST_DRIFT_PATH \
      STUB_DRIFT_REV STUB_DRIFT_VALUES STUB_DRIFT_MANIFEST STUB_DRIFT_REV_TO \
      STUB_STATUS_CALLS_PATH STUB_LIST_CALLS_PATH \
      STUB_STATUS_FAIL_FROM STUB_STATUS_FAIL_TO STUB_STATUS_FAIL_VALUE STUB_STATUS_FAIL_RC \
      STUB_GET_VALUES_FAIL_FROM STUB_GET_VALUES_FAIL_RC \
      STUB_GET_MANIFEST_FAIL_FROM STUB_GET_MANIFEST_FAIL_RC \
      STUB_KC_NETPOL_FAIL_FROM STUB_KC_NETPOL_FAIL_RC \
      STUB_NP_BASELINE_PATH STUB_NP_STEP1_PATH \
      STUB_NP_STEP3_DRIFT_FROM STUB_NP_STEP4_DRIFT_FROM \
      STUB_NP_STEP3_DRIFT_PATH STUB_NP_STEP4_DRIFT_PATH STUB_NP_INVALID_FROM \
            STUB_RELEASE_NAME
[[ "$RC_C13" -ne 0 ]] && pass "control 13: rc != 0 (rc=$RC_C13)" || fail "control 13: expected rc != 0 (Step 2 values)"
for s in upgrade-step2.txt upgrade-step3.txt upgrade-step4.txt; do
    SV=$(cat "$ARTDIR_C13/$s" 2>/dev/null || true)
    [[ ! -f "$ARTDIR_C13/$s" && -z "$SV" ]] && pass "control 13: $s sentinel-absent" || fail "control 13: $s present ('$SV')"
done

# ============================================================================
# Control 14 — Step 2 netpol observation fails.
# ============================================================================
echo "== Control 14: step2 netpol observation failure ==="
TMP14="$(mktemp -d -t d2b-up-c14-XXXXXX)"
ARTDIR_C14="$TMP14/external_artifacts"
CLUSDIR_C14="$TMP14/cluster_state"
mkdir -p "$ARTDIR_C14" "$CLUSDIR_C14"
write_stubs "$TMP14"
seed_cluster_state "$CLUSDIR_C14" same
printf 'before=5\nafter=6\n' > "$TMP14/list_rev"
printf 'cluster-up-ok\n' > "$TMP14/cluster-up.txt"
export STUB_RELEASE_NAME="nexus-cni-upgrade"
export STUB_RC_STEP1=0 STUB_RC_STEP2=0 STUB_RC_STEP3=7
export STUB_STATE_PATH="$TMP14/state"; : > "$TMP14/state"
export STUB_LIST_REV_PATH="$TMP14/list_rev"
export STUB_NP_PATH="$CLUSDIR_C14/np.json"
export STUB_NP_BASELINE_PATH="$CLUSDIR_C14/np_baseline.json"
export STUB_NP_STEP1_PATH="$CLUSDIR_C14/np_step1_disabled.json"
export STUB_KC_NETPOL_CALLS_PATH="$TMP14/kc_netpol_calls"; : > "$TMP14/kc_netpol_calls"
export STUB_LOG_PATH="$TMP14/stub.log"
export STUB_GET_VALUES_CALLS_PATH="$TMP14/get_values_calls"
export STUB_GET_MANIFEST_CALLS_PATH="$TMP14/get_manifest_calls"
export STUB_VALUE_BASELINE_PATH="$CLUSDIR_C14/values_baseline.yaml"
export STUB_VALUE_DRIFT_PATH="$CLUSDIR_C14/values_baseline.yaml"
export STUB_MANIFEST_BASELINE_PATH="$CLUSDIR_C14/manifest_baseline.txt"
export STUB_MANIFEST_DRIFT_PATH="$CLUSDIR_C14/manifest_baseline.txt"
export STUB_DRIFT_REV=0 STUB_DRIFT_VALUES=0 STUB_DRIFT_MANIFEST=0
export STUB_STATUS_CALLS_PATH="$TMP14/status_calls"; : > "$TMP14/status_calls"
export STUB_KC_NETPOL_CALLS_PATH="$TMP14/kc_netpol_calls"; : > "$TMP14/kc_netpol_calls"
export STUB_KC_NETPOL_FAIL_FROM="2"
export STUB_KC_NETPOL_FAIL_RC="1"
export PATH="$TMP14/stub_path:$PATH"
mkdir -p "$TMP14/fake_chart" "$TMP14/fixtures/integrationcni"
printf 'networkPolicy:\n  allowedPeerPorts: []\n' > "$TMP14/fixtures/integrationcni/values-extra-cni.yaml"
export VALUES_EXTRA="$TMP14/fixtures/integrationcni/values-extra-cni.yaml"
export ARTIFACTS="$ARTDIR_C14"
export CHART_PATH="$TMP14/fake_chart"
export RELEASE="nexus-cni-upgrade"
cp "$TMP14/cluster-up.txt" "$ARTDIR_C14/cluster-up.txt"
RC_C14=0
bash "$TARGET" 2>"$TMP14/c14.err" 1>"$TMP14/c14.out" || RC_C14=$?
unset STUB_RC_STEP1 STUB_RC_STEP2 STUB_RC_STEP3 STUB_STATE_PATH STUB_LIST_REV_PATH STUB_NP_PATH \
      STUB_LOG_PATH STUB_GET_VALUES_CALLS_PATH STUB_GET_MANIFEST_CALLS_PATH \
      STUB_VALUE_BASELINE_PATH STUB_VALUE_DRIFT_PATH \
      STUB_MANIFEST_BASELINE_PATH STUB_MANIFEST_DRIFT_PATH \
      STUB_DRIFT_REV STUB_DRIFT_VALUES STUB_DRIFT_MANIFEST STUB_DRIFT_REV_TO \
      STUB_STATUS_CALLS_PATH STUB_LIST_CALLS_PATH STUB_KC_NETPOL_CALLS_PATH \
      STUB_STATUS_FAIL_FROM STUB_STATUS_FAIL_TO STUB_STATUS_FAIL_VALUE STUB_STATUS_FAIL_RC \
      STUB_GET_VALUES_FAIL_FROM STUB_GET_VALUES_FAIL_RC \
      STUB_GET_MANIFEST_FAIL_FROM STUB_GET_MANIFEST_FAIL_RC \
      STUB_KC_NETPOL_FAIL_FROM STUB_KC_NETPOL_FAIL_RC \
      STUB_NP_BASELINE_PATH STUB_NP_STEP1_PATH \
      STUB_NP_STEP3_DRIFT_FROM STUB_NP_STEP4_DRIFT_FROM \
      STUB_NP_STEP3_DRIFT_PATH STUB_NP_STEP4_DRIFT_PATH STUB_NP_INVALID_FROM \
            STUB_RELEASE_NAME
[[ "$RC_C14" -ne 0 ]] && pass "control 14: rc != 0 (rc=$RC_C14)" || fail "control 14: expected rc != 0 (Step 2 netpol)"
for s in upgrade-step2.txt upgrade-step3.txt upgrade-step4.txt; do
    SV=$(cat "$ARTDIR_C14/$s" 2>/dev/null || true)
    [[ ! -f "$ARTDIR_C14/$s" && -z "$SV" ]] && pass "control 14: $s sentinel-absent" || fail "control 14: $s present ('$SV')"
done

# ============================================================================
# Control 15 — NetPol shape failure (invalid JSON on enumerated call).
# The stub kubectl returns valid List JSON for normal calls. On the chosen
# call we return whatever STUB_NP_INVALID_FROM/KIND dictates. The target
# must die, no sentinels survive (specifically no rejected-invalid-upgrade-ok
# sentinel because we want the failure path before Step 3 rc check), and
# stderr must mention NetworkPolicy / NP.
# ============================================================================
echo "== Control 15: step2 NetPol observation shape failure ==="
TMP15="$(mktemp -d -t d2b-up-c15-XXXXXX)"
ARTDIR_C15="$TMP15/external_artifacts"
CLUSDIR_C15="$TMP15/cluster_state"
mkdir -p "$ARTDIR_C15" "$CLUSDIR_C15"
write_stubs "$TMP15"
seed_cluster_state "$CLUSDIR_C15" same
printf 'before=5\nafter=6\n' > "$TMP15/list_rev"
printf 'cluster-up-ok\n' > "$TMP15/cluster-up.txt"
export STUB_RELEASE_NAME="nexus-cni-upgrade"
export STUB_RC_STEP1=0 STUB_RC_STEP2=0 STUB_RC_STEP3=7
export STUB_STATE_PATH="$TMP15/state"; : > "$TMP15/state"
export STUB_LIST_REV_PATH="$TMP15/list_rev"
export STUB_NP_PATH="$CLUSDIR_C15/np.json"
export STUB_NP_BASELINE_PATH="$CLUSDIR_C15/np_baseline.json"
export STUB_NP_STEP1_PATH="$CLUSDIR_C15/np_step1_disabled.json"
export STUB_NP_STEP3_DRIFT_FROM="999999"
export STUB_NP_STEP4_DRIFT_FROM="999999"
export STUB_NP_STEP3_DRIFT_PATH="$CLUSDIR_C15/np_step3_drift.json"
export STUB_NP_STEP4_DRIFT_PATH="$CLUSDIR_C15/np_step4_drift.json"
# Step 2 NetPol capture is the 2nd NetPol netpol call (Step 1 first call already passed).
# Force an array-top document (valid JSON but not an object with items list).
export STUB_NP_INVALID_FROM="2:array_top"
export STUB_KC_NETPOL_CALLS_PATH="$TMP15/kc_netpol_calls"; : > "$TMP15/kc_netpol_calls"
export STUB_LOG_PATH="$TMP15/stub.log"
export STUB_GET_VALUES_CALLS_PATH="$TMP15/get_values_calls"
export STUB_GET_MANIFEST_CALLS_PATH="$TMP15/get_manifest_calls"
export STUB_VALUE_BASELINE_PATH="$CLUSDIR_C15/values_baseline.yaml"
export STUB_VALUE_DRIFT_PATH="$CLUSDIR_C15/values_baseline.yaml"
export STUB_MANIFEST_BASELINE_PATH="$CLUSDIR_C15/manifest_baseline.txt"
export STUB_MANIFEST_DRIFT_PATH="$CLUSDIR_C15/manifest_baseline.txt"
export STUB_DRIFT_REV=0 STUB_DRIFT_VALUES=0 STUB_DRIFT_MANIFEST=0
export STUB_STATUS_CALLS_PATH="$TMP15/status_calls"; : > "$TMP15/status_calls"
export PATH="$TMP15/stub_path:$PATH"
mkdir -p "$TMP15/fake_chart" "$TMP15/fixtures/integrationcni"
printf 'networkPolicy:\n  allowedPeerPorts: []\n' > "$TMP15/fixtures/integrationcni/values-extra-cni.yaml"
export VALUES_EXTRA="$TMP15/fixtures/integrationcni/values-extra-cni.yaml"
export ARTIFACTS="$ARTDIR_C15"
export CHART_PATH="$TMP15/fake_chart"
export RELEASE="nexus-cni-upgrade"
cp "$TMP15/cluster-up.txt" "$ARTDIR_C15/cluster-up.txt"
RC_C15=0
bash "$TARGET" 2>"$TMP15/c15.err" 1>"$TMP15/c15.out" || RC_C15=$?
unset STUB_RC_STEP1 STUB_RC_STEP2 STUB_RC_STEP3 STUB_STATE_PATH STUB_LIST_REV_PATH STUB_NP_PATH \
      STUB_LOG_PATH STUB_GET_VALUES_CALLS_PATH STUB_GET_MANIFEST_CALLS_PATH \
      STUB_VALUE_BASELINE_PATH STUB_VALUE_DRIFT_PATH \
      STUB_MANIFEST_BASELINE_PATH STUB_MANIFEST_DRIFT_PATH \
      STUB_DRIFT_REV STUB_DRIFT_VALUES STUB_DRIFT_MANIFEST STUB_DRIFT_REV_TO \
      STUB_STATUS_CALLS_PATH STUB_LIST_CALLS_PATH \
      STUB_NP_BASELINE_PATH STUB_NP_STEP1_PATH \
      STUB_NP_STEP3_DRIFT_FROM STUB_NP_STEP4_DRIFT_FROM \
      STUB_NP_STEP3_DRIFT_PATH STUB_NP_STEP4_DRIFT_PATH STUB_NP_INVALID_FROM \
      STUB_STATUS_FAIL_FROM STUB_STATUS_FAIL_TO STUB_STATUS_FAIL_VALUE STUB_STATUS_FAIL_RC \
      STUB_GET_VALUES_FAIL_FROM STUB_GET_VALUES_FAIL_RC \
      STUB_GET_MANIFEST_FAIL_FROM STUB_GET_MANIFEST_FAIL_RC \
      STUB_KC_NETPOL_FAIL_FROM STUB_KC_NETPOL_FAIL_RC \
      STUB_RELEASE_NAME
ERR_C15="$(cat "$TMP15/c15.err" 2>/dev/null)"
[[ "$RC_C15" -ne 0 ]] && pass "control 15: rc != 0 (rc=$RC_C15)" || fail "control 15: expected rc != 0 on NetPol array-top; rc=0; stderr='$ERR_C15'"
[[ ! -f "$ARTDIR_C15/upgrade-step2.txt" ]] && pass "control 15: no upgrade-step2 sentinel" || fail "control 15: upgrade-step2 sentinel emitted despite NetPol shape failure"
[[ ! -f "$ARTDIR_C15/upgrade-step3.txt" ]] && pass "control 15: no upgrade-step3 sentinel" || fail "control 15: upgrade-step3 sentinel emitted despite NetPol shape failure"
[[ ! -f "$ARTDIR_C15/upgrade-step4.txt" ]] && pass "control 15: no upgrade-step4 sentinel" || fail "control 15: upgrade-step4 sentinel emitted despite NetPol shape failure"
[[ -n "$ERR_C15" && "$ERR_C15" == *"NP:"* ]] && pass "control 15: stderr identifies invalid NetPol JSON/list" || fail "control 15: stderr did not mention 'NP:'; got '${ERR_C15}'"
# Additional sub-controls to cover the failure modes declared by the stub
# invalid kinds (bad_json, scalar_top, object_no_items, items_not_list).
for iv_kind in bad_json scalar_top object_no_items items_not_list; do
    echo "== Sub-control 15/$iv_kind =="
    TMP15b="$(mktemp -d -t d2b-up-15-$iv_kind-XXXXXX)"
    ARTDIR_C15b="$TMP15b/external_artifacts"
    CLUSDIR_C15b="$TMP15b/cluster_state"
    mkdir -p "$ARTDIR_C15b" "$CLUSDIR_C15b"
    write_stubs "$TMP15b"
    seed_cluster_state "$CLUSDIR_C15b" same
    printf 'before=5\nafter=6\n' > "$TMP15b/list_rev"
    printf 'cluster-up-ok\n' > "$TMP15b/cluster-up.txt"
    export STUB_RELEASE_NAME="nexus-cni-upgrade"
    export STUB_RC_STEP1=0 STUB_RC_STEP2=0 STUB_RC_STEP3=7
    export STUB_STATE_PATH="$TMP15b/state"; : > "$TMP15b/state"
    export STUB_LIST_REV_PATH="$TMP15b/list_rev"
    export STUB_NP_PATH="$CLUSDIR_C15b/np.json"
export STUB_NP_BASELINE_PATH="$CLUSDIR_C15b/np_baseline.json"
export STUB_NP_STEP1_PATH="$CLUSDIR_C15b/np_step1_disabled.json"
    export STUB_NP_BASELINE_PATH="$CLUSDIR_C15b/np_baseline.json"
export STUB_NP_STEP1_PATH="$CLUSDIR_C15b/np_step1_disabled.json"
    export STUB_NP_STEP3_DRIFT_FROM="999999"
    export STUB_NP_STEP4_DRIFT_FROM="999999"
    export STUB_NP_STEP3_DRIFT_PATH="$CLUSDIR_C15b/np_step3_drift.json"
    export STUB_NP_STEP4_DRIFT_PATH="$CLUSDIR_C15b/np_step4_drift.json"
export STUB_NP_INVALID_FROM="2:${iv_kind}"
export STUB_KC_NETPOL_CALLS_PATH="$TMP15b/kc_netpol_calls"; : > "$TMP15b/kc_netpol_calls"
export STUB_LOG_PATH="$TMP15b/stub.log"
    export STUB_GET_VALUES_CALLS_PATH="$TMP15b/get_values_calls"
    export STUB_GET_MANIFEST_CALLS_PATH="$TMP15b/get_manifest_calls"
    export STUB_VALUE_BASELINE_PATH="$CLUSDIR_C15b/values_baseline.yaml"
    export STUB_VALUE_DRIFT_PATH="$CLUSDIR_C15b/values_baseline.yaml"
    export STUB_MANIFEST_BASELINE_PATH="$CLUSDIR_C15b/manifest_baseline.txt"
    export STUB_MANIFEST_DRIFT_PATH="$CLUSDIR_C15b/manifest_baseline.txt"
    export STUB_DRIFT_REV=0 STUB_DRIFT_VALUES=0 STUB_DRIFT_MANIFEST=0
    export STUB_STATUS_CALLS_PATH="$TMP15b/status_calls"; : > "$TMP15b/status_calls"
    export PATH="$TMP15b/stub_path:$PATH"
    mkdir -p "$TMP15b/fake_chart" "$TMP15b/fixtures/integrationcni"
    printf 'networkPolicy:\n  allowedPeerPorts: []\n' > "$TMP15b/fixtures/integrationcni/values-extra-cni.yaml"
    export VALUES_EXTRA="$TMP15b/fixtures/integrationcni/values-extra-cni.yaml"
    export ARTIFACTS="$ARTDIR_C15b"
    export CHART_PATH="$TMP15b/fake_chart"
    export RELEASE="nexus-cni-upgrade"
    cp "$TMP15b/cluster-up.txt" "$ARTDIR_C15b/cluster-up.txt"
    RC_TMP=0
    bash "$TARGET" 2>"$TMP15b/c15b.err" 1>"$TMP15b/c15b.out" || RC_TMP=$?
    unset STUB_RC_STEP1 STUB_RC_STEP2 STUB_RC_STEP3 STUB_STATE_PATH STUB_LIST_REV_PATH STUB_NP_PATH \
          STUB_LOG_PATH STUB_GET_VALUES_CALLS_PATH STUB_GET_MANIFEST_CALLS_PATH \
          STUB_VALUE_BASELINE_PATH STUB_VALUE_DRIFT_PATH \
          STUB_MANIFEST_BASELINE_PATH STUB_MANIFEST_DRIFT_PATH \
          STUB_DRIFT_REV STUB_DRIFT_VALUES STUB_DRIFT_MANIFEST STUB_DRIFT_REV_TO \
          STUB_STATUS_CALLS_PATH STUB_LIST_CALLS_PATH \
          STUB_NP_BASELINE_PATH STUB_NP_STEP1_PATH \
      STUB_NP_STEP3_DRIFT_FROM STUB_NP_STEP4_DRIFT_FROM \
          STUB_NP_STEP3_DRIFT_PATH STUB_NP_STEP4_DRIFT_PATH STUB_NP_INVALID_FROM \
          STUB_STATUS_FAIL_FROM STUB_STATUS_FAIL_TO STUB_STATUS_FAIL_VALUE STUB_STATUS_FAIL_RC \
          STUB_GET_VALUES_FAIL_FROM STUB_GET_VALUES_FAIL_RC \
          STUB_GET_MANIFEST_FAIL_FROM STUB_GET_MANIFEST_FAIL_RC \
          STUB_KC_NETPOL_FAIL_FROM STUB_KC_NETPOL_FAIL_RC \
          STUB_RELEASE_NAME
    [[ "$RC_TMP" -ne 0 ]] && pass "control 15/$iv_kind: rc != 0 (rc=$RC_TMP)" || fail "control 15/$iv_kind: expected rc != 0; rc=0"
    [[ ! -f "$ARTDIR_C15b/upgrade-step2.txt" && ! -f "$ARTDIR_C15b/upgrade-step3.txt" && ! -f "$ARTDIR_C15b/upgrade-step4.txt" ]] && pass "control 15/$iv_kind: step2/3/4 sentinels absent" || fail "control 15/$iv_kind: sentinels emitted despite ${iv_kind}"
done

# ============================================================================
# Control 16 — Step 3 NetPol drift.
# The stub kubectl returns the step-3 drifted NetPolicy on the 3rd NetPol
# capture (which corresponds to Step 3's post-rejection capture). The target
# must die with a NetworkPolicy identity drifted diagnostic and not write
# either step3 or step4 sentinels.
# ============================================================================
echo "== Control 16: step3 NetPol drift =="
TMP16="$(mktemp -d -t d2b-up-c16-XXXXXX)"
ARTDIR_C16="$TMP16/external_artifacts"
CLUSDIR_C16="$TMP16/cluster_state"
mkdir -p "$ARTDIR_C16" "$CLUSDIR_C16"
write_stubs "$TMP16"
seed_cluster_state "$CLUSDIR_C16" same
printf 'before=5\nafter=6\n' > "$TMP16/list_rev"
printf 'cluster-up-ok\n' > "$TMP16/cluster-up.txt"
export STUB_RELEASE_NAME="nexus-cni-upgrade"
export STUB_RC_STEP1=0 STUB_RC_STEP2=0 STUB_RC_STEP3=7
export STUB_STATE_PATH="$TMP16/state"; : > "$TMP16/state"
export STUB_LIST_REV_PATH="$TMP16/list_rev"
export STUB_NP_PATH="$CLUSDIR_C16/np.json"
export STUB_NP_BASELINE_PATH="$CLUSDIR_C16/np_baseline.json"
export STUB_NP_STEP1_PATH="$CLUSDIR_C16/np_step1_disabled.json"
# Step 3 NetPol capture is the 3rd NetPol netpol call.
export STUB_NP_STEP3_DRIFT_FROM="3"
export STUB_NP_STEP4_DRIFT_FROM="999999"
export STUB_KC_NETPOL_CALLS_PATH="$TMP16/kc_netpol_calls"; : > "$TMP16/kc_netpol_calls"
export STUB_NP_STEP3_DRIFT_PATH="$CLUSDIR_C16/np_step3_drift.json"
export STUB_NP_STEP4_DRIFT_PATH="$CLUSDIR_C16/np_step4_drift.json"
export STUB_NP_INVALID_FROM=""
export STUB_LOG_PATH="$TMP16/stub.log"
export STUB_GET_VALUES_CALLS_PATH="$TMP16/get_values_calls"
export STUB_GET_MANIFEST_CALLS_PATH="$TMP16/get_manifest_calls"
export STUB_VALUE_BASELINE_PATH="$CLUSDIR_C16/values_baseline.yaml"
export STUB_VALUE_DRIFT_PATH="$CLUSDIR_C16/values_baseline.yaml"
export STUB_MANIFEST_BASELINE_PATH="$CLUSDIR_C16/manifest_baseline.txt"
export STUB_MANIFEST_DRIFT_PATH="$CLUSDIR_C16/manifest_baseline.txt"
export STUB_DRIFT_REV=0 STUB_DRIFT_VALUES=0 STUB_DRIFT_MANIFEST=0
export STUB_STATUS_CALLS_PATH="$TMP16/status_calls"; : > "$TMP16/status_calls"
export PATH="$TMP16/stub_path:$PATH"
mkdir -p "$TMP16/fake_chart" "$TMP16/fixtures/integrationcni"
printf 'networkPolicy:\n  allowedPeerPorts: []\n' > "$TMP16/fixtures/integrationcni/values-extra-cni.yaml"
export VALUES_EXTRA="$TMP16/fixtures/integrationcni/values-extra-cni.yaml"
export ARTIFACTS="$ARTDIR_C16"
export CHART_PATH="$TMP16/fake_chart"
export RELEASE="nexus-cni-upgrade"
cp "$TMP16/cluster-up.txt" "$ARTDIR_C16/cluster-up.txt"
RC_C16=0
bash "$TARGET" 2>"$TMP16/c16.err" 1>"$TMP16/c16.out" || RC_C16=$?
unset STUB_RC_STEP1 STUB_RC_STEP2 STUB_RC_STEP3 STUB_STATE_PATH STUB_LIST_REV_PATH STUB_NP_PATH \
      STUB_LOG_PATH STUB_GET_VALUES_CALLS_PATH STUB_GET_MANIFEST_CALLS_PATH \
      STUB_VALUE_BASELINE_PATH STUB_VALUE_DRIFT_PATH \
      STUB_MANIFEST_BASELINE_PATH STUB_MANIFEST_DRIFT_PATH \
      STUB_DRIFT_REV STUB_DRIFT_VALUES STUB_DRIFT_MANIFEST STUB_DRIFT_REV_TO \
      STUB_STATUS_CALLS_PATH STUB_LIST_CALLS_PATH \
      STUB_NP_BASELINE_PATH STUB_NP_STEP1_PATH \
      STUB_NP_STEP3_DRIFT_FROM STUB_NP_STEP4_DRIFT_FROM \
      STUB_NP_STEP3_DRIFT_PATH STUB_NP_STEP4_DRIFT_PATH STUB_NP_INVALID_FROM \
      STUB_STATUS_FAIL_FROM STUB_STATUS_FAIL_TO STUB_STATUS_FAIL_VALUE STUB_STATUS_FAIL_RC \
      STUB_GET_VALUES_FAIL_FROM STUB_GET_VALUES_FAIL_RC \
      STUB_GET_MANIFEST_FAIL_FROM STUB_GET_MANIFEST_FAIL_RC \
      STUB_KC_NETPOL_FAIL_FROM STUB_KC_NETPOL_FAIL_RC \
      STUB_RELEASE_NAME
ERR_C16="$(cat "$TMP16/c16.err" 2>/dev/null)"
[[ "$RC_C16" -ne 0 ]] && pass "control 16: rc != 0 (rc=$RC_C16)" || fail "control 16: expected rc != 0 on S3 NetPol drift; rc=0; stderr='$ERR_C16'"
[[ ! -f "$ARTDIR_C16/upgrade-step3.txt" ]] && pass "control 16: no upgrade-step3 sentinel" || fail "control 16: upgrade-step3 sentinel emitted despite NetPol drift"
[[ ! -f "$ARTDIR_C16/upgrade-step4.txt" ]] && pass "control 16: no upgrade-step4 sentinel" || fail "control 16: upgrade-step4 sentinel emitted despite NetPol drift"
[[ -n "$ERR_C16" && ( "$ERR_C16" == *"NetworkPolicy identity drifted"* || "$ERR_C16" == *"np-id"* ) ]] && pass "control 16: stderr identifies NetworkPolicy identity drift" || fail "control 16: stderr did not identify NetPol drift; got '${ERR_C16}'"

# ============================================================================
# Control 17 — Step 4 NetPol drift.
# The stub kubectl returns the step-3-step-4 drifted NetPolicy on the 4th
# NetPol capture. The target must keep the step3 sentinel (it matches the
# rejected-upgrade post-rejection snapshot), but die with a NetworkPolicy
# identity drifted diagnostic before writing the step4 sentinel.
# ============================================================================
echo "== Control 17: step4 NetPol drift =="
TMP17="$(mktemp -d -t d2b-up-c17-XXXXXX)"
ARTDIR_C17="$TMP17/external_artifacts"
CLUSDIR_C17="$TMP17/cluster_state"
mkdir -p "$ARTDIR_C17" "$CLUSDIR_C17"
write_stubs "$TMP17"
seed_cluster_state "$CLUSDIR_C17" same
printf 'before=5\nafter=6\n' > "$TMP17/list_rev"
printf 'cluster-up-ok\n' > "$TMP17/cluster-up.txt"
export STUB_RELEASE_NAME="nexus-cni-upgrade"
export STUB_RC_STEP1=0 STUB_RC_STEP2=0 STUB_RC_STEP3=7
export STUB_STATE_PATH="$TMP17/state"; : > "$TMP17/state"
export STUB_LIST_REV_PATH="$TMP17/list_rev"
export STUB_NP_PATH="$CLUSDIR_C17/np.json"
export STUB_NP_BASELINE_PATH="$CLUSDIR_C17/np_baseline.json"
export STUB_NP_STEP1_PATH="$CLUSDIR_C17/np_step1_disabled.json"
# Step 4 NetPol capture is the 4th call. Drift only at call 4 (post-step3 snapshot).
export STUB_NP_STEP3_DRIFT_FROM="999999"
export STUB_NP_STEP4_DRIFT_FROM="4"
export STUB_KC_NETPOL_CALLS_PATH="$TMP17/kc_netpol_calls"; : > "$TMP17/kc_netpol_calls"
export STUB_NP_STEP3_DRIFT_PATH="$CLUSDIR_C17/np_step3_drift.json"
export STUB_NP_STEP4_DRIFT_PATH="$CLUSDIR_C17/np_step4_drift.json"
export STUB_NP_INVALID_FROM=""
export STUB_LOG_PATH="$TMP17/stub.log"
export STUB_GET_VALUES_CALLS_PATH="$TMP17/get_values_calls"
export STUB_GET_MANIFEST_CALLS_PATH="$TMP17/get_manifest_calls"
export STUB_VALUE_BASELINE_PATH="$CLUSDIR_C17/values_baseline.yaml"
export STUB_VALUE_DRIFT_PATH="$CLUSDIR_C17/values_baseline.yaml"
export STUB_MANIFEST_BASELINE_PATH="$CLUSDIR_C17/manifest_baseline.txt"
export STUB_MANIFEST_DRIFT_PATH="$CLUSDIR_C17/manifest_baseline.txt"
export STUB_DRIFT_REV=0 STUB_DRIFT_VALUES=0 STUB_DRIFT_MANIFEST=0
export STUB_STATUS_CALLS_PATH="$TMP17/status_calls"; : > "$TMP17/status_calls"
export PATH="$TMP17/stub_path:$PATH"
mkdir -p "$TMP17/fake_chart" "$TMP17/fixtures/integrationcni"
printf 'networkPolicy:\n  allowedPeerPorts: []\n' > "$TMP17/fixtures/integrationcni/values-extra-cni.yaml"
export VALUES_EXTRA="$TMP17/fixtures/integrationcni/values-extra-cni.yaml"
export ARTIFACTS="$ARTDIR_C17"
export CHART_PATH="$TMP17/fake_chart"
export RELEASE="nexus-cni-upgrade"
cp "$TMP17/cluster-up.txt" "$ARTDIR_C17/cluster-up.txt"
RC_C17=0
bash "$TARGET" 2>"$TMP17/c17.err" 1>"$TMP17/c17.out" || RC_C17=$?
unset STUB_RC_STEP1 STUB_RC_STEP2 STUB_RC_STEP3 STUB_STATE_PATH STUB_LIST_REV_PATH STUB_NP_PATH \
      STUB_LOG_PATH STUB_GET_VALUES_CALLS_PATH STUB_GET_MANIFEST_CALLS_PATH \
      STUB_VALUE_BASELINE_PATH STUB_VALUE_DRIFT_PATH \
      STUB_MANIFEST_BASELINE_PATH STUB_MANIFEST_DRIFT_PATH \
      STUB_DRIFT_REV STUB_DRIFT_VALUES STUB_DRIFT_MANIFEST STUB_DRIFT_REV_TO \
      STUB_STATUS_CALLS_PATH STUB_LIST_CALLS_PATH \
      STUB_NP_BASELINE_PATH STUB_NP_STEP1_PATH \
      STUB_NP_STEP3_DRIFT_FROM STUB_NP_STEP4_DRIFT_FROM \
      STUB_NP_STEP3_DRIFT_PATH STUB_NP_STEP4_DRIFT_PATH STUB_NP_INVALID_FROM \
      STUB_STATUS_FAIL_FROM STUB_STATUS_FAIL_TO STUB_STATUS_FAIL_VALUE STUB_STATUS_FAIL_RC \
      STUB_GET_VALUES_FAIL_FROM STUB_GET_VALUES_FAIL_RC \
      STUB_GET_MANIFEST_FAIL_FROM STUB_GET_MANIFEST_FAIL_RC \
      STUB_KC_NETPOL_FAIL_FROM STUB_KC_NETPOL_FAIL_RC \
      STUB_RELEASE_NAME
ERR_C17="$(cat "$TMP17/c17.err" 2>/dev/null)"
[[ "$RC_C17" -ne 0 ]] && pass "control 17: rc != 0 (rc=$RC_C17)" || fail "control 17: expected rc != 0 on S4 NetPol drift; rc=0; stderr='$ERR_C17'"
# Step 3 sentinel must be present (matches post-rejection state).
[[ "$(cat "$ARTDIR_C17/upgrade-step3.txt" 2>/dev/null)" == "rejected-invalid-upgrade-ok" ]] && pass "control 17: step3 sentinel present (rejected-invalid-upgrade-ok)" || fail "control 17: step3 sentinel absent"
[[ ! -f "$ARTDIR_C17/upgrade-step4.txt" ]] && pass "control 17: no upgrade-step4 sentinel" || fail "control 17: upgrade-step4 sentinel emitted despite step4 NetPol drift"
[[ -n "$ERR_C17" && ( "$ERR_C17" == *"NetworkPolicy identity drifted"* || "$ERR_C17" == *"np-post-id"* ) ]] && pass "control 17: stderr identifies NetworkPolicy identity drift" || fail "control 17: stderr did not identify NetPol drift; got '${ERR_C17}'"

# ============================================================================
# Control 18 — Step-2 NetworkPolicy enforcement transition absent.
#
# Both Step 1 and Step 2 capture return the disabled baseline (items:[]`).
# The disabled→enforced surface transition is structurally absent, so the
# assert_np_transition helper must die before any Step-2 sentinel.
# ============================================================================
echo "== Control 18: step2 NetworkPolicy transition absent =="
TMP18="$(mktemp -d -t d2b-up-c18-XXXXXX)"
ARTDIR_C18="$TMP18/external_artifacts"
CLUSDIR_C18="$TMP18/cluster_state"
mkdir -p "$ARTDIR_C18" "$CLUSDIR_C18"
write_stubs "$TMP18"
seed_cluster_state "$CLUSDIR_C18" same
# Override disabled and baseline stubs to be the same empty docs so the
# canonical identity cannot differ; mirror `kinds` to enforce the disabled
# baseline documents for every capture.
cp "$CLUSDIR_C18/np_step1_disabled.json" "$CLUSDIR_C18/np_baseline.json"
printf 'before=5\nafter=6\n' > "$TMP18/list_rev"
printf 'cluster-up-ok\n' > "$TMP18/clusterup.txt"
export STUB_RELEASE_NAME="nexus-cni-upgrade"
export STUB_RC_STEP1=0
export STUB_RC_STEP2=0
export STUB_RC_STEP3=7
export STUB_STATE_PATH="$TMP18/state"; : > "$TMP18/state"
export STUB_LIST_REV_PATH="$TMP18/list_rev"
export STUB_NP_PATH="$CLUSDIR_C18/np.json"
export STUB_NP_BASELINE_PATH="$CLUSDIR_C18/np_baseline.json"
export STUB_NP_STEP1_PATH="$CLUSDIR_C18/np_step1_disabled.json"
export STUB_KC_NETPOL_CALLS_PATH="$TMP18/kc_netpol_calls"; : > "$TMP18/kc_netpol_calls"
export STUB_LOG_PATH="$TMP18/stub.log"
export STUB_GET_VALUES_CALLS_PATH="$TMP18/get_values_calls"
export STUB_GET_MANIFEST_CALLS_PATH="$TMP18/get_manifest_calls"
export STUB_VALUE_BASELINE_PATH="$CLUSDIR_C18/values_baseline.yaml"
export STUB_VALUE_DRIFT_PATH="$CLUSDIR_C18/values_baseline.yaml"
export STUB_MANIFEST_BASELINE_PATH="$CLUSDIR_C18/manifest_baseline.txt"
export STUB_MANIFEST_DRIFT_PATH="$CLUSDIR_C18/manifest_baseline.txt"
export STUB_DRIFT_REV=0 STUB_DRIFT_VALUES=0 STUB_DRIFT_MANIFEST=0
export STUB_STATUS_CALLS_PATH="$TMP18/status_calls"; : > "$TMP18/status_calls"
export PATH="$TMP18/stub_path:$PATH"
mkdir -p "$TMP18/fake_chart" "$TMP18/fixtures/integrationcni"
printf 'networkPolicy:\n  allowedPeerPorts: []\n' > "$TMP18/fixtures/integrationcni/values-extra-cni.yaml"
export VALUES_EXTRA="$TMP18/fixtures/integrationcni/values-extra-cni.yaml"
export ARTIFACTS="$ARTDIR_C18"
export CHART_PATH="$TMP18/fake_chart"
export RELEASE="nexus-cni-upgrade"
cp "$TMP18/clusterup.txt" "$ARTDIR_C18/cluster-up.txt"
RC_C18=0
bash "$TARGET" 2>"$TMP18/c18.err" 1>"$TMP18/c18.out" || RC_C18=$?
ERR_C18="$(cat "$TMP18/c18.err" 2>/dev/null)"
NP1_C18="$(cat "$ARTDIR_C18/upgrade-step1-np-id" 2>/dev/null || echo missing)"
NP2_C18="$(cat "$ARTDIR_C18/upgrade-step2-np-id" 2>/dev/null || echo missing)"
unset STUB_RC_STEP1 STUB_RC_STEP2 STUB_RC_STEP3 STUB_STATE_PATH STUB_LIST_REV_PATH \
      STUB_NP_PATH STUB_LOG_PATH STUB_GET_VALUES_CALLS_PATH \
      STUB_GET_MANIFEST_CALLS_PATH STUB_VALUE_BASELINE_PATH STUB_VALUE_DRIFT_PATH \
      STUB_MANIFEST_BASELINE_PATH STUB_MANIFEST_DRIFT_PATH \
      STUB_DRIFT_REV STUB_DRIFT_VALUES STUB_DRIFT_MANIFEST \
      STUB_STATUS_CALLS_PATH STUB_KC_NETPOL_CALLS_PATH \
      STUB_NP_BASELINE_PATH STUB_NP_STEP1_PATH STUB_RELEASE_NAME
[[ "$RC_C18" -ne 0 ]] && pass "control 18: target non-zero on absent transition (rc=$RC_C18)" || fail "control 18: expected non-zero on absent transition, got rc=0; stderr='${ERR_C18}'"
[[ -n "$NP1_C18" && "$NP1_C18" != missing ]] && pass "control 18: step1 disabled identity captured (${NP1_C18:0:12})" || fail "control 18: step1 disabled identity missing"
# NP2 file may exist (written by capture_np_asserted before the transition
# helper fires); the contract is that no upgrade-step2.txt success sentinel
# exists and the stderr names the transition-missing / surface-empty
# diagnostic, both of which are asserted next.
for s in upgrade-step2.txt upgrade-step3.txt upgrade-step4.txt; do
    [[ ! -f "$ARTDIR_C18/$s" ]] && pass "control 18: $s sentinel-absent (transition helper blocks)" || fail "control 18: $s present despite absent transition"
done
if [[ -n "$ERR_C18" && ( "$ERR_C18" == *"NetworkPolicy enforcement transition missing"* || "$ERR_C18" == *"enforced NetworkPolicy surface empty"* ) ]]; then
    pass "control 18: stderr names transition-missing or surface-empty diagnostic"
else
    fail "control 18: stderr did not include transition-missing / surface-empty diagnostic; got '${ERR_C18}'"
fi

# ============================================================================
# Controls 19-22 — d2b.53 bounded Helm client transport.
#
# Heavy run 33642318757 failed Step 1 with "client rate limiter Wait returned
# an error: context deadline exceeded". The repair declares validated
# transport constants and applies
#   --wait --timeout <T> --qps <Q> --burst-limit <B>
# to every mutating helm install / upgrade in Steps 1-3. These controls prove
# the flags are actually on the argv, that an invalid override issues ZERO
# Helm calls, that no retry loop was introduced, and that the original
# install-failure sentinel behaviour is unchanged.
# ============================================================================
echo
echo "== Control 19: static transport-flag contract =="

# 19a. The three constants are declared with the required defaults.
for kv in "D2B_HELM_TIMEOUT:-10m" "D2B_HELM_QPS:-50" "D2B_HELM_BURST_LIMIT:-100"; do
    if grep -qF "\${${kv}}" "$TARGET"; then
        pass "control 19a: constant declared with required default: \${${kv}}"
    else
        fail "control 19a: constant missing or wrong default: \${${kv}}"
    fi
done

# 19b. Every mutating helm install / upgrade carries --wait and all three
#      transport flags within its own continued-line argv block.
C19_MUTATING=0
C19_MISSING=""
while IFS=: read -r ln _; do
    # An argv block is the helm line plus its backslash-continued lines up to
    # the closing paren of run_helm_capture's command substitution.
    blk=$(awk -v start="$ln" 'NR>=start { print; if ($0 ~ /\)[[:space:]]*$/) exit }' "$TARGET")
    C19_MUTATING=$((C19_MUTATING + 1))
    for flag in '--wait' '--timeout "$D2B_HELM_TIMEOUT"' '--qps "$D2B_HELM_QPS"' '--burst-limit "$D2B_HELM_BURST_LIMIT"'; do
        case "$blk" in
            *"$flag"*) : ;;
            *) C19_MISSING="${C19_MISSING} line${ln}:${flag}" ;;
        esac
    done
done < <(grep -nE '^\s*helm (install|upgrade) "\$\{RELEASE\}"' "$TARGET")
if [[ "$C19_MUTATING" -eq 3 ]]; then
    pass "control 19b: exactly 3 mutating helm argv blocks found (Steps 1-3)"
else
    fail "control 19b: expected 3 mutating helm argv blocks, found ${C19_MUTATING}"
fi
if [[ -z "$C19_MISSING" ]]; then
    pass "control 19b: every mutating helm argv carries --wait --timeout --qps --burst-limit"
else
    fail "control 19b: transport flags missing:${C19_MISSING}"
fi

# 19c. --atomic is retained on the two upgrade steps alongside the transport
#      flags (the atomic invalid-upgrade proof must not be weakened).
C19_ATOMIC=0
while IFS=: read -r ln _; do
    blk=$(awk -v start="$ln" 'NR>=start { print; if ($0 ~ /\)[[:space:]]*$/) exit }' "$TARGET")
    case "$blk" in
        *--atomic*) C19_ATOMIC=$((C19_ATOMIC + 1)) ;;
    esac
done < <(grep -nE '^\s*helm upgrade "\$\{RELEASE\}"' "$TARGET")
if [[ "$C19_ATOMIC" -eq 2 ]]; then
    pass "control 19c: --atomic retained on both upgrade steps alongside transport flags"
else
    fail "control 19c: expected --atomic on 2 upgrade steps, found ${C19_ATOMIC}"
fi

# 19d. Validation precedes the first Helm command in source order.
C19_VALIDATE_LN=$(grep -nE '^transport_die\(\)' "$TARGET" | head -n1 | cut -d: -f1)
C19_FIRST_HELM_LN=$(grep -nE '^\s*helm (install|upgrade|uninstall)' "$TARGET" | head -n1 | cut -d: -f1)
if [[ -n "$C19_VALIDATE_LN" && -n "$C19_FIRST_HELM_LN" && "$C19_VALIDATE_LN" -lt "$C19_FIRST_HELM_LN" ]]; then
    pass "control 19d: transport validation (line ${C19_VALIDATE_LN}) precedes the first helm invocation (line ${C19_FIRST_HELM_LN})"
else
    fail "control 19d: transport validation does not precede the first helm invocation (validate=${C19_VALIDATE_LN} helm=${C19_FIRST_HELM_LN})"
fi

# 19e. No retry loop / rerun wrapper around an asserted Helm operation.
if grep -nE '^\s*(for|while|until)\b.*\b(attempt|retry|tries|helm)\b' "$TARGET" >/dev/null 2>&1; then
    fail "control 19e: a loop construct references helm/attempt/retry — retry-until-green is forbidden"
else
    pass "control 19e: no retry/rerun loop wraps an asserted helm operation"
fi

echo
echo "== Control 20: runtime transport argv on the happy path =="
run_control "C20-transport-argv" 0 0 7 same
T20=$(cat /tmp/.d2b-up-last-tmp)
RC20=$(cat "$T20/result.rc")
LOG20="$T20/stub.log"
[[ "$RC20" -eq 0 ]] && pass "control 20: script rc=0 with transport flags applied" || fail "control 20: expected rc=0, got ${RC20}; stderr=$(cat "$T20/result.err.txt" 2>/dev/null)"

# Exactly one install and exactly two upgrades reached the stub: one attempt
# per step, no retry.
C20_INSTALLS=$(grep -c 'args: install ' "$LOG20" 2>/dev/null | tr -d ' \n'); [[ "$C20_INSTALLS" =~ ^[0-9]+$ ]] || C20_INSTALLS=0
C20_UPGRADES=$(grep -c 'args: upgrade ' "$LOG20" 2>/dev/null | tr -d ' \n'); [[ "$C20_UPGRADES" =~ ^[0-9]+$ ]] || C20_UPGRADES=0
[[ "$C20_INSTALLS" -eq 1 ]] && pass "control 20: exactly 1 helm install attempt (no retry)" || fail "control 20: expected 1 helm install attempt, saw ${C20_INSTALLS}"
[[ "$C20_UPGRADES" -eq 2 ]] && pass "control 20: exactly 2 helm upgrade attempts (Steps 2 and 3, no retry)" || fail "control 20: expected 2 helm upgrade attempts, saw ${C20_UPGRADES}"

# Every mutating argv line observed by the stub carries all four flags.
C20_BAD=0
C20_SEEN=0
while IFS= read -r line; do
    C20_SEEN=$((C20_SEEN + 1))
    for flag in '--wait' '--timeout 10m' '--qps 50' '--burst-limit 100'; do
        case "$line" in
            *"$flag"*) : ;;
            *) C20_BAD=$((C20_BAD + 1)) ;;
        esac
    done
done < <(grep -E 'args: (install|upgrade) ' "$LOG20" 2>/dev/null || true)
[[ "$C20_SEEN" -eq 3 ]] && pass "control 20: 3 mutating argv lines captured by the stub" || fail "control 20: expected 3 mutating argv lines, saw ${C20_SEEN}"
[[ "$C20_BAD" -eq 0 ]] && pass "control 20: observed argv carries --wait --timeout 10m --qps 50 --burst-limit 100 on all 3 mutating calls" || fail "control 20: ${C20_BAD} transport flag(s) absent from observed mutating argv"

# --atomic still present on both upgrades in the observed argv.
C20_ATOMIC=$(grep -E 'args: upgrade ' "$LOG20" 2>/dev/null | grep -c -- '--atomic' | tr -d ' \n'); [[ "$C20_ATOMIC" =~ ^[0-9]+$ ]] || C20_ATOMIC=0
[[ "$C20_ATOMIC" -eq 2 ]] && pass "control 20: --atomic observed on both upgrade argv lines" || fail "control 20: --atomic observed on ${C20_ATOMIC}/2 upgrade argv lines"

# The transport record artifact documents the single-attempt contract.
ADIR20=$(cat "$T20/result.artdir")
if [[ -s "$ADIR20/upgrade-helm-transport.txt" ]] \
   && grep -qx 'timeout=10m' "$ADIR20/upgrade-helm-transport.txt" \
   && grep -qx 'qps=50' "$ADIR20/upgrade-helm-transport.txt" \
   && grep -qx 'burst_limit=100' "$ADIR20/upgrade-helm-transport.txt" \
   && grep -qx 'retry_loop=none' "$ADIR20/upgrade-helm-transport.txt" \
   && grep -qx 'attempts_per_step=1' "$ADIR20/upgrade-helm-transport.txt"; then
    pass "control 20: upgrade-helm-transport.txt records the validated bounds and the single-attempt contract"
else
    fail "control 20: upgrade-helm-transport.txt missing or incomplete: $(cat "$ADIR20/upgrade-helm-transport.txt" 2>/dev/null | tr '\n' ' ')"
fi

echo
echo "== Control 21: invalid transport override issues ZERO helm calls =="
run_invalid_transport() {
    set +e              # d2b.54 / machine-local fix: this function uses subshell
    set +u              # rc/calls capture patterns; some hosts / bash 3.2 subshell
    set +o pipefail     # chains die because $? on a piped stage trips set -e inherited.
    local label="$1" var="$2" val="$3"
    local tmp artdir clusterdir
    tmp="$(mktemp -d -t d2b-up-t-XXXXXX)"
    artdir="$tmp/external_artifacts"
    clusterdir="$tmp/cluster_state"
    mkdir -p "$artdir" "$clusterdir"
    write_stubs "$tmp"
    seed_cluster_state "$clusterdir" same
    printf 'before=5\nafter=6\n' > "$tmp/list_rev"
    printf 'cluster-up-ok\n' > "$artdir/cluster-up.txt"
    mkdir -p "$tmp/fake_chart" "$tmp/fixtures/integrationcni"
    printf 'networkPolicy:\n  allowedPeerPorts: []\n' > "$tmp/fixtures/integrationcni/values-extra-cni.yaml"
    : > "$tmp/stub.log"

    local rc=0
    (
      export PATH="$tmp/stub_path:$PATH"
      export ARTIFACTS="$artdir"
      export CHART_PATH="$tmp/fake_chart"
      export RELEASE="nexus-cni-upgrade"
      export VALUES_EXTRA="$tmp/fixtures/integrationcni/values-extra-cni.yaml"
      export STUB_LOG_PATH="$tmp/stub.log"
      export STUB_STATE_PATH="$tmp/state"; : > "$tmp/state"
      export STUB_LIST_REV_PATH="$tmp/list_rev"
      export STUB_NP_PATH="$clusterdir/np.json"
      export STUB_RELEASE_NAME="nexus-cni-upgrade"
      export "${var}=${val}"
      bash "$TARGET" > "$tmp/out" 2> "$tmp/err"
    ) || rc=$?

    local calls
    calls=$(grep -c 'args: ' "$tmp/stub.log" 2>/dev/null | tr -d ' \n'); [[ "$calls" =~ ^[0-9]+$ ]] || calls=0
    printf '  [%s] %s=%s -> rc=%s helm_calls=%s\n' "$label" "$var" "$val" "$rc" "$calls"
    [[ "$rc" -ne 0 ]] && pass "control 21 ${label}: non-zero exit on invalid ${var}='${val}' (rc=${rc})" \
                      || fail "control 21 ${label}: expected non-zero exit for ${var}='${val}', got rc=0"
    [[ "$calls" -eq 0 ]] && pass "control 21 ${label}: ZERO helm calls issued for invalid ${var}='${val}'" \
                         || fail "control 21 ${label}: ${calls} helm call(s) issued despite invalid ${var}='${val}'"
    local sent=0
    for s in upgrade-step1.txt upgrade-step2.txt upgrade-step3.txt upgrade-step4.txt; do
        [[ -f "$artdir/$s" ]] && sent=$((sent + 1))
    done
    [[ "$sent" -eq 0 ]] && pass "control 21 ${label}: no sentinel written for invalid ${var}='${val}'" \
                        || fail "control 21 ${label}: ${sent} sentinel(s) written despite invalid ${var}='${val}'"
    if grep -q 'invalid Helm transport configuration' "$tmp/err" 2>/dev/null; then
        pass "control 21 ${label}: stderr names the invalid-transport diagnostic"
    else
        fail "control 21 ${label}: stderr missing invalid-transport diagnostic: $(head -c 200 "$tmp/err" 2>/dev/null)"
    fi
}
run_invalid_transport "timeout-seconds"  D2B_HELM_TIMEOUT      "30s"
run_invalid_transport "timeout-zero"     D2B_HELM_TIMEOUT      "0m"
run_invalid_transport "timeout-bare-int" D2B_HELM_TIMEOUT      "10"
run_invalid_transport "timeout-overmax"  D2B_HELM_TIMEOUT      "999m"
run_invalid_transport "qps-zero"         D2B_HELM_QPS          "0"
run_invalid_transport "qps-negative"     D2B_HELM_QPS          "-5"
run_invalid_transport "qps-decimal"      D2B_HELM_QPS          "50.5"
run_invalid_transport "qps-padded"       D2B_HELM_QPS          "050"
run_invalid_transport "qps-overmax"      D2B_HELM_QPS          "100000"
run_invalid_transport "burst-zero"       D2B_HELM_BURST_LIMIT  "0"
run_invalid_transport "burst-word"       D2B_HELM_BURST_LIMIT  "many"
run_invalid_transport "burst-below-qps"  D2B_HELM_BURST_LIMIT  "10"
run_invalid_transport "burst-overmax"    D2B_HELM_BURST_LIMIT  "999999"

echo
echo "== Control 22: transport flags do not rescue a failing install =="
run_control "C22-install-fail-with-transport" 2 0 7 same
T22=$(cat /tmp/.d2b-up-last-tmp)
RC22=$(cat "$T22/result.rc")
ADIR22=$(cat "$T22/result.artdir")
ERR22=$(cat "$T22/result.err.txt" 2>/dev/null)
[[ "$RC22" -ne 0 ]] && pass "control 22: non-zero exit when install fails despite transport flags (rc=${RC22})" || fail "control 22: expected non-zero exit, got rc=0"
[[ ! -f "$ADIR22/upgrade-step1.txt" ]] && pass "control 22: install-disabled-ok sentinel absent (original rc semantics preserved)" || fail "control 22: step1 sentinel written despite install failure"
RCF22=$(cat "$ADIR22/upgrade-step1.rc" 2>/dev/null || echo missing)
[[ "$RCF22" == "2" ]] && pass "control 22: upgrade-step1.rc captured the original install exit code (2)" || fail "control 22: upgrade-step1.rc='${RCF22}', expected the original 2"
case "$ERR22" in
    *"refusing to write install-disabled-ok sentinel"*) pass "control 22: stderr names the original fail-closed diagnostic" ;;
    *) fail "control 22: stderr missing the original fail-closed diagnostic: '${ERR22}'" ;;
esac
C22_INSTALLS=$(grep -c 'args: install ' "$T22/stub.log" 2>/dev/null | tr -d ' \n'); [[ "$C22_INSTALLS" =~ ^[0-9]+$ ]] || C22_INSTALLS=0
[[ "$C22_INSTALLS" -eq 1 ]] && pass "control 22: the failing install was attempted exactly once (no retry-until-green)" || fail "control 22: install attempted ${C22_INSTALLS} time(s); expected exactly 1"

echo
echo "d2b-upgrade-rehearsal failclosed contract: PASS"
if (( ${#_FAIL_LIST[@]} > 0 )); then
  echo "FAILURES:" >&2
  printf '  - %s\n' "${_FAIL_LIST[@]}" >&2
  exit 7
fi
exit 0
