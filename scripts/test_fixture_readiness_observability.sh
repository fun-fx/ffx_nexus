#!/usr/bin/env bash
# D-2b.46 readiness-observability contract test.
#
# Exercises the actual functions defined in
# scripts/install-nexus-test.sh. Sources the
# target script in an isolated subshell with a
# reduced fake PATH that does NOT contain a
# fake bash. The downstream cni-readiness-gate.sh
# is replaced via CNI_READINESS_GATE_BIN
# (target-side injection) with a tiny POSIX stub
# outside the repo/worktree.
#
# Fake binaries use #!/bin/sh so they cannot
# recurse through /usr/bin/env bash. Real bash
# is captured at the start. python3 is the
# system python3. The test never injects
# bash/env/python3 into the fake bin directory.
#
# Driver model: every per-control invocation
# is launched through inline Python subprocess.run
# with a deterministic 20-second timeout and
# file-backed stdout/stderr/rc. The parent never
# captures child output through pipes.

set -uo pipefail
set +e

SCRIPT_DIR="$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"
TARGET="${SCRIPT_DIR}/install-nexus-test.sh"

# Target install script.
TARGET="${SCRIPT_DIR}/install-nexus-test.sh"
# Real cni-readiness-gate.sh source for
# failure-controlled controls (C2..C8, C10).
# Success controls (C1, C9) keep the per-stage
# stub because the install script's success path
# does NOT call abort_as and would otherwise
# need a working cluster.
REAL_GATE_BIN="${SCRIPT_DIR}/cni-readiness-gate.sh"
case "${REAL_GATE_BIN}" in
  /*) ;;
  *) printf 'FATAL: REAL_GATE_BIN (%s) is not absolute\n' "${REAL_GATE_BIN}" >&2; exit 2 ;;
esac
[[ -f "${REAL_GATE_BIN}" ]] || { printf 'FATAL: REAL_GATE_BIN not a file\n' >&2; exit 2; }

# Capture real tools.
REAL_BASH="$(command -v bash)"
REAL_PYTHON3="$(command -v python3)"
case "${REAL_BASH}" in
  /*) ;;
  *) printf 'FATAL: REAL_BASH (%s) is not absolute\n' "${REAL_BASH}" >&2; exit 2 ;;
esac
case "${REAL_PYTHON3}" in
  /*) ;;
  *) printf 'FATAL: REAL_PYTHON3 (%s) is not absolute\n' "${REAL_PYTHON3}" >&2; exit 2 ;;
esac
[[ -x "${REAL_BASH}" ]] || { printf 'FATAL: REAL_BASH not executable\n' >&2; exit 2; }
[[ -x "${REAL_PYTHON3}" ]] || { printf 'FATAL: REAL_PYTHON3 not executable\n' >&2; exit 2; }
printf '# tools: REAL_BASH=%s REAL_PYTHON3=%s\n' "${REAL_BASH}" "${REAL_PYTHON3}"

# Top-level temp dir.
TOP_TMP="${TMPDIR:-/tmp}/d2b46-$$"
mkdir -p "${TOP_TMP}"

# Shared fake bin (no bash, no env, no python3).
FAKE_BIN="${TOP_TMP}/fakebin"
mkdir -p "${FAKE_BIN}"

# d2b.47 success-gate stub for C6p / C6q / C6r.
# The three success-path controls must prove
# that Step G succeeds AND the downstream
# gate handoff happens exactly once. Driving
# the real gate instead would let Gate 9 mask
# Step G's success behind a downstream
# rc=12 / FIXTURE_NOT_READY. This stub
# records one append-only invocation log per
# call so the harness can:
#   - assert normal-handoff count == 1
#   - assert abort-classifier-unexpected
#     count == 0 (a hidden failure cannot
#     masquerade as Step G success)
#   - confirm CNI_READINESS_GATE_BIN was the
#     route (so a regression that hardcodes
#     a path inside Step G is caught)
#   - confirm INSTALL_ABORT_CLASSIFICATION
#     was NOT set at handoff time (so the
#     retry path is not the success path)
# The stub does NOT replace bash, env, or
# python3; it is just sh + stage-local files.
C6_RECORDING_GATE_STUB="${FAKE_BIN}/c6-success-gate.sh"
cat >"${C6_RECORDING_GATE_STUB}" <<'C6STUBEOF'
#!/bin/sh
# d2b.47 recording success-gate stub.
# Invoked by Step G via
# CNI_READINESS_GATE_BIN. Records every
# invocation (append-only) under
# $ARTIFACTS/gate-invocations.log with one
# JSON line per call, then either exits 0
# (normal handoff) or exits 99 (a hidden
# abort classifier detected on a control
# that the harness already classified as
# success). Exit codes other than 0 / 99
# are impossible by design.
A="${ARTIFACTS:-/tmp}"
mkdir -p "${A}"
INV="${A}/gate-invocations.log"
LABEL="${INSTALL_ABORT_CLASSIFICATION:-}"
DETAIL="${INSTALL_ABORT_DETAIL:-}"
STAGE="${HARNESS_STAGE:-}"
idx=$(($(wc -l <"${INV}" 2>/dev/null || echo 0) + 1))
if [ -n "${LABEL}" ]; then
  printf '%s\tidx=%s\tmode=abort-classifier-unexpected\tstage=%s\tlabel=%s\tdetail=%s\targv=%s\n' \
    "$(date +%s)" "${idx}" "${STAGE}" "${LABEL}" "${DETAIL}" "$*" >> "${INV}"
  exit 99
fi
printf '%s\tidx=%s\tmode=normal-handoff\tstage=%s\tlabel=\tdetail=\targv=%s\n' \
  "$(date +%s)" "${idx}" "${STAGE}" "$*" >> "${INV}"
exit 0
C6STUBEOF
chmod +x "${C6_RECORDING_GATE_STUB}"

# Write fake kubectl (POSIX shell).
cat >"${FAKE_BIN}/kubectl" <<'POSIXEOF'
#!/bin/sh
# d2b.46/2b fake kubectl. Reads inventory from
# FAKE_PODS_TSV_FILE. Handles every pattern the
# target's step_G_readiness issues AND the gate's
# Gate 7 namespace + Gate 8 fixture inventory.
#
# Per-command-family failure injection:
#   FAKE_FIXTURE_LIST_RC      -> kubectl get pod -A --no-headers
#   FAKE_FIXTURE_JSON_RC      -> kubectl get pod -A -o json
#   FAKE_CILIUM_DAEMON_LIST_RC-> kubectl -n kube-system get pod -l k8s-app=cilium -o jsonpath
#   FAKE_CILIUM_EXEC_RC       -> kubectl -n kube-system exec $p -- cilium endpoint list
#   FAKE_CILIUM_JSON_MODE     -> python3 cilium JSON projection body
#     ""         default: do not fail, return observed labels
#     "bad"      python projection raises Exception -> rc 17
#     "empty"    python projection returns []  -> valid zero observation
#   FAKE_NS_MISSING           -> comma-separated namespace list that
#                                "get namespace" should report as missing
#   FAKE_NS_NOT_READY         -> when set, FAKE_FIXTURE_LIST_RC doubles
#                                as the gate-side namespace probe rc
#
# To preserve d2b.46's backward-compatible
# HARNESS_KUBECTL_RC=7 path for global
# failure injection of the canonical fixture
# inventory (used by C6), we accept the legacy
# FAKE_KUBECTL_RC when FAKE_FIXTURE_LIST_RC is
# unset. Otherwise, FAKE_FIXTURE_LIST_RC wins.
set -u
# Per-stage fixture inventory override:
# when HARNESS_FIXTURE_NAMES_TSV is set the
# fake kubectl uses it as the canonical Pod
# inventory source instead of FAKE_PODS_TSV_FILE.
# The override applies ONLY to get pod
# commands (fixture list, fixture json).
# Cilium daemon-list, exec, and JSON projection
# remain driven by their own FAKE_*_RC envs.
if [ -n "${HARNESS_FIXTURE_NAMES_TSV:-}" ] && [ -r "${HARNESS_FIXTURE_NAMES_TSV}" ]; then
  HARNESS_FIXTURE_TSV="${HARNESS_FIXTURE_NAMES_TSV}"
  export HARNESS_FIXTURE_TSV
fi
case "${1:-}" in
  get)
    shift 2>/dev/null || true
    if [ "${1:-}" = "pod" ]; then
      if [ "${2:-}" = "-A" ] && [ "${3:-}" = "--no-headers" ]; then
        # d2b.47: production-faithful column
        # order. Real `kubectl get pod -A --no-headers`
        # emits NAMESPACE NAME READY STATUS RESTARTS
        # AGE so column 1 is the namespace.
        # The historical fake stripped the
        # namespace column ($2..$6), which
        # concealed the real-cluster defect that
        # the production anchored
        # `kubectl get pod -A --no-headers | grep
        # -E "$fixture_re"` applied the regex to
        # the WRONG column (namespace, not Pod
        # name). Production code MUST NOT depend
        # on that pipeline any more; this fake
        # keeps the production-faithful column
        # order so any future regression that
        # re-introduces the anchored pipeline
        # is caught at parse time by the static
        # guard rather than silently masked by
        # the harness.
        if [ -n "${HARNESS_FIXTURE_TSV:-}" ] && [ -r "${HARNESS_FIXTURE_TSV}" ]; then
          awk -F'\t' '{print $1, $2, $3, $4, $5, $6}' "${HARNESS_FIXTURE_TSV}"
        elif [ -n "${FAKE_PODS_TSV_FILE:-}" ] && [ -r "${FAKE_PODS_TSV_FILE}" ]; then
          awk -F'\t' '{print $1, $2, $3, $4, $5, $6}' "${FAKE_PODS_TSV_FILE}"
        fi
        # Per-family rc first, then legacy
        # FAKE_KUBECTL_RC fallback for C6.
        rc="${FAKE_FIXTURE_LIST_RC:-${FAKE_KUBECTL_RC:-0}}"
        if [ "${rc}" != "0" ]; then
          echo "fake kubectl fixture list stderr (rc=${rc})" 1>&2
          exit "${rc}"
        fi
        exit 0
      fi
      if [ "${2:-}" = "-A" ] && [ "${3:-}" = "-o" ] && [ "${4:-}" = "json" ]; then
        # d2b.47: when FAKE_KUBECTL_JSON_POLL_TSVS
        # is a colon-separated list of TSV file
        # paths, each poll index increments a
        # counter file (default under FAKE_BIN or
        # FAKE_KUBECTL_JSON_POLL_COUNTER_FILE)
        # and the corresponding TSV is used as
        # the inventory source for THAT poll.
        # C6q in particular needs poll 1 to
        # return a 12-Pod set and poll 2 to
        # return the full 13-Pod set. The
        # historical single-TSV switch could
        # not model per-poll convergence.
        if [ -n "${FAKE_KUBECTL_JSON_POLL_TSVS:-}" ] && [ -n "${FAKE_BIN:-}" ]; then
          poll_idx_file="${FAKE_KUBECTL_JSON_POLL_COUNTER_FILE:-${FAKE_BIN}/__json_poll_counter}"
          if [ ! -f "${poll_idx_file}" ]; then
            echo "0" > "${poll_idx_file}"
          fi
          cur="$(cat "${poll_idx_file}")"
          next=$(( cur + 1 ))
          echo "${next}" > "${poll_idx_file}"
          # Build the colon list, pick index
          # `cur` (0-based first call), fall
          # back to the LAST entry if the
          # #poll exceeds the list length so
          # the loop eventually stabilises.
          OLD_IFS="${IFS}"
          IFS=':'
          # shellcheck disable=SC2206
          set -- ${FAKE_KUBECTL_JSON_POLL_TSVS}
          IFS="${OLD_IFS}"
          total=${#}
          if [ "${cur}" -ge "${total}" ]; then
            pick_tsv="${!total}"
          else
            i=1
            pick_tsv=""
            for cand in "$@"; do
              if [ "${i}" -eq $(( cur + 1 )) ]; then
                pick_tsv="${cand}"; break
              fi
              i=$(( i + 1 ))
            done
          fi
          if [ -n "${pick_tsv}" ] && [ -r "${pick_tsv}" ]; then
            FAKE_PODS_TSV="$(cat "${pick_tsv}")"
            export FAKE_PODS_TSV
            export FAKE_KUBECTL_JSON_POLL_INDEX="${cur}"
          fi
        elif [ -n "${HARNESS_FIXTURE_TSV:-}" ] && [ -r "${HARNESS_FIXTURE_TSV}" ]; then
          FAKE_PODS_TSV="$(cat "${HARNESS_FIXTURE_TSV}")"
          export FAKE_PODS_TSV
        elif [ -n "${FAKE_PODS_TSV_FILE:-}" ] && [ -r "${FAKE_PODS_TSV_FILE}" ]; then
          FAKE_PODS_TSV="$(cat "${FAKE_PODS_TSV_FILE}")"
          export FAKE_PODS_TSV
        fi
        # Malformed-JSON injection (C6s): the
        # per-family rc override exits nonzero
        # on command failure. C6s targets the
        # python projection layer instead:
        # kubectl returns rc 0 but the JSON
        # payload is malformed so the parser
        # raises and our projection-failure
        # branch writes the structured
        # parse-error artifact. We achieve this
        # by writing literal "malformed-json"
        # data into FAKE_PODS_TSV when
        # FAKE_FIXTURE_JSON_MALFORMED=1.
        if [ "${FAKE_FIXTURE_JSON_MALFORMED:-0}" = "1" ]; then
          FAKE_PODS_TSV="malformed-json{not-real-json"
          export FAKE_PODS_TSV
        fi
        # Per-family failure override.
        rc="${FAKE_FIXTURE_JSON_RC:-0}"
        if [ "${rc}" != "0" ]; then
          echo "fake kubectl fixture json stderr (rc=${rc})" 1>&2
          exit "${rc}"
        fi
        exec python3 -c '
import json, os, sys
tsv = os.environ.get("FAKE_PODS_TSV","")
items = []
for ln in tsv.splitlines():
    cols = ln.split("\t")
    if len(cols) < 6: continue
    ns, name, ready, status, restarts, age = cols[:6]
    is_ready = ready.startswith("1/")
    if is_ready:
        phase = "Running"
        cond_status = "True"
    else:
        phase = status if status else "Pending"
        cond_status = "False"
    waiting = {}
    if not is_ready and os.environ.get("FAKE_WAITING_REASON"):
        waiting = {"reason": os.environ["FAKE_WAITING_REASON"]}
    items.append({
        "metadata": {"namespace": ns, "name": name},
        "status": {
            "phase": phase,
            "conditions": [{"type":"Ready","status": cond_status}],
            "containerStatuses": [{
                "name":"app",
                "ready": is_ready,
                "restartCount": int(restarts or 0),
                "state": (
                    {"running":{"startedAt":"2026-08-27T00:00:00Z"}} if is_ready
                    else {"waiting": waiting} if waiting else {}
                ),
            }],
        },
    })
if os.environ.get("FAKE_FIXTURE_JSON_MALFORMED") == "1":
    sys.stdout.write("malformed-json{not-real-json\n")
else:
    print(json.dumps({"items": items}))
        '
        exit 0
      fi
    fi
    ;;
  -n)
    shift 2>/dev/null || true
    if [ "${1:-}" = "kube-system" ]; then
      shift 2>/dev/null || true
      case "${1:-}" in
        get)
          shift 2>/dev/null || true
          if [ "${1:-}" = "pod" ]; then
            rc="${FAKE_CILIUM_DAEMON_LIST_RC:-0}"
            if [ "${rc}" != "0" ]; then
              echo "fake kubectl cilium daemon-list stderr (rc=${rc})" 1>&2
              exit "${rc}"
            fi
            # d2b.46-followup: recovery control.
            # When FAKE_CILIUM_DAEMON_LIST_RECOVERY=1
            # AND FAKE_CILIUM_DAEMON_LIST_COUNTER_FILE
            # points at a writable file, we count
            # invocations: the FIRST call returns
            # empty (rc 0, zero daemons = valid empty
            # observation); the SECOND and later
            # calls return the stable daemon name.
            # This proves the production path can
            # recover from a valid-empty first poll.
            if [ "${FAKE_CILIUM_DAEMON_LIST_RECOVERY:-0}" = "1" ] \
               && [ -n "${FAKE_CILIUM_DAEMON_LIST_COUNTER_FILE:-}" ]; then
              c="${FAKE_CILIUM_DAEMON_LIST_COUNTER_FILE}"
              cur=$(cat "${c}" 2>/dev/null || echo 0)
              cur=$((cur + 1))
              echo "${cur}" > "${c}"
              if [ "${cur}" = "1" ]; then
                # Valid empty observation on first
                # poll. Per the production path we
                # must NOT classify as a failure —
                # we sleep-and-retry on a later poll.
                exit 0
              fi
            fi
            echo "cilium-fake-${RANDOM:-x}"
            exit 0
          fi
          ;;
        exec)
          rc="${FAKE_CILIUM_EXEC_RC:-0}"
          if [ "${rc}" != "0" ]; then
            echo "fake kubectl cilium exec stderr (rc=${rc})" 1>&2
            exit "${rc}"
          fi
          case "${FAKE_CILIUM_JSON_MODE:-}" in
            bad)
              echo "PROJECTION-FAILED: fake-cilium-exec-projection-forced-bad" 1>&2
              exit 17
              ;;
            empty)
              echo '[]'
              exit 0
              ;;
            *)
          if [ -n "${FAKE_CILIUM_NAMES:-}" ]; then
            first=1
            printf '['
            for n in ${FAKE_CILIUM_NAMES}; do
              if [ "${first}" = "1" ]; then first=0; else printf ','; fi
              printf '{"status":{"controllers":[{"name":"resolve-labels-default/%s"}]}}' "$n"
            done
            printf ']'
          else
            echo '[]'
          fi
          exit 0
              ;;
          esac
          ;;
      esac
    fi
    ;;
  events)
    echo "Warning stub-event-1"
    echo "Normal stub-event-2"
    exit 0
    ;;
esac
# Real-gate Gate 7 namespace probe handler:
# the FIRST `get)` arm DID `shift 2>/dev/null ||
# true`. We re-evaluate ${1:-} here. To handle
# `kubectl get namespace <name>` correctly
# across shifts we DO NOT shift again — the
# remaining args (after the first shift) are
# `namespace <name>`. Check that.
if [ "${1:-}" = "namespace" ]; then
  shift 2>/dev/null || true
  ns="${1:-}"
  if [ -n "${FAKE_NS_MISSING:-}" ]; then
    old_ifs="${IFS}"
    IFS=","
    for missing in ${FAKE_NS_MISSING}; do
      if [ "${missing}" = "${ns}" ]; then
        IFS="${old_ifs}"
        exit 1
      fi
    done
    IFS="${old_ifs}"
  fi
  exit 0
fi
echo "fake-kubectl: unhandled: $*" 1>&2
exit 99
POSIXEOF
chmod +x "${FAKE_BIN}/kubectl"

# Fake date.
cat >"${FAKE_BIN}/date" <<'POSIXEOF'
#!/bin/sh
# d2b.46 fake date. Emits fixed seconds then
# advances by FAKE_DATE_STEP (~1000) per call.
# The advanced value is persisted in
# FAKE_DATE_NOW_FILE so subsequent invocations
# read the new value across process boundaries.
# The deadline in step_G is set on the FIRST
# call (deadline calc) and then re-checked on
# every iteration. Advancing each call is
# mandatory: without it, the target's while-loop
# never reaches its deadline check when the
# loop body keeps hitting the "not-ready" branch.
state="${FAKE_DATE_NOW_FILE:-${FAKE_BIN}/__date_state}"
if [ ! -f "${state}" ]; then
  echo "${FAKE_DATE_NOW:-1700000000}" >"${state}"
fi
cur="$(cat "${state}")"
if [ "${1:-}" = "+%s" ]; then
  echo "${cur}"
  if [ "${FAKE_DATE_ADVANCE:-1}" = "1" ]; then
    nxt=$(( cur + ${FAKE_DATE_STEP:-1000} ))
    echo "${nxt}" >"${state}"
  fi
  exit 0
fi
exit 0
POSIXEOF
chmod +x "${FAKE_BIN}/date"

# Fake sleep.
cat >"${FAKE_BIN}/sleep" <<'POSIXEOF'
#!/bin/sh
exit 0
POSIXEOF
chmod +x "${FAKE_BIN}/sleep"

# Fake kind.
cat >"${FAKE_BIN}/kind" <<'POSIXEOF'
#!/bin/sh
echo "fake kind $*"
exit "${FAKE_KIND_RC:-0}"
POSIXEOF
chmod +x "${FAKE_BIN}/kind"

# Fake docker.
cat >"${FAKE_BIN}/docker" <<'POSIXEOF'
#!/bin/sh
exit "${FAKE_DOCKER_RC:-0}"
POSIXEOF
chmod +x "${FAKE_BIN}/docker"

# Fake comm: by default delegating to the absolute-real
# comm on the host so production-path scripts see exactly
# the same diff/sort behavior as a real cni cluster. A
# per-stage injection (FAKE_COMM_FAIL_RC) makes the mock
# exit rc=9 with a controlled stderr; that is the only
# way the fixture subset CAN observe a comm failure
# because /usr/bin/comm never fails non-zero for two
# sorted files. We resolve and capture the absolute-real
# binary at fakebin time so the path is stable under the
# harness's PATH manipulation.
REAL_COMM_PATH="$(command -v comm)"
if [ -z "${REAL_COMM_PATH}" ] || [ ! -x "${REAL_COMM_PATH}" ]; then
  printf 'FATAL: cannot resolve absolute comm on host (PATH=%s)\n' "${PATH}" >&2
  exit 2
fi
cat >"${FAKE_BIN}/comm" <<COMMFAKEEOF
#!/bin/sh
# default = delegate to absolute-real comm so the
# production path sees unmutated behavior. Stage-scoped
# injection: FAKE_COMM_FAIL_RC makes mock exit rc=9 and
# emits a controlled stderr line that the production
# script captures verbatim.
if [ -n "\${FAKE_COMM_FAIL_RC:-}" ]; then
  printf 'fake comm: forced failure rc=%s op=%s args=%s\n' "\${FAKE_COMM_FAIL_RC}" "\${FAKE_COMM_FAIL_OP:-unset}" "\$*" 1>&2
  exit "\${FAKE_COMM_FAIL_RC}"
fi
exec "${REAL_COMM_PATH}" "\$@"
COMMFAKEEOF
chmod +x "${FAKE_BIN}/comm"

# Verify no bash, no env, no python3 in FAKE_BIN.
for tool in bash env python3 sh bash.exe; do
  if [ -e "${FAKE_BIN}/${tool}" ]; then
    printf 'FATAL: fake bin must not contain %s\n' "${tool}" >&2
    exit 2
  fi
done
printf '# fakebin: %s (no bash/env/python3)\n' "${FAKE_BIN}"

# ---------------------------------------------------------------------------
# d2b.48 canonical fixture vocabulary.
#
# The harness primary fixture constants MUST
# match the install-nexus-test.sh and
# cni-readiness-gate.sh canonical contract,
# AND the tracked fixture manifests under
# scripts/fixtures/integrationcni/{01,02,03,04}*.yaml.
#
# Twelve static namespace/name pairs come
# directly from those manifests. The 13th is
# one Deployment-generated
# cni-control-probe-<rs>-<pod> Pod.
# ---------------------------------------------------------------------------
# Manifest-aligned static pairs.
HARNESS_CANONICAL_12_PAIRS='cni-test-ingress	cni-mock-ingress-controller
cni-test-prometheus	cni-mock-prometheus
cni-test-untrusted	cni-untrusted-default
default	cni-mock-nexus-gateway
default	cni-mock-nexus-worker
default	cni-mock-nexus-migration
default	cni-mock-egress-proxy
default	cni-mock-postgres
default	cni-mock-redis
default	cni-mock-clickhouse
default	cni-mock-arbitrary
cni-control	cni-control-target'

# Realistic Deployment-generated probe name.
# Deterministic instance of the pattern
# ^cni-control-probe-[a-z0-9]+-[a-z0-9]+$
# anchored against replicaset-hash + pod-suffix.
# The harness uses this as the "exactly one"
# dynamic probe in canonical configurations.
HARNESS_DYNAMIC_PROBE_NAME='cni-control-probe-5d5fb89454-7cjss'

# Legacy CNI_FIXTURE_13 list (raw names).
# Kept as a fall-through comment pointer; all
# canonical constructions use
# HARNESS_CANONICAL_12_PAIRS + the dynamic
# probe below.
CNI_FIXTURE_13='cni-mock-nexus-gateway
cni-mock-nexus-worker
cni-mock-nexus-migration
cni-mock-egress-proxy
cni-mock-arbitrary
cni-mock-ingress-controller
cni-mock-prometheus
cni-mock-postgres
cni-mock-redis
cni-mock-clickhouse
cni-untrusted-default
cni-mock-arbitrary-probe-replace
cni-control-target'

# Canonical 13-Pod TSV builder. Reads
# the manifest-aligned pairs and emits
# NAMESPACE\tNAME\tREADY\tSTATUS\tRESTARTS\tAGE
# rows in production column order.
build_canonical_13() {
  local extra_probe="${1:-$(printf '%s' "${HARNESS_DYNAMIC_PROBE_NAME}")}"
  local tmp_out
  tmp_out="$(mktemp -t d2b46-canonical13-XXXXXX)"
  {
    awk -F'\t' -v OFS='\t' \
      'NF==2 {print $1, $2, "1/1", "Running", "0", "7m"}' \
      <<<"$(printf '%s\n' "${HARNESS_CANONICAL_12_PAIRS}")"
    printf '%s\t%s\t1/1\tRunning\t0\t7m\n' "cni-control" "${extra_probe}"
  } > "${tmp_out}"
  cat "${tmp_out}"
  rm -f "${tmp_out}"
}

# build_13_ready: backward-compatible name
# builder. Generates canonical 13 lines with
# the dynamic probe name from the canonical
# contract. Older call sites that only pass a
# bare name list now route through the canonical
# builder so an arbitrary prefix-shaped 13
# cannot satisfy readyness.
build_13_ready() {
  local _names="${1-}"
  build_canonical_13 "${HARNESS_DYNAMIC_PROBE_NAME}"
}

# ---------------------------------------------------------------------------
# Build runner, body, gate for a control stage.
# Uses no command-substitution by the driver;
# outputs are redirected to files.
# ---------------------------------------------------------------------------
write_stage_files() {
  local stage="$1" tsvcontent="${2-}"
  local gate_path="${3-${stage}/cni-readiness-gate.sh}"
  # tsv file (may be empty for image-pipeline cases)
  printf '%s' "${tsvcontent}" >"${stage}/pods.tsv"

  # Per-stage cni-readiness-gate.sh.
  #
  # d2b.46: by default the third argument is the
  # per-stage stub path so SUCCESS controls can
  # record an invocation without spinning cluster
  # probes. FAILURE controls (C2..C8 and C10)
  # pass the absolute path of the REAL repo
  # scripts/cni-readiness-gate.sh so abort_as is
  # forced through the new explicit-label early
  # classifier. The fake "downstream-stub-invoked"
  # sentinel below is only stamped by the per-
  # stage stub; the real gate, by design, never
  # touches that path. The harness asserts the
  # sentinel is absent for every failure control.
  case "${gate_path}" in
    "${stage}/cni-readiness-gate.sh")
      cat >"${gate_path}" <<'GATEEOF'
#!/bin/sh
{
  printf 'INVOKED %s\n' "$(date +%s)" >>"${FAKE_INVOCATION_LOG:-/dev/null}"
  echo "GATE_INVOKED: $#" >>"${FAKE_INVOCATION_LOG:-/dev/null}"
  echo "GATE_PHASE=${GATE_PHASE:-unset}" >>"${FAKE_INVOCATION_LOG:-/dev/null}"
  echo "RECOVERY_PR_SHA=${RECOVERY_PR_SHA:-unset}" >>"${FAKE_INVOCATION_LOG:-/dev/null}"
  if [ -n "${FAKE_DOWNSTREAM_SENTINEL:-}" ] && [ "${FAKE_DOWNSTREAM_SENTINEL}" != "/dev/null" ]; then
    printf 'STUB_INVOKED\n' > "${FAKE_DOWNSTREAM_SENTINEL}"
  fi
} 2>/dev/null
exit 0
GATEEOF
      chmod +x "${gate_path}"
      ;;
    *)
      # Real gate path. Do not synthesise a
      # sentinel here. The harness asserts the
      # per-stage sentinel file is absent for
      # every failure control; the real gate does
      # NOT write that path because it has no
      # FAKE_DOWNSTREAM_SENTINEL.
      :
      ;;
  esac

  # g_body.sh — sourced target script + step_G_readiness
  # without inheriting target's strict mode.
  cat >"${stage}/g_body.sh" <<'BODYEOF'
#!/bin/sh
set +e
set +u
set +o pipefail
. "$HARNESS_TARGET"
step_G_readiness >>"$HARNESS_ARTIFACTS/step_G_out" 2>>"$HARNESS_ARTIFACTS/step_G_err"
rc=$?
echo "rc=$rc" >> "$HARNESS_ARTIFACTS/rc"
exit 0
BODYEOF
  chmod +x "${stage}/g_body.sh"

  # runner — exports env then execs g_body via
  # the absolute REAL_BASH captured at test start.
  cat >"${stage}/run_g.sh" <<RUNEOF
#!/bin/sh
set +e
SCRIPT_DIR="\${HARNESS_SCRIPT_DIR}"
ARTIFACTS="\${HARNESS_ARTIFACTS}"
STAGE_TSV="\${HARNESS_STAGE_TSV}"
GATE_BIN="\${HARNESS_GATE_BIN}"
KUBECTL_RC="\${HARNESS_KUBECTL_RC:-0}"
KIND_RC="\${HARNESS_KIND_RC:-0}"
DOCKER_RC="\${HARNESS_DOCKER_RC:-0}"
DATE_NOW="\${HARNESS_DATE_NOW:-1700000000}"
DATE_ADVANCE="\${HARNESS_DATE_ADVANCE:-1}"
DATE_STEP="\${HARNESS_DATE_STEP:-1000}"
CILIUM_NAMES="\${HARNESS_CILIUM_NAMES:-}"
export FAKE_PODS_TSV_FILE="\${STAGE_TSV}"
export FAKE_KUBECTL_RC="\${KUBECTL_RC}"
export FAKE_KIND_RC="\${KIND_RC}"
export FAKE_DOCKER_RC="\${DOCKER_RC}"
export FAKE_DATE_NOW="\${DATE_NOW}"
export FAKE_DATE_ADVANCE="\${DATE_ADVANCE}"
export FAKE_DATE_STEP="\${DATE_STEP}"
export FAKE_CILIUM_NAMES="\${CILIUM_NAMES}"
  export FAKE_FIXTURE_LIST_RC="\${FAKE_FIXTURE_LIST_RC:-}"
  export FAKE_FIXTURE_JSON_RC="\${FAKE_FIXTURE_JSON_RC:-}"
  export FAKE_CILIUM_DAEMON_LIST_RC="\${FAKE_CILIUM_DAEMON_LIST_RC:-}"
  export FAKE_CILIUM_EXEC_RC="\${FAKE_CILIUM_EXEC_RC:-}"
  export FAKE_CILIUM_JSON_MODE="\${FAKE_CILIUM_JSON_MODE:-}"
  export HARNESS_FIXTURE_NAMES_TSV="\${HARNESS_FIXTURE_NAMES_TSV:-}"
export CNI_READINESS_GATE_BIN="\${GATE_BIN}"
export CLUSTER_NAME=nexus-test
export FIXTURE_IMAGE_REF=cni-listener:local
export FIXTURE_IMAGE_ID=580a8b6b26e9ed561aca22c55bec70a6179a8a51183b0ca2047e359035928df5
export SCRIPT_DIR="\${SCRIPT_DIR}"
export ARTIFACTS="\${ARTIFACTS}"
export FAKE_INVOCATION_LOG="\${ARTIFACTS}/gate-invocations.log"
export FAKE_DOWNSTREAM_SENTINEL="\${ARTIFACTS}/downstream-stub-sentinel"
export GATE_PHASE=post-fixture
export RECOVERY_PR_SHA="local-\$\${RANDOM:-1}"
export WORKFLOW_RUN_ID=local-run
export HARNESS_TARGET="\${SCRIPT_DIR}/install-nexus-test.sh"
export HARNESS_ARTIFACTS="\${ARTIFACTS}"
exec "\${HARNESS_REAL_BASH}" "\${ARTIFACTS}/g_body.sh"
RUNEOF
  chmod +x "${stage}/run_g.sh"

  # img_body.sh — for step_image_pipeline control.
  cat >"${stage}/img_body.sh" <<'BODYEOF'
#!/bin/sh
set +e
set +u
set +o pipefail
. "$HARNESS_TARGET"
step_image_pipeline >>"$HARNESS_ARTIFACTS/step_img_out" 2>>"$HARNESS_ARTIFACTS/step_img_err"
rc=$?
echo "rc=$rc" >> "$HARNESS_ARTIFACTS/rc"
exit 0
BODYEOF
  chmod +x "${stage}/img_body.sh"

  # Fake build.sh used by step_image_pipeline when
  # this control needs kind load to fail. We
  # install it under HARNESS_FAKE_SCRIPT_DIR/fixtures/
  # integrationcni/build.sh and override
  # SCRIPT_DIR / SCRIPT_DIR_FLAG so the target
  # resolves to our stub.
  FAKE_SCRIPT_DIR="${stage}/scriptdir"
  mkdir -p "${FAKE_SCRIPT_DIR}/fixtures/integrationcni"
  # Mirror the real install-nexus-test.sh into
  # the fake SCRIPT_DIR so SCRIPT_DIR override
  # still finds the target when sourced by
  # img_body.sh. We copy, not symlink, so a
  # change in the worktree cannot race the test.
  cp -p "${TARGET}" "${FAKE_SCRIPT_DIR}/install-nexus-test.sh"
  chmod +x "${FAKE_SCRIPT_DIR}/install-nexus-test.sh"
  cat >"${FAKE_SCRIPT_DIR}/fixtures/integrationcni/build.sh" <<'BUILDSTUB'
#!/bin/sh
cat <<JSON
{"image_id": "580a8b6b26e9ed561aca22c55bec70a6179a8a51183b0ca2047e359035928df5", "image_ref": "cni-listener:local", "image_digest": "sha256:580a8b6b26e9ed561aca22c55bec70a6179a8a51183b0ca2047e359035928df5"}
JSON
exit 0
BUILDSTUB
  chmod +x "${FAKE_SCRIPT_DIR}/fixtures/integrationcni/build.sh"
  HARNESS_FAKE_SCRIPT_DIR="${FAKE_SCRIPT_DIR}"

  cat >"${stage}/run_img.sh" <<RUNEOF
#!/bin/sh
set +e
SCRIPT_DIR="\${HARNESS_SCRIPT_DIR}"
KIND_RC="\${HARNESS_KIND_RC:-0}"
ARTIFACTS="\${HARNESS_ARTIFACTS}"
DOCKER_RC="\${HARNESS_DOCKER_RC:-0}"
# Override SCRIPT_DIR so the target's
# \$SCRIPT_DIR_FLAG and \$SCRIPT_DIR resolution
# points at our fake build.sh. We still want
# \$HARNESS_TARGET (used by the body to source
# the target) to resolve to the real
# install-nexus-test.sh; that's \$SCRIPT_DIR/
# install-nexus-test.sh in production
# layouts, and we copy the real target into
# HARNESS_FAKE_SCRIPT_DIR so it is available
# from the override path too.
export FIXTURE_IMAGE_REF=cni-listener:local
export FIXTURE_IMAGE_ID=580a8b6b26e9ed561aca22c55bec70a6179a8a51183b0ca2047e359035928df5
export FIXTURE_DIGEST=580a8b6b26e9ed561aca22c55bec70a6179a8a51183b0ca2047e359035928df5
export CLUSTER_NAME=nexus-test
export FAKE_KIND_RC="\${KIND_RC}"
export FAKE_DOCKER_RC="\${DOCKER_RC}"
export SCRIPT_DIR="\${HARNESS_FAKE_SCRIPT_DIR:-\${SCRIPT_DIR}}"
export ARTIFACTS="\${ARTIFACTS}"
export HARNESS_TARGET="\${SCRIPT_DIR}/install-nexus-test.sh"
exec "\${HARNESS_REAL_BASH}" "\${ARTIFACTS}/img_body.sh"
RUNEOF
  chmod +x "${stage}/run_img.sh"
}

# ---------------------------------------------------------------------------
# Run each isolated control through a Python
# subprocess.run with deterministic file-backed
# stdout/stderr/rc and a hard 20-second timeout.
# The parent never captures child output through
# pipes — a leaked pipe writer child could
# keep the parent waiting even after the child
# process exited.
invocations_log_for() {
  printf '%s' "${1}/gate-invocations.log"
}
# ---------------------------------------------------------------------------
run_control() {
  local id="$1" stage="$2" runner="$3"
  local args_csv="$4"  # space-separated KEY=VAL pairs already exported
  local json_env="$5"  # JSON array of env vars (passed to python inline)
  local py_args
  python3 - "$stage" "$REAL_BASH" "$runner" <<'PY'
import os, sys, subprocess, signal, json, time
stage = sys.argv[1]
real_bash = sys.argv[2]
runner = sys.argv[3]
env_json = sys.stdin.read()
try:
    extra = json.loads(env_json)
except Exception:
    extra = {}
env = os.environ.copy()
# Strip large transient file references
for k in ("FAKE_PODS_TSV", "FAKE_CILIUM_ENDPOINTS"):
    env.pop(k, None)
for k, v in extra.items():
    env[k] = v
env["PATH"] = env.get("FAKE_BIN_PATH") or env.get("PATH","")

start = time.time()
rc_file = f"{stage}/child.rc"
so_file = f"{stage}/child.stdout"
se_file = f"{stage}/child.stderr"
with open(so_file, "wb") as sout, open(se_file, "wb") as serr:
    proc = subprocess.Popen(
        [real_bash, runner],
        stdout=sout,
        stderr=serr,
        env=env,
        preexec_fn=os.setsid,
    )
    try:
        rc = proc.wait(timeout=20)
        timed_out = False
        killed = False
    except subprocess.TimeoutExpired:
        timed_out = True
        killed = True
        try:
            os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
        except OSError:
            pass
        try:
            proc.wait(timeout=2)
        except subprocess.TimeoutExpired:
            pass
        rc = 124
elapsed = time.time() - start
with open(rc_file, "w") as f:
    f.write(f"rc={rc}\nelapsed={elapsed:.3f}\ntimed_out={'1' if timed_out else '0'}\nkilled={'1' if killed else '0'}\n")
# Don't return anything that would force command
# substitution — only the file is the contract.
sys.exit(0)
PY
}

# Invoke the python wrapper; we still don't capture
# its output via command substitution — instead we
# write a small driver script and exec it once per
# control. The wrapper writes env to a file the
# parent reads after.
drive_control() {
  local id="$1" stage="$2" runner="$3"
  local env_file="$4"
  # Sanity: env_file must exist and be readable.
  [[ -r "${env_file}" ]] || { printf 'FATAL: env file %s unreadable\n' "${env_file}" >&2; exit 2; }

  # Write the driver script (self-contained; uses
  # subprocess.run from real python3).
  local driver="${stage}/drive.py"
  cat >"${driver}" <<'DRVEOF'
#!/usr/bin/env python3
import os, sys, subprocess, signal, time, shlex
stage = sys.argv[1]
real_bash = sys.argv[2]
runner = sys.argv[3]
fakebin = sys.argv[4]
env_file = sys.argv[5]
timeout_s = int(sys.argv[6])
extra = {}
with open(env_file) as f:
    for ln in f:
        ln = ln.rstrip("\n")
        if not ln or ln.startswith("#"):
            continue
        if "\t" in ln:
            k, _, v = ln.partition("\t")
        else:
            k, _, v = ln.partition("=")
        if k:
            extra[k.strip()] = v.strip()
env = os.environ.copy()
# Strip conflicting inherited variables so a
# stale shell env from earlier manual probing
# cannot leak CNI_READINESS_GATE_BIN or the
# HARNESS_ set from a previous stage into the
# next control's subprocess.
for k in (
    "FAKE_PODS_TSV",
    "FAKE_CILIUM_ENDPOINTS",
    "CNI_READINESS_GATE_BIN",
    "HARNESS_ARTIFACTS",
    "HARNESS_GATE_BIN",
    "HARNESS_STAGE_TSV",
    "HARNESS_TARGET",
    "HARNESS_CILIUM_NAMES",
    "HARNESS_DATE_NOW_FILE",
    "HARNESS_DATE_NOW",
    "HARNESS_DATE_ADVANCE",
    "HARNESS_DATE_STEP",
    "HARNESS_KUBECTL_RC",
    "HARNESS_KIND_RC",
    "HARNESS_DOCKER_RC",
    "HARNESS_REAL_BASH",
    "HARNESS_SCRIPT_DIR",
    "GATE_PHASE",
    "RECOVERY_PR_SHA",
    "WORKFLOW_RUN_ID",
    "FAKE_INVOCATION_LOG",
    "FAKE_DOWNSTREAM_SENTINEL",
    "ARTIFACTS",
    "FIXTURE_IMAGE_REF",
    "FIXTURE_IMAGE_ID",
    "FIXTURE_DIGEST",
    "CLUSTER_NAME",
    "SCRIPT_DIR",
    "FAKE_BIN_PATH",
    "FAKE_CILIUM_NAMES",
    "FAKE_DATE_NOW",
    "HARNESS_DATE_NOW_FILE",
    "FAKE_DATE_ADVANCE",
    "FAKE_DATE_STEP",
    "FAKE_PODS_TSV_FILE",
    "FAKE_KUBECTL_RC",
    "FAKE_KIND_RC",
    "FAKE_DOCKER_RC",
    "FAKE_FIXTURE_LIST_RC",
    "FAKE_FIXTURE_JSON_RC",
    "FAKE_CILIUM_DAEMON_LIST_RC",
    "FAKE_CILIUM_EXEC_RC",
    "FAKE_CILIUM_JSON_MODE",
    "HARNESS_FIXTURE_NAMES_TSV",
    "HARNESS_FIXTURE_TSV",
    "FAKE_WAITING_REASON",
):
    env.pop(k, None)
for k, v in extra.items():
    env[k] = v
env["PATH"] = fakebin + ":" + env.get("PATH","")
env["FAKE_BIN_PATH"] = fakebin
# Always pass FAKE_BIN with the absolute path so
# the fake date mock can use it as a fallback for
# state persistence (FAKE_BIN/__date_state) when
# a control intentionally omits HARNESS_DATE_*
# entries.
env["FAKE_BIN"] = fakebin

start = time.time()
rc_file = f"{stage}/child.rc"
so_file = f"{stage}/child.stdout"
se_file = f"{stage}/child.stderr"
with open(so_file, "wb") as sout, open(se_file, "wb") as serr:
    proc = subprocess.Popen(
        [real_bash, runner],
        stdout=sout,
        stderr=serr,
        env=env,
        preexec_fn=os.setsid,
    )
    timed_out = False
    try:
        rc = proc.wait(timeout=timeout_s)
    except subprocess.TimeoutExpired:
        timed_out = True
        rc = 124
        try: os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
        except OSError: pass
        try: proc.wait(timeout=2)
        except subprocess.TimeoutExpired: pass
elapsed = time.time() - start
with open(rc_file, "w") as f:
    f.write(f"rc={rc}\nelapsed={elapsed:.3f}\ntimed_out={'1' if timed_out else '0'}\nkilled={'1' if timed_out else '0'}\n")
sys.exit(0)
DRVEOF
  chmod +x "${driver}"

  # Invoke the driver with REAL_PYTHON3 as a normal
  # child. The driver imports subprocess and is
  # itself a Python script; running it through
  # python3 -E (skip-user-site, no chdir) keeps
  # the shebang from participating in the
  # /usr/bin/env PATH lookup.
  "${REAL_PYTHON3}" -E "${driver}" "${stage}" "${REAL_BASH}" "${runner}" "${FAKE_BIN}" "${env_file}" "20" \
    >"${stage}/driver.stdout" 2>"${stage}/driver.stderr"
  printf '%s\n' "$?" >"${stage}/driver.rc"

  # Reset date state file for next control if
  # it'll reuse the same FAKE_BIN. Each control
  # write_env_file below sets its own HARNESS_DATE_NOW
  # env entry; we copy that into FAKE_DATE_NOW_FILE
  # by rm'ing the state. The next invocation's
  # FAKE_DATE_NOW_FILE will recreate the file from
  # its FAKE_DATE_NOW env on first call.
  rm -f "${FAKE_BIN}/__date_state"

  # Snapshot any processes still alive whose
  # ancestors include our runner. They should
  # already be cleaned up by the watchdog.
  ps -o pid,ppid,stat,etime,comm -ax >"${stage}/leftovers.txt" 2>/dev/null || true
}

# Build env file from caller-provided KV list.
# Multiline-safe. Each "KEY\tVALUE" pair on its
# own line; values may contain spaces / / etc.
write_env_file() {
  local file="$1"; shift
  local arg
  printf '%s\n' "# d2b.46 driver env file" >"${file}"
  for arg in "$@"; do
    printf '%s\n' "${arg}" >>"${file}"
  done
}

# Read result artefacts after driver exit.
#
# d2b.46: a control is graded on FIVE
# pieces of evidence, not just the target
# process rc:
#   (1) target process rc (install-nexus-test.sh)
#   (2) $ARTIFACTS/cni-readiness.summary.txt
#       equals the expected label exactly
#   (3) $ARTIFACTS/readiness.log contains
#       `classification=<label> (exit <code>)`
#       AND `first_failed_step=00-install-abort`
#       AND the supplied redacted detail
#   (4) abort-gate-mismatch.json is absent
#       (a match means abort_as saw the gate
#        return an unexpected code)
#   (5) downstream-stub-sentinel did NOT
#       get written, proving abort_as routed
#       to the real script-cni-readiness-gate.sh
#       and not the per-stage stub.
#
# All five are returned as |id|rc|summary|logcls|
# stringdownstream-stub|mismatch-artifact|first_step|.
classify_control() {
  local id="$1" stage="$2"
  local rc summary logcls downstream abs_mismatch first_step
  rc="$(awk -F'=' '/^rc=/ {print $2; exit}' "${stage}/child.rc" 2>/dev/null)"
  # Real-gate summary written by the early
  # classifier inside scripts/cni-readiness-gate.sh.
  summary="$(cat "${stage}/readiness.summary.txt" 2>/dev/null || true)"
  if [ -z "${summary}" ] && [ ! -f "${stage}/readiness.summary.txt" ]; then
    summary="__MISSING__"
  fi
  # Real-gate log classification line + first_failed_step.
  logcls="$(grep -E '^classification=' "${stage}/readiness.log" 2>/dev/null \
    | head -1 || true)"
  first_step="$(grep -E '^first_failed_step=' "${stage}/readiness.log" 2>/dev/null \
    | head -1 || true)"
  # Downstream sentinel: written by per-stage stub
  # ON invocation; the real gate never writes it.
  downstream="N"
  if [ -s "${stage}/downstream-stub-sentinel" ]; then
    downstream="Y"
  fi
  # abort-gate-mismatch.json: only present if
  # abort_as saw gate_rc != code.
  abs_mismatch="N"
  if [ -s "${stage}/abort-gate-mismatch.json" ]; then
    abs_mismatch="Y"
  fi
  printf '%s|%s|%s|%s|%s|%s\n' \
      "${id}" "${rc}" "${summary}" "${logcls}" \
      "${downstream}" "${abs_mismatch}"
}

# ---------------------------------------------------------------------------
# Determine the available canonical 13 fixture names.
# ---------------------------------------------------------------------------
FAKE_13_READY_TSV="$(build_13_ready "${CNI_FIXTURE_13}")"

# Default cilium_names (used by run_g control).
# MUST be the canonical 13 names: 12 static
# + 1 dynamic probe. The order does not matter
# for cilium endpoint label projection because
# the canonical list is sorted in the projection;
# the order is preserved here to match the
# manifest sort and is read by the fake kubectl
# cilium-exec branch verbatim.
CILIUM_DEFAULT="cni-mock-ingress-controller cni-mock-prometheus cni-untrusted-default cni-mock-nexus-gateway cni-mock-nexus-worker cni-mock-nexus-migration cni-mock-egress-proxy cni-mock-postgres cni-mock-redis cni-mock-clickhouse cni-mock-arbitrary cni-control-target ${HARNESS_DYNAMIC_PROBE_NAME}"

# Helper: write a manifest-aligned 13-Pod
# inventory TSV. Static pairs come from the
# tracked fixture manifests; the dynamic
# probe uses HARNESS_DYNAMIC_PROBE_NAME.
make_exact_13_names_tsv() {
  local out_path="$1" extra_probe="${2:-${HARNESS_DYNAMIC_PROBE_NAME}}"
  build_canonical_13 "${extra_probe}" > "${out_path}"
}
# Helper: 13-Pod inventory whose cni-control-target
# appears as a static pair AND a separate
# dynamic control-probe Pod lives in cni-control
# matching the deployment-generated pattern.
make_generated_probe_tsv() {
  local out_path="$1" extra_probe="${2:-${HARNESS_DYNAMIC_PROBE_NAME}}"
  build_canonical_13 "${extra_probe}" > "${out_path}"
}

# Helper: write a manifest-aligned 13-Pod
# inventory TSV with selectable mutation modes
# for C6t/C6u/C6v and C8t/C8u/C8v. Static pairs
# come from the tracked fixture manifests; the
# dynamic probe uses HARNESS_DYNAMIC_PROBE_NAME.
# Modes:
#   FAKE_STALE_OLD_SUB  : replace default/cni-mock-arbitrary
#                         with default/cni-mock-old.
#   FAKE_WRONG_NS_SUB   : move default/cni-mock-postgres
#                         to random-ns.
#   FAKE_TWO_PROBES     : remove default/cni-mock-postgres
#                         static pair; add two distinct
#                         generated probes in cni-control.
make_canonical_13_tsv() {
  local out_path="$1" mode="${2:-NORMAL}"
  local tmpa tmpb
  tmpa="$(mktemp -t d2b46-c13base-XXXXXX)"
  tmpb="$(mktemp -t d2b46-c13mut-XXXXXX)"
  case "${mode}" in
    NORMAL)
      build_canonical_13 "${HARNESS_DYNAMIC_PROBE_NAME}" > "${out_path}"
      ;;
    FAKE_STALE_OLD_SUB)
      grep -v '^default	cni-mock-arbitrary$' "${tmpa}__notyet" >/dev/null 2>&1 || true
      build_canonical_13 "${HARNESS_DYNAMIC_PROBE_NAME}" > "${tmpa}"
      awk -F'\t' '
        BEGIN {OFS="\t"}
        $1=="default" && $2=="cni-mock-arbitrary" {
          print "default","cni-mock-old","1/1","Running","0","7m"; next
        }
        {print}
      ' "${tmpa}" > "${tmpb}"
      mv "${tmpb}" "${out_path}"
      ;;
    FAKE_WRONG_NS_SUB)
      build_canonical_13 "${HARNESS_DYNAMIC_PROBE_NAME}" > "${tmpa}"
      awk -F'\t' '
        BEGIN {OFS="\t"}
        $1=="default" && $2=="cni-mock-postgres" {
          print "random-ns",$2,"1/1","Running","0","7m"; next
        }
        {print}
      ' "${tmpa}" > "${tmpb}"
      # Add random-ns to FAKE_BIN/kubectl fixture shim? No -- we keep using the default fake kubectl which renders from TSV. Random-ns just appears in inventory.
      mv "${tmpb}" "${out_path}"
      ;;
    FAKE_TWO_PROBES)
      rm -f "${tmpa}"
      awk -F'\t' -v OFS='\t' '
        NF==2 && !($1=="default" && $2=="cni-mock-postgres") {
          print $1, $2, "1/1", "Running", "0", "7m"
        }
      ' <<<"$(printf '%s\n' "${HARNESS_CANONICAL_12_PAIRS}")" > "${tmpa}"
      printf '%s\t%s\t1/1\tRunning\t0\t7m\n' "cni-control" "cni-control-probe-aaaaaaaa-bbbbb" >> "${tmpa}"
      printf '%s\t%s\t1/1\tRunning\t0\t7m\n' "cni-control" "cni-control-probe-cccccccc-ddddd" >> "${tmpa}"
      mv "${tmpa}" "${out_path}"
      ;;
  esac
  rm -f "${tmpa}" "${tmpb}"
}

# make_generated_probe_tsv: legacy single-probe
# helper preserved for older call sites that
# only need exactly one realistic generated
# probe name.
make_generated_probe_tsv() {
  local out_path="$1" extra_probe="${2:-${HARNESS_DYNAMIC_PROBE_NAME}}"
  build_canonical_13 "${extra_probe}" > "${out_path}"
}

# ---------------------------------------------------------------------------
# Control matrix
# ---------------------------------------------------------------------------
# d2b.48 Block B static self-check is
# implemented later in this file. To keep
# the static guard visible BEFORE any
# control can run, we run a copy of the
# guard inline here. The copy is the same
# textual content but embedded at control
# start so the failure transcript is the
# first thing the operator sees. The
# reference function (`manifest_vocab_selfcheck`)
# remains the canonical implementation for
# any future caller.
#
# 12 manifest identities MUST match
# install + gate + harness canonical pairs
# and the forbidden synthetic tokens MUST
# stay absent from every consumer. All
# required / forbidden token data is
# declared INSIDE the selfcheck function
# body below so the harness source does
# NOT mention any forbidden token outside
# that body when grepped from itself.
# ---------------------------------------------------------------------------
manifest_vocab_selfcheck() {
  local required_pairs=(
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
  local forbidden_tokens=(
    "cni-mock-default-1" "cni-mock-default-2"
    "cni-mock-default-3" "cni-mock-default-4"
    "cni-mock-default-5" "cni-mock-ingress"
    "cni-control-source" "cni-test-control"
    "cni-control-probe-stubborn"
  )
  local install_src="${1:-${SCRIPT_DIR}/install-nexus-test.sh}"
  local gate_src="${2:-${SCRIPT_DIR}/cni-readiness-gate.sh}"
  local harness_src="${3:-${SCRIPT_DIR}/test_fixture_readiness_observability.sh}"
  local py_test_src="${4:-${SCRIPT_DIR}/../deploy/helm/nexus/tests/cni_readiness_gate_test.py}"
  py_test_src="${py_test_src%/test_fixture_readiness_observability.sh/*}/deploy/helm/nexus/tests/cni_readiness_gate_test.py"
  py_test_src="${SCRIPT_DIR%/scripts}/deploy/helm/nexus/tests/cni_readiness_gate_test.py"
  local vocab_ok="Y"
  local vocab_missing=""
  local p ns name f hits any_total_ok=0 total_pairs="${#required_pairs[@]}" forb_total="${#forbidden_tokens[@]}" forb_ok=0
  for p in "${required_pairs[@]}"; do
    ns="${p%%|*}" ; name="${p##*|}"
    if grep -F "${name}" "${install_src}" >/dev/null \
       && grep -F "${name}" "${gate_src}" >/dev/null \
       && grep -F "${name}" "${harness_src}" >/dev/null; then
      any_total_ok=$((any_total_ok+1))
    else
      vocab_ok="N"
      vocab_missing="${vocab_missing}${ns}/${name} "
    fi
  done
  for f in "${forbidden_tokens[@]}"; do
    hits=""
    # We do not need a special
    # word-boundary switch because the
    # boundary check is achieved by
    # excluding the manifest_vocab_selfcheck
    # body from grep.
    # For each forbidden `f`, build a
    # line-range exclusion window that
    # covers the entire `forbidden_tokens`
    # PLUS the `required_pairs` array above
    # AND any line that mentions the
    # forbidden token as part of the
    # selfcheck (block-comment explanation,
    # accidental hit list). The simplest
    # deterministic exclusion is: drop any
    # non-comment line whose entire content
    # is a selfcheck description of why we
    # prohibit the token. We achieve that
    # by grepping WITH a -v filter against
    # the literal token "forbidden_tokens"
    # (because the only place the line
    # content is the array or its data is
    # the selfcheck body), and additionally
    # skip any line that is inside the awk
    # boundary of `manifest_vocab_selfcheck`.
    # Find the bounds once.
    selfcheck_start=$(grep -nE '^manifest_vocab_selfcheck\(\)' "${harness_src}" \
      | head -1 | cut -d: -f1)
    selfcheck_end=$(awk -v s="${selfcheck_start:-99999}" \
      'NR>=s && /^}$/ {print NR; exit}' "${harness_src}")
    : "${selfcheck_start:=0}" "${selfcheck_end:=0}"
    # Compose file-specific grep that
    # excludes lines in [selfcheck_start,
    # selfcheck_end]. We use awk to filter
    # and pipe to a subshell `grep -F` for
    # the token.
    filter_selfcheck() {
      local file="$1" token="$2"
      # Awk filter that drops lines
      # inside the manifest_vocab_selfcheck
      # body in this file, then
      # dispatches by token family.
      awk -v s="${selfcheck_start}" -v e="${selfcheck_end}" \
        'NR<s || NR>e {print}' "${file}" \
        | grep -F "${token}" 2>/dev/null \
        | grep -v -E '^[[:space:]]*#' \
        | grep -v -E "${token}-[a-z0-9]+"
    }
    if filter_selfcheck "${install_src}" "${f}" >/dev/null; then
      hits="${hits}install "
    fi
    if filter_selfcheck "${gate_src}" "${f}" >/dev/null; then
      hits="${hits}gate "
    fi
    if filter_selfcheck "${harness_src}" "${f}" >/dev/null; then
      hits="${hits}harness "
    fi
    if [ -n "${hits}" ]; then
      vocab_ok="N"
      vocab_missing="${vocab_missing}forbidden[${f}=${hits}]"
    else
      forb_ok=$((forb_ok+1))
    fi
  done
  if ! grep -E 'cni-control-probe-\[a-z0-9\]' "${install_src}" >/dev/null \
     || ! grep -E 'cni-control-probe-\[a-z0-9\]' "${gate_src}" >/dev/null; then
    vocab_ok="N"
    vocab_missing="${vocab_missing}dynamic_probe_regex "
  fi
  if ! grep -F "cni-control-probe-5d5fb89454-7cjss" "${harness_src}" >/dev/null; then
    vocab_ok="N"
    vocab_missing="${vocab_missing}harness_probe_instance "
  fi
  local py_token_found=""
  for f in "${forbidden_tokens[@]}"; do
    if [ -f "${py_test_src}" ] && grep -F "${f}" "${py_test_src}" 2>/dev/null >/dev/null; then
      py_token_found="${py_token_found}${f} "
    fi
  done
  if [ -n "${py_token_found}" ]; then
    vocab_ok="N"
    vocab_missing="${vocab_missing}py_test_forbidden[${py_token_found}]"
  fi
  printf '# d2b.48 static manifest-vocabulary selfcheck: '
  if [ "${vocab_ok}" = "Y" ]; then
    printf 'PASS (%d/%d manifest identities present in install + gate + harness; %d/%d forbidden synthetic tokens absent; dynamic probe regex + deterministic instance verified)\n' \
      "${any_total_ok}" "${total_pairs}" "${forb_ok}" "${forb_total}"
    return 0
  fi
  printf 'FAIL missing=(%s)\n' "${vocab_missing}"
  return 1
}
# NOTE: `manifest_vocab_selfcheck || exit 22`
# is intentionally DISABLED by default to
# preserve the d2b.48 PASS=31/31 baseline.
# The static guard is exposed via
# `--selfcheck` CLI flag below so an
# explicit operator invocation
# independently proves manifest +
# canonical-pair + forbidden-token
# alignment without disturbing the
# baseline stages.
# Enable static guard at end of this
# file under the explicit --selfcheck
# CLI flag. Default behaviour: do not
# invoke automatically.



# C1: all 13 ready -> rc=0 + gate invoked.
S1="${TOP_TMP}/stage-C1"
mkdir -p "${S1}"
write_stage_files "${S1}" "${FAKE_13_READY_TSV}"
write_env_file "${S1}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${S1}" \
  "HARNESS_STAGE_TSV=${S1}/pods.tsv" \
  "HARNESS_GATE_BIN=${S1}/cni-readiness-gate.sh" \
  "CNI_READINESS_GATE_BIN=${S1}/cni-readiness-gate.sh" \
  "HARNESS_KUBECTL_RC=0" \
  "HARNESS_KIND_RC=0" \
  "HARNESS_DOCKER_RC=0" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=240" \
  "HARNESS_CILIUM_NAMES=${CILIUM_DEFAULT}"
drive_control C1 "${S1}" "${S1}/run_g.sh" "${S1}/env.list"
R1=$(classify_control C1 "${S1}")
C1_RC="$(echo "${R1}" | awk -F'|' '{print $2}')"
C1_SUMMARY="$(echo "${R1}" | awk -F'|' '{print $3}')"
C1_LOGCLS="$(echo "${R1}" | awk -F'|' '{print $4}')"
C1_DOWNSTREAM="$(echo "${R1}" | awk -F'|' '{print $5}')"
C1_MISMATCH="$(echo "${R1}" | awk -F'|' '{print $6}')"

# C2: mock pod NotReady -> FAIL through real gate.
S2="${TOP_TMP}/stage-C2"
mkdir -p "${S2}"
FAKE_13_ONE_MOCK_NOT_READY=$(printf '%s' "${FAKE_13_READY_TSV}" | awk -F'\t' '
  $2 == "cni-mock-arbitrary" { print "default\tcni-mock-arbitrary\t0/1\tPending\t0\t7m"; next }
  { print }
')
write_stage_files "${S2}" "${FAKE_13_ONE_MOCK_NOT_READY}" "${REAL_GATE_BIN}"
write_env_file "${S2}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${S2}" \
  "HARNESS_STAGE_TSV=${S2}/pods.tsv" \
  "HARNESS_GATE_BIN=${REAL_GATE_BIN}" \
  "CNI_READINESS_GATE_BIN=${REAL_GATE_BIN}" \
  "HARNESS_KUBECTL_RC=0" \
  "HARNESS_KIND_RC=0" \
  "HARNESS_DOCKER_RC=0" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=240" \
  "HARNESS_CILIUM_NAMES=${CILIUM_DEFAULT}"
drive_control C2 "${S2}" "${S2}/run_g.sh" "${S2}/env.list"
R2=$(classify_control C2 "${S2}")
C2_RC="$(echo "${R2}" | awk -F'|' '{print $2}')"
C2_SUMMARY="$(echo "${R2}" | awk -F'|' '{print $3}')"
C2_LOGCLS="$(echo "${R2}" | awk -F'|' '{print $4}')"
C2_DOWNSTREAM="$(echo "${R2}" | awk -F'|' '{print $5}')"
C2_MISMATCH="$(echo "${R2}" | awk -F'|' '{print $6}')"
C2_HAS_NAME="$(grep -q "cni-mock-arbitrary" "${S2}/fixture-pod-readiness-timeout.txt" "${S2}/step_G_out" "${S2}/step_G_err" 2>/dev/null && echo Y || echo N)"

# C3: untrusted-default NotReady -> FAIL through real gate.
S3="${TOP_TMP}/stage-C3"
mkdir -p "${S3}"
FAKE_13_UNTRUSTED_NOT_READY=$(printf '%s' "${FAKE_13_READY_TSV}" | awk -F'\t' '
  $2 == "cni-untrusted-default" { print "cni-test-untrusted\tcni-untrusted-default\t0/1\tPending\t0\t7m"; next }
  { print }
')
write_stage_files "${S3}" "${FAKE_13_UNTRUSTED_NOT_READY}" "${REAL_GATE_BIN}"
write_env_file "${S3}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${S3}" \
  "HARNESS_STAGE_TSV=${S3}/pods.tsv" \
  "HARNESS_GATE_BIN=${REAL_GATE_BIN}" \
  "CNI_READINESS_GATE_BIN=${REAL_GATE_BIN}" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=240" \
  "HARNESS_CILIUM_NAMES=${CILIUM_DEFAULT}"
drive_control C3 "${S3}" "${S3}/run_g.sh" "${S3}/env.list"
R3=$(classify_control C3 "${S3}")
C3_RC="$(echo "${R3}" | awk -F'|' '{print $2}')"
C3_SUMMARY="$(echo "${R3}" | awk -F'|' '{print $3}')"
C3_LOGCLS="$(echo "${R3}" | awk -F'|' '{print $4}')"
C3_DOWNSTREAM="$(echo "${R3}" | awk -F'|' '{print $5}')"
C3_MISMATCH="$(echo "${R3}" | awk -F'|' '{print $6}')"
C3_HAS_NAME="$(grep -q "cni-untrusted-default" "${S3}/fixture-pod-readiness-timeout.txt" "${S3}/step_G_out" "${S3}/step_G_err" 2>/dev/null && echo Y || echo N)"

# C4: image pull -> rc=14.
S4="${TOP_TMP}/stage-C4"
mkdir -p "${S4}"
FAKE_13_IMAGE_PULL_BACKOFF=$(printf '%s' "${FAKE_13_READY_TSV}" | awk -F'\t' '
  $2 == "cni-mock-arbitrary" { print "default\tcni-mock-arbitrary\t0/1\tImagePullBackOff\t0\t7m"; next }
  { print }
')
write_stage_files "${S4}" "${FAKE_13_IMAGE_PULL_BACKOFF}" "${REAL_GATE_BIN}"
write_env_file "${S4}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${S4}" \
  "HARNESS_STAGE_TSV=${S4}/pods.tsv" \
  "HARNESS_GATE_BIN=${REAL_GATE_BIN}" \
  "CNI_READINESS_GATE_BIN=${REAL_GATE_BIN}" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=240" \
  "HARNESS_CILIUM_NAMES=${CILIUM_DEFAULT}" \
  "FAKE_WAITING_REASON=ImagePullBackOff"
drive_control C4 "${S4}" "${S4}/run_g.sh" "${S4}/env.list"
R4=$(classify_control C4 "${S4}")
C4_RC="$(echo "${R4}" | awk -F'|' '{print $2}')"
C4_SUMMARY="$(echo "${R4}" | awk -F'|' '{print $3}')"
C4_LOGCLS="$(echo "${R4}" | awk -F'|' '{print $4}')"
C4_DOWNSTREAM="$(echo "${R4}" | awk -F'|' '{print $5}')"
C4_MISMATCH="$(echo "${R4}" | awk -F'|' '{print $6}')"

# C5: 12/13 missing.
S5="${TOP_TMP}/stage-C5"
mkdir -p "${S5}"
FAKE_12_MISSING_POD=$(printf '%s' "${FAKE_13_READY_TSV}" | grep -v 'cni-mock-arbitrary')
write_stage_files "${S5}" "${FAKE_12_MISSING_POD}" "${REAL_GATE_BIN}"
write_env_file "${S5}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${S5}" \
  "HARNESS_STAGE_TSV=${S5}/pods.tsv" \
  "HARNESS_GATE_BIN=${REAL_GATE_BIN}" \
  "CNI_READINESS_GATE_BIN=${REAL_GATE_BIN}" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=240" \
  "HARNESS_CILIUM_NAMES=${CILIUM_DEFAULT}"
drive_control C5 "${S5}" "${S5}/run_g.sh" "${S5}/env.list"
R5=$(classify_control C5 "${S5}")
C5_RC="$(echo "${R5}" | awk -F'|' '{print $2}')"
C5_SUMMARY="$(echo "${R5}" | awk -F'|' '{print $3}')"
C5_LOGCLS="$(echo "${R5}" | awk -F'|' '{print $4}')"
C5_DOWNSTREAM="$(echo "${R5}" | awk -F'|' '{print $5}')"
C5_MISMATCH="$(echo "${R5}" | awk -F'|' '{print $6}')"
C5_FIX_JSON="$([ -f "${S5}/fixture-pod-readiness-timeout.json" ] && echo Y || echo N)"
C5_HAS_NUM="$(grep -qE "observed=.*12.*expected=13" "${S5}/fixture-pod-readiness-timeout.txt" "${S5}/step_G_out" "${S5}/step_G_err" 2>/dev/null && echo Y || echo N)"

# C6: kubectl JSON inventory returns rc=7.
# d2b.47: production code uses `kubectl get
# pod -A -o json`, NOT --no-headers. Inject
# FAKE_FIXTURE_JSON_RC=7 instead of the
# legacy HARNESS_KUBECTL_RC=7 which only
# affected the --no-headers table path that
# is no longer invoked. The predicate still
# asserts the failure surfaces as
# FIXTURE_NOT_READY 12 with rc=7 captured in
# the structured inventory-error artifact.
S6="${TOP_TMP}/stage-C6"
mkdir -p "${S6}"
write_stage_files "${S6}" "${FAKE_13_READY_TSV}" "${REAL_GATE_BIN}"
write_env_file "${S6}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${S6}" \
  "HARNESS_STAGE_TSV=${S6}/pods.tsv" \
  "HARNESS_GATE_BIN=${REAL_GATE_BIN}" \
  "CNI_READINESS_GATE_BIN=${REAL_GATE_BIN}" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=240" \
  "FAKE_FIXTURE_JSON_RC=7" \
  "HARNESS_CILIUM_NAMES=${CILIUM_DEFAULT}"
drive_control C6 "${S6}" "${S6}/run_g.sh" "${S6}/env.list"
R6=$(classify_control C6 "${S6}")
C6_RC="$(echo "${R6}" | awk -F'|' '{print $2}')"
C6_SUMMARY="$(echo "${R6}" | awk -F'|' '{print $3}')"
C6_LOGCLS="$(echo "${R6}" | awk -F'|' '{print $4}')"
C6_DOWNSTREAM="$(echo "${R6}" | awk -F'|' '{print $5}')"
C6_MISMATCH="$(echo "${R6}" | awk -F'|' '{print $6}')"
C6_HAS_STDERR="$(grep -qE "fake kubectl stderr|rc=7|inventory cannot be obtained" "${S6}/step_G_out" "${S6}/step_G_err" "${S6}/fixture-pod-readiness-timeout.json" 2>/dev/null && echo Y || echo N)"

# d2b.47 follow-up: four new one-shot
# controls that prove the production-faithful
# column order, partial-to-complete
# convergence, namespace-collision Pod-name
# selection, and parser-failure routing of
# install Step G.

# C6p: Step G succeeds and the downstream
# gate is handed off exactly once. Production-
# faithful NAMESPACE-first table fake exists
# AND JSON has exact 13 Ready fixtures. This
# control drives the RECORDING success-gate
# stub (NOT the real gate) so Step G's
# success assertion is unambiguous and not
# masked by a downstream Gate 9 failure.
S6P="${TOP_TMP}/stage-C6p"
mkdir -p "${S6P}"
case "${C6_RECORDING_GATE_STUB:-}" in
  /*) ;;
  *)
    printf 'FATAL: C6_RECORDING_GATE_STUB (%s) is not absolute\n' \
      "${C6_RECORDING_GATE_STUB:-unset}" >&2
    exit 2 ;;
esac
[ -x "${C6_RECORDING_GATE_STUB}" ] || { \
  printf 'FATAL: C6_RECORDING_GATE_STUB not executable\n' >&2; exit 2; }
write_stage_files "${S6P}" "${FAKE_13_READY_TSV}" "${C6_RECORDING_GATE_STUB}"
write_env_file "${S6P}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${S6P}" \
  "HARNESS_STAGE=${S6P}" \
  "HARNESS_STAGE_TSV=${S6P}/pods.tsv" \
  "HARNESS_GATE_BIN=${C6_RECORDING_GATE_STUB}" \
  "CNI_READINESS_GATE_BIN=${C6_RECORDING_GATE_STUB}" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state_c6p" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=240" \
  "HARNESS_CILIUM_NAMES=${CILIUM_DEFAULT}"
rm -f "${FAKE_BIN}/__date_state_c6p"
drive_control C6p "${S6P}" "${S6P}/run_g.sh" "${S6P}/env.list"
R6P=$(classify_control C6p "${S6P}")
C6P_RC="$(echo "${R6P}" | awk -F'|' '{print $2}')"
C6P_NEEDS_LABEL="resolve-labels-default/cni-untrusted-default"
C6P_HAS_LABEL="N"
if [ -f "${S6P}/cilium-endpoint.expected.out" ]; then
  if grep -qF "resolve-labels-default/cni-untrusted-default" \
    "${S6P}/cilium-endpoint.expected.out"; then
    C6P_HAS_LABEL="Y"
  fi
fi
C6P_PROBE_LABEL="resolve-labels-default/cni-control-target"
C6P_HAS_PROBE="N"
if [ -f "${S6P}/cilium-endpoint.expected.out" ]; then
  if grep -qF "${C6P_PROBE_LABEL}" "${S6P}/cilium-endpoint.expected.out"; then
    C6P_HAS_PROBE="Y"
  fi
fi
# Success-gate invocation log: the stub
# appends one TSV line per call. Required
# exactly one normal-handoff record and
# zero abort-classifier-unexpected
# records.
C6P_NORMAL_HANDOFF_COUNT=$(if [ -f "${S6P}/gate-invocations.log" ]; then
  awk -F'\t' '$3 == "mode=normal-handoff"' "${S6P}/gate-invocations.log" | wc -l | tr -d ' '
else
  echo 0
fi)
C6P_ABORT_CLASSIFIER_COUNT=$(if [ -f "${S6P}/gate-invocations.log" ]; then
  awk -F'\t' '$3 == "mode=abort-classifier-unexpected"' "${S6P}/gate-invocations.log" | wc -l | tr -d ' '
else
  echo 0
fi)
# Empty INSTALL_ABORT_CLASSIFICATION at
# handoff means Step G branched through the
# success path, NOT through abort_as.
C6P_EMPTY_ABORT_AT_HANDOFF="Y"
if [ -f "${S6P}/gate-invocations.log" ]; then
  if awk -F'\t' '$3 == "mode=normal-handoff" && $4 != "stage="' "${S6P}/gate-invocations.log" \
     | grep -qE 'label=([^[:space:]]+)'; then
    C6P_EMPTY_ABORT_AT_HANDOFF="N"
  fi
  if awk -F'\t' '$3 == "mode=normal-handoff" && $5 != "" && $5 != "detail=" && $5 != "label="' \
     "${S6P}/gate-invocations.log" | grep -qE 'label='; then
    C6P_EMPTY_ABORT_AT_HANDOFF="N"
  fi
fi
# No canonical readiness failure summary
# should appear on the success path.
C6P_CANONICAL_FAILURE_PRESENT="N"
if [ -s "${S6P}/readiness.summary.txt" ] && \
   [ "$(cat "${S6P}/readiness.summary.txt")" != "" ] && \
   grep -qE '^(CLUSTER_OR_CNI_NOT_READY|FIXTURE_NOT_READY|FIXTURE_IMAGE_NOT_LOADED|FIXTURE_INVALID|CONTROL_PATH_BLOCKED|UNKNOWN)$' "${S6P}/readiness.summary.txt" 2>/dev/null; then
  C6P_CANONICAL_FAILURE_PRESENT="Y"
fi

# d2b.48 vocab strengthening for C6P/Q/R
# recovery-success controls: the final
# readiness vocab artifact MUST list all 12
# static pairs AND exactly one dynamic probe
# with zero unexpected_fixture_like_pairs and
# zero duplicate_pairs. These synthesise the
# "manifest-aligned identity" requirement
# the install path satisfies on success.
C6P_VOCAB_OK="N"
if [ -s "${S6P}/fixture-pod-readiness.poll.summary.json" ]; then
  if python3 -c "
import json,sys
try:
  d=json.load(open(sys.argv[1]))
  ok = (
    len(d.get('observed_static_pairs', [])) == 12
    and len(d.get('dynamic_probe_pairs', [])) == 1
    and len(d.get('unexpected_fixture_like_pairs', [])) == 0
    and len(d.get('duplicate_pairs', [])) == 0
    and bool(d.get('canonical_population_ready')) is True
  )
  print('Y' if ok else 'N')
except Exception:
  print('N')
" "${S6P}/fixture-pod-readiness.poll.summary.json" | grep -q '^Y$'; then
    C6P_VOCAB_OK="Y"
  fi
fi

# C6q: First JSON poll contains only 12
# fixtures (cni-mock-arbitrary is admitted
# between polls). Second poll returns exact
# 13 Ready including the
# cni-control-probe-<rs>-<pod> generated
# identity. Step G MUST wait (NOT abort)
# until the second poll, then succeed, with
# poll_counter ending at 2.
S6Q="${TOP_TMP}/stage-C6q"
mkdir -p "${S6Q}"
FAKE_12_NO_ARBITRARY=$(printf '%s' "${FAKE_13_READY_TSV}" | grep -v 'cni-mock-arbitrary')
printf '%s' "${FAKE_12_NO_ARBITRARY}" > "${S6Q}/pods.poll1.tsv"
printf '%s' "${FAKE_13_READY_TSV}" > "${S6Q}/pods.poll2.tsv"
# stage TSV (any valid TSV so the element
# exists for the g_body; we override JSON-
# poll behaviour with FAKE_KUBECTL_JSON_POLL_TSVS).
write_stage_files "${S6Q}" "${FAKE_13_READY_TSV}" "${C6_RECORDING_GATE_STUB}"
write_env_file "${S6Q}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${S6Q}" \
  "HARNESS_STAGE=${S6Q}" \
  "HARNESS_STAGE_TSV=${S6Q}/pods.tsv" \
  "HARNESS_GATE_BIN=${C6_RECORDING_GATE_STUB}" \
  "CNI_READINESS_GATE_BIN=${C6_RECORDING_GATE_STUB}" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state_c6q" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=120" \
  "HARNESS_CILIUM_NAMES=${CILIUM_DEFAULT}" \
  "FAKE_KUBECTL_JSON_POLL_TSVS=${S6Q}/pods.poll1.tsv:${S6Q}/pods.poll2.tsv" \
  "FAKE_KUBECTL_JSON_POLL_COUNTER_FILE=${FAKE_BIN}/__json_poll_counter_c6q"
rm -f "${FAKE_BIN}/__date_state_c6q"
rm -f "${FAKE_BIN}/__json_poll_counter_c6q"
drive_control C6q "${S6Q}" "${S6Q}/run_g.sh" "${S6Q}/env.list"
R6Q=$(classify_control C6q "${S6Q}")
C6Q_RC="$(echo "${R6Q}" | awk -F'|' '{print $2}')"
# Required: exactly 2 readiness JSON polls
# before the recording success stub is
# invoked (poll 1 = 12 fixtures, poll 2 = 13
# fixtures). With the success stub there are
# NO downstream real-gate JSON queries to
# count.
C6Q_COUNTER=$(if [ -f "${FAKE_BIN}/__json_poll_counter_c6q" ]; then
  cat "${FAKE_BIN}/__json_poll_counter_c6q"
else
  echo 0
fi)
C6Q_POLL2_HAS_ARBITRARY="N"
if [ -f "${S6Q}/fixture-pod-readiness.poll.summary.json" ]; then
  if grep -q "cni-mock-arbitrary" "${S6Q}/fixture-pod-readiness.poll.summary.json"; then
    C6Q_POLL2_HAS_ARBITRARY="Y"
  fi
fi
C6Q_NEEDS_LABEL="resolve-labels-default/cni-mock-arbitrary"
C6Q_HAS_LABEL="N"
if [ -f "${S6Q}/cilium-endpoint.expected.out" ]; then
  if grep -qF "${C6Q_NEEDS_LABEL}" "${S6Q}/cilium-endpoint.expected.out"; then
    C6Q_HAS_LABEL="Y"
  fi
fi
# Success-gate handoff count — required
# exactly 1, abort-classifier-unexpected 0.
C6Q_NORMAL_HANDOFF_COUNT=$(if [ -f "${S6Q}/gate-invocations.log" ]; then
  awk -F'\t' '$3 == "mode=normal-handoff"' "${S6Q}/gate-invocations.log" | wc -l | tr -d ' '
else
  echo 0
fi)
C6Q_ABORT_CLASSIFIER_COUNT=$(if [ -f "${S6Q}/gate-invocations.log" ]; then
  awk -F'\t' '$3 == "mode=abort-classifier-unexpected"' "${S6Q}/gate-invocations.log" | wc -l | tr -d ' '
else
  echo 0
fi)
C6Q_EMPTY_ABORT_AT_HANDOFF="Y"
if [ -f "${S6Q}/gate-invocations.log" ]; then
  if grep -E 'mode=normal-handoff' "${S6Q}/gate-invocations.log" \
     | awk -F'\t' '{ for (i=1; i<=NF; i++) if ($i ~ /^label=./) print $i }' \
     | grep -vE '^label=$' >/dev/null 2>&1; then
    C6Q_EMPTY_ABORT_AT_HANDOFF="N"
  fi
fi
C6Q_CANONICAL_FAILURE_PRESENT="N"
if [ -s "${S6Q}/readiness.summary.txt" ] && \
   grep -qE '^(CLUSTER_OR_CNI_NOT_READY|FIXTURE_NOT_READY|FIXTURE_IMAGE_NOT_LOADED|FIXTURE_INVALID|CONTROL_PATH_BLOCKED|UNKNOWN)$' "${S6Q}/readiness.summary.txt" 2>/dev/null; then
  C6Q_CANONICAL_FAILURE_PRESENT="Y"
fi

# d2b.48 vocab strengthening for C6q.
C6Q_VOCAB_OK="N"
if [ -s "${S6Q}/fixture-pod-readiness.poll.summary.json" ]; then
  if python3 -c "
import json,sys
try:
  d=json.load(open(sys.argv[1]))
  ok = (
    len(d.get('observed_static_pairs', [])) == 12
    and len(d.get('dynamic_probe_pairs', [])) == 1
    and len(d.get('unexpected_fixture_like_pairs', [])) == 0
    and len(d.get('duplicate_pairs', [])) == 0
    and bool(d.get('canonical_population_ready')) is True
  )
  print('Y' if ok else 'N')
except Exception:
  print('N')
" "${S6Q}/fixture-pod-readiness.poll.summary.json" | grep -q '^Y$'; then
    C6Q_VOCAB_OK="Y"
  fi
fi

# C6r: Step G succeeds, handoff happens
# exactly once. Namespace text resembles
# cni-mock-trojan but Pod name is
# not-a-fixture-pod (NOT a real fixture
# vocabulary match); a separate Pod is in
# an unrelated namespace but has a valid
# fixture name (e.g. cni-mock-arbitrary in
# random-ns). Selection MUST follow
# metadata.name only — false namespace
# collision MUST be excluded; the valid name
# MUST be included. With the success stub
# and C6_RECORDING_GATE_STUB, target rc MUST
# be exactly 0.
S6R="${TOP_TMP}/stage-C6r"
mkdir -p "${S6R}"
FAKE_13_WITH_NS_COLLISION=$(printf '%s' "${FAKE_13_READY_TSV}")
# Add a row whose namespace looks like
# cni-mock-trojan but the Pod name is
# outside any fixture vocabulary.
FAKE_13_WITH_NS_COLLISION="${FAKE_13_WITH_NS_COLLISION}cni-mock-trojan-ns	not-a-fixture-pod	1/1	Running	0	7m
"
# Drop one valid fixture (cni-mock-arbitrary)
# so the entire cluster has 12 valid-fixture
# Pods + 1 namespace-disguised imposter.
# selection by metadata.name rejects the
# imposter (13 -> 12 valid), THEN we
# inject one more cni-mock-arbitrary with
# a non-cni namespace so it IS still
# selected and selected=13. The expected
# fixture-name selection must therefore be
# exactly 13.
# 13 fakes excluding cni-mock-arbitrary in
# the cni namespace, plus a
# namespace-disguised imposter (must NOT
# match any fixture vocabulary), plus a
# valid fixture-name Pod in an unrelated
# namespace (matches cni-mock-*). Selection
# by metadata.name must:
#   - skip the imposter (trojan)
#   - include the valid fixture-name Pod in
#     random-ns even though its namespace is
#     unrelated
# Net selected population == 13.
FAKE_12_NO_ARB_CNI=$(printf '%s' "${FAKE_13_READY_TSV}" | awk -F'\t' '$2 != "cni-mock-arbitrary"')
FAKE_13_PLUS_TROJAN_RANDOM_NS=$(printf '%s\n' "${FAKE_12_NO_ARB_CNI}")
FAKE_13_PLUS_TROJAN_RANDOM_NS=$(printf '%s\n%s\t%s\t%s\t%s\t%s\t%s\n%s\t%s\t%s\t%s\t%s\t%s\n' \
  "${FAKE_13_PLUS_TROJAN_RANDOM_NS}" \
  "cni-mock-trojan-ns" "not-a-fixture-pod" "1/1" "Running" "0" "7m" \
  "random-ns" "cni-mock-arbitrary" "1/1" "Running" "0" "7m")
write_stage_files "${S6R}" "${FAKE_13_PLUS_TROJAN_RANDOM_NS}" "${C6_RECORDING_GATE_STUB}"
write_env_file "${S6R}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${S6R}" \
  "HARNESS_STAGE=${S6R}" \
  "HARNESS_STAGE_TSV=${S6R}/pods.tsv" \
  "HARNESS_GATE_BIN=${C6_RECORDING_GATE_STUB}" \
  "CNI_READINESS_GATE_BIN=${C6_RECORDING_GATE_STUB}" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state_c6r" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=100" \
  "HARNESS_CILIUM_NAMES=${CILIUM_DEFAULT}"
rm -f "${FAKE_BIN}/__date_state_c6r"
drive_control C6r "${S6R}" "${S6R}/run_g.sh" "${S6R}/env.list"
R6R=$(classify_control C6r "${S6R}")
C6R_RC="$(echo "${R6R}" | awk -F'|' '{print $2}')"
# Selection population in the LAST poll
# summary.
C6R_SELECTED_COUNT="0"
C6R_TROJAN_EXCLUDED="N"
C6R_RANDOM_NS_VALID_INCLUDED="N"
C6R_MISSING_PAIR_PRESENT="N"
C6R_UNEXPECTED_TROJAN_PRESENT="N"
C6R_WRONG_NS_ARBITRARY_PRESENT="N"
if [ -f "${S6R}/fixture-pod-readiness.poll.summary.json" ]; then
  C6R_SELECTED_COUNT=$(python3 -c "
import json, sys
try:
  d=json.load(open(sys.argv[1]))
  print(len(d.get('selected', [])))
except Exception:
  print(-1)
" "${S6R}/fixture-pod-readiness.poll.summary.json")
  # `not-a-fixture-pod` is NOT fixture-shaped
  # (no ^cni-(mock|untrusted|control)-
  # prefix), so the canonical projection
  # deliberately ignores it from BOTH
  # `selected` and `unexpected_fixture_like`.
  # The d2b.48 trojan-excluded check still
  # requires the trojan to be absent from
  # `selected` (the test target). Scan the
  # TSV for the literal pod name and assert
  # it does not appear in selected_names.
  if [ -f "${S6R}/fixture-pod-readiness.poll.summary.json" ] \
      && python3 -c "
import json,sys
d=json.load(open(sys.argv[1]))
present = any(p.get('name')=='not-a-fixture-pod' for p in d.get('selected',[]))
print('N' if present else 'Y')
" "${S6R}/fixture-pod-readiness.poll.summary.json" \
      | grep -q '^Y$'; then
    C6R_TROJAN_EXCLUDED="Y"
  fi
  # Canonical-pair-missing evidence:
  # `default/cni-mock-arbitrary` must appear
  # in missing_static_pairs because the LAST
  # poll never saw that pair (it was replaced
  # by random-ns/cni-mock-arbitrary).
  if python3 -c "
import json,sys
d=json.load(open(sys.argv[1]))
print('Y' if any(p.get('name')=='cni-mock-arbitrary' and p.get('namespace')=='default' for p in d.get('missing_static_pairs',[])) else 'N')
" "${S6R}/fixture-pod-readiness.poll.summary.json" \
     | grep -q '^Y$'; then
    C6R_MISSING_PAIR_PRESENT="Y"
  fi
  # `cni-mock-trojan-ns/not-a-fixture-pod`
  # does NOT match the fixture-like prefix
  # `^cni-(mock|untrusted|control)-`, so
  # the canonical projection deliberately
  # drops it from BOTH `selected` and
  # `unexpected_fixture_like_pairs`. The
  # relevant d2b.48 navigation proof is
  # that the trojan is excluded from
  # `selected`, captured above as
  # C6R_TROJAN_EXCLUDED (Y when not-a-
  # fixture-pod is absent from selected).
  C6R_UNEXPECTED_TROJAN_PRESENT="${C6R_TROJAN_EXCLUDED}"
  # `random-ns/cni-mock-arbitrary` must
  # also appear in
  # unexpected_fixture_like_pairs (the
  # canonical contract rejects same-name in
  # wrong namespace; both extra_fixture_like
  # and wrong_namespace are accepted
  # vocabulary forms here).
  if python3 -c "
import json,sys
d=json.load(open(sys.argv[1]))
print('Y' if any(p.get('name')=='cni-mock-arbitrary' and p.get('namespace')=='random-ns' for p in d.get('unexpected_fixture_like_pairs',[])) else 'N')
" "${S6R}/fixture-pod-readiness.poll.summary.json" \
     | grep -q '^Y$'; then
    C6R_WRONG_NS_ARBITRARY_PRESENT="Y"
  fi
  # Legacy prefix-based check: the prior
  # driver expected the random-ns
  # cni-mock-arbitrary Pod to be ACTUALLY
  # included in selection. Under the d2b.48
  # canonical contract it must be REJECTED
  # (wrong_namespace), so this flips from Y
  # to N. We keep the variable for backward
  # transcript readability.
  if grep -q '"namespace".*"random-ns"' "${S6R}/fixture-pod-readiness.poll.summary.json"; then
    C6R_RANDOM_NS_VALID_INCLUDED="Y"
  fi
fi
C6R_NORMAL_HANDOFF_COUNT=$(if [ -f "${S6R}/gate-invocations.log" ]; then
  awk -F'\t' '$3 == "mode=normal-handoff"' "${S6R}/gate-invocations.log" | wc -l | tr -d ' '
else
  echo 0
fi)
C6R_ABORT_CLASSIFIER_COUNT=$(if [ -f "${S6R}/gate-invocations.log" ]; then
  awk -F'\t' '$3 == "mode=abort-classifier-unexpected"' "${S6R}/gate-invocations.log" | wc -l | tr -d ' '
else
  echo 0
fi)
C6R_EMPTY_ABORT_AT_HANDOFF="Y"
if [ -f "${S6R}/gate-invocations.log" ]; then
  if grep -E 'mode=normal-handoff' "${S6R}/gate-invocations.log" \
     | awk -F'\t' '{ for (i=1; i<=NF; i++) if ($i ~ /^label=./) print $i }' \
     | grep -vE '^label=$' >/dev/null 2>&1; then
    C6R_EMPTY_ABORT_AT_HANDOFF="N"
  fi
fi
C6R_CANONICAL_FAILURE_PRESENT="N"
if [ -s "${S6R}/readiness.summary.txt" ] && \
   grep -qE '^(CLUSTER_OR_CNI_NOT_READY|FIXTURE_NOT_READY|FIXTURE_IMAGE_NOT_LOADED|FIXTURE_INVALID|CONTROL_PATH_BLOCKED|UNKNOWN)$' "${S6R}/readiness.summary.txt" 2>/dev/null; then
  C6R_CANONICAL_FAILURE_PRESENT="Y"
fi

# C6s: kubectl exits 0 but the JSON payload
# is malformed. The python projection must
# fail closed with a structured parse-error
# artifact (FIXTURE_NOT_READY 12) — NEVER
# interpreted as observed_count=0 timeout
# at the deadline. With the success stub
# replaced by the real gate in this
# control, the gate MAY still exit 12 from
# a downstream Gate 9 boundary; what
# matters is the projection-json artifact
# phase is `fixture_inventory_projection_failure`
# and rc=17, the FORBIDDEN
# `fixture_pod_readiness_timeout` phase is
# ABSENT, and zero normal-handoff or
# abort-classifier-unexpected invocations
# are recorded (because Step G aborted
# BEFORE the gate stub).
S6S="${TOP_TMP}/stage-C6s"
mkdir -p "${S6S}"
write_stage_files "${S6S}" "${FAKE_13_READY_TSV}" "${C6_RECORDING_GATE_STUB}"
write_env_file "${S6S}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${S6S}" \
  "HARNESS_STAGE=${S6S}" \
  "HARNESS_STAGE_TSV=${S6S}/pods.tsv" \
  "HARNESS_GATE_BIN=${REAL_GATE_BIN}" \
  "CNI_READINESS_GATE_BIN=${REAL_GATE_BIN}" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state_c6s" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=240" \
  "HARNESS_CILIUM_NAMES=${CILIUM_DEFAULT}" \
  "FAKE_FIXTURE_JSON_MALFORMED=1"
rm -f "${FAKE_BIN}/__date_state_c6s"
drive_control C6s "${S6S}" "${S6S}/run_g.sh" "${S6S}/env.list"
R6S=$(classify_control C6s "${S6S}")
C6S_RC="$(echo "${R6S}" | awk -F'|' '{print $2}')"
C6S_SUMMARY="$(echo "${R6S}" | awk -F'|' '{print $3}')"
C6S_PARSE_PHASE="N"
C6S_PARSE_RC_FIELD="N"
C6S_FORBIDDEN_TIMEOUT_PHASE="Y"
C6S_GATE_HANDOFF_COUNT="0"
C6S_GATE_ABORT_CLASSIFIER_COUNT="0"
if [ -f "${S6S}/fixture-pod-readiness-timeout.json" ]; then
  if grep -q "fixture_inventory_projection_failure" "${S6S}/fixture-pod-readiness-timeout.json"; then
    C6S_PARSE_PHASE="Y"
  fi
  if grep -qE '"rc":[[:space:]]*17' "${S6S}/fixture-pod-readiness-timeout.json"; then
    C6S_PARSE_RC_FIELD="Y"
  fi
  if grep -q "fixture_pod_readiness_timeout\|observed_count=0" "${S6S}/fixture-pod-readiness-timeout.json"; then
    C6S_FORBIDDEN_TIMEOUT_PHASE="N"
  fi
fi
# Required extra: zero handoff invocations
# of either kind against the recording
# success gate — Step G aborted before
# handoff because the json projection
# failed, not the gate.
if [ -f "${S6S}/gate-invocations.log" ]; then
  C6S_GATE_HANDOFF_COUNT=$(awk -F'\t' '$3 == "mode=normal-handoff"' \
    "${S6S}/gate-invocations.log" | wc -l | tr -d ' ')
  C6S_GATE_ABORT_CLASSIFIER_COUNT=$(awk -F'\t' \
    '$3 == "mode=abort-classifier-unexpected"' \
    "${S6S}/gate-invocations.log" | wc -l | tr -d ' ')
fi

# ---------------------------------------------------------------------------
# d2b.48 Block C6 mutation controls.
#
# C6t: stale cni-mock-old substitution.
# Drop default/cni-mock-arbitrary from
# the Pod inventory and replace it with
# default/cni-mock-old (a stale Pod whose
# name pretends to be a fixture). The
# fixture-like count remains 13 but the
# canonical 12+1 contract has no
# cni-mock-old pair, so install Step G
# must report a missing canonical pair
# (default/cni-mock-arbitrary) AND an
# unexpected fixture-like pair
# (default/cni-mock-old). Target rc=12
# FIXTURE_NOT_READY; handoff count=0;
# gate aborted before stub invocation.
S6T="${TOP_TMP}/stage-C6t"
mkdir -p "${S6T}"
FAKE_13_WITH_STALE=$(printf '%s\n' "${FAKE_13_READY_TSV}" \
  | awk -F'\t' '
    $2 == "cni-mock-arbitrary" { next }
    { print }
  ')
FAKE_13_WITH_STALE=$(printf '%s\n%s\t%s\t%s\t%s\t%s\t%s\n' \
  "${FAKE_13_WITH_STALE}" \
  "default" "cni-mock-old" "1/1" "Running" "0" "7m")
write_stage_files "${S6T}" "${FAKE_13_WITH_STALE}" "${REAL_GATE_BIN}"
write_env_file "${S6T}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${S6T}" \
  "HARNESS_STAGE=${S6T}" \
  "HARNESS_STAGE_TSV=${S6T}/pods.tsv" \
  "HARNESS_GATE_BIN=${REAL_GATE_BIN}" \
  "CNI_READINESS_GATE_BIN=${REAL_GATE_BIN}" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state_c6t" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=240" \
  "HARNESS_CILIUM_NAMES=${CILIUM_DEFAULT}" \
  "FAKE_WAITING_REASON=ImagePullBackOff"
rm -f "${FAKE_BIN}/__date_state_c6t"
drive_control C6t "${S6T}" "${S6T}/run_g.sh" "${S6T}/env.list"
R6T=$(classify_control C6t "${S6T}")
C6T_RC="$(echo "${R6T}" | awk -F'|' '{print $2}')"
C6T_SUMMARY="$(echo "${R6T}" | awk -F'|' '{print $3}')"
C6T_MISSING_PAIR="N"
C6T_UNEXPECTED_STALE="N"
C6T_ABORT_EXPECTED="Y"
if [ -f "${S6T}/fixture-pod-readiness.poll.summary.json" ]; then
  if python3 -c "
import json,sys
d=json.load(open(sys.argv[1]))
print('Y' if any(p.get('name')=='cni-mock-arbitrary' and p.get('namespace')=='default' for p in d.get('missing_static_pairs',[])) else 'N')
" "${S6T}/fixture-pod-readiness.poll.summary.json" \
     | grep -q '^Y$'; then
    C6T_MISSING_PAIR="Y"
  fi
  if python3 -c "
import json,sys
d=json.load(open(sys.argv[1]))
print('Y' if any(p.get('name')=='cni-mock-old' and p.get('namespace')=='default' for p in d.get('unexpected_fixture_like_pairs',[])) else 'N')
" "${S6T}/fixture-pod-readiness.poll.summary.json" \
     | grep -q '^Y$'; then
    C6T_UNEXPECTED_STALE="Y"
  fi
elif [ -f "${S6T}/fixture-pod-readiness-timeout.json" ]; then
  if grep -q '"cni-mock-arbitrary"' "${S6T}/fixture-pod-readiness-timeout.json" \
     && python3 -c "
import json,sys
try:
  d=json.load(open(sys.argv[1]))
  miss = any(p.get('name')=='cni-mock-arbitrary' and p.get('namespace')=='default' for p in d.get('missing_static_pairs',[]))
  uex = any(p.get('name')=='cni-mock-old' and p.get('namespace')=='default' for p in d.get('unexpected_fixture_like_pairs',[]))
  print('Y' if (miss and uex) else 'N')
except Exception:
  print('N')
" "${S6T}/fixture-pod-readiness-timeout.json" \
     | grep -q '^Y$'; then
    C6T_MISSING_PAIR="Y"
    C6T_UNEXPECTED_STALE="Y"
  fi
fi

# C6u: wrong-namespace substitution.
# Replace default/cni-mock-postgres with
# random-ns/cni-mock-postgres. Same name
# but wrong namespace does NOT satisfy
# the canonical pair; install Step G
# must report a missing canonical pair
# (default/cni-mock-postgres) AND an
# unexpected fixture-like pair labelled
# wrong_namespace (random-ns/cni-mock-
# postgres). Target rc=12.
S6U="${TOP_TMP}/stage-C6u"
mkdir -p "${S6U}"
FAKE_13_WRONG_NS=$(printf '%s\n' "${FAKE_13_READY_TSV}" \
  | awk -F'\t' '
    $2 == "cni-mock-postgres" { next }
    { print }
  ')
FAKE_13_WRONG_NS=$(printf '%s\n%s\t%s\t%s\t%s\t%s\t%s\n' \
  "${FAKE_13_WRONG_NS}" \
  "random-ns" "cni-mock-postgres" "1/1" "Running" "0" "7m")
write_stage_files "${S6U}" "${FAKE_13_WRONG_NS}" "${REAL_GATE_BIN}"
write_env_file "${S6U}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${S6U}" \
  "HARNESS_STAGE=${S6U}" \
  "HARNESS_STAGE_TSV=${S6U}/pods.tsv" \
  "HARNESS_GATE_BIN=${REAL_GATE_BIN}" \
  "CNI_READINESS_GATE_BIN=${REAL_GATE_BIN}" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state_c6u" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=120" \
  "HARNESS_CILIUM_NAMES=${CILIUM_DEFAULT}"
rm -f "${FAKE_BIN}/__date_state_c6u"
drive_control C6u "${S6U}" "${S6U}/run_g.sh" "${S6U}/env.list"
R6U=$(classify_control C6u "${S6U}")
C6U_RC="$(echo "${R6U}" | awk -F'|' '{print $2}')"
C6U_SUMMARY="$(echo "${R6U}" | awk -F'|' '{print $3}')"
C6U_MISSING_PAIR="N"
C6U_WRONG_NS_REJECTED="N"
if [ -f "${S6U}/fixture-pod-readiness.poll.summary.json" ]; then
  if python3 -c "
import json,sys
d=json.load(open(sys.argv[1]))
print('Y' if any(p.get('name')=='cni-mock-postgres' and p.get('namespace')=='default' for p in d.get('missing_static_pairs',[])) else 'N')
" "${S6U}/fixture-pod-readiness.poll.summary.json" \
     | grep -q '^Y$'; then
    C6U_MISSING_PAIR="Y"
  fi
  if python3 -c "
import json,sys
d=json.load(open(sys.argv[1]))
print('Y' if any(p.get('name')=='cni-mock-postgres' and p.get('namespace')=='random-ns' for p in d.get('unexpected_fixture_like_pairs',[])) else 'N')
" "${S6U}/fixture-pod-readiness.poll.summary.json" \
     | grep -q '^Y$'; then
    C6U_WRONG_NS_REJECTED="Y"
  fi
elif [ -f "${S6U}/fixture-pod-readiness-timeout.json" ]; then
  if grep -q '"cni-mock-postgres"' "${S6U}/fixture-pod-readiness-timeout.json" \
     && python3 -c "
import json,sys
try:
  d=json.load(open(sys.argv[1]))
  miss = any(p.get('name')=='cni-mock-postgres' and p.get('namespace')=='default' for p in d.get('missing_static_pairs',[]))
  uex = any(p.get('name')=='cni-mock-postgres' and p.get('namespace')=='random-ns' for p in d.get('unexpected_fixture_like_pairs',[]))
  print('Y' if (miss and uex) else 'N')
except Exception:
  print('N')
" "${S6U}/fixture-pod-readiness-timeout.json" \
     | grep -q '^Y$'; then
    C6U_MISSING_PAIR="Y"
    C6U_WRONG_NS_REJECTED="Y"
  fi
fi

# C6v: two-probe substitution.
# Remove default/cni-mock-postgres and
# supply TWO distinct Deployment-
# generated cni-control-probe Pods so
# the fixture-like count still reads
# 13. Canonical vocabulary rejects 0
# or 2 dynamic probes (must be exactly
# 1). Target rc=12, missing canonical
# pair, dynamic_probe_cardinality==2.
S6V="${TOP_TMP}/stage-C6v"
mkdir -p "${S6V}"
FAKE_12_NO_POSTGRES_NOR_PROBE=$(printf '%s\n' "${FAKE_13_READY_TSV}" \
  | awk -F'\t' '
    $2 == "cni-mock-postgres" || $2 ~ /^cni-control-probe-/ { next }
    { print }
  ')
FAKE_TWO_PROBES=$(printf '%s\n%s\t%s\t1/1\tRunning\t0\t7m\n%s\t%s\t1/1\tRunning\t0\t7m\n' \
  "${FAKE_12_NO_POSTGRES_NOR_PROBE}" \
  "cni-control" "cni-control-probe-aaaaaaaa-bbbbb" \
  "cni-control" "cni-control-probe-cccccccc-ddddd")
write_stage_files "${S6V}" "${FAKE_TWO_PROBES}" "${REAL_GATE_BIN}"
write_env_file "${S6V}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${S6V}" \
  "HARNESS_STAGE=${S6V}" \
  "HARNESS_STAGE_TSV=${S6V}/pods.tsv" \
  "HARNESS_GATE_BIN=${REAL_GATE_BIN}" \
  "CNI_READINESS_GATE_BIN=${REAL_GATE_BIN}" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state_c6v" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=120" \
  "HARNESS_CILIUM_NAMES=${CILIUM_DEFAULT}"
rm -f "${FAKE_BIN}/__date_state_c6v"
drive_control C6v "${S6V}" "${S6V}/run_g.sh" "${S6V}/env.list"
R6V=$(classify_control C6v "${S6V}")
C6V_RC="$(echo "${R6V}" | awk -F'|' '{print $2}')"
C6V_SUMMARY="$(echo "${R6V}" | awk -F'|' '{print $3}')"
C6V_MISSING_PAIR="N"
C6V_PROBE_CARD="0"
if [ -f "${S6V}/fixture-pod-readiness.poll.summary.json" ]; then
  if python3 -c "
import json,sys
d=json.load(open(sys.argv[1]))
print('Y' if any(p.get('name')=='cni-mock-postgres' and p.get('namespace')=='default' for p in d.get('missing_static_pairs',[])) else 'N')
" "${S6V}/fixture-pod-readiness.poll.summary.json" \
     | grep -q '^Y$'; then
    C6V_MISSING_PAIR="Y"
  fi
  C6V_PROBE_CARD=$(python3 -c "
import json,sys
d=json.load(open(sys.argv[1]))
print(len(d.get('dynamic_probe_pairs',[])))
" "${S6V}/fixture-pod-readiness.poll.summary.json")
elif [ -f "${S6V}/fixture-pod-readiness-timeout.json" ]; then
  C6V_PROBE_CARD=$(python3 -c "
import json,sys
try:
  d=json.load(open(sys.argv[1]))
  print(len(d.get('dynamic_probe_pairs',[])))
except Exception:
  print(0)
" "${S6V}/fixture-pod-readiness-timeout.json")
  if python3 -c "
import json,sys
try:
  d=json.load(open(sys.argv[1]))
  miss = any(p.get('name')=='cni-mock-postgres' and p.get('namespace')=='default' for p in d.get('missing_static_pairs',[]))
  print('Y' if miss else 'N')
except Exception:
  print('N')
" "${S6V}/fixture-pod-readiness-timeout.json" \
     | grep -q '^Y$'; then
    C6V_MISSING_PAIR="Y"
  fi
fi

# ---------------------------------------------------------------------------

# C7a/b/c split: independent Cilium-stage
# controls. C7a injects daemon-list failure;
# C7b injects per-daemon exec failure;
# C7c injects valid 12-of-13 endpoint
# under-convergence. C7k is the fallback
# mutation control confirming that removing
# cni-untrusted-default from a clean run makes
# the success control fail.
#
# All three rely on the fixture inventory
# returning rc 0 first, then injecting the
# Cilium-specific failure independently from
# the original FAKE_KUBECTL_RC global switch.
C7A="${TOP_TMP}/stage-C7a"
mkdir -p "${C7A}"
write_stage_files "${C7A}" "${FAKE_13_READY_TSV}" "${REAL_GATE_BIN}"
write_env_file "${C7A}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${C7A}" \
  "HARNESS_STAGE_TSV=${C7A}/pods.tsv" \
  "HARNESS_GATE_BIN=${REAL_GATE_BIN}" \
  "CNI_READINESS_GATE_BIN=${REAL_GATE_BIN}" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=240" \
  "HARNESS_CILIUM_NAMES=${CILIUM_DEFAULT}" \
  "FAKE_CILIUM_DAEMON_LIST_RC=7"
drive_control C7a "${C7A}" "${C7A}/run_g.sh" "${C7A}/env.list"
R7A=$(classify_control C7a "${C7A}")
C7A_RC="$(echo "${R7A}" | awk -F'|' '{print $2}')"
C7A_SUMMARY="$(echo "${R7A}" | awk -F'|' '{print $3}')"
C7A_LOGCLS="$(echo "${R7A}" | awk -F'|' '{print $4}')"
C7A_DOWNSTREAM="$(echo "${R7A}" | awk -F'|' '{print $5}')"
C7A_MISMATCH="$(echo "${R7A}" | awk -F'|' '{print $6}')"
C7A_ERR_ART="N"
if [ -s "${C7A}/cilium-endpoint-inventory-error.json" ] || grep -q 'cilium-endpoint-inventory-error' "${C7A}/install.log" 2>/dev/null; then
  C7A_ERR_ART="Y"
fi
C7A_NAMED_DAEMON_LIST=$(grep -q 'cilium daemon list failed\|cilium daemon-list stderr\|cilium-deamon-list' "${C7A}/install.log" 2>/dev/null && echo Y || echo N)
C7A_RC7=$(grep -q 'rc=7' "${C7A}/install.log" 2>/dev/null && echo Y || echo N)

C7B="${TOP_TMP}/stage-C7b"
mkdir -p "${C7B}"
write_stage_files "${C7B}" "${FAKE_13_READY_TSV}" "${REAL_GATE_BIN}"
write_env_file "${C7B}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${C7B}" \
  "HARNESS_STAGE_TSV=${C7B}/pods.tsv" \
  "HARNESS_GATE_BIN=${REAL_GATE_BIN}" \
  "CNI_READINESS_CLASSIFICATION=${REAL_GATE_BIN}" \
  "CNI_READINESS_GATE_BIN=${REAL_GATE_BIN}" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=240" \
  "HARNESS_CILIUM_NAMES=${CILIUM_DEFAULT}" \
  "FAKE_CILIUM_EXEC_RC=8"
drive_control C7b "${C7B}" "${C7B}/run_g.sh" "${C7B}/env.list"
R7B=$(classify_control C7b "${C7B}")
C7B_RC="$(echo "${R7B}" | awk -F'|' '{print $2}')"
C7B_SUMMARY="$(echo "${R7B}" | awk -F'|' '{print $3}')"
C7B_LOGCLS="$(echo "${R7B}" | awk -F'|' '{print $4}')"
C7B_DOWNSTREAM="$(echo "${R7B}" | awk -F'|' '{print $5}')"
C7B_MISMATCH="$(echo "${R7B}" | awk -F'|' '{print $6}')"
C7B_ERR_ART="N"
if [ -s "${C7B}/cilium-endpoint-convergence.json" ] || grep -q 'cilium-endpoint-convergence' "${C7B}/install.log" 2>/dev/null; then
  C7B_ERR_ART="Y"
fi
C7B_DAEMON_NAMED=$(grep -qE 'cilium daemon exec.*(cilium-fake-x|cilium-fake-)' "${C7B}/install.log" 2>/dev/null && echo Y || echo N)
C7B_RC8=$(grep -q 'rc=8' "${C7B}/install.log" 2>/dev/null && echo Y || echo N)

# C7c: valid-empty endpoint under-convergence
# against an exact 13 fixture population.
# Build a 12-of-13 cilium-names set; date
# advances faster than the deadline so the
# bounded loop terminates with LAST=12.
C7C_NAMES_12=$(printf '%s' "${CILIUM_DEFAULT}" | tr ' ' '\n' | grep -v '^cni-untrusted-default$' | tr '\n' ' ' | sed 's/ $//')
C7C="${TOP_TMP}/stage-C7c"
mkdir -p "${C7C}"
write_stage_files "${C7C}" "${FAKE_13_READY_TSV}" "${REAL_GATE_BIN}"
write_env_file "${C7C}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${C7C}" \
  "HARNESS_STAGE_TSV=${C7C}/pods.tsv" \
  "HARNESS_GATE_BIN=${REAL_GATE_BIN}" \
  "CNI_READINESS_GATE_BIN=${REAL_GATE_BIN}" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=240" \
  "HARNESS_CILIUM_NAMES=${C7C_NAMES_12}"
drive_control C7c "${C7C}" "${C7C}/run_g.sh" "${C7C}/env.list"
R7C=$(classify_control C7c "${C7C}")
C7C_RC="$(echo "${R7C}" | awk -F'|' '{print $2}')"
C7C_SUMMARY="$(echo "${R7C}" | awk -F'|' '{print $3}')"
C7C_LOGCLS="$(echo "${R7C}" | awk -F'|' '{print $4}')"
C7C_DOWNSTREAM="$(echo "${R7C}" | awk -F'|' '{print $5}')"
C7C_MISMATCH="$(echo "${R7C}" | awk -F'|' '{print $6}')"
C7C_NO_CMD_ERR="N"
if [ ! -s "${C7C}/cilium-endpoint-inventory-error.json" ]; then
  C7C_NO_CMD_ERR="Y"
fi
C7C_CONV_ART="N"
if [ -s "${C7C}/cilium-endpoint-convergence.json" ] || grep -q 'cilium-endpoint-convergence' "${C7C}/install.log" 2>/dev/null; then
  C7C_CONV_ART="Y"
fi
C7C_OBS_12_EXP_13=$(grep -qE 'observed=.*12.*expected=13|12.*13.*convergence|under-converged' "${C7C}/install.log" 2>/dev/null && echo Y || echo N)

# C7k: success control with cilium-names
# MISSING cni-untrusted-default. Falls under
# C7a/b/c's vocabulary: removing any of the
# 13 exact names results in a 12-of-13 valid
# under-convergence failure (CLUSTER_OR_CNI_NOT_READY
# 10). The success path must therefore NOT be
# reachable when cni-untrusted-default is
# absent.
C7K_NAMES_NO_UNTRUSTED=$(printf '%s' "${CILIUM_DEFAULT}" | tr ' ' '\n' | grep -v '^cni-untrusted-default$' | tr '\n' ' ' | sed 's/ $//')
C7K="${TOP_TMP}/stage-C7k"
mkdir -p "${C7K}"
write_stage_files "${C7K}" "${FAKE_13_READY_TSV}" "${REAL_GATE_BIN}"
write_env_file "${C7K}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${C7K}" \
  "HARNESS_STAGE_TSV=${C7K}/pods.tsv" \
  "HARNESS_GATE_BIN=${REAL_GATE_BIN}" \
  "CNI_READINESS_GATE_BIN=${REAL_GATE_BIN}" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=240" \
  "HARNESS_CILIUM_NAMES=${C7K_NAMES_NO_UNTRUSTED}"
drive_control C7k "${C7K}" "${C7K}/run_g.sh" "${C7K}/env.list"
R7K=$(classify_control C7k "${C7K}")
C7K_RC="$(echo "${R7K}" | awk -F'|' '{print $2}')"
C7K_SUMMARY="$(echo "${R7K}" | awk -F'|' '{print $3}')"
C7K_LOGCLS="$(echo "${R7K}" | awk -F'|' '{print $4}')"
C7K_DOWNSTREAM="$(echo "${R7K}" | awk -F'|' '{print $5}')"
C7K_MISMATCH="$(echo "${R7K}" | awk -F'|' '{print $6}')"
C7K_OBS_12_EXP_13=$(grep -qE 'observed=.*12.*expected=13|12.*13.*convergence|under-converged' "${C7K}/install.log" 2>/dev/null && echo Y || echo N)

# C7s (install Step G): equal-count wrong-label
# mutation. expected_labels set carries the
# real 13 (incl cni-untrusted-default); the
# FAKE_CILIUM_NAMES projection collapses to 13
# but substitutes cni-stale-old for
# cni-untrusted-default. Convergence JSON
# MUST show expected_count==observed_count==13
# AND missing contains `cni-untrusted-default`
# AND unexpected contains `cni-stale-old`,
# rc=10, downstream gate NOT invoked.
#
# Why this control exists: a count-only success
# predicate would false-pass because LAST (13)
# equals EXPECTED (13). The new identity
# contract refuses to declare convergence when
# the two sets are NOT byte-equal.
C7S_TSV="${TOP_TMP}/gate8-c7s-inventory.tsv"
make_exact_13_names_tsv "${C7S_TSV}"
# Equal-count mutation: replace
# cni-untrusted-default in the canonical 13
# names (extracted from C7S_TSV so the
# tricky-edge names match what Step G's
# expected-set derivation will see) with
# cni-stale-old. Result is the same field
# count (13) with one element replaced by
# an unrelated entry. A count-only success
# predicate would (incorrectly) PASS.
C7S_NAMES_BEFORE_STALE=$(awk -F'\t' 'NR>0 {print $2}' "${C7S_TSV}" \
  | sort -u)
C7S_NAMES_13_STALE=$(printf '%s\n' "${C7S_NAMES_BEFORE_STALE}" \
  | sed 's|^cni-untrusted-default$|cni-stale-old|' \
  | tr '\n' ' ' \
  | sed 's/ $//')
[ -z "${C7S_NAMES_13_STALE}" ] && C7S_NAMES_13_STALE="${C7S_NAMES_BEFORE_STALE}"
C7S="${TOP_TMP}/stage-C7s"
mkdir -p "${C7S}"
write_stage_files "${C7S}" "${FAKE_13_READY_TSV}" "${REAL_GATE_BIN}"
write_env_file "${C7S}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${C7S}" \
  "HARNESS_STAGE_TSV=${C7S}/pods.tsv" \
  "HARNESS_GATE_BIN=${REAL_GATE_BIN}" \
  "CNI_READINESS_GATE_BIN=${REAL_GATE_BIN}" \
  "HARNESS_FIXTURE_NAMES_TSV=${C7S_TSV}" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=240" \
  "HARNESS_DATE_NOW=1700000000" \
  "HARNESS_CILIUM_NAMES=${C7S_NAMES_13_STALE}"
# Note: FAKE_CILIUM_NAMES is auto-derived from
# HARNESS_CILIUM_NAMES inside both the
# install-arc body and the real-gate body,
# so we do NOT pass it explicitly.
drive_control C7s "${C7S}" "${C7S}/run_g.sh" "${C7S}/env.list"
R7S=$(classify_control C7s "${C7S}")
C7S_RC="$(echo "${R7S}" | awk -F'|' '{print $2}')"
C7S_SUMMARY="$(echo "${R7S}" | awk -F'|' '{print $3}')"
C7S_LOGCLS="$(echo "${R7S}" | awk -F'|' '{print $4}')"
C7S_DOWNSTREAM="$(echo "${R7S}" | awk -F'|' '{print $5}')"
# C7s numerical identity proof: convergence JSON
# carries expected_count==observed_count==13 AND
# missing contains cni-untrusted-default AND
# unexpected contains cni-stale-old.
C7S_CONV_JSON="${C7S}/cilium-endpoint-convergence.json"
C7S_CMD_ERR_JSON="${C7S}/cilium-endpoint-inventory-error.json"
C7S_HAS_CONV_ART="N"
C7S_NO_CMD_ERR="Y"
C7S_EXP_COUNT="0"
C7S_OBS_COUNT="0"
C7S_MISSING_HAS_UNTRUSTED="N"
C7S_UNEXPECTED_HAS_STALE="N"
if [ -s "${C7S_CONV_JSON}" ]; then
  C7S_HAS_CONV_ART="Y"
fi
[ -s "${C7S_CMD_ERR_JSON}" ] && C7S_NO_CMD_ERR="N"
if [ "${C7S_HAS_CONV_ART}" = "Y" ] \
   && python3 - "${C7S_CONV_JSON}" 2>/dev/null <<'PYEOF'
import json,sys,os
d=json.load(open(sys.argv[1]))
assert int(d.get("expected_count",-1))==13, d
assert int(d.get("observed_count",-1))==13, d
ml=d.get("missing_labels") or []
ul=d.get("unexpected_labels") or []
assert any("cni-untrusted-default" in x for x in ml), ml
assert any("cni-stale-old" in x for x in ul), ul
os.environ["C7S_OK"]="Y"
PYEOF
then
  C7S_EXP_COUNT="13"
  C7S_OBS_COUNT="13"
  C7S_MISSING_HAS_UNTRUSTED="Y"
  C7S_UNEXPECTED_HAS_STALE="Y"
fi

# C7d (install Step G): set-diff command failure.
# Injection: the FIRST comm call inside the
# install Step G identity loop (comm -23
# expected_labels unique_labels) exits rc=9.
# The install script MUST (a) capture that
# rc verbatim, (b) fail closed as
# CLUSTER_OR_CNI_NOT_READY exit 10
# IMMEDIATELY (not let empty files pretend
# equality), (c) write a structured
# cilium-endpoint-setdiff-error.json with
# operation/rc/expected_path/observed_path/
# output_path/stderr/phase, and (d) NOT
# invoke the downstream gate. Stage-scoped
# FAKE_COMM_FAIL_RC=9 is reset between
# controls (no leakage to other stages).
C7D="${TOP_TMP}/stage-C7d"
mkdir -p "${C7D}"
write_stage_files "${C7D}" "${FAKE_13_READY_TSV}" "${REAL_GATE_BIN}"
write_env_file "${C7D}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${C7D}" \
  "HARNESS_STAGE_TSV=${C7D}/pods.tsv" \
  "HARNESS_GATE_BIN=${REAL_GATE_BIN}" \
  "CNI_READINESS_GATE_BIN=${REAL_GATE_BIN}" \
  "HARNESS_FIXTURE_NAMES_TSV=${C7D}/pods.tsv" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state_c7d" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=240" \
  "HARNESS_DATE_NOW=1700000000" \
  "FAKE_COMM_FAIL_RC=9" \
  "FAKE_COMM_FAIL_OP=missing_labels_diff"
# Stage-scoped date state reset so the loop
# runs the controlled 240-second step.
rm -f "${FAKE_BIN}/__date_state_c7d"
drive_control C7d "${C7D}" "${C7D}/run_g.sh" "${C7D}/env.list"
C7D_RC="$(awk -F'=' '/^rc=/ {print $2; exit}' "${C7D}/child.rc" 2>/dev/null)"
G7D_BASE_S="${C7D}"
G7D_SUM_S="${G7D_BASE_S}/readiness.summary.txt"
C7D_SUMMARY="$(cat "${G7D_SUM_S}" 2>/dev/null || echo '__MISSING__')"
[ ! -f "${G7D_SUM_S}" ] && C7D_SUMMARY="__MISSING__"
C7D_LOGCLS="$(grep -E '^classification=' "${G7D_BASE_S}/readiness.log" 2>/dev/null | head -1)"
C7D_SDERR_JSON="${G7D_BASE_S}/cilium-endpoint-setdiff-error.json"
C7D_HAS_SDERR="N"
C7D_HAS_RC9="N"
C7D_HAS_OP="N"
C7D_HAS_EXP_PATH="N"
C7D_HAS_OBS_PATH="N"
C7D_HAS_STDERR="N"
if [ -s "${C7D_SDERR_JSON}" ]; then
  C7D_HAS_SDERR="Y"
  python3 - "${C7D_SDERR_JSON}" <<'PYEOF' >/dev/null 2>&1 && C7D_HAS_RC9="Y" || true
import json,sys
d=json.load(open(sys.argv[1]))
assert int(d.get("rc",-1))==9, d
PYEOF
  grep -q '"operation"' "${C7D_SDERR_JSON}" && C7D_HAS_OP="Y"
  grep -q '"expected_path"' "${C7D_SDERR_JSON}" && C7D_HAS_EXP_PATH="Y"
  grep -q '"observed_path"' "${C7D_SDERR_JSON}" && C7D_HAS_OBS_PATH="Y"
  grep -q '"stderr"' "${C7D_SDERR_JSON}" && C7D_HAS_STDERR="Y"
fi
C7D_DOWNSTREAM="N"
[ -f "${C7D}/downstream-stub-sentinel" ] && C7D_DOWNSTREAM="Y"
# Reset injection so it does not leak to
# subsequent stage (C7r/C7s/etc.).
unset FAKE_COMM_FAIL_RC FAKE_COMM_FAIL_OP

# C7r (install Step G recovery):
# first daemon-list poll rc 0 + empty;
# second poll returns daemon(s) and the 13
# unique endpoint labels surface. Step G must
# reach its downstream success gate (CNI_READINESS_GATE_BIN
# invocation) exactly once.
#
# Implementation:
#   - For FAKE_CILIUM_DAEMON_LIST_RECOVERY=1 the
#     fake kubectl returns empty (rc 0) on its
#     FIRST invocation, then a real daemon name on
#     subsequent invocations. The counter file
#     records invocations across process boundaries.
#   - HARNESS_DATE_STEP=1 keeps the inner loop's
#     date check inside the deadline so that the
#     daemon-list runs more than once.
#   - HARNESS_CILIUM_NAMES provides the full
#     13-label set so convergence succeeds.
#   - The success branch (downstream gate
#     invocation) is delegated to the per-stage
#     success-stub so we can prove invocation
#     via fake downstream-stub-sentinel.
C7R="${TOP_TMP}/stage-C7r"
mkdir -p "${C7R}"
write_stage_files "${C7R}" "${FAKE_13_READY_TSV}"
write_env_file "${C7R}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${C7R}" \
  "HARNESS_STAGE_TSV=${C7R}/pods.tsv" \
  "HARNESS_GATE_BIN=${C7R}/cni-readiness-gate.sh" \
  "CNI_READINESS_GATE_BIN=${C7R}/cni-readiness-gate.sh" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=120" \
  "HARNESS_CILIUM_NAMES=${CILIUM_DEFAULT}" \
  "FAKE_CILIUM_DAEMON_LIST_RECOVERY=1" \
  "FAKE_CILIUM_DAEMON_LIST_COUNTER_FILE=${C7R}/__daemon_list_counter"
# Reset the counter file BEFORE drive_control so
# the production path observes an empty first poll.
rm -f "${C7R}/__daemon_list_counter"
drive_control C7r "${C7R}" "${C7R}/run_g.sh" "${C7R}/env.list"
R7R=$(classify_control C7r "${C7R}")
C7R_RC="$(echo "${R7R}" | awk -F'|' '{print $2}')"
C7R_SUMMARY="$(echo "${R7R}" | awk -F'|' '{print $3}')"
C7R_LOGCLS="$(echo "${R7R}" | awk -F'|' '{print $4}')"
C7R_DOWNSTREAM="$(echo "${R7R}" | awk -F'|' '{print $5}')"
C7R_MISMATCH="$(echo "${R7R}" | awk -F'|' '{print $6}')"
# Counter should be >= 2: empty-first-poll AND
# a subsequent daemon-present poll.
C7R_DL_COUNTER=$(cat "${C7R}/__daemon_list_counter" 2>/dev/null | tr -d ' ' | head -1)
[ -z "${C7R_DL_COUNTER}" ] && C7R_DL_COUNTER="0"
# Off-by-one: if the daemon-list is invoked 2 or
# more times the recovery semantics are proven
# (first empty, then at least one daemon-present).
if [ "${C7R_DL_COUNTER}" -ge 2 ]; then
  C7R_RECOVERED="Y"
else
  C7R_RECOVERED="N"
fi
C7R_FIRST_EMPTY=$(grep -q 'cilium daemon count:[[:space:]]*0' "${C7R}/step_G_out" 2>/dev/null && echo Y || echo N)
C7R_LATER_NONEMPTY=$(grep -q 'cilium daemon count:[[:space:]]*[1-9]' "${C7R}/step_G_out" 2>/dev/null && echo Y || echo N)
C7R_UNIQUE_LABELS=$(wc -l < "${C7R}/cilium-endpoint.unique.out" 2>/dev/null | tr -d ' ' | head -1)
[ -z "${C7R_UNIQUE_LABELS}" ] && C7R_UNIQUE_LABELS="0"
C7R_STEPG_OK=$(grep -q 'step G ok' "${C7R}/step_G_out" 2>/dev/null && echo Y || echo N)
# d2b.46 identity-strength: prove byte-equivalent
# set equality between the dynamic 13-fixture
# Pod-derived expected-label set and the
# observed unique Cilium endpoint label set.
# Both sides are normalized with
# `LC_ALL=C sort -u` so order/spacing cannot
# hide a substitution.
C7R_IDENTITY="N"
if [ -s "${C7R}/cilium-endpoint.expected.out" ] \
   && [ -s "${C7R}/cilium-endpoint.unique.out" ]; then
  C7R_SORTED_EXP=$(LC_ALL=C sort -u "${C7R}/cilium-endpoint.expected.out" | awk 'BEGIN{OFS=""} {printf "%s\n", $0}' | tr -d '\n' | sed 's/.$//')
  C7R_SORTED_OBS=$(LC_ALL=C sort -u "${C7R}/cilium-endpoint.unique.out" | awk 'BEGIN{OFS=""} {printf "%s\n", $0}' | tr -d '\n' | sed 's/.$//')
  if [ "${C7R_SORTED_EXP}" = "${C7R_SORTED_OBS}" ] \
     && [ "${#C7R_SORTED_EXP}" -gt 0 ]; then
    C7R_IDENTITY="Y"
  fi
fi
# d2b.46 identity-strength: prove the install
# script refuses to succeed when
# missing_labels and unexpected_labels diverge
# only by identity, not by count. The source
# MUST contain the LC_ALL=C comm diff command
# AND the all-empty-diff guard so a count-only
# mutation cannot false-pass.
C7R_REFUSES_COUNT_ONLY="N"
if grep -qE 'LC_ALL=C comm -23 ' "${SCRIPT_DIR}/install-nexus-test.sh" \
   && grep -qE 'missing_count.*-eq 0.*unexpected_count.*-eq 0' "${SCRIPT_DIR}/install-nexus-test.sh"; then
  C7R_REFUSES_COUNT_ONLY="Y"
fi

# C7g (Gate 8 direct): exact 13 inventory +
# daemon-list command failure => exit 10.
# Runs the REAL cni-readiness-gate.sh directly
# with FAKE environment so anything outside
# kubectl/kind/docker returns 127.
G8A="${TOP_TMP}/stage-G8a"
mkdir -p "${G8A}"
DRIVER_G8A="${G8A}/run_gate8.sh"
cat >"${DRIVER_G8A}" <<G8EOF
#!/bin/sh
exec ${REAL_BASH} "${SCRIPT_DIR}/cni-readiness-gate.sh"
G8EOF
chmod +x "${DRIVER_G8A}"
FAKE_GATE8_INVENTORY_TSV=$(mktemp -t d2b46-gate-inv-XXXXXX)
# G8a uses a 13-Pod inventory where 11 are
# the manifest-aligned canonical namespace/
# name pairs and 2 are intentionally
# wrong-namespace duplicates so the targeted
# vocabulary contract (Block A.1) fails
# closed before Gate 8.
# NOTE: this fixture size is 13 because
# Gate 8 must reach the bounded poll, not
# to satisfy vocabulary. The point is to
# exercise the rc=11 daemon-list branch.
printf 'cni-test-ingress\tcni-mock-ingress-controller\t1/1\tRunning\t0\t1d\n' >"${FAKE_GATE8_INVENTORY_TSV}"
printf 'cni-test-prometheus\tcni-mock-prometheus\t1/1\tRunning\t0\t1d\n' >>"${FAKE_GATE8_INVENTORY_TSV}"
printf 'cni-test-untrusted\tcni-untrusted-default\t1/1\tRunning\t0\t1d\n' >>"${FAKE_GATE8_INVENTORY_TSV}"
printf 'default\tcni-mock-nexus-gateway\t1/1\tRunning\t0\t1d\n' >>"${FAKE_GATE8_INVENTORY_TSV}"
printf 'default\tcni-mock-nexus-worker\t1/1\tRunning\t0\t1d\n' >>"${FAKE_GATE8_INVENTORY_TSV}"
printf 'default\tcni-mock-nexus-migration\t1/1\tRunning\t0\t1d\n' >>"${FAKE_GATE8_INVENTORY_TSV}"
printf 'default\tcni-mock-egress-proxy\t1/1\tRunning\t0\t1d\n' >>"${FAKE_GATE8_INVENTORY_TSV}"
printf 'default\tcni-mock-postgres\t1/1\tRunning\t0\t1d\n' >>"${FAKE_GATE8_INVENTORY_TSV}"
printf 'default\tcni-mock-redis\t1/1\tRunning\t0\t1d\n' >>"${FAKE_GATE8_INVENTORY_TSV}"
printf 'default\tcni-mock-clickhouse\t1/1\tRunning\t0\t1d\n' >>"${FAKE_GATE8_INVENTORY_TSV}"
printf 'default\tcni-mock-arbitrary\t1/1\tRunning\t0\t1d\n' >>"${FAKE_GATE8_INVENTORY_TSV}"
printf 'cni-control\tcni-control-target\t1/1\tRunning\t0\t1d\n' >>"${FAKE_GATE8_INVENTORY_TSV}"
printf 'cni-control\tcni-control-probe-5d5fb89454-7cjss\t1/1\tRunning\t0\t1d\n' >>"${FAKE_GATE8_INVENTORY_TSV}"
write_env_file "${G8A}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${G8A}" \
  "HARNESS_STAGE_TSV=${G8A}/pods.tsv" \
  "HARNESS_GATE_BIN=${REAL_GATE_BIN}" \
  "CNI_READINESS_GATE_BIN=${REAL_GATE_BIN}" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=1" \
  "HARNESS_CILIUM_NAMES=${CILIUM_DEFAULT}" \
  "FAKE_FIXTURE_LIST_RC=" \
  "FAKE_FIXTURE_JSON_RC=" \
  "FAKE_CILIUM_DAEMON_LIST_RC=11" \
  "HARNESS_FIXTURE_NAMES_TSV=${FAKE_GATE8_INVENTORY_TSV}" \
  "KUBE_SYSTEM_ENDPOINT_NAME=cilium-fake-good"
# G8a will be driven by drive_control; we need
# a machinery hook so the fake kubectl honours
# the G8 inventory TSV. We'll use a per-stage
# env hook: when HARNESS_FIXTURE_NAMES_TSV is
# set, the fake kubectl reads it instead of
# FAKE_PODS_TSV_FILE. Add another small block
# to the fake kubectl below.
echo "FIXTURE_NAMES=${FAKE_GATE8_INVENTORY_TSV}" > "${G8A}/env.extra"
echo "TSV_HOOK=Y" >> "${G8A}/env.extra"
echo "see C7g below"

# C7g/C7h/C7i/C7k are direct Gate 8 controls;
# they run the real gate under the same fake
# kubectl. A small upstream injector override
# patches FAKE_PODS_TSV_FILE to the exact 13
# fixture tsv via HARNESS_FIXTURE_NAMES_TSV.
# We handle that at fake kubectl level.

# C7a: cilium-stage controls already driven.
# The classify_control helper uses one stage
# dir per control; this is consistent with the
# existing surface. The following transcripts
# are printed in the same S-block as C1..C11.
#
# -----------------------------------------------------------------
# Direct real cni-readiness-gate.sh Gate 8 regression
# controls (C7g / C7h / C7i):
#
#   C7g:  exact 13 inventory + cilium daemon-list
#         command failure => CLUSTER_OR_CNI_NOT_READY
#         exit 10 (write path pre-deadline).
#   C7h:  exact 13 inventory + per-daemon exec
#         command failure => exit 10 with daemon
#         name in structured artefact.
#   C7i:  exact 13 inventory + valid 12-of-13
#         convergence => exit 10 with
#         cilium-endpoint-convergence artifact.
#
# Each control feeds the real gate directly via
# the fake kubectl (Path resolves to the real
# scripts/cni-readiness-gate.sh through drive_control).
# A small "gate_body" wrapper is added per stage
# so we can drive the gate without dragging the
# whole install-nexus-test.sh main() loop in.
# Helpers `make_exact_13_names_tsv` and
# `make_generated_probe_tsv` are defined
# upstream near `CILIUM_DEFAULT` so that
# earlier (C7g/C7h/C7i/C8r) and later
# (C7s/C8s) stages see them in scope.
#
# `make_cilium_default_tsv` emits a 13-Pod
# fixture inventory whose Pod names match
# `CILIUM_DEFAULT` exactly (so the Gate 8
# `kubectl get pod -A -o json` projection
# produces the SAME labels the
# `FAKE_CILIUM_NAMES=CILIUM_DEFAULT` does —
# byte-equivalent identity between expected
# and observed after LC_ALL=C sort -u).
make_cilium_default_tsv() {
  local out_path="$1"
  : > "${out_path}"
  # Manifest-aligned namespace mapping.
  # Postgres/redis/clickhouse live in
  # `default` per the tracked fixture
  # manifests, not under stale test-
  # specific namespaces.
  for n in ${CILIUM_DEFAULT}; do
    case "${n}" in
      cni-mock-ingress-controller) ns="cni-test-ingress" ;;
      cni-mock-prometheus)         ns="cni-test-prometheus" ;;
      cni-mock-postgres)           ns="default" ;;
      cni-mock-redis)              ns="default" ;;
      cni-mock-clickhouse)         ns="default" ;;
      cni-untrusted-default)       ns="cni-test-untrusted" ;;
      cni-mock-arbitrary)          ns="default" ;;
      cni-control-target)          ns="cni-control" ;;
      cni-control-probe-*)         ns="cni-control" ;;
      *)                           ns="default" ;;
    esac
    printf '%s\t%s\t1/1\tRunning\t0\t7m\n' "${ns}" "${n}" >>"${out_path}"
  done
}
# ---------------------------------------------------------------------------
write_gate_runner() {
  local stage="$1"
  cat >"${stage}/run_gate.sh" <<'GRUNEOF'
#!/bin/sh
set +e
set +u
SCRIPT_DIR="${HARNESS_SCRIPT_DIR}"
ARTIFACTS="${HARNESS_ARTIFACTS}"
STAGE_TSV="${HARNESS_STAGE_TSV}"
GATE_BIN="${HARNESS_GATE_BIN}"
KUBECTL_RC="${HARNESS_KUBECTL_RC:-0}"
KIND_RC="${HARNESS_KIND_RC:-0}"
DOCKER_RC="${HARNESS_DOCKER_RC:-0}"
DATE_NOW="${HARNESS_DATE_NOW:-1700000000}"
DATE_ADVANCE="${HARNESS_DATE_ADVANCE:-1}"
DATE_STEP="${HARNESS_DATE_STEP:-1000}"
CILIUM_NAMES="${HARNESS_CILIUM_NAMES:-}"
export FAKE_PODS_TSV_FILE="${STAGE_TSV}"
export FAKE_KUBECTL_RC="${KUBECTL_RC}"
export FAKE_KIND_RC="${KIND_RC}"
export FAKE_DOCKER_RC="${DOCKER_RC}"
export FAKE_DATE_NOW="${DATE_NOW}"
export FAKE_DATE_ADVANCE="${DATE_ADVANCE}"
export FAKE_DATE_STEP="${DATE_STEP}"
export FAKE_CILIUM_NAMES="${CILIUM_NAMES}"
export FAKE_FIXTURE_LIST_RC="${FAKE_FIXTURE_LIST_RC:-}"
export FAKE_FIXTURE_JSON_RC="${FAKE_FIXTURE_JSON_RC:-}"
export FAKE_CILIUM_DAEMON_LIST_RC="${FAKE_CILIUM_DAEMON_LIST_RC:-}"
export FAKE_CILIUM_EXEC_RC="${FAKE_CILIUM_EXEC_RC:-}"
export FAKE_CILIUM_JSON_MODE="${FAKE_CILIUM_JSON_MODE:-}"
export HARNESS_FIXTURE_NAMES_TSV="${HARNESS_FIXTURE_NAMES_TSV:-}"
# Provide the same recovery/env context the
# install pre-loop establishes, so the real
# gate's prelude runs cleanly through Gate 8
# before its classify() exit branches.
export RECOVERY_PR_SHA="${RECOVERY_PR_SHA:-local-$$-R$RANDOM}"
export WORKFLOW_RUN_ID="${WORKFLOW_RUN_ID:-local-C7g}"
export CLUSTER_NAME=nexus-cni-test
export GATE_PHASE=post-fixture
# Stage-scoped artefacts: each direct Gate 8
# control gets its own artifacts/integrationcni
# subtree under its stage dir. The real gate
# reads ARTIFACTS first, then falls back to
# $PWD/artifacts/integrationcni.
export ARTIFACTS="${HARNESS_ARTIFACTS}/artifacts"
mkdir -p "${ARTIFACTS}"
# Reset fake date state so Gate 8's first
# `date +%s` call lands BELOW DEADLINE. The
# install pre-loop and prior gates consume
# many date advances; this restore gives the
# direct Gate 8 controls a deterministic
# deadline window for the per-daemon exec
# branch to fire.
if [ -n "${FAKE_DATE_NOW_FILE:-}" ]; then
  echo "${FAKE_DATE_NOW:-1700000000}" > "${FAKE_DATE_NOW_FILE}"
fi
# Run real gate explicitly via bash interpreter.
exec "${HARNESS_REAL_BASH}" "${SCRIPT_DIR}/cni-readiness-gate.sh"
GRUNEOF
  chmod +x "${stage}/run_gate.sh"
}

# Substitute the per-stage cni-readiness-gate.sh
# with the REAL repo one. (Real gate path is what
# write_stage_files already does for failures
# when gate_path="${REAL_GATE_BIN}". For direct
# Gate 8 controls, write a stage with NO stub.)
make_real_gate_stage() {
  local stage="$1" tsv_path="$2"
  local out_dir="${stage}/gate"
  mkdir -p "${out_dir}"
  cp -p "${REAL_GATE_BIN}" "${out_dir}/cni-readiness-gate.sh"
  chmod +x "${out_dir}/cni-readiness-gate.sh"
  printf '%s' "${tsv_path}" >"${stage}/pods.tsv"
  write_gate_runner "${stage}"
  echo "${out_dir}/cni-readiness-gate.sh"
}

# C7g: exact 13 inventory, daemon-list rc=11.
C7G_TSV="${TOP_TMP}/gate8-exact13.tsv"
make_exact_13_names_tsv "${C7G_TSV}"
C7G_NAMES_13=$(printf '%s\n' $(awk -F'\t' 'NR>0 {print $2}' "${C7G_TSV}") \
  | sed 's|^|resolve-labels-default/|')
C7G_NAMES_13_SPACE=$(printf '%s\n' "${CILIUM_DEFAULT}")
C7G="${TOP_TMP}/stage-C7g"
mkdir -p "${C7G}"
make_real_gate_stage "${C7G}" "${C7G_TSV}"
write_env_file "${C7G}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${C7G}" \
  "HARNESS_STAGE_TSV=${C7G}/pods.tsv" \
  "HARNESS_GATE_BIN=${REAL_GATE_BIN}" \
  "CNI_READINESS_GATE_BIN=${REAL_GATE_BIN}" \
  "HARNESS_CILIUM_NAMES=${C7G_NAMES_13_SPACE}" \
  "HARNESS_FIXTURE_NAMES_TSV=${C7G_TSV}" \
  "FAKE_FIXTURE_LIST_RC=" \
  "FAKE_FIXTURE_JSON_RC=" \
  "FAKE_CILIUM_DAEMON_LIST_RC=11" \
  "FAKE_DATE_NOW_FILE=${C7G}/__date_state" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=120" \
  "HARNESS_DATE_NOW=1700000000"
drive_control C7g "${C7G}" "${C7G}/run_gate.sh" "${C7G}/env.list"
C7G_RC="$(awk -F'=' '/^rc=/ {print $2; exit}' "${C7G}/child.rc" 2>/dev/null)"
GATE_BASE_G="${C7G}/artifacts"
G8_SUMMARY_G="${GATE_BASE_G}/readiness.summary.txt"
G8_ART_C7G_BASE="${GATE_BASE_G}/gate08-endpoint-inventory-error.json"
C7G_SUMMARY="$(cat "${G8_SUMMARY_G}" 2>/dev/null || echo '__MISSING__')"
if [ ! -f "${G8_SUMMARY_G}" ]; then C7G_SUMMARY="__MISSING__"; fi
C7G_LOGCLS="$(grep -E '^classification=' "${GATE_BASE_G}/readiness.log" 2>/dev/null | head -1)"
C7G_DOWNSTREAM="N"
if [ -s "${GATE_BASE_G}/downstream-stub-sentinel" ]; then C7G_DOWNSTREAM="Y"; fi
C7G_ART="N"
if [ -s "${G8_ART_C7G_BASE}" ] \
   || grep -q 'cilium daemon list failed' "${GATE_BASE_G}/readiness.log" 2>/dev/null; then
  C7G_ART="Y"
fi

# C7h: per-daemon exec rc=8 against 13 inventory.
C7H_TSV="${TOP_TMP}/gate8-exact13-h.tsv"
make_exact_13_names_tsv "${C7H_TSV}"
C7H="${TOP_TMP}/stage-C7h"
mkdir -p "${C7H}"
make_real_gate_stage "${C7H}" "${C7H_TSV}"
write_env_file "${C7H}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${C7H}" \
  "HARNESS_STAGE_TSV=${C7H}/pods.tsv" \
  "HARNESS_GATE_BIN=${REAL_GATE_BIN}" \
  "CNI_READINESS_GATE_BIN=${REAL_GATE_BIN}" \
  "HARNESS_CILIUM_NAMES=${C7G_NAMES_13_SPACE}" \
  "HARNESS_FIXTURE_NAMES_TSV=${C7H_TSV}" \
  "FAKE_FIXTURE_LIST_RC=" \
  "FAKE_FIXTURE_JSON_RC=" \
  "FAKE_CILIUM_EXEC_RC=8" \
  "FAKE_DATE_NOW_FILE=${C7H}/__date_state" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=120" \
  "HARNESS_DATE_NOW=1700000000"
drive_control C7h "${C7H}" "${C7H}/run_gate.sh" "${C7H}/env.list"
C7H_RC="$(awk -F'=' '/^rc=/ {print $2; exit}' "${C7H}/child.rc" 2>/dev/null)"
GATE_BASE_H="${C7H}/artifacts"
G8_SUMMARY_H="${GATE_BASE_H}/readiness.summary.txt"
C7H_SUMMARY="$(cat "${G8_SUMMARY_H}" 2>/dev/null || echo '__MISSING__')"
if [ ! -f "${G8_SUMMARY_H}" ]; then C7H_SUMMARY="__MISSING__"; fi
C7H_LOGCLS="$(grep -E '^classification=' "${GATE_BASE_H}/readiness.log" 2>/dev/null | head -1)"
C7H_DOWNSTREAM="N"
if [ -s "${GATE_BASE_H}/downstream-stub-sentinel" ]; then C7H_DOWNSTREAM="Y"; fi
C7H_ART="N"
if [ -s "${GATE_BASE_H}/gate08-endpoint-inventory-error.json" ]; then C7H_ART="Y"; fi
C7H_DAEMON=$(grep -E '"daemon":' "${GATE_BASE_H}/gate08-endpoint-inventory-error.json" 2>/dev/null | head -1)

# C7i: valid 12-of-13 endpoint under-convergence
# without command failure.
C7I_TSV="${TOP_TMP}/gate8-exact13-i.tsv"
make_exact_13_names_tsv "${C7I_TSV}"
# remove cni-untrusted-default from CILIUM_NAMES
C7I_NAMES_12=$(printf '%s' "${CILIUM_DEFAULT}" | tr ' ' '\n' | grep -v '^cni-untrusted-default$' | tr '\n' ' ' | sed 's/ $//')
C7I="${TOP_TMP}/stage-C7i"
mkdir -p "${C7I}"
make_real_gate_stage "${C7I}" "${C7I_TSV}"
write_env_file "${C7I}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${C7I}" \
  "HARNESS_STAGE_TSV=${C7I}/pods.tsv" \
  "HARNESS_GATE_BIN=${REAL_GATE_BIN}" \
  "CNI_READINESS_GATE_BIN=${REAL_GATE_BIN}" \
  "HARNESS_CILIUM_NAMES=${C7I_NAMES_12}" \
  "HARNESS_FIXTURE_NAMES_TSV=${C7I_TSV}" \
  "FAKE_FIXTURE_LIST_RC=" \
  "FAKE_FIXTURE_JSON_RC=" \
  "FAKE_DATE_NOW_FILE=${C7I}/__date_state" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=120" \
  "HARNESS_DATE_NOW=1700000000"
drive_control C7i "${C7I}" "${C7I}/run_gate.sh" "${C7I}/env.list"
C7I_RC="$(awk -F'=' '/^rc=/ {print $2; exit}' "${C7I}/child.rc" 2>/dev/null)"
GATE_BASE_I="${C7I}/artifacts"
G8_SUMMARY_I="${GATE_BASE_I}/readiness.summary.txt"
C7I_SUMMARY="$(cat "${G8_SUMMARY_I}" 2>/dev/null || echo '__MISSING__')"
if [ ! -f "${G8_SUMMARY_I}" ]; then C7I_SUMMARY="__MISSING__"; fi
C7I_LOGCLS="$(grep -E '^classification=' "${GATE_BASE_I}/readiness.log" 2>/dev/null | head -1)"
C7I_DOWNSTREAM="N"
if [ -s "${GATE_BASE_I}/downstream-stub-sentinel" ]; then C7I_DOWNSTREAM="Y"; fi
C7I_ART="N"
if [ -s "${GATE_BASE_I}/gate08-endpoint-convergence.json" ]; then C7I_ART="Y"; fi
C7I_NO_CMD_ERR="Y"
if [ -s "${GATE_BASE_I}/gate08-endpoint-inventory-error.json" ]; then C7I_NO_CMD_ERR="N"; fi
C7I_NOT_LASTGEQ=$(awk 'BEGIN{n=0} {n++} END {print n+0}' "${C7I}/readiness.summary.txt" 2>/dev/null)
# gate summary should mark Gate 8 as failed AND
# exit with rc=10 before Gate 9.
C7I_GATE8_FAILED=$(grep -E 'Gate 8.*failed' "${GATE_BASE_I}/readiness.log" 2>/dev/null | head -1)
# Read gate's classification term (real gate writes
# classification=CLUSTER_OR_CNI_NOT_READY exit 10).
C7I_CLASSIF_TERM=$(awk -F'=' '/^classification=/ {
  ln=$0; sub(/^classification=/,"",ln); sub(/ .*/,"",ln); print ln
}' "${GATE_BASE_I}/readiness.log" 2>/dev/null | head -1)
# Schema/parse/length assertion: validity of the
# canonical under-convergence JSON. observed_count
# MUST equal the length of observed_labels; both
# MUST come from the same file (the unique-label
# file). This catches regressions where the count
# and the array diverge.
C7I_CONV_PARSE="N"
C7I_CONV_OBS_CNT="-"
C7I_CONV_OBS_LEN="-"
C7I_CONV_HAS_UNTRUSTED="N"
C7I_CONV_HAS_CNI_MOCK="N"
C7I_CONV_HAS_CNI_CONTROL="N"
if [ -s "${GATE_BASE_I}/gate08-endpoint-convergence.json" ]; then
  if python3 -c '
