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

# Write fake kubectl (POSIX shell).
cat >"${FAKE_BIN}/kubectl" <<'POSIXEOF'
#!/bin/sh
# d2b.46 fake kubectl. Reads inventory from
# FAKE_PODS_TSV_FILE. Handles every pattern
# the target's step_G_readiness issues.
set -u
case "${1:-}" in
  get)
    shift 2>/dev/null || true
    if [ "${1:-}" = "pod" ]; then
      if [ "${2:-}" = "-A" ] && [ "${3:-}" = "--no-headers" ]; then
        # Canonical NAME READY STATUS RESTARTS
        # AGE so the target's anchored regex
        # matches col 1 and the readiness awk
        # reads $2=READY, $3=STATUS.
        if [ -n "${FAKE_PODS_TSV_FILE:-}" ] && [ -r "${FAKE_PODS_TSV_FILE}" ]; then
          awk -F'\t' '{print $2, $3, $4, $5, $6}' "${FAKE_PODS_TSV_FILE}"
        fi
        rc="${FAKE_KUBECTL_RC:-0}"
        if [ "${rc}" != "0" ]; then
          echo "fake kubectl stderr (rc=${rc})" 1>&2
          exit "${rc}"
        fi
        exit 0
      fi
      if [ "${2:-}" = "-A" ] && [ "${3:-}" = "-o" ] && [ "${4:-}" = "json" ]; then
        if [ -n "${FAKE_PODS_TSV_FILE:-}" ] && [ -r "${FAKE_PODS_TSV_FILE}" ]; then
          FAKE_PODS_TSV="$(cat "${FAKE_PODS_TSV_FILE}")"
          export FAKE_PODS_TSV
        fi
        exec python3 -c '
import json, os
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
            echo "cilium-fake-${RANDOM:-x}"
            exit 0
          fi
          ;;
        exec)
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
    fi
    ;;
  events)
    echo "Warning stub-event-1"
    echo "Normal stub-event-2"
    exit 0
    ;;
esac
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

# Verify no bash, no env, no python3 in FAKE_BIN.
for tool in bash env python3 sh bash.exe; do
  if [ -e "${FAKE_BIN}/${tool}" ]; then
    printf 'FATAL: fake bin must not contain %s\n' "${tool}" >&2
    exit 2
  fi
done
printf '# fakebin: %s (no bash/env/python3)\n' "${FAKE_BIN}"

# ---------------------------------------------------------------------------
# Fixture TSV content (newline-delimited; columns
# separated by tab). Names use the d2b.46 contract
# ^cni-(mock|untrusted|control)- anchored matcher.
# ---------------------------------------------------------------------------
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
cni-control-probe-stubborn
cni-control-target'