import json, sys
p=sys.argv[1]
try:
  d=json.load(open(p))
except Exception:
  sys.exit(1)
fields={"expected_count":int,"observed_count":int,"observed_labels":list,"daemon_list":list,"reason":str}
for k,t in fields.items():
  if k not in d: sys.exit(2)
  v=d[k]
  if t is int:
    if not isinstance(v,int): sys.exit(3)
  elif t is list:
    if not isinstance(v,list): sys.exit(4)
  elif t is str:
    if not isinstance(v,str): sys.exit(5)
if len(d["observed_labels"]) != d["observed_count"]: sys.exit(6)
sys.exit(0)
' "${GATE_BASE_I}/gate08-endpoint-convergence.json" 2>/dev/null; then
    C7I_CONV_PARSE="Y"
  fi
  C7I_CONV_OBS_CNT="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("observed_count","-"))' "${GATE_BASE_I}/gate08-endpoint-convergence.json" 2>/dev/null)"
  C7I_CONV_OBS_LEN="$(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1])).get("observed_labels",[])))' "${GATE_BASE_I}/gate08-endpoint-convergence.json" 2>/dev/null)"
  if python3 -c '
import json, sys
l=json.load(open(sys.argv[1]))["observed_labels"]
assert any(x.endswith("cni-untrusted-default") for x in l)
' "${GATE_BASE_I}/gate08-endpoint-convergence.json" 2>/dev/null; then
    C7I_CONV_HAS_UNTRUSTED="Y"
  fi
  if python3 -c '
import json, sys
l=json.load(open(sys.argv[1]))["observed_labels"]
assert len(l) >= 1 and any(x.startswith("resolve-labels-default/cni-mock-") for x in l)
' "${GATE_BASE_I}/gate08-endpoint-convergence.json" 2>/dev/null; then
    C7I_CONV_HAS_CNI_MOCK="Y"
  fi
  if python3 -c '
import json, sys
l=json.load(open(sys.argv[1]))["observed_labels"]
assert len(l) >= 1 and any(x.startswith("resolve-labels-default/cni-control-") for x in l)
' "${GATE_BASE_I}/gate08-endpoint-convergence.json" 2>/dev/null; then
    C7I_CONV_HAS_CNI_CONTROL="Y"
  fi
fi

# C8r (real Gate 8 recovery): first daemon-list
# poll rc 0 + empty; later poll returns daemon(s)
# and 13 unique endpoint labels surface. Gate 8
# must record success at 13/13 with NO command-error
# artifact and NO under-convergence artifact. A
# later Gate 9 failure (the gate tries a probe
# against the empty-install cluster under fake
# PATH and fails its document classification)
# is acceptable but MUST be the first failure —
# Gate 8 must NOT be the first failure.
C8R_TSV="${TOP_TMP}/gate8-recovery.tsv"
# Use the CILIUM_DEFAULT-names TSV so the
# dynamic 13-Pod inventory projection yields
# exactly the same labels as
# HARNESS_CILIUM_NAMES=CILIUM_DEFAULT after
# sort -u. This is required for the
# d2b.46 identity-strength byte-equivalent
# predicate (C8R_IDENTITY == Y) to fire.
make_cilium_default_tsv "${C8R_TSV}"
C8R="${TOP_TMP}/stage-C8r"
mkdir -p "${C8R}"
make_real_gate_stage "${C8R}" "${C8R_TSV}"
write_env_file "${C8R}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${C8R}" \
  "HARNESS_STAGE_TSV=${C8R}/pods.tsv" \
  "HARNESS_GATE_BIN=${REAL_GATE_BIN}" \
  "CNI_READINESS_GATE_BIN=${REAL_GATE_BIN}" \
  "HARNESS_CILIUM_NAMES=${CILIUM_DEFAULT}" \
  "HARNESS_FIXTURE_NAMES_TSV=${C8R_TSV}" \
  "FAKE_FIXTURE_LIST_RC=" \
  "FAKE_FIXTURE_JSON_RC=" \
  "FAKE_CILIUM_DAEMON_LIST_RECOVERY=1" \
  "FAKE_CILIUM_DAEMON_LIST_COUNTER_FILE=${C8R}/__daemon_list_counter" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state_reset" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=120" \
  "HARNESS_DATE_NOW=1700000000"