build_13_ready() {
  local names="$1"
  local out=""
  local n
  for n in ${names}; do
    out="${out}cni	${n}	1/1	Running	0	7m
"
  done
  printf '%s' "${out}"
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
    "FAKE_DATE_NOW_FILE",
    "FAKE_DATE_ADVANCE",
    "FAKE_DATE_STEP",
    "FAKE_PODS_TSV_FILE",
    "FAKE_KUBECTL_RC",
    "FAKE_KIND_RC",
    "FAKE_DOCKER_RC",
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
CILIUM_DEFAULT="cni-mock-nexus-gateway cni-mock-nexus-worker cni-mock-nexus-migration cni-mock-egress-proxy cni-mock-arbitrary cni-mock-ingress-controller cni-mock-prometheus cni-mock-postgres cni-mock-redis cni-mock-clickhouse cni-untrusted-default cni-control-probe-stubborn cni-control-target"

# ---------------------------------------------------------------------------
# Control matrix
# ---------------------------------------------------------------------------

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
  /^cni\tcni-mock-arbitrary\t/ { print "cni\tcni-mock-arbitrary\t0/1\tPending\t0\t7m"; next }
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
  /^cni\tcni-untrusted-default\t/ { print "cni\tcni-untrusted-default\t0/1\tPending\t0\t7m"; next }
  /^cni-untrusted-default\tcni-untrusted-default\t/ { print "cni-untrusted-default\tcni-untrusted-default\t0/1\tPending\t0\t7m"; next }
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
  /^cni\tcni-mock-arbitrary\t/ { print "cni\tcni-mock-arbitrary\t0/1\tImagePullBackOff\t0\t7m"; next }
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

# C6: kubectl return rc != 0 on inventory.
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
  "HARNESS_KUBECTL_RC=7" \
  "HARNESS_CILIUM_NAMES=${CILIUM_DEFAULT}"
drive_control C6 "${S6}" "${S6}/run_g.sh" "${S6}/env.list"
R6=$(classify_control C6 "${S6}")
C6_RC="$(echo "${R6}" | awk -F'|' '{print $2}')"
C6_SUMMARY="$(echo "${R6}" | awk -F'|' '{print $3}')"
C6_LOGCLS="$(echo "${R6}" | awk -F'|' '{print $4}')"
C6_DOWNSTREAM="$(echo "${R6}" | awk -F'|' '{print $5}')"
C6_MISMATCH="$(echo "${R6}" | awk -F'|' '{print $6}')"
C6_HAS_STDERR="$(grep -qE "fake kubectl stderr|rc=7|inventory cannot be obtained" "${S6}/step_G_out" "${S6}/step_G_err" "${S6}/fixture-pod-readiness-timeout.json" 2>/dev/null && echo Y || echo N)"

# C7: Cilium endpoint inventory failure (make
# kubectl return rc=7 for the inner cilium list
# path so the loop never reaches the count threshold).
S7="${TOP_TMP}/stage-C7"
mkdir -p "${S7}"
write_stage_files "${S7}" "${FAKE_13_READY_TSV}" "${REAL_GATE_BIN}"
write_env_file "${S7}/env.list" \
  "HARNESS_REAL_BASH=${REAL_BASH}" \
  "HARNESS_SCRIPT_DIR=${SCRIPT_DIR}" \
  "HARNESS_ARTIFACTS=${S7}" \
  "HARNESS_STAGE_TSV=${S7}/pods.tsv" \
  "HARNESS_GATE_BIN=${REAL_GATE_BIN}" \
  "CNI_READINESS_GATE_BIN=${REAL_GATE_BIN}" \
  "FAKE_DATE_NOW_FILE=${FAKE_BIN}/__date_state" \
  "HARNESS_DATE_ADVANCE=1" \
  "HARNESS_DATE_STEP=240" \
  "HARNESS_CILIUM_NAMES=" \
  "HARNESS_KUBECTL_RC=7"
drive_control C7 "${S7}" "${S7}/run_g.sh" "${S7}/env.list"
R7=$(classify_control C7 "${S7}")
C7_RC="$(echo "${R7}" | awk -F'|' '{print $2}')"
C7_SUMMARY="$(echo "${R7}" | awk -F'|' '{print $3}')"
C7_LOGCLS="$(echo "${R7}" | awk -F'|' '{print $4}')"
C7_DOWNSTREAM="$(echo "${R7}" | awk -F'|' '{print $5}')"
C7_MISMATCH="$(echo "${R7}" | awk -F'|' '{print $6}')"
C7_FIX_JSON="$([ -f "${S7}/fixture-pod-readiness-timeout.json" ] && echo Y || echo N)"

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
  "HARNESS_DATE_STEP=1"
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
printf 'C7:  rc=%s summary=%s logcls=%s downstream-stub-invoked=%s match=%s fix-json=%s\n' \
  "${C7_RC}" "${C7_SUMMARY}" "${C7_LOGCLS}" "${C7_DOWNSTREAM}" "${C7_MISMATCH}" "${C7_FIX_JSON}"
printf 'C8:  rc=%s summary=%s logcls=%s downstream-stub-invoked=%s match=%s kind-load-log=%s\n' \
  "${C8_RC}" "${C8_SUMMARY}" "${C8_LOGCLS}" "${C8_DOWNSTREAM}" "${C8_MISMATCH}" "${C8_FIX_LOG}"
printf 'C9:  rc=%s downstream-stub-invoked=%s\n' "${C9_RC}" "${C9_DOWNSTREAM}"
printf 'C10: rc=%s summary=%s logcls=%s downstream-stub-invoked=%s match=%s fix-json=%s fix-txt=%s fix-events=%s\n' \
  "${C10_RC}" "${C10_SUMMARY}" "${C10_LOGCLS}" "${C10_DOWNSTREAM}" "${C10_MISMATCH}" "${C10_FIX_JSON}" "${C10_FIX_TXT}" "${C10_FIX_LOG}"
printf 'C11: ok=%s\n' "${C11_OK}"

PASS=0
TOTAL=11

# C1 success: gate stub invoked exactly once,
# no mismatch, target rc=0.
if [ "${C1_RC}" = "0" ] && [ "${C1_DOWNSTREAM}" = "Y" ] && [ "${C1_MISMATCH}" = "N" ]; then PASS=$((PASS+1)); fi
# C2..C7 NOT_READY failure controls: target rc=12,
# real-gate summary = FIXTURE_NOT_READY, log
# contains classification=FIXTURE_NOT_READY (exit 12),
# downstream stub NEVER invoked, NO mismatch artifact.
if [ "${C2_RC}" = "12" ] \
   && [ "${C2_SUMMARY}" = "FIXTURE_NOT_READY" ] \
   && printf '%s' "${C2_LOGCLS}" | grep -q 'FIXTURE_NOT_READY (exit 12)' \
   && [ "${C2_DOWNSTREAM}" = "N" ] \
   && [ "${C2_MISMATCH}" = "N" ] \
   && [ "${C2_HAS_NAME}" = "Y" ]; then PASS=$((PASS+1)); fi
if [ "${C3_RC}" = "12" ] \
   && [ "${C3_SUMMARY}" = "FIXTURE_NOT_READY" ] \
   && printf '%s' "${C3_LOGCLS}" | grep -q 'FIXTURE_NOT_READY (exit 12)' \
   && [ "${C3_DOWNSTREAM}" = "N" ] \
   && [ "${C3_MISMATCH}" = "N" ] \
   && [ "${C3_HAS_NAME}" = "Y" ]; then PASS=$((PASS+1)); fi
if [ "${C4_RC}" = "14" ] \
   && [ "${C4_SUMMARY}" = "FIXTURE_IMAGE_NOT_LOADED" ] \
   && printf '%s' "${C4_LOGCLS}" | grep -q 'FIXTURE_IMAGE_NOT_LOADED (exit 14)' \
   && [ "${C4_DOWNSTREAM}" = "N" ] \
   && [ "${C4_MISMATCH}" = "N" ]; then PASS=$((PASS+1)); fi
if [ "${C5_RC}" = "12" ] \
   && [ "${C5_SUMMARY}" = "FIXTURE_NOT_READY" ] \
   && printf '%s' "${C5_LOGCLS}" | grep -q 'FIXTURE_NOT_READY (exit 12)' \
   && [ "${C5_DOWNSTREAM}" = "N" ] \
   && [ "${C5_MISMATCH}" = "N" ] \
   && [ "${C5_HAS_NUM}" = "Y" ] \
   && [ "${C5_FIX_JSON}" = "Y" ]; then PASS=$((PASS+1)); fi
if [ "${C6_RC}" = "12" ] \
   && [ "${C6_SUMMARY}" = "FIXTURE_NOT_READY" ] \
   && printf '%s' "${C6_LOGCLS}" | grep -q 'FIXTURE_NOT_READY (exit 12)' \
   && [ "${C6_DOWNSTREAM}" = "N" ] \
   && [ "${C6_MISMATCH}" = "N" ] \
   && [ "${C6_HAS_STDERR}" = "Y" ]; then PASS=$((PASS+1)); fi
if [ "${C7_RC}" = "12" ] \
   && [ "${C7_SUMMARY}" = "FIXTURE_NOT_READY" ] \
   && printf '%s' "${C7_LOGCLS}" | grep -q 'FIXTURE_NOT_READY (exit 12)' \
   && [ "${C7_DOWNSTREAM}" = "N" ] \
   && [ "${C7_MISMATCH}" = "N" ] \
   && [ "${C7_FIX_JSON}" = "Y" ]; then PASS=$((PASS+1)); fi
# C8 image-pipeline failure: target rc=14,
# real-gate summary = FIXTURE_IMAGE_NOT_LOADED.
if [ "${C8_RC}" = "14" ] \
   && [ "${C8_SUMMARY}" = "FIXTURE_IMAGE_NOT_LOADED" ] \
   && printf '%s' "${C8_LOGCLS}" | grep -q 'FIXTURE_IMAGE_NOT_LOADED (exit 14)' \
   && [ "${C8_DOWNSTREAM}" = "N" ] \
   && [ "${C8_MISMATCH}" = "N" ] \
   && [ "${C8_FIX_LOG}" = "Y" ]; then PASS=$((PASS+1)); fi
# C9 success: gate stub invoked exactly once.
if [ "${C9_RC}" = "0" ] && [ "${C9_DOWNSTREAM}" = "Y" ] && [ "${C9_MISMATCH}" = "N" ]; then PASS=$((PASS+1)); fi
# C10 real timeout: rc=12, summary FIXTURE_NOT_READY,
# 3 timeout artefacts, real gate, no stub, no mismatch.
if [ "${C10_RC}" = "12" ] \
   && [ "${C10_SUMMARY}" = "FIXTURE_NOT_READY" ] \
   && printf '%s' "${C10_LOGCLS}" | grep -q 'FIXTURE_NOT_READY (exit 12)' \
   && [ "${C10_DOWNSTREAM}" = "N" ] \
   && [ "${C10_MISMATCH}" = "N" ] \
   && [ "${C10_FIX_JSON}" = "Y" ] \
   && [ "${C10_FIX_TXT}"  = "Y" ] \
   && [ "${C10_FIX_LOG}"  = "Y" ]; then PASS=$((PASS+1)); fi
if [ "${C11_OK}" = "Y" ]; then PASS=$((PASS+1)); fi

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
  "HARNESS_DATE_STEP=1" \
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
TOTAL=12
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

TOTAL=14
if [ "${M2A_RC}" = "22" ] \
   && [ "${M2A_STDERR_NAMED_GATE}" = "Y" ] \
   && [ "${M2A_STDERR_NAMED_MISSING}" = "Y" ] \
   && [ "${M2A_STUB_SENTINEL_PRESENT}" = "N" ] \
   && [ "${M2A_READINESS_SUMMARY_PRESENT}" = "N" ] \
   && [ "${M2A_READINESS_LOG_PRESENT}" = "N" ] \
   && [ "${M2A_MISMATCH_JSON_PRESENT}" = "N" ]; then
  PASS=$((PASS+1))
fi
if [ "${M2B_RC}" = "22" ] \
   && [ "${M2B_STDERR_NAMED_GATE}" = "Y" ] \
   && [ "${M2B_STDERR_NAMED_NONEXEC}" = "Y" ] \
   && [ "${M2B_STUB_SENTINEL_PRESENT}" = "N" ] \
   && [ "${M2B_READINESS_SUMMARY_PRESENT}" = "N" ] \
   && [ "${M2B_READINESS_LOG_PRESENT}" = "N" ] \
   && [ "${M2B_MISMATCH_JSON_PRESENT}" = "N" ]; then
  PASS=$((PASS+1))
fi

printf '\n# C1..C11 + M1 + M2a + M2b PASS=%d/TOTAL=%d\n' "${PASS}" "${TOTAL}"
if [ "${PASS}" = "${TOTAL}" ]; then
  exit 0
fi
exit 1