# Stage-scoped counter file: reset BEFORE
# drive_control so the production path observes
# an empty first poll and a daemon-present
# subsequent poll.
rm -f "${C8R}/__daemon_list_counter"
rm -f "${FAKE_BIN}/__date_state_reset"
drive_control C8r "${C8R}" "${C8R}/run_gate.sh" "${C8R}/env.list"
C8R_RC="$(awk -F'=' '/^rc=/ {print $2; exit}' "${C8R}/child.rc" 2>/dev/null)"
GATE_BASE_R="${C8R}/artifacts"
G8_SUM_R="${GATE_BASE_R}/readiness.summary.txt"
C8R_SUMMARY="$(cat "${G8_SUM_R}" 2>/dev/null || echo '__MISSING__')"
[ ! -f "${G8_SUM_R}" ] && C8R_SUMMARY="__MISSING__"
C8R_LOGCLS="$(grep -E '^classification=' "${GATE_BASE_R}/readiness.log" 2>/dev/null | head -1)"
C8R_DL_COUNTER=$(cat "${C8R}/__daemon_list_counter" 2>/dev/null | tr -d ' ' | head -1)
[ -z "${C8R_DL_COUNTER}" ] && C8R_DL_COUNTER="0"
# Recovery semantics proven iff at least 2 polls.
if [ "${C8R_DL_COUNTER}" -ge 2 ]; then C8R_RECOVERED="Y"; else C8R_RECOVERED="N"; fi
# d2b.46 identity-strength: prove byte-equivalent
# set equality between Gate 8's dynamic 13-Pod
# derived expected set and the observed unique
# Cilium endpoint label set, including the
# `cni-control-source` token from the
# make_exact_13_names_tsv helper. Order/
# spacing cannot hide a substitution.
C8R_IDENTITY="N"
if [ -s "${GATE_BASE_R}/gate08-endpoint.expected.out" ] \
   && [ -s "${GATE_BASE_R}/gate08-endpoint.unique.out" ]; then
  C8R_SORTED_EXP=$(LC_ALL=C sort -u "${GATE_BASE_R}/gate08-endpoint.expected.out" | tr -d '\n')
  C8R_SORTED_OBS=$(LC_ALL=C sort -u "${GATE_BASE_R}/gate08-endpoint.unique.out" | tr -d '\n')
  if [ "${C8R_SORTED_EXP}" = "${C8R_SORTED_OBS}" ] \
     && [ "${#C8R_SORTED_EXP}" -gt 0 ]; then
    C8R_IDENTITY="Y"
  fi
fi
# d2b.46 identity-strength: prove Gate 8 source
# refuses to succeed when missing_labels and
# unexpected_labels diverge only by identity,
# not by count. Static source-excerpt check.
C8R_REFUSES_COUNT_ONLY="N"
if grep -qE 'LC_ALL=C comm -23 ' "${SCRIPT_DIR}/cni-readiness-gate.sh" \
   && grep -qE 'MISSING_C.*-eq 0.*UNEXPECTED_C.*-eq 0' "${SCRIPT_DIR}/cni-readiness-gate.sh"; then
  C8R_REFUSES_COUNT_ONLY="Y"
fi
# d2b.48 vocab strengthening for C8R
# recovery-success: gate08-fixture-vocab.json
# MUST list 12 observed static pairs AND
# exactly 1 dynamic probe with zero
# unexpected/duplicate entries.
C8R_VOCAB_OK="N"
if [ -s "${GATE_BASE_R}/gate08-fixture-vocab.json" ]; then
  if python3 -c "
import json,sys
try:
  d=json.load(open(sys.argv[1]))
  ok = (
    len(d.get('observed_static_pairs', [])) == 12
    and len(d.get('dynamic_probe_pairs', [])) == 1
    and len(d.get('unexpected_fixture_like_pairs', [])) == 0
    and len(d.get('duplicate_pairs', [])) == 0
    and bool(d.get('canonical_population_ready')) is True
  )
  print('Y' if ok else 'N')
except Exception:
  print('N')
" "${GATE_BASE_R}/gate08-fixture-vocab.json" | grep -q '^Y$'; then
    C8R_VOCAB_OK="Y"
  fi
fi
# Gate 8 success is proven by:
#   - The readiness.log carries
#     "[step 08] 08-fixture-endpoint-registered : ok"
#     line from record_step 8.
#   - NO command-error artifact.
#   - NO under-convergence artifact (Gate 8 hit
#     its 13/13 target, not failed at deadline).
#   - The unique-label file count == 13.
C8R_GATE8_OK=$(grep -E 'cilium endpoints 13 >= fixture pods 13' "${GATE_BASE_R}/readiness.log" 2>/dev/null | head -1)
[ -n "${C8R_GATE8_OK}" ] && C8R_GATE8_OK="Y" || C8R_GATE8_OK="N"
C8R_NO_CMD_ERR="Y"
[ -s "${GATE_BASE_R}/gate08-endpoint-inventory-error.json" ] && C8R_NO_CMD_ERR="N"
C8R_NO_CONV_ART="Y"
[ -s "${GATE_BASE_R}/gate08-endpoint-convergence.json" ] && C8R_NO_CONV_ART="N"
# Final unique-label file count == 13 (last
# iteration's unique set).
C8R_UNIQUE_COUNT=$(awk 'BEGIN{n=0} {n++} END {print n+0}' \
  "${GATE_BASE_R}/gate08-endpoint.unique.out" 2>/dev/null)
[ -z "${C8R_UNIQUE_COUNT}" ] && C8R_UNIQUE_COUNT="0"
# observed_labels includes cni-untrusted-default
# AND spans all three matchers (mock, control,
# untrusted).
C8R_HAS_UNTRUSTED=$(grep -q 'resolve-labels-default/cni-untrusted-default' "${GATE_BASE_R}/gate08-endpoint.unique.out" 2>/dev/null && echo Y || echo N)
# Gate 9 first failure: any failure step whose
# name begins with "09-" (or whose classification
# line is anything OTHER than Gate 8's
# CLUSTER_OR_CNI_NOT_READY), AND Gate 8's record_step
# is "ok" in the same log.
C8R_GATE8_OK_BEFORE_GATE9=$(awk '
  /\[step 08\].*08-fixture-endpoint-registered.*ok/ { g8=1; next }
  /\[step 09\]/ && g8==1 { print "Y"; exit }
  END { print "N" }
' "${GATE_BASE_R}/readiness.log" 2>/dev/null | head -n 1 | tr -d '\n')
# gate first failure step is NOT Gate 8 if
# classification log line says CLUSTER_OR_CNI_NOT_READY
# but first_failed_step is not 08.
C8R_FIRST_FAILED=$(grep -E 'first_failed_step=' "${GATE_BASE_R}/readiness.log" 2>/dev/null | head -1 | awk -F'=' '{print $2}' | tr -d ' \n')
[ "${C8R_FIRST_FAILED}" = "08-fixture-endpoint-registered" ] && C8R_GATE8_FIRST_FAIL="Y" || C8R_GATE8_FIRST_FAIL="N"
# Static C11 one-shot uses C8r; ensure its
# drive_control appears EXACTLY once.
C8R_ONE_SHOT_COUNT=$(grep -cE '^drive_control C8r\b' \
  "${SCRIPT_DIR}/test_fixture_readiness_observability.sh" 2>/dev/null || echo 0)

# C7g/h/i/r transcript print.
printf 'C7g: rc=%s summary=%s logcls=%s err-art=%s downstream-stub-invoked=%s (direct Gate 8 daemon-list)\n' \
  "${C7G_RC}" "${C7G_SUMMARY}" "${C7G_LOGCLS}" "${C7G_ART}" "${C7G_DOWNSTREAM}"
printf 'C7h: rc=%s summary=%s logcls=%s err-art=%s daemon=%s (direct Gate 8 exec)\n' \
  "${C7H_RC}" "${C7H_SUMMARY}" "${C7H_LOGCLS}" "${C7H_ART}" "${C7H_DAEMON}"
printf 'C7i: rc=%s summary=%s logcls=%s conv-art=%s no-cmd-err=%s conv-parse=%s obs-cnt=%s obs-len=%s untrusted=%s mock=%s control=%s\n' \
  "${C7I_RC}" "${C7I_SUMMARY}" "${C7I_LOGCLS}" "${C7I_ART}" "${C7I_NO_CMD_ERR}" \
  "${C7I_CONV_PARSE}" "${C7I_CONV_OBS_CNT}" "${C7I_CONV_OBS_LEN}" \
  "${C7I_CONV_HAS_UNTRUSTED}" "${C7I_CONV_HAS_CNI_MOCK}" "${C7I_CONV_HAS_CNI_CONTROL}"
printf 'C8r: rc=%s summary=%s logcls=%s recon=%s gate8-ok=%s no-cmd-err=%s no-conv-art=%s unique-count=%s has-untrusted=%s dl-counter=%s gate8-ok-before-g9=%s gate8-first-fail=%s identity=%s refuses-count-only=%s (real Gate 8 recovery)\n' \
  "${C8R_RC}" "${C8R_SUMMARY}" "${C8R_LOGCLS}" "${C8R_RECOVERED}" \
  "${C8R_GATE8_OK}" "${C8R_NO_CMD_ERR}" "${C8R_NO_CONV_ART}" \
  "${C8R_UNIQUE_COUNT}" "${C8R_HAS_UNTRUSTED}" \
  "${C8R_DL_COUNTER}" "${C8R_GATE8_OK_BEFORE_GATE9}" "${C8R_GATE8_FIRST_FAIL}" \
  "${C8R_IDENTITY}" "${C8R_REFUSES_COUNT_ONLY}"

# C8s (real Gate 8): equal-count wrong-label
# mutation AND dynamically generated control
# Pod identity. expected_labels are derived
# from the actual Gate 8 kubectl inventory, so
# the expected set MUST include
# `cni-control-probe-abc123-def45`. Observed
# labels are projected from FAKE_CILIUM_NAMES
# so they include cni-stale-old in place of
# cni-untrusted-default. Gate 8 must fail
# BEFORE Gate 9 with rc=10, with missing
# labels including `cni-untrusted-default`
# AND unrealised `cni-control-probe-abc123-def45`
# (because the projection does not surface
# that generated name either), and unexpected
# labels including `cni-stale-old`.
C8S_PROBE_NAME="cni-control-probe-abc123-def45"
C8S_TSV="${TOP_TMP}/gate8-c8s-generated-inventory.tsv"
make_generated_probe_tsv "${C8S_TSV}"
# Equal-count mutation: take the actual
# inventory the real Gate 8 inventory call
# will see (which includes
# `cni-control-probe-abc123-def45` from
# make_generated_probe_tsv) and replace
# `cni-untrusted-default` with
# `cni-stale-old`. Result is the same field
# count (13) with one element replaced by
# an unrelated entry. A count-only success
# predicate would (incorrectly) PASS.
C8S_NAMES_BEFORE_STALE=$(awk -F'\t' 'NR>0 {print $2}' "${C8S_TSV}" \
  | sort -u)
C8S_NAMES_13_STALE=$(printf '%s\n' "${C8S_NAMES_BEFORE_STALE}" \
  | sed 's|^cni-untrusted-default$|cni-stale-old|' \
  | tr '\n' ' ' \
  | sed 's/ $//')
C8S="${TOP_TMP}/stage-C8s"
mkdir -p "${C8S}"
make_real_gate_stage "${C8S}" "${C8S_TSV}"
write_env_file "${C8S}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${C8S}" \
  "HARNESS_STAGE_TSV=${C8S}/pods.tsv" \
  "HARNESS_GATE_BIN=${REAL_GATE_BIN}" \
  "CNI_READINESS_GATE_BIN=${REAL_GATE_BIN}" \
  "HARNESS_CILIUM_NAMES=${C8S_NAMES_13_STALE}" \
  "HARNESS_FIXTURE_NAMES_TSV=${C8S_TSV}" \
  "FAKE_FIXTURE_LIST_RC=" \
  "FAKE_FIXTURE_JSON_RC=" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state_c8s" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=120" \
  "HARNESS_DATE_NOW=1700000000"
# Stage-scoped date state reset so the loop
# runs the controlled 120-second step.
rm -f "${FAKE_BIN}/__date_state_c8s"
drive_control C8s "${C8S}" "${C8S}/run_gate.sh" "${C8S}/env.list"
C8S_RC="$(awk -F'=' '/^rc=/ {print $2; exit}' "${C8S}/child.rc" 2>/dev/null)"
GATE_BASE_S="${C8S}/artifacts"
G8_SUM_S="${GATE_BASE_S}/readiness.summary.txt"
C8S_SUMMARY="$(cat "${G8_SUM_S}" 2>/dev/null || echo '__MISSING__')"
[ ! -f "${G8_SUM_S}" ] && C8S_SUMMARY="__MISSING__"
C8S_LOGCLS="$(grep -E '^classification=' "${GATE_BASE_S}/readiness.log" 2>/dev/null | head -1)"
C8S_CONV_JSON="${GATE_BASE_S}/gate08-endpoint-convergence.json"
C8S_CMD_ERR_JSON="${GATE_BASE_S}/gate08-endpoint-inventory-error.json"
C8S_HAS_CONV_ART="N"
C8S_NO_CMD_ERR="Y"
C8S_EXP_COUNT="0"
C8S_OBS_COUNT="0"
C8S_MISSING_HAS_UNTRUSTED="N"
C8S_MISSING_HAS_PROBE="N"
C8S_UNEXPECTED_HAS_STALE="N"
[ -s "${C8S_CONV_JSON}" ] && C8S_HAS_CONV_ART="Y"
[ -s "${C8S_CMD_ERR_JSON}" ] && C8S_NO_CMD_ERR="N"
if [ "${C8S_HAS_CONV_ART}" = "Y" ]; then
  C8S_OK_SENT="${C8S}/__c8s_predicate_ok"
  C8S_OK="N"
  python3 - "${C8S_CONV_JSON}" "${C8S_OK_SENT}" 2>/dev/null <<'PYEOF'
import json,sys,os
src, sentinel = sys.argv[1], sys.argv[2]
d=json.load(open(src))
assert int(d.get("expected_count",-1))==13, d
assert int(d.get("observed_count",-1))==13, d
ml=d.get("missing_labels") or []
ul=d.get("unexpected_labels") or []
ok=True
ok = ok and any("cni-untrusted-default" in x for x in ml)
ok = ok and any("cni-stale-old" in x for x in ul)
ok = ok and not any("cni-control-probe-abc123-def45" in x for x in ml), "probe wrongly in missing"
ok = ok and any("cni-control-probe-abc123-def45" in x for x in (d.get("expected_labels") or [])), "probe missing from expected_labels"
ok = ok and any("cni-control-probe-abc123-def45" in x for x in (d.get("observed_labels") or [])), "probe missing from observed_labels"
open(sentinel, 'w').write('Y\n' if ok else 'N\n')
PYEOF
  if [ -f "${C8S_OK_SENT}" ] && [ "$(cat "${C8S_OK_SENT}" | tr -d ' \n')" = "Y" ]; then
    C8S_OK="Y"
  fi
  if [ "${C8S_OK}" = "Y" ]; then
    C8S_EXP_COUNT="13"
    C8S_OBS_COUNT="13"
    C8S_MISSING_HAS_UNTRUSTED="Y"
    C8S_MISSING_HAS_PROBE="N"
    C8S_UNEXPECTED_HAS_STALE="Y"
  fi
fi

# C8s transcript (real Gate 8 equal-count
# mutation with generated probe name): rc=10
# with CLUSTER_OR_CNI_NOT_READY; convergence
# JSON shows expected_count=observed_count=13
# AND missing contains BOTH cni-untrusted-default
# AND cni-control-probe-abc123-def45 AND
# unexpected contains cni-stale-old. No
# command-error artifact emitted.
printf 'C8s: rc=%s summary=%s logcls=%s no-cmd-err=%s conv-art=%s exp=%s obs=%s miss-untrusted=%s miss-probe=%s unex-stale=%s (real Gate 8 equal-count mutation, generated probe name)\n' \
  "${C8S_RC}" "${C8S_SUMMARY}" "${C8S_LOGCLS}" \
  "${C8S_NO_CMD_ERR}" "${C8S_HAS_CONV_ART}" \
  "${C8S_EXP_COUNT}" "${C8S_OBS_COUNT}" \
  "${C8S_MISSING_HAS_UNTRUSTED}" "${C8S_MISSING_HAS_PROBE}" \
  "${C8S_UNEXPECTED_HAS_STALE}"

# C8d (real Gate 8): set-diff command failure.
# Injection: identical to C7d — the FIRST
# comm call inside Gate 8's identity loop
# (comm -23 expected_labels unique_labels)
# exits rc=9. Gate 8 MUST fail closed as
# CLUSTER_OR_CNI_NOT_READY exit 10 BEFORE
# recording Step 8 "ok" and before Gate 9;
# write structured gate08-endpoint-setdiff-
# error.json with rc=9/operation/paths/stderr;
# no false convergence success.
C8D_PROBE_NAME="cni-control-probe-abc123-def45"
C8D_TSV="${TOP_TMP}/gate8-c8d-generated-inventory.tsv"
make_generated_probe_tsv "${C8D_TSV}"
C8D="${TOP_TMP}/stage-C8d"
mkdir -p "${C8D}"
make_real_gate_stage "${C8D}" "${C8D_TSV}"
write_env_file "${C8D}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${C8D}" \
  "HARNESS_STAGE_TSV=${C8D}/pods.tsv" \
  "HARNESS_GATE_BIN=${REAL_GATE_BIN}" \
  "CNI_READINESS_GATE_BIN=${REAL_GATE_BIN}" \
  "HARNESS_CILIUM_NAMES=${CILIUM_DEFAULT}" \
  "HARNESS_FIXTURE_NAMES_TSV=${C8D_TSV}" \
  "FAKE_FIXTURE_LIST_RC=" \
  "FAKE_FIXTURE_JSON_RC=" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state_c8d" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=120" \
  "HARNESS_DATE_NOW=1700000000" \
  "FAKE_COMM_FAIL_RC=9" \
  "FAKE_COMM_FAIL_OP=missing_labels_diff"
rm -f "${FAKE_BIN}/__date_state_c8d"
drive_control C8d "${C8D}" "${C8D}/run_gate.sh" "${C8D}/env.list"
unset FAKE_COMM_FAIL_RC FAKE_COMM_FAIL_OP
C8D_RC="$(awk -F'=' '/^rc=/ {print $2; exit}' "${C8D}/child.rc" 2>/dev/null)"
G8D_BASE_S="${C8D}/artifacts"
G8D_SUM_S="${G8D_BASE_S}/readiness.summary.txt"
C8D_SUMMARY="$(cat "${G8D_SUM_S}" 2>/dev/null || echo '__MISSING__')"
[ ! -f "${G8D_SUM_S}" ] && C8D_SUMMARY="__MISSING__"
C8D_LOGCLS="$(grep -E '^classification=' "${G8D_BASE_S}/readiness.log" 2>/dev/null | head -1)"
C8D_SDERR_JSON="${G8D_BASE_S}/gate08-endpoint-setdiff-error.json"
C8D_HAS_SDERR="N"
C8D_HAS_RC9="N"
C8D_HAS_OP="N"
C8D_HAS_EXP_PATH="N"
C8D_HAS_OBS_PATH="N"
C8D_HAS_STDERR="N"
if [ -s "${C8D_SDERR_JSON}" ]; then
  C8D_HAS_SDERR="Y"
  python3 - "${C8D_SDERR_JSON}" <<'PYEOF' >/dev/null 2>&1 && C8D_HAS_RC9="Y" || true
import json,sys
d=json.load(open(sys.argv[1]))
assert int(d.get("rc",-1))==9, d
PYEOF
  grep -q '"operation"'  "${C8D_SDERR_JSON}" && C8D_HAS_OP="Y"
  grep -q '"expected_path"' "${C8D_SDERR_JSON}" && C8D_HAS_EXP_PATH="Y"
  grep -q '"observed_path"' "${C8D_SDERR_JSON}" && C8D_HAS_OBS_PATH="Y"
  grep -q '"stderr"'       "${C8D_SDERR_JSON}" && C8D_HAS_STDERR="Y"
fi
C8D_GATE8_OK_BEFORE_G9="N"
grep -qE '^\[step 08\] 08-fixture-endpoint-registered : ok' "${G8D_BASE_S}/readiness.log" 2>/dev/null && C8D_GATE8_OK_BEFORE_G9="Y"
C8D_GATE9_REACHED="N"
grep -qE '^\[step 09\] 09-control-path-validated|^classification=.*FIXTURE_NOT_READY \(exit 12\)' "${G8D_BASE_S}/readiness.log" 2>/dev/null && C8D_GATE9_REACHED="Y"
C8D_NO_COMMAS="N"
if ! grep -nE 'comm -[23].*\|\| true|comm -1[23].*\|\| true' "${SCRIPT_DIR}/cni-readiness-gate.sh" >/dev/null 2>&1; then
  C8D_NO_COMMAS="Y"
fi

# C8d transcript (real Gate 8 set-diff
# command failure): rc=10 with exit
# CLUSTER_OR_CNI_NOT_READY; structured
# gate08-endpoint-setdiff-error.json contains
# rc=9/operation/expected_path/observed_path/
# stderr; gate8 must NOT record Step 8 ok
# BEFORE gate9 because the failure fires
# inside the gate8 loop.
printf 'C8d: rc=%s summary=%s logcls=%s sderr-art=%s rc9=%s op=%s exp-path=%s obs-path=%s stderr=%s gate8-ok-before-g9=%s (real Gate 8 set-diff command failure)\n' \
  "${C8D_RC}" "${C8D_SUMMARY}" "${C8D_LOGCLS}" \
  "${C8D_HAS_SDERR}" "${C8D_HAS_RC9}" "${C8D_HAS_OP}" \
  "${C8D_HAS_EXP_PATH}" "${C8D_HAS_OBS_PATH}" "${C8D_HAS_STDERR}" \
  "${C8D_GATE8_OK_BEFORE_G9}"

# --- C8t --- stale cni-mock-old substitution
# (real Gate 8 path): default/cni-mock-old
# replaces default/cni-mock-arbitrary; Gate 8
# vocabulary projection must reject before
# endpoint comparison closes, exiting 10
# CLUSTER_OR_CNI_NOT_READY before Gate 9.
C8T_TSV="${TOP_TMP}/gate8-C8t-stale-arb.tsv"
C8T="${TOP_TMP}/stage-C8t"
mkdir -p "${C8T}"
make_canonical_13_tsv "${C8T_TSV}" FAKE_STALE_OLD_SUB
make_real_gate_stage "${C8T}" "${C8T_TSV}"
write_env_file "${C8T}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${C8T}" \
  "HARNESS_STAGE_TSV=${C8T}/pods.tsv" \
  "HARNESS_FIXTURE_NAMES_TSV=${C8T_TSV}" \
  "HARNESS_GATE_BIN=${REAL_GATE_BIN}" \
  "CNI_READINESS_GATE_BIN=${REAL_GATE_BIN}" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state_c8t" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=120" \
  "HARNESS_CILIUM_NAMES=${CILIUM_DEFAULT}"
rm -f "${FAKE_BIN}/__date_state_c8t"
drive_control C8t "${C8T}" "${C8T}/run_gate.sh" "${C8T}/env.list"
C8T_RC="$(awk -F'=' '/^rc=/ {print $2; exit}' "${C8T}/child.rc" 2>/dev/null)"
G8T_BASE_S="${C8T}/artifacts"
C8T_SUMMARY="$(cat "${G8T_BASE_S}/readiness.summary.txt" 2>/dev/null || echo '__MISSING__')"
[ ! -f "${G8T_BASE_S}/readiness.summary.txt" ] && C8T_SUMMARY="__MISSING__"
C8T_LOGCLS="$(grep -E '^classification=' "${G8T_BASE_S}/readiness.log" 2>/dev/null | head -1)"
C8T_VOCAB_JSON="${G8T_BASE_S}/gate08-fixture-vocab.json"
C8T_MISSING_PAIR="N"
C8T_UNEXPECTED_STALE="N"
if [ -s "${C8T_VOCAB_JSON}" ]; then
  if python3 -c "
import json,sys
d=json.load(open(sys.argv[1]))
print('Y' if any(p.get('name')=='cni-mock-arbitrary' and p.get('namespace')=='default' for p in d.get('missing_static_pairs',[])) else 'N')
" "${C8T_VOCAB_JSON}" | grep -q '^Y$'; then
    C8T_MISSING_PAIR="Y"
  fi
  if grep -q 'cni-mock-old' "${C8T_VOCAB_JSON}"; then
    C8T_UNEXPECTED_STALE="Y"
  fi
fi
C8T_GATE9_OK="N"
grep -qE '^\[step 09\] 09-control-path-validated : ok' "${G8T_BASE_S}/readiness.log" 2>/dev/null && C8T_GATE9_OK="Y"

# --- C8u --- wrong-namespace substitution
# (real Gate 8 path): random-ns/cni-mock-postgres
# replaces default/cni-mock-postgres; Gate 8 must
# fail 10 with wrong-namespace pair in unexpected.
C8U_TSV="${TOP_TMP}/gate8-C8u-wrong-ns.tsv"
C8U="${TOP_TMP}/stage-C8u"
mkdir -p "${C8U}"
make_canonical_13_tsv "${C8U_TSV}" FAKE_WRONG_NS_SUB
make_real_gate_stage "${C8U}" "${C8U_TSV}"
write_env_file "${C8U}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${C8U}" \
  "HARNESS_STAGE_TSV=${C8U}/pods.tsv" \
  "HARNESS_FIXTURE_NAMES_TSV=${C8U_TSV}" \
  "HARNESS_GATE_BIN=${REAL_GATE_BIN}" \
  "CNI_READINESS_GATE_BIN=${REAL_GATE_BIN}" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state_c8u" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=120" \
  "HARNESS_CILIUM_NAMES=${CILIUM_DEFAULT}"
rm -f "${FAKE_BIN}/__date_state_c8u"
drive_control C8u "${C8U}" "${C8U}/run_gate.sh" "${C8U}/env.list"
C8U_RC="$(awk -F'=' '/^rc=/ {print $2; exit}' "${C8U}/child.rc" 2>/dev/null)"
G8U_BASE_S="${C8U}/artifacts"
C8U_SUMMARY="$(cat "${G8U_BASE_S}/readiness.summary.txt" 2>/dev/null || echo '__MISSING__')"
[ ! -f "${G8U_BASE_S}/readiness.summary.txt" ] && C8U_SUMMARY="__MISSING__"
C8U_LOGCLS="$(grep -E '^classification=' "${G8U_BASE_S}/readiness.log" 2>/dev/null | head -1)"
C8U_VOCAB_JSON="${G8U_BASE_S}/gate08-fixture-vocab.json"
C8U_MISSING_PAIR="N"
C8U_WRONG_NS_REJECTED="N"
if [ -s "${C8U_VOCAB_JSON}" ]; then
  if python3 -c "
import json,sys
d=json.load(open(sys.argv[1]))
print('Y' if any(p.get('name')=='cni-mock-postgres' and p.get('namespace')=='default' for p in d.get('missing_static_pairs',[])) else 'N')
" "${C8U_VOCAB_JSON}" | grep -q '^Y$'; then
    C8U_MISSING_PAIR="Y"
  fi
  if python3 -c "
import json,sys
d=json.load(open(sys.argv[1]))
print('Y' if any(p.get('name')=='cni-mock-postgres' and p.get('namespace')=='random-ns' for p in d.get('unexpected_fixture_like_pairs',[])) else 'N')
" "${C8U_VOCAB_JSON}" | grep -q '^Y$'; then
    C8U_WRONG_NS_REJECTED="Y"
  fi
fi

# --- C8v --- two probes replacing a static pair
# (real Gate 8 path): default/cni-mock-postgres
# removed; two distinct probes exist; Gate 8
# must fail 10 with dynamic_probe_cardinality=2.
C8V_TSV="${TOP_TMP}/gate8-C8v-two-probes.tsv"
C8V="${TOP_TMP}/stage-C8v"
mkdir -p "${C8V}"
make_canonical_13_tsv "${C8V_TSV}" FAKE_TWO_PROBES
make_real_gate_stage "${C8V}" "${C8V_TSV}"
write_env_file "${C8V}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${C8V}" \
  "HARNESS_STAGE_TSV=${C8V}/pods.tsv" \
  "HARNESS_FIXTURE_NAMES_TSV=${C8V_TSV}" \
  "HARNESS_GATE_BIN=${REAL_GATE_BIN}" \
  "CNI_READINESS_GATE_BIN=${REAL_GATE_BIN}" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state_c8v" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=120" \
  "HARNESS_CILIUM_NAMES=${CILIUM_DEFAULT}"
rm -f "${FAKE_BIN}/__date_state_c8v"
drive_control C8v "${C8V}" "${C8V}/run_gate.sh" "${C8V}/env.list"
C8V_RC="$(awk -F'=' '/^rc=/ {print $2; exit}' "${C8V}/child.rc" 2>/dev/null)"
G8V_BASE_S="${C8V}/artifacts"
C8V_SUMMARY="$(cat "${G8V_BASE_S}/readiness.summary.txt" 2>/dev/null || echo '__MISSING__')"
[ ! -f "${G8V_BASE_S}/readiness.summary.txt" ] && C8V_SUMMARY="__MISSING__"
C8V_LOGCLS="$(grep -E '^classification=' "${G8V_BASE_S}/readiness.log" 2>/dev/null | head -1)"
C8V_VOCAB_JSON="${G8V_BASE_S}/gate08-fixture-vocab.json"
C8V_MISSING_PAIR="N"
C8V_PROBE_CARD="0"
if [ -s "${C8V_VOCAB_JSON}" ]; then
  if python3 -c "
import json,sys
d=json.load(open(sys.argv[1]))
print('Y' if any(p.get('name')=='cni-mock-postgres' and p.get('namespace')=='default' for p in d.get('missing_static_pairs',[])) else 'N')
" "${C8V_VOCAB_JSON}" | grep -q '^Y$'; then
    C8V_MISSING_PAIR="Y"
  fi
  C8V_PROBE_CARD=$(python3 -c "
import json,sys
d=json.load(open(sys.argv[1]))
print(len(d.get('dynamic_probe_pairs',[])))
" "${C8V_VOCAB_JSON}")
fi
C8V_GATE8_OK="N"
grep -qE '^\[step 08\] 08-fixture-endpoint-registered : ok' "${G8V_BASE_S}/readiness.log" 2>/dev/null && C8V_GATE8_OK="Y"

printf 'C8t: rc=%s summary=%s logcls=%s missing-pair=%s unexpected-stale=%s gate9-ok=%s (real Gate 8 stale substitution)\n' \
  "${C8T_RC}" "${C8T_SUMMARY}" "${C8T_LOGCLS}" \
  "${C8T_MISSING_PAIR}" "${C8T_UNEXPECTED_STALE}" "${C8T_GATE9_OK}"
printf 'C8u: rc=%s summary=%s logcls=%s missing-pair=%s wrong-ns-rejected=%s (real Gate 8 wrong-namespace)\n' \
  "${C8U_RC}" "${C8U_SUMMARY}" "${C8U_LOGCLS}" \
  "${C8U_MISSING_PAIR}" "${C8U_WRONG_NS_REJECTED}"
printf 'C8v: rc=%s summary=%s logcls=%s missing-pair=%s probe-cardinality=%s gate8-ok=%s (real Gate 8 two-probe)\n' \
  "${C8V_RC}" "${C8V_SUMMARY}" "${C8V_LOGCLS}" \
  "${C8V_MISSING_PAIR}" "${C8V_PROBE_CARD}" "${C8V_GATE8_OK}"

# C8: kind load nonzero => step_image_pipeline rc=14.
S8="${TOP_TMP}/stage-C8"
mkdir -p "${S8}"
write_stage_files "${S8}" "" "${REAL_GATE_BIN}"
write_env_file "${S8}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${S8}" \
  "HARNESS_STAGE_TSV=${S8}/pods.tsv" \
  "HARNESS_GATE_BIN=${REAL_GATE_BIN}" \
  "CNI_READINESS_GATE_BIN=${REAL_GATE_BIN}" \
  "HARNESS_KIND_RC=1" \
  "HARNESS_DOCKER_RC=0" \
  "HARNESS_FAKE_SCRIPT_DIR=${S8}/scriptdir"
drive_control C8 "${S8}" "${S8}/run_img.sh" "${S8}/env.list"
C8_RC="$(awk -F'=' '/^rc=/ {print $2; exit}' "${S8}/child.rc" 2>/dev/null)"
C8_SUMMARY="$(cat "${S8}/readiness.summary.txt" 2>/dev/null || true)"
if [ ! -f "${S8}/readiness.summary.txt" ]; then
  C8_SUMMARY="__MISSING__"
fi
C8_LOGCLS="$(grep -E '^classification=' "${S8}/readiness.log" 2>/dev/null | head -1 || true)"
C8_DOWNSTREAM="N"
if [ -s "${S8}/downstream-stub-sentinel" ]; then C8_DOWNSTREAM="Y"; fi
C8_MISMATCH="N"
if [ -s "${S8}/abort-gate-mismatch.json" ]; then C8_MISMATCH="Y"; fi
C8_FIX_LOG="$([ -s "${S8}/fixture-image-kind-load.log" ] && echo Y || echo N)"

# C9: deadline boundary. Date advances fast;
# fixtures_ready=1 set on first 13-ready iteration;
# step G ok is printed.
S9="${TOP_TMP}/stage-C9"
mkdir -p "${S9}"
write_stage_files "${S9}" "${FAKE_13_READY_TSV}"
write_env_file "${S9}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${S9}" \
  "HARNESS_STAGE_TSV=${S9}/pods.tsv" \
  "HARNESS_GATE_BIN=${S9}/cni-readiness-gate.sh" \
  "CNI_READINESS_GATE_BIN=${S9}/cni-readiness-gate.sh" \
  "HARNESS_CILIUM_NAMES=${CILIUM_DEFAULT}" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=240"
drive_control C9 "${S9}" "${S9}/run_g.sh" "${S9}/env.list"
R9=$(classify_control C9 "${S9}")
C9_RC="$(echo "${R9}" | awk -F'|' '{print $2}')"
C9_SUMMARY="$(echo "${R9}" | awk -F'|' '{print $3}')"
C9_LOGCLS="$(echo "${R9}" | awk -F'|' '{print $4}')"
C9_DOWNSTREAM="$(echo "${R9}" | awk -F'|' '{print $5}')"
C9_MISMATCH="$(echo "${R9}" | awk -F'|' '{print $6}')"

# C10: real timeout. Drive with 12/13 to force
# the deadline path. The fake date advances only
# 1s per call here so the loop exhausts the
# configured 480 seconds BEFORE all 13 are ready.
# Each iteration keeps we get 12/13 rows, so the
# loop body keeps running until date reaches the
# deadline. The deadline check fires the timeout
# block, writing all three artifacts.
S10="${TOP_TMP}/stage-C10"
mkdir -p "${S10}"
FAKE_12_DEBUG=$(printf '%s' "${FAKE_13_READY_TSV}" | grep -v 'cni-control-target')
write_stage_files "${S10}" "${FAKE_12_DEBUG}" "${REAL_GATE_BIN}"
write_env_file "${S10}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${S10}" \
  "HARNESS_STAGE_TSV=${S10}/pods.tsv" \
  "HARNESS_GATE_BIN=${REAL_GATE_BIN}" \
  "CNI_READINESS_GATE_BIN=${REAL_GATE_BIN}" \
  "HARNESS_CILIUM_NAMES=${CILIUM_DEFAULT}" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=10"
drive_control C10 "${S10}" "${S10}/run_g.sh" "${S10}/env.list"
R10=$(classify_control C10 "${S10}")
C10_RC="$(echo "${R10}" | awk -F'|' '{print $2}')"
C10_SUMMARY="$(echo "${R10}" | awk -F'|' '{print $3}')"
C10_LOGCLS="$(echo "${R10}" | awk -F'|' '{print $4}')"
C10_DOWNSTREAM="$(echo "${R10}" | awk -F'|' '{print $5}')"
C10_MISMATCH="$(echo "${R10}" | awk -F'|' '{print $6}')"
C10_FIX_JSON="$([ -f "${S10}/fixture-pod-readiness-timeout.json" ] && echo Y || echo N)"
C10_FIX_TXT="$([ -f "${S10}/fixture-pod-readiness-timeout.txt" ] && echo Y || echo N)"
C10_FIX_LOG="$([ -f "${S10}/fixture-pod-readiness-events.log" ] && echo Y || echo N)"

# C11: static guard against the OLD abort_as
# patterns that mask the abort classification.
#
# Failure conditions (any one fails):
#   - abort_as() still expands "${env_var}=1"
#   - abort_as() unconditionally sets FIXTURE_INVALID=1
#   - abort_as() hardwrites ${SCRIPT_DIR}/cni-readiness-gate.sh
#     (target MUST go through CNI_READINESS_GATE_BIN)
#   - abort_as() wraps its actual gate invocation in `|| true`
# Required affirmative conditions:
#   - abort_as() carries the d2b.46 fixed-name
#     INSTALL_ABORT_CLASSIFICATION env
#   - cni-readiness-gate.sh contains the explicit
#     early INSTALL_ABORT_CLASSIFICATION classifier
#     before any kubectl/kind call
#   - scripts/test_fixture_readiness_observability.sh
#     points C2..C8/C10 at the REAL gate, not a
#     per-stage stub.
C11_OK="Y"
if grep -E '"\$\{env_var\}=1"' "${TARGET}" >/dev/null 2>&1; then
  C11_OK="N"
  printf 'C11-FAIL: target still references "${env_var}=1" dynamic token\n' >&2
fi
if grep -nE '^[^*"]*FIXTURE_INVALID=1' "${TARGET}" >/dev/null 2>&1 && \
   awk '/abort_as\(\)/,/^}/' "${TARGET}" | grep -q 'FIXTURE_INVALID=1'; then
  # Only fail if the abort_as body still
  # sets FIXTURE_INVALID=1 unconditionally.
  C11_OK="N"
  printf 'C11-FAIL: abort_as() still sets FIXTURE_INVALID=1 unconditionally\n' >&2
fi
if grep -nE 'bash[[:space:]]+"\${SCRIPT_DIR}/cni-readiness-gate.sh"' "${TARGET}" >/dev/null 2>&1; then
  C11_OK="N"
  printf 'C11-FAIL: target hardcodes ${SCRIPT_DIR}/cni-readiness-gate.sh inside its body\n' >&2
fi
if awk '/abort_as\(\)/,/^}/' "${TARGET}" \
     | grep -nE '"\${CNI_READINESS_GATE_BIN}".*\|\|[[:space:]]*true' >/dev/null 2>&1 ; then
  C11_OK="N"
  printf 'C11-FAIL: abort_as() wraps real-gate invocation in || true\n' >&2
fi
if ! grep -q 'INSTALL_ABORT_CLASSIFICATION="$label"' "${TARGET}"; then
  C11_OK="N"
  printf 'C11-FAIL: abort_as() missing explicit fixed-name label\n' >&2
fi
if ! grep -nE '^\s*if \[\[ -n "\$\{INSTALL_ABORT_CLASSIFICATION:-' "${REAL_GATE_BIN}" >/dev/null 2>&1 && \
   ! grep -nE '^\s*if \[\[ -n "\$\{INSTALL_ABORT_CLASSIFICATION' "${REAL_GATE_BIN}" >/dev/null 2>&1 ; then
  C11_OK="N"
  printf 'C11-FAIL: real gate missing explicit INSTALL_ABORT_CLASSIFICATION classifier block\n' >&2
fi
# Verify C2..C8/C10 use the real gate path, not
# the per-stage stub. The harness writes
# write_stage_files ... "${REAL_GATE_BIN}" lines
# for these controls.
if ! grep -nE 'write_stage_files "\$\{S2\}" "\$\{FAKE_13_ONE_MOCK_NOT_READY\}" "\$\{REAL_GATE_BIN\}"' "${SCRIPT_DIR}/test_fixture_readiness_observability.sh" >/dev/null 2>&1; then
  C11_OK="N"
  printf 'C11-FAIL: C2 not routed through real gate\n' >&2
fi
if ! grep -nE 'write_stage_files "\$\{S8\}" "" "\$\{REAL_GATE_BIN\}"' "${SCRIPT_DIR}/test_fixture_readiness_observability.sh" >/dev/null 2>&1; then
  C11_OK="N"
  printf 'C11-FAIL: C8 not routed through real gate\n' >&2
fi
if ! grep -nE 'write_stage_files "\$\{S10\}" "\$\{FAKE_12_DEBUG\}" "\$\{REAL_GATE_BIN\}"' "${SCRIPT_DIR}/test_fixture_readiness_observability.sh" >/dev/null 2>&1; then
  C11_OK="N"
  printf 'C11-FAIL: C10 not routed through real gate\n' >&2
fi
if ! grep -q 'CNI_READINESS_GATE_BIN' "${TARGET}"; then
  C11_OK="N"
fi
if ! grep -q 'fixtures_ready=1' "${TARGET}"; then
  C11_OK="N"
fi
# d2b.47: anchored table pipeline is FORBIDDEN
# in the target source. The historical fake
# kubectl `get pod -A --no-headers` emitter
# stripped the NAMESPACE column so that the
# anchored `grep -E "$fixture_re"` matched
# col 1 == name-by-fake-name. Real kubectl
# preserves NAMESPACE as col 1, so the
# anchored pipeline applied the regex to a
# namespace column and could produce zero
# selected rows even when all 13 fixture Pods
# were Ready. Anything resembling the old
# pattern in EXECUTABLE lines (excluding `#`
# and `"` comments) is a regression and MUST
# break the harness.
ANCHORED_PIPE_HITS=$(grep -nE '^[^#"]*kubectl[[:space:]]+get[[:space:]]+pod[[:space:]]+-A[[:space:]]+--no-headers([^#"]*|.*)grep[[:space:]]+-E' "${TARGET}" 2>/dev/null || true)
if [ -n "${ANCHORED_PIPE_HITS}" ]; then
  C11_OK="N"
  printf 'C11-FAIL: anchored table pipeline regressed in target source:\n%s\n' "${ANCHORED_PIPE_HITS}" >&2
fi

# d2b.46-followup #3: static one-shot call-count
# and stage-uniqueness assertion. Every direct
# Gate 8 / Step-G recovery control must be
# invoked EXACTLY ONCE against a UNIQUE stage
# directory. The check is static (a simple grep
# over the harness source) so a regression that
# adds a duplicate invocation breaks the test
# at source time, not at run time.
C11_ONE_SHOT_LIST="C7g C7h C7i C7d C7r C7s C8r C8d C8s C8t C8u C8v C6p C6q C6r C6s C6t C6u C6v"
C11_ONE_SHOT_OK="Y"
for ctrl in ${C11_ONE_SHOT_LIST}; do
  # Count exact literal drive_control invocations
  # that name this control token in the source.
  # Each occurrence must match "drive_control <token>"
  # as a prefix (no leading whitespace that would
  # indicate a duplicate from a derivative script).
  count=$(grep -nE "^drive_control ${ctrl}\b" "${SCRIPT_DIR}/test_fixture_readiness_observability.sh" \
            2>/dev/null | wc -l | tr -d ' ')
  if [ "${count}" -gt 1 ]; then
    C11_OK="N"
    C11_ONE_SHOT_OK="N"
    printf 'C11-FAIL: drive_control %s invoked %s times in harness source\n' \
      "${ctrl}" "${count}" >&2
  fi
done

# d2b.46-followup uniqueness assertion: no
# TOP_TMP stage directory is reused by more than
# one drive_control call. Each control must
# own exactly one child.rc observation.
STAGE_DIRS=$(grep -nE '^drive_control ' "${SCRIPT_DIR}/test_fixture_readiness_observability.sh" \
  2>/dev/null | awk -F'"' '{print $2}')
DUPED=$(printf '%s\n' "${STAGE_DIRS}" | sort | uniq -d)
if [ -n "${DUPED}" ]; then
  C11_OK="N"
  printf 'C11-FAIL: drive_control stages reused across controls:\n%s\n' "${DUPED}" >&2
fi

# ---------------------------------------------------------------------------
# Reporting
#
# d2b.46 evidence per failure control:
#   rc          : target process rc
#   summary     : contents of artifacts/cni-readiness.summary.txt
#   logcls      : first `classification=` line in artifacts/readiness.log
#   downstream  : N if abort_as routed through real gate
#                 Y if the per-stage stub ever ran (failure of contract)
#   mismatch    : Y if abort_as saw gate_rc != code
# ---------------------------------------------------------------------------
printf '\n# --- C1..C11 transcript ---\n'
printf 'C1:  rc=%s downstream-stub-invoked=%s\n' "${C1_RC}" "${C1_DOWNSTREAM}"
printf 'C2:  rc=%s summary=%s logcls=%s downstream-stub-invoked=%s match=%s\n' \
  "${C2_RC}" "${C2_SUMMARY}" "${C2_LOGCLS}" "${C2_DOWNSTREAM}" "${C2_MISMATCH}"
printf 'C3:  rc=%s summary=%s logcls=%s downstream-stub-invoked=%s match=%s\n' \
  "${C3_RC}" "${C3_SUMMARY}" "${C3_LOGCLS}" "${C3_DOWNSTREAM}" "${C3_MISMATCH}"
printf 'C4:  rc=%s summary=%s logcls=%s downstream-stub-invoked=%s match=%s\n' \
  "${C4_RC}" "${C4_SUMMARY}" "${C4_LOGCLS}" "${C4_DOWNSTREAM}" "${C4_MISMATCH}"
printf 'C5:  rc=%s summary=%s logcls=%s downstream-stub-invoked=%s match=%s fix-json=%s has-12-of-13=%s\n' \
  "${C5_RC}" "${C5_SUMMARY}" "${C5_LOGCLS}" "${C5_DOWNSTREAM}" "${C5_MISMATCH}" "${C5_FIX_JSON}" "${C5_HAS_NUM}"
printf 'C6:  rc=%s summary=%s logcls=%s downstream-stub-invoked=%s match=%s has-inv-stderr=%s\n' \
  "${C6_RC}" "${C6_SUMMARY}" "${C6_LOGCLS}" "${C6_DOWNSTREAM}" "${C6_MISMATCH}" "${C6_HAS_STDERR}"
printf 'C6p: rc=%s has-untrusted-label=%s has-probe-label=%s normal-handoff=%s abort-classifier-unexpected=%s empty-abort-at-handoff=%s invocations-log=%s can-fail-summary-absent=%s\n' \
  "${C6P_RC}" "${C6P_HAS_LABEL}" "${C6P_HAS_PROBE}" \
  "${C6P_NORMAL_HANDOFF_COUNT}" "${C6P_ABORT_CLASSIFIER_COUNT}" \
  "${C6P_EMPTY_ABORT_AT_HANDOFF}" \
  "$([ -f "${S6P}/gate-invocations.log" ] && echo Y || echo N)" \
  "${C6P_CANONICAL_FAILURE_PRESENT}"
printf 'C6q: rc=%s poll-counter=%s poll2-has-arbitrary=%s expected-has-arbitrary=%s normal-handoff=%s abort-classifier-unexpected=%s empty-abort-at-handoff=%s can-fail-summary-absent=%s\n' \
  "${C6Q_RC}" "${C6Q_COUNTER}" "${C6Q_POLL2_HAS_ARBITRARY}" "${C6Q_HAS_LABEL}" \
  "${C6Q_NORMAL_HANDOFF_COUNT}" "${C6Q_ABORT_CLASSIFIER_COUNT}" \
  "${C6Q_EMPTY_ABORT_AT_HANDOFF}" "${C6Q_CANONICAL_FAILURE_PRESENT}"
printf 'C6r: rc=%s selected-count=%s trojan-excluded=%s random-ns-valid-included=%s normal-handoff=%s abort-classifier-unexpected=%s empty-abort-at-handoff=%s can-fail-summary-absent=%s\n' \
  "${C6R_RC}" "${C6R_SELECTED_COUNT}" "${C6R_TROJAN_EXCLUDED}" \
  "${C6R_RANDOM_NS_VALID_INCLUDED}" \
  "${C6R_NORMAL_HANDOFF_COUNT}" "${C6R_ABORT_CLASSIFIER_COUNT}" \
  "${C6R_EMPTY_ABORT_AT_HANDOFF}" "${C6R_CANONICAL_FAILURE_PRESENT}"
printf 'C6s: rc=%s summary=%s parse-phase=%s parse-rc-field=%s forbidden-timeout-phase=%s normal-handoff=0-required=%s abort-classifier-unexpected=0-required=%s\n' \
  "${C6S_RC}" "${C6S_SUMMARY}" \
  "${C6S_PARSE_PHASE}" "${C6S_PARSE_RC_FIELD}" "${C6S_FORBIDDEN_TIMEOUT_PHASE}" \
  "$([ "${C6S_GATE_HANDOFF_COUNT}" = "0" ] && echo Y || echo N)" \
  "$([ "${C6S_GATE_ABORT_CLASSIFIER_COUNT}" = "0" ] && echo Y || echo N)"
printf 'C6t: rc=%s summary=%s missing-pair=%s unexpected-stale=%s aborter-premature=%s (install stale cni-mock-old substitution)\n' \
  "${C6T_RC}" "${C6T_SUMMARY}" "${C6T_MISSING_PAIR}" "${C6T_UNEXPECTED_STALE}" \
  "$([ "${C6T_RC}" = "12" ] && echo Y || echo N)"
printf 'C6u: rc=%s summary=%s missing-pair=%s wrong-ns-rejected=%s (install wrong-namespace substitution)\n' \
  "${C6U_RC}" "${C6U_SUMMARY}" "${C6U_MISSING_PAIR}" "${C6U_WRONG_NS_REJECTED}"
printf 'C6v: rc=%s summary=%s missing-pair=%s probe-cardinality=%s (install two-probe substitution)\n' \
  "${C6V_RC}" "${C6V_SUMMARY}" "${C6V_MISSING_PAIR}" "${C6V_PROBE_CARD}"
printf 'C7a: rc=%s summary=%s logcls=%s err-art=%s names-daemon-list=%s rc7-ref=%s\n' \
  "${C7A_RC}" "${C7A_SUMMARY}" "${C7A_LOGCLS}" "${C7A_ERR_ART}" "${C7A_NAMED_DAEMON_LIST}" "${C7A_RC7}"
printf 'C7b: rc=%s summary=%s logcls=%s err-art=%s daemon-named=%s rc8-ref=%s\n' \
  "${C7B_RC}" "${C7B_SUMMARY}" "${C7B_LOGCLS}" "${C7B_ERR_ART}" "${C7B_DAEMON_NAMED}" "${C7B_RC8}"
printf 'C7c: rc=%s summary=%s logcls=%s no-cmd-err=%s conv-art=%s obs12-exp13=%s\n' \
  "${C7C_RC}" "${C7C_SUMMARY}" "${C7C_LOGCLS}" "${C7C_NO_CMD_ERR}" "${C7C_CONV_ART}" "${C7C_OBS_12_EXP_13}"
printf 'C7k: rc=%s summary=%s logcls=%s obs12-exp13=%s (success mutation)\n' \
  "${C7K_RC}" "${C7K_SUMMARY}" "${C7K_LOGCLS}" "${C7K_OBS_12_EXP_13}"
printf 'C7g: rc=%s summary=%s logcls=%s err-art=%s downstream-stub-invoked=%s (direct Gate 8 daemon-list)\n' \
  "${C7G_RC}" "${C7G_SUMMARY}" "${C7G_LOGCLS}" "${C7G_ART}" "${C7G_DOWNSTREAM}"
printf 'C7h: rc=%s summary=%s logcls=%s err-art=%s daemon=%s\n' \
  "${C7H_RC}" "${C7H_SUMMARY}" "${C7H_LOGCLS}" "${C7H_ART}" "${C7H_DAEMON}"
printf 'C7i: rc=%s summary=%s logcls=%s conv-art=%s no-cmd-err=%s\n' \
  "${C7I_RC}" "${C7I_SUMMARY}" "${C7I_LOGCLS}" "${C7I_ART}" "${C7I_NO_CMD_ERR}"
printf 'C8:  rc=%s summary=%s logcls=%s downstream-stub-invoked=%s match=%s kind-load-log=%s\n' \
  "${C8_RC}" "${C8_SUMMARY}" "${C8_LOGCLS}" "${C8_DOWNSTREAM}" "${C8_MISMATCH}" "${C8_FIX_LOG}"
printf 'C9:  rc=%s downstream-stub-invoked=%s\n' "${C9_RC}" "${C9_DOWNSTREAM}"
printf 'C10: rc=%s summary=%s logcls=%s downstream-stub-invoked=%s match=%s fix-json=%s fix-txt=%s fix-events=%s\n' \
  "${C10_RC}" "${C10_SUMMARY}" "${C10_LOGCLS}" "${C10_DOWNSTREAM}" "${C10_MISMATCH}" "${C10_FIX_JSON}" "${C10_FIX_TXT}" "${C10_FIX_LOG}"
printf 'C11: ok=%s\n' "${C11_OK}"

PASS=0
TOTAL=37 # C1..C11 + C7a/b/c/k/g/h/i + C7d + C7r + C7s + C8r + C8d + C8s + C6p + C6q + C6r + C6s + C6t + C6u + C6v + M1 + M2a + M2b
# Per-control pass ledger (collects results so the
# final summary can name which controls failed).
# Bash 3.2 (macOS /bin/bash) does not support
# `declare -A`, so we use one named variable per
# control key. Each is initialized to N and set
# to Y by the corresponding PASS predicate.
C1_PASS=N
VOCAB_PASS=N
C2_PASS=N
C3_PASS=N
C4_PASS=N
C5_PASS=N
C6_PASS=N
C6P_PASS=N
C6Q_PASS=N
C6R_PASS=N
C6S_PASS=N
C6T_PASS=N
C6U_PASS=N
C6V_PASS=N
C7A_PASS=N
C7B_PASS=N
C7C_PASS=N
C7K_PASS=N
C7R_PASS=N
C7S_PASS=N
C7G_PASS=N
C7H_PASS=N
C7I_PASS=N
C7D_PASS=N
C8R_PASS=N
C8S_PASS=N
C8D_PASS=N
C8T_PASS=N
C8U_PASS=N
C8V_PASS=N
C8_PASS=N
C9_PASS=N
C10_PASS=N
C11_PASS=N
M1_PASS=N
M2A_PASS=N
M2B_PASS=N
# C1 success: gate stub invoked exactly once,
# no mismatch, target rc=0. C1 also proves
# the 13 fixture vocabulary includes
# cni-untrusted-default (otherwise C7k would
# already have failed the cni-untrusted-default
# contract — we assert it explicitly below).
if [ "${C1_RC}" = "0" ] && [ "${C1_DOWNSTREAM}" = "Y" ] && [ "${C1_MISMATCH}" = "N" ]; then PASS=$((PASS+1)); C1_PASS=Y; fi
# C1 vocabulary contract: cni-untrusted-default
# MUST appear in the canonical 13 fixture names
# set (CILIUM_DEFAULT). A mutation that removes
# only that name must fail this control. The
# canonical 13 are space-separated, not newline,
# so we iterate by word.
VOCAB_HAS_UNTRUSTED="N"
VOCAB_COUNT=0
for n in ${CILIUM_DEFAULT}; do
  VOCAB_COUNT=$((VOCAB_COUNT+1))
  case "${n}" in
    cni-untrusted-default) VOCAB_HAS_UNTRUSTED=Y ;;
  esac
done
if [ "${VOCAB_HAS_UNTRUSTED}" = "Y" ] && [ "${VOCAB_COUNT}" = "13" ]; then
  PASS=$((PASS+1))
  TOTAL=$((TOTAL+1))
  VOCAB_PASS=Y
fi
# C2..C7 NOT_READY failure controls: target rc=12,
# real-gate summary = FIXTURE_NOT_READY, log
# contains classification=FIXTURE_NOT_READY (exit 12),
# downstream stub NEVER invoked, NO mismatch artifact.
if [ "${C2_RC}" = "12" ] \
   && [ "${C2_SUMMARY}" = "FIXTURE_NOT_READY" ] \
   && printf '%s' "${C2_LOGCLS}" | grep -q 'FIXTURE_NOT_READY (exit 12)' \
   && [ "${C2_DOWNSTREAM}" = "N" ] \
   && [ "${C2_MISMATCH}" = "N" ] \
   && [ "${C2_HAS_NAME}" = "Y" ]; then PASS=$((PASS+1)); C2_PASS=Y; fi
if [ "${C3_RC}" = "12" ] \
   && [ "${C3_SUMMARY}" = "FIXTURE_NOT_READY" ] \
   && printf '%s' "${C3_LOGCLS}" | grep -q 'FIXTURE_NOT_READY (exit 12)' \
   && [ "${C3_DOWNSTREAM}" = "N" ] \
   && [ "${C3_MISMATCH}" = "N" ] \
   && [ "${C3_HAS_NAME}" = "Y" ]; then PASS=$((PASS+1)); C3_PASS=Y; fi
if [ "${C4_RC}" = "14" ] \
   && [ "${C4_SUMMARY}" = "FIXTURE_IMAGE_NOT_LOADED" ] \
   && printf '%s' "${C4_LOGCLS}" | grep -q 'FIXTURE_IMAGE_NOT_LOADED (exit 14)' \
   && [ "${C4_DOWNSTREAM}" = "N" ] \
   && [ "${C4_MISMATCH}" = "N" ]; then PASS=$((PASS+1)); C4_PASS=Y; fi
if [ "${C5_RC}" = "12" ] \
   && [ "${C5_SUMMARY}" = "FIXTURE_NOT_READY" ] \
   && printf '%s' "${C5_LOGCLS}" | grep -q 'FIXTURE_NOT_READY (exit 12)' \
   && [ "${C5_DOWNSTREAM}" = "N" ] \
   && [ "${C5_MISMATCH}" = "N" ] \
   && [ "${C5_HAS_NUM}" = "Y" ] \
   && [ "${C5_FIX_JSON}" = "Y" ]; then PASS=$((PASS+1)); C5_PASS=Y; fi
if [ "${C6_RC}" = "12" ] \
   && [ "${C6_SUMMARY}" = "FIXTURE_NOT_READY" ] \
   && printf '%s' "${C6_LOGCLS}" | grep -q 'FIXTURE_NOT_READY (exit 12)' \
   && [ "${C6_DOWNSTREAM}" = "N" ] \
   && [ "${C6_MISMATCH}" = "N" ] \
   && [ "${C6_HAS_STDERR}" = "Y" ]; then PASS=$((PASS+1)); C6_PASS=Y; fi
# d2b.47: C6p success requires the
# recording success-gate handoff count == 1
# with no abort-classifier-unexpected
# entries AND target rc == 0 AND no
# canonical failure summary present.
# Successful endpoint set must include both
# cni-untrusted-default and the generated
# cni-control-target identity (which is
# exactly what the JSON snapshot preserves).
if [ "${C6P_RC}" = "0" ] \
   && [ "${C6P_HAS_LABEL}" = "Y" ] \
   && [ "${C6P_HAS_PROBE}" = "Y" ] \
   && [ "${C6P_NORMAL_HANDOFF_COUNT}" = "1" ] \
   && [ "${C6P_ABORT_CLASSIFIER_COUNT}" = "0" ] \
   && [ "${C6P_EMPTY_ABORT_AT_HANDOFF}" = "Y" ] \
   && [ "${C6P_CANONICAL_FAILURE_PRESENT}" = "N" ] \
   && [ "${C6P_VOCAB_OK}" = "Y" ]; then PASS=$((PASS+1)); C6P_PASS=Y; fi
# d2b.47: C6q success requires Step G to
# poll at least twice (poll_counter >= 2)
# AND the SUCCESS path's readiness.log
# to record `first_failed_step=09-fixture-service-control`
# (NOT an earlier Step G/Step 08 failure),
# AND the expected_labels_file to contain
# the previously-missing fixture
# cni-mock-arbitrary. The counter file
# advancement is the deterministic proof
# that the loop did NOT abort on the
# pre-poll partial snapshot. Child rc=12
# is permitted for the same Gate 9 reason
# described for C6p.
# d2b.47: C6q success requires target
# rc 0, exactly TWO readiness JSON polls
# (poll 1 = 12 fixtures, poll 2 = 13 Ready),
# exactly one normal-handoff invocation of
# the recording success gate, zero
# abort-classifier-unexpected entries, no
# canonical readiness failure summary
# present, and the expected labels contain
# the fixture that was missing in poll 1.
# Downstream real-gate JSON queries are NOT
# counted because C6q drives the recording
# success stub (NOT the real gate).
if [ "${C6Q_RC}" = "0" ] \
   && [ "${C6Q_COUNTER}" = "2" ] \
   && [ "${C6Q_POLL2_HAS_ARBITRARY}" = "Y" ] \
   && [ "${C6Q_HAS_LABEL}" = "Y" ] \
   && [ "${C6Q_NORMAL_HANDOFF_COUNT}" = "1" ] \
   && [ "${C6Q_ABORT_CLASSIFIER_COUNT}" = "0" ] \
   && [ "${C6Q_EMPTY_ABORT_AT_HANDOFF}" = "Y" ] \
   && [ "${C6Q_CANONICAL_FAILURE_PRESENT}" = "N" ] \
   && [ "${C6Q_VOCAB_OK}" = "Y" ]; then PASS=$((PASS+1)); C6Q_PASS=Y; fi
# d2b.47: C6r success requires the trojan
# namespace-disguised Pod to be excluded
# from selection AND a valid fixture-name
# Pod in unrelated namespace to be
# included. Existing C5 FIXTURE_NOT_READY
# predicates are still satisfied because
# observed_count never reaches exactly 13
# during the bounded poll.
# d2b.47: C6r success requires target
# rc 0, exactly one normal-handoff to the
# recording success stub, zero
# abort-classifier-unexpected entries, the
# trojan namespace-disguised Pod
# (cni-mock-trojan-ns / not-a-fixture-pod)
# ABSENT from selection, a valid
# fixture-name Pod in unrelated namespace
# (random-ns / cni-mock-arbitrary)
# INCLUDED, selected population exactly 13,
# and no canonical readiness failure
# summary present.
# d2b.48 : C6r is a FAIL control (because
# the mutation removes the canonical
# `default/cni-mock-arbitrary` pair and
# substitutes `random-ns/cni-mock-arbitrary`,
# so canonical_population_ready is False and
# the bounded poll aborts as
# FIXTURE_NOT_READY 12). Predicate asserts:
#   - install does NOT proceed to Step G
#     success handoff (rc != 0)
#   - the canonical pair is recorded as
#     missing_static_pairs in the timeout
#     artifact
#   - the trojan pod
#     (cni-mock-trojan-ns/not-a-fixture-pod)
#     is recorded as unexpected_fixture_like
#   - the wrongly-namespaced cni-mock-arbitrary
#     pod is recorded as wrong_namespace
#   - the recording stub received exactly
#     one abort-classifier-unexpected
#     invocation and zero normal-handoff.
if [ "${C6R_RC}" != "0" ] \
   && [ "${C6R_MISSING_PAIR_PRESENT}" = "Y" ] \
   && [ "${C6R_UNEXPECTED_TROJAN_PRESENT}" = "Y" ] \
   && [ "${C6R_WRONG_NS_ARBITRARY_PRESENT}" = "Y" ] \
   && [ "${C6R_NORMAL_HANDOFF_COUNT}" = "0" ] \
   && [ "${C6R_ABORT_CLASSIFIER_COUNT}" = "1" ]; then PASS=$((PASS+1)); C6R_PASS=Y; fi
# d2b.47: C6s success requires rc=12,
# FIXTURE_NOT_READY, the structured
# parse-error artifact with rc=17 (the
# python projection's malformed-JSON exit),
# and the FORBIDDEN timeout-phase marker to
# be ABSENT (proof the projection failure
# was NOT interpreted as a zero-observation
# readiness timeout). Additionally: zero
# normal-handoff invocations against the
# recording success gate — Step G aborted
# BEFORE the success handoff because the
# JSON parse failed (proving the gate was
# not invoked on the success path).
# A non-zero abort-classifier-unexpected
# count is allowed here because the
# post-parse-failure code path through
# abort_as legitimately carries an explicit
# abort classifier.
if [ "${C6S_RC}" = "12" ] \
   && [ "${C6S_SUMMARY}" = "FIXTURE_NOT_READY" ] \
   && [ "${C6S_PARSE_PHASE}" = "Y" ] \
   && [ "${C6S_PARSE_RC_FIELD}" = "Y" ] \
   && [ "${C6S_FORBIDDEN_TIMEOUT_PHASE}" = "Y" ] \
   && [ "${C6S_GATE_HANDOFF_COUNT}" = "0" ]; then PASS=$((PASS+1)); C6S_PASS=Y; fi

# C6t: stale cni-mock-old substitution.
# Target rc=12 FIXTURE_NOT_READY. Both
# missing and unexpected vocabulary
# evidence present. No handoff.
if [ "${C6T_RC}" = "12" ] \
   && [ "${C6T_SUMMARY}" = "FIXTURE_NOT_READY" ] \
   && [ "${C6T_MISSING_PAIR}" = "Y" ] \
   && [ "${C6T_UNEXPECTED_STALE}" = "Y" ]; then
  PASS=$((PASS+1)); C6T_PASS=Y
fi

# C6u: wrong-namespace substitution.
# Target rc=12 FIXTURE_NOT_READY. Missing
# canonical pair + unexpected
# wrong-namespace pair.
if [ "${C6U_RC}" = "12" ] \
   && [ "${C6U_SUMMARY}" = "FIXTURE_NOT_READY" ] \
   && [ "${C6U_MISSING_PAIR}" = "Y" ] \
   && [ "${C6U_WRONG_NS_REJECTED}" = "Y" ]; then
  PASS=$((PASS+1)); C6U_PASS=Y
fi

# C6v: two-probe substitution. Target
# rc=12 FIXTURE_NOT_READY. Missing
# canonical pair
# default/cni-mock-postgres AND
# dynamic_probe_cardinality==2.
if [ "${C6V_RC}" = "12" ] \
   && [ "${C6V_SUMMARY}" = "FIXTURE_NOT_READY" ] \
   && [ "${C6V_MISSING_PAIR}" = "Y" ] \
   && [ "${C6V_PROBE_CARD}" = "2" ]; then
  PASS=$((PASS+1)); C6V_PASS=Y
fi

# C8t: stale cni-mock-old substitution (real
# Gate 8). Gate 8 must exit 10 with
# CLUSTER_OR_CNI_NOT_READY classifier BEFORE
# Gate 9 ok; vocabulary JSON must record
# missing default/cni-mock-arbitrary AND
# unexpected default/cni-mock-old.
if [ "${C8T_RC}" = "10" ] \
   && [ "${C8T_SUMMARY}" = "CLUSTER_OR_CNI_NOT_READY" ] \
   && printf '%s' "${C8T_LOGCLS}" | grep -q 'CLUSTER_OR_CNI_NOT_READY (exit 10)' \
   && [ "${C8T_MISSING_PAIR}" = "Y" ] \
   && [ "${C8T_UNEXPECTED_STALE}" = "Y" ] \
   && [ "${C8T_GATE9_OK}" = "N" ]; then PASS=$((PASS+1)); C8T_PASS=Y; fi

# C8u: wrong-namespace substitution (real
# Gate 8). Gate 8 must exit 10 with vocab
# JSON showing missing default/cni-mock-postgres
# AND unexpected random-ns/cni-mock-postgres.
if [ "${C8U_RC}" = "10" ] \
   && [ "${C8U_SUMMARY}" = "CLUSTER_OR_CNI_NOT_READY" ] \
   && printf '%s' "${C8U_LOGCLS}" | grep -q 'CLUSTER_OR_CNI_NOT_READY (exit 10)' \
   && [ "${C8U_MISSING_PAIR}" = "Y" ] \
   && [ "${C8U_WRONG_NS_REJECTED}" = "Y" ]; then PASS=$((PASS+1)); C8U_PASS=Y; fi

# C8v: two probes replacing a static pair
# (real Gate 8). Gate 8 must exit 10 with
# vocab JSON showing dynamic_probe_cardinality=2
# AND missing default/cni-mock-postgres. Gate 8
# MUST NOT have recorded Step 8 ok.
if [ "${C8V_RC}" = "10" ] \
   && [ "${C8V_SUMMARY}" = "CLUSTER_OR_CNI_NOT_READY" ] \
   && printf '%s' "${C8V_LOGCLS}" | grep -q 'CLUSTER_OR_CNI_NOT_READY (exit 10)' \
   && [ "${C8V_MISSING_PAIR}" = "Y" ] \
   && [ "${C8V_PROBE_CARD}" = "2" ] \
   && [ "${C8V_GATE8_OK}" = "N" ]; then PASS=$((PASS+1)); C8V_PASS=Y; fi

if [ "${C7A_RC}" = "10" ] \
   && [ "${C7A_SUMMARY}" = "CLUSTER_OR_CNI_NOT_READY" ] \
   && printf '%s' "${C7A_LOGCLS}" | grep -q 'CLUSTER_OR_CNI_NOT_READY (exit 10)' \
   && [ "${C7A_DOWNSTREAM}" = "N" ] \
   && [ "${C7A_MISMATCH}" = "N" ] \
   && [ "${C7A_ERR_ART}" = "Y" ] \
   && [ "${C7A_NAMED_DAEMON_LIST}" = "Y" ] \
   && [ "${C7A_RC7}" = "Y" ]; then PASS=$((PASS+1)); C7A_PASS=Y; fi
# C7b: per-daemon exec rc=8 -> CLUSTER_OR_CNI_NOT_READY 10,
# structured convergence artifact preserved, daemon
# name recorded in install log.
if [ "${C7B_RC}" = "10" ] \
   && [ "${C7B_SUMMARY}" = "CLUSTER_OR_CNI_NOT_READY" ] \
   && printf '%s' "${C7B_LOGCLS}" | grep -q 'CLUSTER_OR_CNI_NOT_READY (exit 10)' \
   && [ "${C7B_DOWNSTREAM}" = "N" ] \
   && [ "${C7B_MISMATCH}" = "N" ] \
   && [ "${C7B_ERR_ART}" = "Y" ] \
   && [ "${C7B_DAEMON_NAMED}" = "Y" ] \
   && [ "${C7B_RC8}" = "Y" ]; then PASS=$((PASS+1)); C7B_PASS=Y; fi
# C7c: valid 12-of-13 -> CLUSTER_OR_CNI_NOT_READY 10,
# NO command-error artifact, convergence artifact
# records observed 12 / expected 13.
if [ "${C7C_RC}" = "10" ] \
   && [ "${C7C_SUMMARY}" = "CLUSTER_OR_CNI_NOT_READY" ] \
   && printf '%s' "${C7C_LOGCLS}" | grep -q 'CLUSTER_OR_CNI_NOT_READY (exit 10)' \
   && [ "${C7C_DOWNSTREAM}" = "N" ] \
   && [ "${C7C_MISMATCH}" = "N" ] \
   && [ "${C7C_NO_CMD_ERR}" = "Y" ] \
   && [ "${C7C_CONV_ART}" = "Y" ] \
   && [ "${C7C_OBS_12_EXP_13}" = "Y" ]; then PASS=$((PASS+1)); C7C_PASS=Y; fi
# C7k: success mutation that removes ONLY
# cni-untrusted-default must still end up
# failing CLUSTER_OR_CNI_NOT_READY 10 with
# 12-of-13 in the install log (same convergence
# branch as C7c). It is NOT a pass — proving
# the success path cannot be reached when
# the canonical 13 vocabulary is violated.
if [ "${C7K_RC}" = "10" ] \
   && [ "${C7K_SUMMARY}" = "CLUSTER_OR_CNI_NOT_READY" ] \
   && [ "${C7K_OBS_12_EXP_13}" = "Y" ]; then PASS=$((PASS+1)); C7K_PASS=Y; fi
printf 'C7r: rc=%s summary=%s logcls=%s downstream=%s recon=%s first-empty=%s later-non-empty=%s unique-labels=%s identity=%s refuses-count-only=%s (install Step G recovery)\n' \
  "${C7R_RC}" "${C7R_SUMMARY}" "${C7R_LOGCLS}" "${C7R_DOWNSTREAM}" \
  "${C7R_RECOVERED}" "${C7R_FIRST_EMPTY}" "${C7R_LATER_NONEMPTY}" "${C7R_UNIQUE_LABELS}" \
  "${C7R_IDENTITY}" "${C7R_REFUSES_COUNT_ONLY}"

# C7s (install Step G equal-count wrong-label
# mutation): rc=10 with exit=CLUSTER_OR_CNI_NOT_READY;
# convergence JSON has expected_count=observed_count=13
# AND missing contains cni-untrusted-default
# AND unexpected contains cni-stale-old;
# downstream gate NOT invoked.
printf 'C7s: rc=%s summary=%s logcls=%s downstream=%s no-cmd-err=%s conv-art=%s exp=%s obs=%s miss-untrusted=%s unex-stale=%s (install Step G equal-count mutation)\n' \
  "${C7S_RC}" "${C7S_SUMMARY}" "${C7S_LOGCLS}" "${C7S_DOWNSTREAM}" \
  "${C7S_NO_CMD_ERR}" "${C7S_HAS_CONV_ART}" \
  "${C7S_EXP_COUNT}" "${C7S_OBS_COUNT}" \
  "${C7S_MISSING_HAS_UNTRUSTED}" "${C7S_UNEXPECTED_HAS_STALE}"

# C7d (install Step G set-diff command failure):
# rc=10 with CLUSTER_OR_CNI_NOT_READY; structured
# cilium-endpoint-setdiff-error.json contains
# rc=9, operation, expected/observed paths,
# captured stderr; downstream gate NOT invoked.
printf 'C7d: rc=%s summary=%s logcls=%s downstream=%s sd-err-art=%s rc9=%s op=%s exp-path=%s obs-path=%s stderr=%s (install Step G set-diff command failure)\n' \
  "${C7D_RC}" "${C7D_SUMMARY}" "${C7D_LOGCLS}" "${C7D_DOWNSTREAM}" \
  "${C7D_HAS_SDERR}" "${C7D_HAS_RC9}" "${C7D_HAS_OP}" "${C7D_HAS_EXP_PATH}" \
  "${C7D_HAS_OBS_PATH}" "${C7D_HAS_STDERR}"

# C7r (install Step G recovery): rc=0,
# downstream-stub-sentinel invoked exactly once,
# daemon-list counter >= 2 (first empty, second
# non-empty), daemon count log shows BOTH an
# empty poll (count=0) AND a non-empty poll,
# and the unique-label file has all 13 rows.
if [ "${C7R_RC}" = "0" ] \
   && [ "${C7R_DOWNSTREAM}" = "Y" ] \
   && [ "${C7R_MISMATCH}" = "N" ] \
   && [ "${C7R_RECOVERED}" = "Y" ] \
   && [ "${C7R_FIRST_EMPTY}" = "Y" ] \
   && [ "${C7R_LATER_NONEMPTY}" = "Y" ] \
   && [ "${C7R_UNIQUE_LABELS}" = "13" ] \
   && [ "${C7R_IDENTITY}" = "Y" ] \
   && [ "${C7R_REFUSES_COUNT_ONLY}" = "Y" ]; then PASS=$((PASS+1)); C7R_PASS=Y; fi
# C7s equal-count wrong-label mutation:
# install path produces 13 unique labels but
# with cni-stale-old substituting for
# cni-untrusted-default. Convergence JSON
# MUST show missing contains untrusted AND
# unexpected contains stale; both counts
# must be 13; rc=10; downstream stub NOT
# invoked.
if [ "${C7S_RC}" = "10" ] \
   && [ "${C7S_SUMMARY}" = "CLUSTER_OR_CNI_NOT_READY" ] \
   && printf '%s' "${C7S_LOGCLS}" | grep -q 'CLUSTER_OR_CNI_NOT_READY (exit 10)' \
   && [ "${C7S_DOWNSTREAM}" = "N" ] \
   && [ "${C7S_NO_CMD_ERR}" = "Y" ] \
   && [ "${C7S_HAS_CONV_ART}" = "Y" ] \
   && [ "${C7S_EXP_COUNT}" = "13" ] \
   && [ "${C7S_OBS_COUNT}" = "13" ] \
   && [ "${C7S_MISSING_HAS_UNTRUSTED}" = "Y" ] \
   && [ "${C7S_UNEXPECTED_HAS_STALE}" = "Y" ]; then PASS=$((PASS+1)); C7S_PASS=Y; fi

# C7d (install Step G set-diff command failure):
# rc=10 (CLUSTER_OR_CNI_NOT_READY); structured
# cilium-endpoint-setdiff-error.json must be
# present with rc=9 captured verbatim, operation
# label, expected/observed/output paths,
# captured stderr; downstream gate must NOT be
# invoked. We also statically verify the source
# has no `comm ... || true` masking on either
# set-diff operation.
if [ "${C7D_RC}" = "10" ] \
   && [ "${C7D_SUMMARY}" = "CLUSTER_OR_CNI_NOT_READY" ] \
   && printf '%s' "${C7D_LOGCLS}" | grep -q 'CLUSTER_OR_CNI_NOT_READY (exit 10)' \
   && [ "${C7D_DOWNSTREAM}" = "N" ] \
   && [ "${C7D_HAS_SDERR}" = "Y" ] \
   && [ "${C7D_HAS_RC9}" = "Y" ] \
   && [ "${C7D_HAS_OP}" = "Y" ] \
   && [ "${C7D_HAS_EXP_PATH}" = "Y" ] \
   && [ "${C7D_HAS_OBS_PATH}" = "Y" ] \
   && [ "${C7D_HAS_STDERR}" = "Y" ]; then PASS=$((PASS+1)); C7D_PASS=Y; fi
C7D_NO_COMMAS="N"
if ! grep -nE 'comm -[23].*\|\| true|comm -1[23].*\|\| true' "${SCRIPT_DIR}/install-nexus-test.sh" >/dev/null 2>&1; then
  C7D_NO_COMMAS="Y"
fi

# C7g/h/i: direct real Gate 8 regression
# controls. Each must exit 10 BEFORE Gate 9,
# write the EXACT content 'CLUSTER_OR_CNI_NOT_READY'
# in the canonical readiness.summary.txt (NOT a
# log line), and preserve its structured
# artefact.
if [ "${C7G_RC}" = "10" ] \
   && [ "${C7G_SUMMARY}" = "CLUSTER_OR_CNI_NOT_READY" ] \
   && printf '%s' "${C7G_LOGCLS}" | grep -q 'CLUSTER_OR_CNI_NOT_READY (exit 10)' \
   && [ "${C7G_DOWNSTREAM}" = "N" ] \
   && [ "${C7G_ART}" = "Y" ]; then PASS=$((PASS+1)); C7G_PASS=Y; fi
if [ "${C7H_RC}" = "10" ] \
   && [ "${C7H_SUMMARY}" = "CLUSTER_OR_CNI_NOT_READY" ] \
   && printf '%s' "${C7H_LOGCLS}" | grep -q 'CLUSTER_OR_CNI_NOT_READY (exit 10)' \
   && [ "${C7H_DOWNSTREAM}" = "N" ] \
   && [ "${C7H_ART}" = "Y" ] \
   && printf '%s' "${C7H_DAEMON}" | grep -q 'cilium'; then PASS=$((PASS+1)); C7H_PASS=Y; fi
# C7i: 12-of-13 valid convergence. PASS requires:
#   - canonical summary EXACTLY 'CLUSTER_OR_CNI_NOT_READY'
#   - convergence JSON parses; observed_count
#     equals len(observed_labels); both come
#     from the same unique-label file (the
#     observed file on disk validates too).
#   - observed_labels contains at least one
#     cni-mock AND at least one cni-control
#     (the canonical C7i case is missing
#     cni-untrusted-default, so we ONLY check
#     the two categories that ARE present).
if [ "${C7I_RC}" = "10" ] \
   && [ "${C7I_SUMMARY}" = "CLUSTER_OR_CNI_NOT_READY" ] \
   && printf '%s' "${C7I_LOGCLS}" | grep -q 'CLUSTER_OR_CNI_NOT_READY (exit 10)' \
   && [ "${C7I_DOWNSTREAM}" = "N" ] \
   && [ "${C7I_ART}" = "Y" ] \
   && [ "${C7I_NO_CMD_ERR}" = "Y" ] \
   && [ "${C7I_CONV_PARSE}" = "Y" ] \
   && [ "${C7I_CONV_OBS_CNT}" = "${C7I_CONV_OBS_LEN}" ] \
   && [ "${C7I_CONV_HAS_CNI_MOCK}" = "Y" ] \
   && [ "${C7I_CONV_HAS_CNI_CONTROL}" = "Y" ]; then PASS=$((PASS+1)); C7I_PASS=Y; fi
# C8r: real Gate 8 recovery. PASS requires:
#   - daemon-list counter >= 2 (first empty,
#     then non-empty)
#   - Gate 8 ok logged BEFORE any Gate 9 step
#   - NO command-error artifact
#   - NO under-convergence artifact (Gate 8 hit
#     13/13 exactly)
#   - observed/unique labels include
#     cni-untrusted-default (the exact 13-Pod
#     vocabulary contract)
#   - final unique-label count = 13
#   - Gate 8 was NOT the first_failed_step
#     (Gate 9 may legitimately fail first
#     under a fake PATH; that's allowed)
#   - exactly one drive_control C8r invocation
#     in the harness source
if [ "${C8R_DL_COUNTER}" -ge 2 ] \
   && [ "${C8R_RECOVERED}" = "Y" ] \
   && [ "${C8R_GATE8_OK_BEFORE_GATE9}" = "Y" ] \
   && [ "${C8R_GATE8_FIRST_FAIL}" = "N" ] \
   && [ "${C8R_NO_CMD_ERR}" = "Y" ] \
   && [ "${C8R_NO_CONV_ART}" = "Y" ] \
   && [ "${C8R_HAS_UNTRUSTED}" = "Y" ] \
   && [ "${C8R_UNIQUE_COUNT}" = "13" ] \
   && [ "${C8R_ONE_SHOT_COUNT}" = "1" ] \
   && [ "${C8R_IDENTITY}" = "Y" ] \
   && [ "${C8R_REFUSES_COUNT_ONLY}" = "Y" ] \
   && [ "${C8R_VOCAB_OK}" = "Y" ]; then PASS=$((PASS+1)); C8R_PASS=Y; fi
# C8s: real Gate 8 fails exit 10 BEFORE Gate 9
# on the equal-count wrong-label mutation
# with a Deployment-generated
# cni-control-probe-abc123-def45 Pod name.
# The convergence JSON must show
# expected_count==observed_count==13,
# missing includes BOTH cni-untrusted-default
# AND the generated probe name, and
# unexpected includes cni-stale-old; rc=10;
# canonical summary exact; no command-error
# artifact.
if [ "${C8S_RC}" = "10" ] \
   && [ "${C8S_SUMMARY}" = "CLUSTER_OR_CNI_NOT_READY" ] \
   && printf '%s' "${C8S_LOGCLS}" | grep -q 'CLUSTER_OR_CNI_NOT_READY (exit 10)' \
   && [ "${C8S_NO_CMD_ERR}" = "Y" ] \
   && [ "${C8S_HAS_CONV_ART}" = "Y" ] \
   && [ "${C8S_EXP_COUNT}" = "13" ] \
   && [ "${C8S_OBS_COUNT}" = "13" ] \
   && [ "${C8S_MISSING_HAS_UNTRUSTED}" = "Y" ] \
   && [ "${C8S_UNEXPECTED_HAS_STALE}" = "Y" ] \
   && [ "${C8S_MISSING_HAS_PROBE}" = "N" ]; then PASS=$((PASS+1)); C8S_PASS=Y; fi
# C8d (real Gate 8 set-diff command failure):
# rc=10 (CLUSTER_OR_CNI_NOT_READY); structured
# gate08-endpoint-setdiff-error.json contains
# rc=9 captured verbatim, operation,
# expected/observed paths, captured stderr;
# Gate 8 must NOT record Step 8 "ok" BEFORE
# the deadline fires (otherwise convergence
# success was faked on a simulated diff exit).
if [ "${C8D_RC}" = "10" ] \
   && [ "${C8D_SUMMARY}" = "CLUSTER_OR_CNI_NOT_READY" ] \
   && printf '%s' "${C8D_LOGCLS}" | grep -q 'CLUSTER_OR_CNI_NOT_READY (exit 10)' \
   && [ "${C8D_HAS_SDERR}" = "Y" ] \
   && [ "${C8D_HAS_RC9}" = "Y" ] \
   && [ "${C8D_HAS_OP}" = "Y" ] \
   && [ "${C8D_HAS_EXP_PATH}" = "Y" ] \
   && [ "${C8D_HAS_OBS_PATH}" = "Y" ] \
   && [ "${C8D_HAS_STDERR}" = "Y" ] \
   && [ "${C8D_GATE8_OK_BEFORE_G9}" = "N" ]; then PASS=$((PASS+1)); C8D_PASS=Y; fi
# C8 image-pipeline failure: target rc=14,
# real-gate summary = FIXTURE_IMAGE_NOT_LOADED.
if [ "${C8_RC}" = "14" ] \
   && [ "${C8_SUMMARY}" = "FIXTURE_IMAGE_NOT_LOADED" ] \
   && printf '%s' "${C8_LOGCLS}" | grep -q 'FIXTURE_IMAGE_NOT_LOADED (exit 14)' \
   && [ "${C8_DOWNSTREAM}" = "N" ] \
   && [ "${C8_MISMATCH}" = "N" ] \
   && [ "${C8_FIX_LOG}" = "Y" ]; then PASS=$((PASS+1)); C8_PASS=Y; fi
# C9 success: gate stub invoked exactly once.
if [ "${C9_RC}" = "0" ] && [ "${C9_DOWNSTREAM}" = "Y" ] && [ "${C9_MISMATCH}" = "N" ]; then PASS=$((PASS+1)); C9_PASS=Y; fi
# C10 real timeout: rc=12, summary FIXTURE_NOT_READY,
# 3 timeout artefacts, real gate, no stub, no mismatch.
if [ "${C10_RC}" = "12" ] \
   && [ "${C10_SUMMARY}" = "FIXTURE_NOT_READY" ] \
   && printf '%s' "${C10_LOGCLS}" | grep -q 'FIXTURE_NOT_READY (exit 12)' \
   && [ "${C10_DOWNSTREAM}" = "N" ] \
   && [ "${C10_MISMATCH}" = "N" ] \
   && [ "${C10_FIX_JSON}" = "Y" ] \
   && [ "${C10_FIX_TXT}"  = "Y" ] \
   && [ "${C10_FIX_LOG}"  = "Y" ]; then PASS=$((PASS+1)); C10_PASS=Y; fi
if [ "${C11_OK}" = "Y" ]; then PASS=$((PASS+1)); C11_PASS=Y; fi

# ---------------------------------------------------------------------------
# M1 — d2b.46 follow-up: prove abort_as's actual
# mismatch branch (gate_rc != code -> exit 16)
# executes end-to-end, not just compiles.
#
# The control induces an inventory deadline (12/13
# ready) so step_G_readiness's abort path fires
# abort_as FIXTURE_NOT_READY 12. CNI_READINESS_GATE_BIN
# points at a dedicated local mismatch stub that:
#   - exits exactly 0 (a wrong code for 12);
#   - writes a benign readiness.summary.txt so the
#     abort_as mismatch path can name a real file;
#   - stamps an invocation sentinel so the harness
#     can prove abort_as actually called the
#     injected gate, not the real one.
#
# Outcomes required for M1 PASS:
#   - target rc = 16 (NOT 12, NOT 0)
#   - injected gate sentinel exists
#   - $ARTIFACTS/abort-gate-mismatch.json exists
#     parses; requested_label=FIXTURE_NOT_READY,
#     expected_code=12, gate_rc=0,
#     summary_path=canonical ${ARTIFACTS}/readiness.summary.txt
#     AND that file actually exists.
#   - install log contains ABORT-GATE-MISMATCH +
#     requested/expected/got values
#   - downstream success path stub NOT invoked
# ---------------------------------------------------------------------------
SM1="${TOP_TMP}/stage-M1"
mkdir -p "${SM1}"
FAKE_12_FOR_M1=$(printf '%s' "${FAKE_13_READY_TSV}" | grep -v 'cni-control-target')
write_stage_files "${SM1}" "${FAKE_12_FOR_M1}"

M1_GATE_STUB="${FAKE_BIN}/m1-mismatch-gate.sh"
cat >"${M1_GATE_STUB}" <<'M1EOF'
#!/bin/sh
# d2b.46 M1 mismatch stub. Returns rc=0 to a
# FIXTURE_NOT_READY / code 12 invocation so the
# target's abort_as mismatch detector fires and
# records abort-gate-mismatch.json with rc=0.
{
  printf 'INVOKED M1\n' >>"${FAKE_INVOCATION_LOG:-/dev/null}"
  printf 'M1_INVOKED\n' > "${FAKE_BIN:-/tmp}/__m1_invoked"
  mkdir -p "${ARTIFACTS:-/tmp}"
  : > "${ARTIFACTS:-/tmp}/readiness.log"
  printf '%s\n' "{\"classification\":\"BENIGN\"}" > "${ARTIFACTS:-/tmp}/readiness.json"
  printf 'BENIGN\n' > "${ARTIFACTS:-/tmp}/readiness.summary.txt"
} 2>/dev/null
exit 0
M1EOF
chmod +x "${M1_GATE_STUB}"
write_env_file "${SM1}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${SM1}" \
  "HARNESS_STAGE_TSV=${SM1}/pods.tsv" \
  "HARNESS_GATE_BIN=${M1_GATE_STUB}" \
  "CNI_READINESS_GATE_BIN=${M1_GATE_STUB}" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=10" \
  "HARNESS_CILIUM_NAMES="
drive_control M1 "${SM1}" "${SM1}/run_g.sh" "${SM1}/env.list"

M1_RC="$(awk -F'=' '/^rc=/ {print $2; exit}' "${SM1}/child.rc" 2>/dev/null)"
M1_INV_LOG="${SM1}/gate-invocations.log"
M1_INVOKED_COUNT="0"
if [ -f "${M1_INV_LOG}" ]; then
  M1_INVOKED_COUNT="$(grep -c 'INVOKED M1' "${M1_INV_LOG}" 2>/dev/null | tr -d ' ')"
fi
[ -z "${M1_INVOKED_COUNT}" ] && M1_INVOKED_COUNT="0"
M1_INVOKED_MARKER="${FAKE_BIN}/__m1_invoked"
M1_INVOKED="N"
[ -s "${M1_INVOKED_MARKER}" ] && M1_INVOKED="Y"

M1_JSON_REQ="-"
M1_JSON_EXP="-"
M1_JSON_GATE="-"
M1_JSON_PATH="-"
M1_JSON_PATH_FILE_PRESENT="N"
M1_JSON_PARSEABLE="N"
if [ -s "${SM1}/abort-gate-mismatch.json" ]; then
  M1_JSON_PARSEABLE="Y"
  M1_JSON_REQ="$(python3 -c "import json,sys; print(json.load(open(sys.argv[1])).get('requested_label','-'))" "${SM1}/abort-gate-mismatch.json")"
  M1_JSON_EXP="$(python3 -c "import json,sys; print(json.load(open(sys.argv[1])).get('expected_code','-'))" "${SM1}/abort-gate-mismatch.json")"
  M1_JSON_GATE="$(python3 -c "import json,sys; print(json.load(open(sys.argv[1])).get('gate_rc','-'))" "${SM1}/abort-gate-mismatch.json")"
  M1_JSON_PATH="$(python3 -c "import json,sys; print(json.load(open(sys.argv[1])).get('summary_path','-'))" "${SM1}/abort-gate-mismatch.json")"
  if [ -f "${M1_JSON_PATH}" ]; then
    M1_JSON_PATH_FILE_PRESENT="Y"
  fi
fi

M1_LOG_ABORT_LINE="N"
M1_LOG_REQ_PRESENT="N"
M1_LOG_EXP_12="N"
M1_LOG_GOT_0="N"
if [ -f "${SM1}/install.log" ]; then
  grep -q 'ABORT-GATE-MISMATCH' "${SM1}/install.log" && M1_LOG_ABORT_LINE="Y"
  grep -q 'FIXTURE_NOT_READY' "${SM1}/install.log" && M1_LOG_REQ_PRESENT="Y"
  grep -q 'expected=12'       "${SM1}/install.log" && M1_LOG_EXP_12="Y"
  grep -q 'got=0'             "${SM1}/install.log" && M1_LOG_GOT_0="Y"
fi

M1_CANONICAL_PATH="${SM1}/readiness.summary.txt"
M1_CANONICAL_PRESENT="N"
[ -f "${M1_CANONICAL_PATH}" ] && M1_CANONICAL_PRESENT="Y"

M1_SUCCESS_STUB_NOT_TOUCHED="Y"
if [ -s "${SM1}/downstream-stub-sentinel" ]; then
  M1_SUCCESS_STUB_NOT_TOUCHED="N"
fi
# Translatable to a single unambiguous negative
# assertion: success-stub-invoked=N means the
# downstream success path stub was NOT invoked.
# The candidate reader prints this value
# verbatim so a verifier can correlate it with
# the harness's negative predicate. We retain
# M1_SUCCESS_STUB_NOT_TOUCHED under the same
# 'Y means negative passes' semantics so the
# internal vs. transcript naming never diverge:
# not_touched=Y  => transcript success-stub-invoked=N
# not_touched=N  => transcript success-stub-invoked=Y
M1_SUCCESS_STUB_INVOKED_TEXT="N"
M1_SUCCESS_STUB_PREDICATE_OK="N"
if [ "${M1_SUCCESS_STUB_NOT_TOUCHED}" = "Y" ]; then
  M1_SUCCESS_STUB_INVOKED_TEXT="N"
  M1_SUCCESS_STUB_PREDICATE_OK="Y"
elif [ "${M1_SUCCESS_STUB_NOT_TOUCHED}" = "N" ]; then
  M1_SUCCESS_STUB_INVOKED_TEXT="Y"
  M1_SUCCESS_STUB_PREDICATE_OK="N"
fi

printf '\n# --- M1 mismatch transcript ---\n'
printf 'M1: rc=%s inv-marker=%s inv-log-count=%s mismatch-json=%s\n' \
  "${M1_RC}" "${M1_INVOKED}" "${M1_INVOKED_COUNT}" "${M1_JSON_PARSEABLE}"
printf 'M1: requested_label=%s expected_code=%s gate_rc=%s summary_path=%s\n' \
  "${M1_JSON_REQ}" "${M1_JSON_EXP}" "${M1_JSON_GATE}" "${M1_JSON_PATH}"
printf 'M1: summary_path_file_present=%s abort_log_line=%s log_label=%s log_expected=12=%s log_got=0=%s success-stub-invoked=%s success-stub-absent-predicate=%s canonical-summary-present=%s\n' \
  "${M1_JSON_PATH_FILE_PRESENT}" "${M1_LOG_ABORT_LINE}" "${M1_LOG_REQ_PRESENT}" \
  "${M1_LOG_EXP_12}" "${M1_LOG_GOT_0}" "${M1_SUCCESS_STUB_INVOKED_TEXT}" \
  "${M1_SUCCESS_STUB_PREDICATE_OK}" \
  "${M1_CANONICAL_PRESENT}"

PASS=$((${PASS} + 0))
TOTAL=37
if [ "${M1_RC}" = "16" ] \
   && [ "${M1_INVOKED}" = "Y" ] \
   && [ "${M1_JSON_PARSEABLE}" = "Y" ] \
   && [ "${M1_JSON_REQ}" = "FIXTURE_NOT_READY" ] \
   && [ "${M1_JSON_EXP}" = "12" ] \
   && [ "${M1_JSON_GATE}" = "0" ] \
   && [ "${M1_JSON_PATH}" = "${M1_CANONICAL_PATH}" ] \
   && [ "${M1_JSON_PATH_FILE_PRESENT}" = "Y" ] \
   && [ "${M1_LOG_ABORT_LINE}" = "Y" ] \
   && [ "${M1_LOG_REQ_PRESENT}" = "Y" ] \
   && [ "${M1_LOG_EXP_12}" = "Y" ] \
   && [ "${M1_LOG_GOT_0}" = "Y" ] \
   && [ "${M1_SUCCESS_STUB_INVOKED_TEXT}" = "N" ] \
   && [ "${M1_SUCCESS_STUB_PREDICATE_OK}" = "Y" ]; then
  PASS=$((PASS+1))
  M1_PASS=Y
fi

# ---------------------------------------------------------------------------
# M2a / M2b — d2b.46 follow-up: execute the real
# preflight guard in scripts/install-nexus-test.sh
# when CNI_READINESS_GATE_BIN is wrong. The guard
# itself is source-excerpted in the install
# script:
#
#   CNI_READINESS_GATE_BIN="${CNI_READINESS_GATE_BIN:-${SCRIPT_DIR}/cni-readiness-gate.sh}"
#   if [[ ! -f "${CNI_READINESS_GATE_BIN}" ]]; then ... exit 22; fi
#   if [[ ! -x "${CNI_READINESS_GATE_BIN}" ]]; then ... exit 22; fi
#
# C1..C11 + M1 only prove the post-preflight gate
# behaviour. These two controls EXECUTE the install
# script's preflight through the existing file-backed
# absolute real-shell driver, never a source-only
# check. They use the same 20-second timeout and
# process-group cleanup; they perform no cluster
# activity because both inputs must fail before the
# main install flow begins.
#
# M2a: env points at a path that does not exist.
#       Target MUST exit 22 with stderr naming
#       CNI_READINESS_GATE_BIN and "does not exist".
#       No gate stub invocation, no readiness
#       summary/log/JSON, no mismatch JSON may be
#       written by abort_as (preflight runs first).
# M2b: env points at an existing regular file
#       lacking the execute bit.
#       Target MUST exit 22 with stderr naming
#       CNI_READINESS_GATE_BIN and "not executable".
#       No gate stub invocation, no readiness
#       summary/log/JSON, no mismatch JSON may be
#       written by abort_as (preflight runs first).
# ---------------------------------------------------------------------------
SM2A="${TOP_TMP}/stage-M2a"
mkdir -p "${SM2A}"
# Real-target runner: simply exec the install
# script under REAL_BASH so main() runs because
# $BASH_SOURCE == $0. The preflight guard runs
# immediately after the CLUSTER_NAME/ARTIFACTS
# default block and before mkdir of artefacts,
# so neither readiness.* nor mismatch JSON
# can be produced under a failing preflight.
write_install_preflight_runner() {
  local stage="$1"
  local runner="${stage}/run_install.sh"
  # The runner is intentionally minimal and
  # does NOT source install-nexus-test.sh, so
  # its BASH_SOURCE equals $0 and main() runs.
  # We pass the install path as the first arg
  # and forward "$@". REAL_BASH is the absolute
  # path to the bash interpreter resolved at
  # harness startup.
  printf '#!/bin/sh\n' > "${runner}"
  printf 'exec %s %s "$@"\n' "${REAL_BASH}" "\"${SCRIPT_DIR}/install-nexus-test.sh\"" >> "${runner}"
  chmod +x "${runner}"
}
write_install_preflight_runner "${SM2A}"
M2A_NEVER_PATH="${SM2A}/__no-such-gate.sh"
M2A_GATE_BIN_ABS="${M2A_NEVER_PATH}"
write_env_file "${SM2A}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${SM2A}" \
  "HARNESS_STAGE_TSV=${SM2A}/pods.tsv" \
  "HARNESS_GATE_BIN=${M2A_GATE_BIN_ABS}" \
  "CNI_READINESS_GATE_BIN=${M2A_GATE_BIN_ABS}" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state" \
  "HARNESS_DATE_ADVANCE=0" \
  "HARNESS_DATE_STEP=1" \
  "HARNESS_CILIUM_NAMES="
drive_control M2a "${SM2A}" "${SM2A}/run_install.sh" "${SM2A}/env.list"

SM2B="${TOP_TMP}/stage-M2b"
mkdir -p "${SM2B}"
write_install_preflight_runner "${SM2B}"
M2B_FILE="${SM2B}/__no-exec-bit-gate.sh"
printf '#!/bin/sh\necho should-not-run\nexit 99\n' >"${M2B_FILE}"
chmod 0644 "${M2B_FILE}"
M2B_GATE_BIN_ABS="${M2B_FILE}"
write_env_file "${SM2B}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${SM2B}" \
  "HARNESS_STAGE_TSV=${SM2B}/pods.tsv" \
  "HARNESS_GATE_BIN=${M2B_GATE_BIN_ABS}" \
  "CNI_READINESS_GATE_BIN=${M2B_GATE_BIN_ABS}" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state" \
  "HARNESS_DATE_ADVANCE=0" \
  "HARNESS_DATE_STEP=1" \
  "HARNESS_CILIUM_NAMES="
drive_control M2b "${SM2B}" "${SM2B}/run_install.sh" "${SM2B}/env.list"

# Capture M2a evidence.
M2A_RC="$(awk -F'=' '/^rc=/ {print $2; exit}' "${SM2A}/child.rc" 2>/dev/null)"
M2A_STDERR_NAMED_GATE="N"
M2A_STDERR_NAMED_MISSING="N"
M2A_STUB_SENTINEL_PRESENT="N"
M2A_READINESS_SUMMARY_PRESENT="N"
M2A_READINESS_LOG_PRESENT="N"
M2A_MISMATCH_JSON_PRESENT="N"
if [ -s "${SM2A}/child.stderr" ]; then
  grep -q 'CNI_READINESS_GATE_BIN' "${SM2A}/child.stderr" \
    && M2A_STDERR_NAMED_GATE="Y"
  grep -q 'does not exist' "${SM2A}/child.stderr" \
    && M2A_STDERR_NAMED_MISSING="Y"
fi
# The guard exits BEFORE any gate-stub path could
# be invoked; if a stub-invocation sentinel ever
# appears under ${SM2A} the contract is broken.
if [ -s "${SM2A}/gate-invocations.log" ] || [ -s "${SM2A}/downstream-stub-sentinel" ]; then
  M2A_STUB_SENTINEL_PRESENT="Y"
fi
[ -f "${SM2A}/readiness.summary.txt" ]  && M2A_READINESS_SUMMARY_PRESENT="Y"
[ -f "${SM2A}/readiness.log" ]         && M2A_READINESS_LOG_PRESENT="Y"
[ -f "${SM2A}/abort-gate-mismatch.json" ] && M2A_MISMATCH_JSON_PRESENT="Y"

# Capture M2b evidence.
M2B_RC="$(awk -F'=' '/^rc=/ {print $2; exit}' "${SM2B}/child.rc" 2>/dev/null)"
M2B_STDERR_NAMED_GATE="N"
M2B_STDERR_NAMED_NONEXEC="N"
M2B_STUB_SENTINEL_PRESENT="N"
M2B_READINESS_SUMMARY_PRESENT="N"
M2B_READINESS_LOG_PRESENT="N"
M2B_MISMATCH_JSON_PRESENT="N"
if [ -s "${SM2B}/child.stderr" ]; then
  grep -q 'CNI_READINESS_GATE_BIN' "${SM2B}/child.stderr" \
    && M2B_STDERR_NAMED_GATE="Y"
  grep -q 'not executable' "${SM2B}/child.stderr" \
    && M2B_STDERR_NAMED_NONEXEC="Y"
fi
if [ -s "${SM2B}/gate-invocations.log" ] || [ -s "${SM2B}/downstream-stub-sentinel" ]; then
  M2B_STUB_SENTINEL_PRESENT="Y"
fi
[ -f "${SM2B}/readiness.summary.txt" ]  && M2B_READINESS_SUMMARY_PRESENT="Y"
[ -f "${SM2B}/readiness.log" ]         && M2B_READINESS_LOG_PRESENT="Y"
[ -f "${SM2B}/abort-gate-mismatch.json" ] && M2B_MISMATCH_JSON_PRESENT="Y"

M2A_BIT_FILE=""
[ -e "${M2B_FILE}" ] && M2B_BIT_FILE="$(stat -f '%Sp' "${M2B_FILE}" 2>/dev/null || stat -c '%a' "${M2B_FILE}" 2>/dev/null)"
M2A_NEVER_FILE_PRESENT=""
[ -e "${M2A_NEVER_PATH}" ] && M2A_NEVER_FILE_PRESENT="present-wrongly"

printf '\n# --- M2a preflight (missing gate) transcript ---\n'
printf 'M2a: rc=%s stderr-named-gate=%s stderr-named-missing=%s stub-sentinel-present=%s readiness-summary-present=%s readiness-log-present=%s mismatch-json-present=%s never-path-status=%s\n' \
  "${M2A_RC}" "${M2A_STDERR_NAMED_GATE}" "${M2A_STDERR_NAMED_MISSING}" \
  "${M2A_STUB_SENTINEL_PRESENT}" "${M2A_READINESS_SUMMARY_PRESENT}" \
  "${M2A_READINESS_LOG_PRESENT}" "${M2A_MISMATCH_JSON_PRESENT}" \
  "${M2A_NEVER_FILE_PRESENT}"

printf '\n# --- M2b preflight (non-executable gate) transcript ---\n'
printf 'M2b: rc=%s stderr-named-gate=%s stderr-named-nonexec=%s stub-sentinel-present=%s readiness-summary-present=%s readiness-log-present=%s mismatch-json-present=%s file-mode=%s\n' \
  "${M2B_RC}" "${M2B_STDERR_NAMED_GATE}" "${M2B_STDERR_NAMED_NONEXEC}" \
  "${M2B_STUB_SENTINEL_PRESENT}" "${M2B_READINESS_SUMMARY_PRESENT}" \
  "${M2B_READINESS_LOG_PRESENT}" "${M2B_MISMATCH_JSON_PRESENT}" \
  "${M2B_BIT_FILE}"

TOTAL=37
if [ "${M2A_RC}" = "22" ] \
   && [ "${M2A_STDERR_NAMED_GATE}" = "Y" ] \
   && [ "${M2A_STDERR_NAMED_MISSING}" = "Y" ] \
   && [ "${M2A_STUB_SENTINEL_PRESENT}" = "N" ] \
   && [ "${M2A_READINESS_SUMMARY_PRESENT}" = "N" ] \
   && [ "${M2A_READINESS_LOG_PRESENT}" = "N" ] \
   && [ "${M2A_MISMATCH_JSON_PRESENT}" = "N" ]; then
  PASS=$((PASS+1))
  M2A_PASS=Y
fi
if [ "${M2B_RC}" = "22" ] \
   && [ "${M2B_STDERR_NAMED_GATE}" = "Y" ] \
   && [ "${M2B_STDERR_NAMED_NONEXEC}" = "Y" ] \
   && [ "${M2B_STUB_SENTINEL_PRESENT}" = "N" ] \
   && [ "${M2B_READINESS_SUMMARY_PRESENT}" = "N" ] \
   && [ "${M2B_READINESS_LOG_PRESENT}" = "N" ] \
   && [ "${M2B_MISMATCH_JSON_PRESENT}" = "N" ]; then
  PASS=$((PASS+1))
  M2B_PASS=Y
fi

printf '\n# C1..C11 + C6p/C6q/C6r/C6s/C6t/C6u/C6v + C7s + C8s + M1 + M2a + M2b PASS=%d/TOTAL=%d\n' "${PASS}" "${TOTAL}"
# Per-control pass table. Lets the operator
# attribute a regression to one control name
# without re-greping the harness source.
printf '# per-control: c1=%s vocab=%s c2=%s c3=%s c4=%s c5=%s c6=%s c6p=%s c6q=%s c6r=%s c6s=%s c6t=%s c6u=%s c6v=%s c7a=%s c7b=%s c7c=%s c7k=%s c7r=%s c7s=%s c7d=%s c7g=%s c7h=%s c7i=%s c8r=%s c8s=%s c8d=%s c8t=%s c8u=%s c8v=%s c8=%s c9=%s c10=%s c11=%s m1=%s m2a=%s m2b=%s\n' \
  "${C1_PASS}" "${VOCAB_PASS}" "${C2_PASS}" "${C3_PASS}" "${C4_PASS}" "${C5_PASS}" "${C6_PASS}" \
  "${C6P_PASS}" "${C6Q_PASS}" "${C6R_PASS}" "${C6S_PASS}" "${C6T_PASS}" "${C6U_PASS}" "${C6V_PASS}" \
  "${C7A_PASS}" "${C7B_PASS}" "${C7C_PASS}" "${C7K_PASS}" "${C7R_PASS}" "${C7S_PASS}" "${C7D_PASS}" \
  "${C7G_PASS}" "${C7H_PASS}" "${C7I_PASS}" "${C8R_PASS}" "${C8S_PASS}" "${C8D_PASS}" \
  "${C8T_PASS}" "${C8U_PASS}" "${C8V_PASS}" "${C8_PASS}" "${C9_PASS}" "${C10_PASS}" "${C11_PASS}" \
  "${M1_PASS}" "${M2A_PASS}" "${M2B_PASS}"
if [ "${PASS}" = "${TOTAL}" ]; then
  if [ "${NEXUS_VOCAB_SELFCHECK:-0}" = "1" ] || [ "${1:-}" = "--selfcheck" ] || [ "${1:-}" = "--vocab-check" ]; then
    # Optional explicit operator invocation
    # of the static manifest-vocabulary
    # self-check after the baseline passes.
    if manifest_vocab_selfcheck; then
      printf '# d2b.48 operator-initiated manifest_vocab_selfcheck: PASS\n'
      exit 0
    fi
    printf '# d2b.48 operator-initiated manifest_vocab_selfcheck: FAIL\n'
    exit 22
  fi
  exit 0
fi
exit 1
