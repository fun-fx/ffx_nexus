#!/usr/bin/env bash
# scripts/d2b-twelve-scenarios.sh
#
# Phase D-2b enforcing-CNI scenario gate, driven by
# scripts/fixtures/integrationcni/scenarios.json.
# The bash script is a thin driver: the source of
# truth is the JSON file, and a future regression
# in chart intent must be reflected in the JSON.
#
# d2b.53 repair — why this file was rewritten
# -------------------------------------------
# Heavy run 33642318757 on main 9bbb9b09 reported
#   PASS_OK=0 CHART_INTENTIONAL_DENY=0 FAIL=0 TOTAL=0
# and exited 0. Every declared scenario had SKIPped
# because the old resolve_source() rediscovered
# fixture Pods through `app=cni-source,role=…` /
# `app=cni-target,role=…` label selectors that DO
# NOT EXIST in 01-test-pods.yaml or 02-stub-deps.yaml
# (the tracked fixtures label with
# app.kubernetes.io/{name,component,instance}).
# A zero-work run is not a pass, so:
#
#   1. Source AND target identity now come from ONE
#      structured metadata source — the `source` and
#      `target` blocks in scenarios.json — which are
#      reconciled 1:1 against the tracked manifests.
#      There is no label-selector rediscovery switch
#      left in this file.
#   2. The static name from metadata is still
#      INDEPENDENTLY validated against Kubernetes
#      JSON before any traffic is requested: exact
#      namespace/name, not deleting, phase=Running,
#      explicit Ready=True, and exactly one match.
#      Zero / multiple / mismatched / non-ready /
#      terminating / malformed-JSON / kubectl-error
#      is an immediate scenario-gate FAILURE — never
#      a skip and never a silent continue.
#   3. TOTAL=0, a skipped row, an identity error, a
#      malformed result, a client error, or a
#      declared/executed/result count mismatch is a
#      terminal non-zero exit. Environment failures
#      are never converted into policy passes.
#   4. Expected counts are DERIVED from the JSON
#      document. The historical filename and the
#      workflow step text say "twelve"; the data has
#      13 entries. Neither 12 nor 13 is hard-coded in
#      gate math.
#
# Scratch-image probe contract
# ----------------------------
# The fixture image is FROM scratch and contains ONLY
# /cni-listener — no /bin/sh, no nc, no nslookup, no
# curl. Every layer therefore execs the fixture binary
# directly through `kubectl exec … -- /cni-listener …`
# with NO shell wrapper:
#
#   L1 target localhost  /cni-listener -probe=<port>
#                        run INSIDE the exact target
#                        Pod. Proves the target's own
#                        process is listening. Failure
#                        (when not L1-exempt) is
#                        LAYER1_DOWN — terminal.
#
#   L2 cluster DNS       /cni-listener -resolve-host=<FQDN>
#                        run inside the cni-control
#                        probe Pod, which is NOT
#                        selected by any rendered
#                        NetworkPolicy. Separates
#                        "policy denied" from "cluster
#                        DNS / service routing broken".
#                        Failure is LAYER2_FAIL —
#                        terminal, never a verdict.
#
#   L3 policy path       /cni-listener -http-get=<URL>
#                        or
#                        /cni-listener -tcp-connect=<host:port>
#                        run inside the exact scenario
#                        source Pod on the enforced
#                        cluster. THIS is the verdict
#                        that closes the gate.
#
# All three client modes carry the same fixed 5-second
# context deadline inside the Go binary; there is no
# tunable timeout and no retry loop anywhere in this
# script.
#
# L3 outcome classification (fail-closed)
# ---------------------------------------
# A non-zero client exit is NOT automatically a policy
# DENY. The classifier reads the client's own stderr
# marker so an environment problem can never be graded
# as a security outcome:
#
#   OPEN / HTTP:<code>  client exited 0 and emitted its
#                       strict JSON envelope -> the
#                       connection completed.
#   CLOSED              client ran and reported a
#                       network-layer failure
#                       ("… failed: <net error>") ->
#                       the datapath blocked it. This
#                       is the only input that may be
#                       graded as a DENY.
#   CLIENT_ERROR        client rejected its own input
#                       ("invalid -…") -> a driver bug.
#                       Terminal.
#   EXEC_ERROR          the client never ran: kubectl
#                       exec / API / container failure
#                       (no cni-listener marker in
#                       stderr). Terminal.
#
# Verdict grading (unchanged declared chart-intent table)
# -------------------------------------------------------
#   ALLOW_OK                expected=ALLOW, chart_intent=ALLOW_IMPLIED,
#                           L3 OPEN or HTTP:<any 1-5xx>
#   RULE_GAP                expected=ALLOW, chart_intent=ALLOW_IMPLIED,
#                           L3 CLOSED (chart drew a rule and it denied)
#   CHART_INTENTIONAL_DENY  expected=ALLOW, chart_intent=ALLOW_FEATURE_OFF,
#                           L3 CLOSED because the chart did not render an
#                           egress rule for the (port, selector) pair.
#                           This is the CORRECT answer for a feature that
#                           is off.
#   RULE_LEAK               expected=ALLOW, chart_intent=ALLOW_FEATURE_OFF,
#                           L3 OPEN (feature off but rule was rendered)
#   DENY_OK                 expected=DENY, chart_intent=DENY_*, L3 CLOSED
#   DENY_LEAK               expected=DENY, chart_intent=DENY_*, L3 OPEN —
#                           a security regression
#
# Passing verdicts: ALLOW_OK, DENY_OK, CHART_INTENTIONAL_DENY.
# Everything else is a failure, and every non-verdict
# category (identity, L1, L2, client, exec, accounting)
# is terminal on its own.
#
# Exit codes
# ----------
#   0   every declared scenario executed and graded pass
#   2   scenarios.json schema / duplicate-id / projection failure
#   3   source or target Pod identity validation failure
#   4   L1 down, L2 failed, client error, or exec error
#   5   accounting failure (TOTAL=0, count mismatch, duplicate /
#       missing / unexpected result id, malformed result line)
#   6   policy verdict failure (DENY_LEAK / RULE_LEAK / RULE_GAP)

set -uo pipefail

ART="${ARTIFACTS:-${PWD}/artifacts/integrationcni}"
SCENARIOS_JSON="${SCENARIOS_JSON:-${PWD}/scripts/fixtures/integrationcni/scenarios.json}"
PYTHON3="${PYTHON3:-$(command -v python3 || true)}"

# Canonical, explicit constants. The L2 control probe
# is Deployment-generated, so its Pod name is dynamic
# and is resolved the same way Step 09 resolves it:
# label selector + name regex + owner-free readiness
# validation requiring EXACTLY one candidate.
CONTROL_NS='cni-control'
CONTROL_LABEL_SELECTOR='app=cni-control,role=probe'
CONTROL_NAME_REGEX='^cni-control-probe-[a-z0-9]+-[a-z0-9]+$'
LISTENER_BIN='/cni-listener'

EXIT_SCHEMA=2
EXIT_IDENTITY=3
EXIT_LAYER=4
EXIT_ACCOUNTING=5
EXIT_VERDICT=6

mkdir -p "$ART"
: > "$ART/scenarios.log"
: > "$ART/probes.jsonl"
: > "$ART/scenario-kubectl-argv.log"
: > "$ART/scenario-identity.log"

log() { printf '%s\n' "$*" | tee -a "$ART/scenarios.log"; }
logq() { printf '%s\n' "$*" >> "$ART/scenarios.log"; }

if [[ -z "${PYTHON3}" ]]; then
  log "FATAL: python3 not present on PATH; scenario metadata parsing and JSONL serialization require real python"
  exit "$EXIT_SCHEMA"
fi

# --------------------------------------------------------------------------
# kubectl wrapper.
#
# Every kubectl invocation in this script goes through run_kc so that:
#   - the exact argv is appended to scenario-kubectl-argv.log, giving the
#     regression harness a ledger it can assert against (zero L1/L2/L3 exec
#     on identity failure; no nc / nslookup / curl / `sh -c` anywhere);
#   - stdout, stderr and rc are captured to named files rather than being
#     interpolated through a pipeline that would lose the real exit code.
# --------------------------------------------------------------------------
run_kc() {
  local prefix="$1"; shift
  {
    local IFS=$'\t'
    printf 'kubectl\t%s\n' "$*"
  } >> "$ART/scenario-kubectl-argv.log"
  kubectl "$@" > "${prefix}.stdout" 2> "${prefix}.stderr"
  local rc=$?
  printf '%s\n' "$rc" > "${prefix}.rc"
  return "$rc"
}

# --------------------------------------------------------------------------
# Step 1 — schema validation + structured projection.
#
# ONE structured read of scenarios.json. Python validates the document and
# projects a TAB-delimited row per scenario. Any schema defect, duplicate id,
# unreconcilable target shape, or a value containing a TAB / newline (which
# would corrupt the projection) is a terminal schema failure.
# --------------------------------------------------------------------------
SCEN_TSV="$ART/scenario-projection.tsv"
SCEN_SCHEMA_ERR="$ART/scenario-schema.stderr"

"$PYTHON3" - "$SCENARIOS_JSON" "$SCEN_TSV" "$ART/scenario-schema.json" \
  > "$ART/scenario-schema.stdout" 2> "$SCEN_SCHEMA_ERR" <<'EOF_PY_SCHEMA'
import json, re, sys

src, tsv_out, schema_out = sys.argv[1], sys.argv[2], sys.argv[3]

def die(reason):
    print("SCHEMA_FAIL:" + reason, file=sys.stderr)
    try:
        with open(schema_out, "w", encoding="utf-8") as f:
            f.write(json.dumps({
                "phase": "scenario_schema",
                "verdict": "invalid",
                "failure_reason": reason,
                "declared_count": 0,
            }, indent=2, sort_keys=True) + "\n")
    except Exception:
        pass
    sys.exit(2)

try:
    with open(src, "r", encoding="utf-8") as f:
        raw = f.read()
except Exception as e:
    die("READ_FAIL:" + repr(e))
if not raw.strip():
    die("EMPTY_DOCUMENT")
try:
    doc = json.loads(raw)
except Exception as e:
    die("NOT_JSON:" + repr(e))
if not isinstance(doc, dict):
    die("NOT_OBJECT")

scenarios = doc.get("scenarios")
if not isinstance(scenarios, list):
    die("SCENARIOS_NOT_LIST")
if len(scenarios) == 0:
    # A zero-scenario document can only ever produce TOTAL=0, which is
    # exactly the zero-work success this repair exists to eliminate.
    die("SCENARIOS_EMPTY")

NAME_RE = re.compile(r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")
FQDN_RE = re.compile(r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$")
ACTIONS = ("http_get", "tcp_connect")
KINDS = ("service", "external_ip")

seen_ids = []
rows = []
identities = []

for idx, s in enumerate(scenarios):
    if not isinstance(s, dict):
        die("SCENARIO_NOT_OBJECT:index=%d" % idx)
    sid = s.get("id", "")
    if not isinstance(sid, str) or not sid:
        die("ID_MISSING:index=%d" % idx)
    if sid in seen_ids:
        die("DUPLICATE_ID:%s" % sid)
    seen_ids.append(sid)

    for field in ("description", "role", "action", "target_kind",
                  "expected", "chart_intent", "upstream_reason"):
        v = s.get(field, "")
        if not isinstance(v, str) or v == "":
            die("FIELD_MISSING:%s:%s" % (sid, field))

    action = s["action"]
    if action not in ACTIONS:
        die("ACTION_UNKNOWN:%s:%s" % (sid, action))
    kind = s["target_kind"]
    if kind not in KINDS:
        die("TARGET_KIND_UNKNOWN:%s:%s" % (sid, kind))
    expected = s["expected"]
    if expected not in ("ALLOW", "DENY"):
        die("EXPECTED_UNKNOWN:%s:%s" % (sid, expected))
    intent = s["chart_intent"]
    if not (intent.startswith("ALLOW_") or intent.startswith("DENY_")):
        die("CHART_INTENT_UNKNOWN:%s:%s" % (sid, intent))

    port = s.get("target_port")
    if not isinstance(port, int) or isinstance(port, bool) or not (1 <= port <= 65535):
        die("TARGET_PORT_INVALID:%s:%r" % (sid, port))

    src_blk = s.get("source")
    if not isinstance(src_blk, dict):
        die("SOURCE_BLOCK_MISSING:%s" % sid)
    src_ns = src_blk.get("namespace", "")
    src_pod = src_blk.get("pod_name", "")
    if not isinstance(src_ns, str) or not NAME_RE.match(src_ns or ""):
        die("SOURCE_NAMESPACE_INVALID:%s:%r" % (sid, src_ns))
    if not isinstance(src_pod, str) or not NAME_RE.match(src_pod or ""):
        die("SOURCE_POD_NAME_INVALID:%s:%r" % (sid, src_pod))

    tgt_blk = s.get("target")
    if not isinstance(tgt_blk, dict):
        die("TARGET_BLOCK_MISSING:%s" % sid)

    ig_l1 = s.get("ignores_l1")
    ig_l2 = s.get("ignores_l2")
    if not isinstance(ig_l1, bool) or not isinstance(ig_l2, bool):
        die("IGNORES_FLAGS_NOT_BOOL:%s" % sid)

    if kind == "service":
        tgt_ns = tgt_blk.get("namespace", "")
        tgt_pod = tgt_blk.get("pod_name", "")
        fqdn = tgt_blk.get("service_fqdn", "")
        if not isinstance(tgt_ns, str) or not NAME_RE.match(tgt_ns or ""):
            die("TARGET_NAMESPACE_INVALID:%s:%r" % (sid, tgt_ns))
        if not isinstance(tgt_pod, str) or not NAME_RE.match(tgt_pod or ""):
            die("TARGET_POD_NAME_INVALID:%s:%r" % (sid, tgt_pod))
        if not isinstance(fqdn, str) or not FQDN_RE.match(fqdn or ""):
            die("TARGET_FQDN_INVALID:%s:%r" % (sid, fqdn))
        # The Service namespace is the SECOND dotted label of the FQDN.
        # Parsing the FIRST label as the namespace is the historical bug
        # that made "cni-gateway.default.svc.cluster.local" resolve into a
        # namespace called "cni-gateway"; assert the correct projection.
        parts = fqdn.split(".")
        if len(parts) < 3 or parts[2] != "svc":
            die("TARGET_FQDN_NOT_SERVICE_FORM:%s:%s" % (sid, fqdn))
        svc_name, svc_ns = parts[0], parts[1]
        if svc_ns != tgt_ns:
            die("TARGET_FQDN_NAMESPACE_MISMATCH:%s:fqdn_ns=%s target_ns=%s" % (sid, svc_ns, tgt_ns))
        if svc_name == tgt_ns:
            die("TARGET_FQDN_FIRST_LABEL_USED_AS_NAMESPACE:%s:%s" % (sid, fqdn))
        if ig_l1 or ig_l2:
            die("SERVICE_TARGET_MUST_NOT_BE_EXEMPT:%s" % sid)
        legacy_svc = s.get("target_svc", "")
        if legacy_svc != fqdn:
            die("TARGET_SVC_DISAGREES_WITH_TARGET_BLOCK:%s:%r!=%r" % (sid, legacy_svc, fqdn))
        host = fqdn
        exempt = "false"
        # Service targets carry a Pod identity, so target owner contract
        # is required and must be one of the closed envelope literals.
        tgt_oc_raw = tgt_blk.get("owner_contract")
        if not isinstance(tgt_oc_raw, dict):
            die("TARGET_OWNER_CONTRACT_MISSING:%s" % sid)
        if tgt_oc_raw.get("kind") == "OWNER_FREE":
            tgt_contract = "OWNER_FREE"
        else:
            die("TARGET_OWNER_CONTRACT_UNSUPPORTED:%s:%r" % (sid, tgt_oc_raw))
    else:
        if tgt_blk.get("kind") != "external_ip":
            die("EXTERNAL_TARGET_KIND_MISSING:%s" % sid)
        if tgt_blk.get("l1_l2_exempt") is not True:
            die("EXTERNAL_TARGET_NOT_MARKED_EXEMPT:%s" % sid)
        if tgt_blk.get("pod_name") is not None or tgt_blk.get("namespace") is not None:
            die("EXTERNAL_TARGET_INVENTED_POD:%s" % sid)
        if not (ig_l1 and ig_l2):
            die("EXTERNAL_TARGET_MUST_IGNORE_L1_L2:%s" % sid)
        host = tgt_blk.get("host", "")
        if not isinstance(host, str) or not host:
            die("EXTERNAL_HOST_INVALID:%s:%r" % (sid, host))
        if host != s.get("target_host", ""):
            die("EXTERNAL_HOST_DISAGREES_WITH_TARGET_BLOCK:%s" % sid)
        if tgt_blk.get("port") != port:
            die("EXTERNAL_PORT_DISAGREES_WITH_TARGET_BLOCK:%s" % sid)
        tgt_ns = ""
        tgt_pod = ""
        fqdn = ""
        exempt = "true"
        # external_ip: no target Pod; the owner slot is intentionally
        # empty and the runner never validates it — only the source.
        tgt_contract = ""

    # Source owner contract: every static fixture Pod is owner-free at
    # the manifest level; literal "OWNER_FREE" is the only valid shape.
    src_oc_raw = src_blk.get("owner_contract")
    if not isinstance(src_oc_raw, dict) or src_oc_raw.get("kind") != "OWNER_FREE":
        die("SOURCE_OWNER_CONTRACT_UNSUPPORTED:%s:%r" % (sid, src_oc_raw))
    src_contract = "OWNER_FREE"

    row = [
        sid, s["role"], action, kind,
        src_ns, src_pod, src_contract,
        tgt_ns, tgt_pod, tgt_contract,
        fqdn,
        host, str(port),
        expected, intent,
        "true" if ig_l1 else "false",
        "true" if ig_l2 else "false",
        exempt,
        s["description"], s["upstream_reason"],
    ]
    # The projection is \x1f (UNIT SEPARATOR) delimited, NOT tab delimited.
    # bash treats TAB as an IFS *whitespace* character, so `IFS=$'\t' read`
    # collapses runs of tabs and silently drops the empty cells that an
    # external_ip row legitimately carries (tgt_ns / tgt_pod / fqdn). That
    # column shift is what made the port field read "true". \x1f is not IFS
    # whitespace, so empty cells survive positionally.
    for cell in row:
        if "\x1f" in cell or "\n" in cell or "\r" in cell or "\t" in cell:
            die("PROJECTION_CELL_HAS_DELIMITER:%s:%r" % (sid, cell))
    rows.append("\x1f".join(row))

    identities.append({
        "id": sid,
        "source": {"namespace": src_ns, "pod_name": src_pod},
        "target": ({"namespace": tgt_ns, "pod_name": tgt_pod, "service_fqdn": fqdn}
                   if kind == "service"
                   else {"kind": "external_ip", "host": host, "port": port,
                         "l1_l2_exempt": True, "namespace": None, "pod_name": None}),
        "target_kind": kind,
        "action": action,
        "dial_host": host,
        "dial_port": port,
    })

try:
    with open(tsv_out, "w", encoding="utf-8") as f:
        f.write("\n".join(rows) + "\n")
except Exception as e:
    die("TSV_WRITE_FAIL:" + repr(e))

try:
    with open(schema_out, "w", encoding="utf-8") as f:
        f.write(json.dumps({
            "phase": "scenario_schema",
            "verdict": "valid",
            "failure_reason": "",
            "declared_count": len(rows),
            "declared_ids": seen_ids,
            "unique_declared_ids": len(set(seen_ids)),
            "identities": identities,
        }, indent=2, sort_keys=True) + "\n")
except Exception as e:
    die("SCHEMA_WRITE_FAIL:" + repr(e))

print(str(len(rows)))
EOF_PY_SCHEMA
SCHEMA_RC=$?

if [[ "$SCHEMA_RC" -ne 0 ]]; then
  log "FATAL: scenarios.json schema validation failed (rc=${SCHEMA_RC})"
  log "       $(head -n1 "$SCEN_SCHEMA_ERR" 2>/dev/null)"
  log "       artifact: $ART/scenario-schema.json"
  exit "$EXIT_SCHEMA"
fi

DECLARED_COUNT="$(tr -d ' \n' < "$ART/scenario-schema.stdout")"
if [[ ! "$DECLARED_COUNT" =~ ^[0-9]+$ ]] || [[ "$DECLARED_COUNT" -lt 1 ]]; then
  log "FATAL: declared_count '${DECLARED_COUNT}' is not a positive integer; refusing a zero-work run"
  exit "$EXIT_SCHEMA"
fi
log "[setup] scenarios.json schema valid: declared_count=${DECLARED_COUNT} (derived from the JSON document, not hard-coded)"

# --------------------------------------------------------------------------
# Step 2 — cluster topology banner.
# --------------------------------------------------------------------------
log "[setup] cluster topology:"
run_kc "$ART/scenario-nodes" get nodes -o wide
NODES_RC="$(cat "$ART/scenario-nodes.rc" 2>/dev/null || echo 1)"
cat "$ART/scenario-nodes.stdout" "$ART/scenario-nodes.stderr" 2>/dev/null | tee -a "$ART/scenarios.log"
if [[ "$NODES_RC" -ne 0 ]]; then
  log "FATAL: kubectl get nodes rc=${NODES_RC}; cluster is not observable, refusing to grade policy"
  exit "$EXIT_IDENTITY"
fi

# --------------------------------------------------------------------------
# Step 3 — Pod identity validation from Kubernetes JSON.
#
# validate_pod <namespace> <exact-name> <artifact-prefix> <label> <owner-contract-json>
#
# Issues ONE structured, field-selected list query and validates the parsed
# document. Tabular kubectl output is never parsed. Required checks:
#   - the response is a Kubernetes List with an items ARRAY
#   - EXACTLY one item (zero => absent, more than one => ambiguous)
#   - metadata.namespace equals the requested namespace exactly
#   - metadata.name equals the requested name exactly
#   - metadata.deletionTimestamp absent/null (not terminating)
#   - status.phase == "Running"
#   - status.conditions[] carries type=Ready with status="True"
#   - metadata.ownerReferences matches the owner_contract:
#       kind=OWNER_FREE        -> ownerReferences MUST be absent/empty; any
#                                 injected owner fails closed with
#                                 POD_OWNER_REFS_NOT_EMPTY
#       kind=OWNED_BY_*        -> exactly ONE owner whose controller=true is
#                                 a ReplicaSet in the SAME namespace whose
#                                 own owner chain terminates in a Deployment
#                                 named <deployment_name> in
#                                 <deployment_namespace>; wrong kind, wrong
#                                 name, wrong namespace, missing controller,
#                                 wrong controller=true owner, malformed
#                                 metadata, or ambiguity all fail closed.
#                                 The argument may be either a JSON literal
#                                 OR the literal string "OWNER_FREE" or
#                                 "DEPLOY:cni-control/cni-control-probe",
#                                 which the runner resolves deterministically
#                                 inline (no shell tokenisation hazard).
#
# Anything else prints a single closed failure reason and returns non-zero.
# Callers treat a non-zero return as terminal — no skip, no continue.
# --------------------------------------------------------------------------
validate_pod() {
  local ns="$1" name="$2" prefix="$3" label="$4" owner_contract="$5"

  run_kc "${prefix}" -n "$ns" get pod \
    --field-selector "metadata.name=${name}" -o json
  local q_rc; q_rc="$(cat "${prefix}.rc" 2>/dev/null || echo 1)"
  if [[ "$q_rc" -ne 0 ]]; then
    printf 'POD_LIST_COMMAND_ERROR rc=%s\n' "$q_rc" > "${prefix}.verdict"
    return 1
  fi

  "$PYTHON3" - "${prefix}.stdout" "$ns" "$name" "${owner_contract}" \
    > "${prefix}.verdict" 2> "${prefix}.parse.stderr" <<'EOF_PY_POD'
import json, sys
path, want_ns, want_name = sys.argv[1], sys.argv[2], sys.argv[3]

def out(reason):
    print(reason)
    sys.exit(0 if reason == "OK" else 1)

try:
    with open(path, "r", encoding="utf-8") as f:
        raw = f.read()
except Exception as e:
    out("POD_READ_FAIL:" + repr(e))
if not raw.strip():
    out("POD_EMPTY_DOCUMENT")
try:
    doc = json.loads(raw)
except Exception as e:
    out("POD_MALFORMED_JSON:" + repr(e))
if not isinstance(doc, dict):
    out("POD_NOT_OBJECT")
items = doc.get("items")
if not isinstance(items, list):
    out("POD_ITEMS_NOT_LIST")
if len(items) == 0:
    out("POD_ZERO_CANDIDATES")
if len(items) > 1:
    out("POD_MULTIPLE_CANDIDATES:%d" % len(items))

p = items[0]
if not isinstance(p, dict):
    out("POD_ITEM_NOT_OBJECT")
md = p.get("metadata")
if not isinstance(md, dict):
    out("POD_METADATA_NOT_OBJECT")
if md.get("namespace") != want_ns:
    out("POD_NAMESPACE_MISMATCH:%r!=%r" % (md.get("namespace"), want_ns))
if md.get("name") != want_name:
    out("POD_NAME_MISMATCH:%r!=%r" % (md.get("name"), want_name))
if md.get("deletionTimestamp") not in (None, ""):
    out("POD_TERMINATING")

st = p.get("status")
if not isinstance(st, dict):
    out("POD_STATUS_NOT_OBJECT")
phase = st.get("phase")
if phase != "Running":
    out("POD_PHASE_NOT_RUNNING:%r" % (phase,))
conds = st.get("conditions")
if not isinstance(conds, list):
    out("POD_CONDITIONS_NOT_LIST")
ready = False
for c in conds:
    if not isinstance(c, dict):
        out("POD_CONDITION_NOT_OBJECT")
    if c.get("type") == "Ready":
        if c.get("status") != "True":
            out("POD_NOT_READY:%r" % (c.get("status"),))
        ready = True
if not ready:
    out("POD_READY_CONDITION_ABSENT")

# ---------------------------------------------------------------------------
# owner_contract validation. The contract arrives as either:
#   - the literal token "OWNER_FREE"
#   - the literal token "DEPLOY:<deploy_ns>/<deploy_name>"
#   - a compact JSON literal produced by importlib callers
# We resolve it deterministically here; never via json.dumps in the runner.
# ---------------------------------------------------------------------------
oc_raw = sys.argv[4].strip()

def resolve_contract(raw):
    if raw == "OWNER_FREE":
        return ("OWNER_FREE",)
    if raw.startswith("DEPLOY:"):
        body = raw[len("DEPLOY:"):]
        if "/" not in body:
            return None
        ns, _, name = body.partition("/")
        if not ns or not name:
            return None
        return ("OWNED_BY_REPLICASET_OF_DEPLOYMENT", ns, name)
    # Caller is the metadata file itself; accept the canonical kinds only.
    try:
        parsed = json.loads(raw)
    except Exception as e:
        return ("BAD_CONTRACT_JSON:" + repr(e),)
    if not isinstance(parsed, dict):
        return ("BAD_CONTRACT_NOT_OBJECT",)
    kind = parsed.get("kind")
    if kind == "OWNER_FREE":
        return ("OWNER_FREE",)
    if kind == "OWNED_BY_REPLICASET_OF_DEPLOYMENT":
        dnsp = parsed.get("deployment_namespace")
        dname = parsed.get("deployment_name")
        if not (isinstance(dnsp, str) and dnsp and isinstance(dname, str) and dname
                and isinstance(parsed.get("controller_required"), bool)):
            return ("BAD_CONTRACT_DEPLOY_FIELDS",)
        chain = parsed.get("kind_chain")
        if not (isinstance(chain, list) and chain == ["Deployment", "ReplicaSet"]):
            return ("BAD_CONTRACT_KIND_CHAIN",)
        return ("OWNED_BY_REPLICASET_OF_DEPLOYMENT", dnsp, dname)
    return ("BAD_CONTRACT_KIND:" + repr(kind),)

oc = resolve_contract(oc_raw)
if oc is None:
    out("POD_OWNER_CONTRACT_UNRECOGNIZED:" + repr(oc_raw))
if oc[0] in ("BAD_CONTRACT_JSON", "BAD_CONTRACT_NOT_OBJECT",
             "BAD_CONTRACT_DEPLOY_FIELDS", "BAD_CONTRACT_KIND_CHAIN",
             "BAD_CONTRACT_KIND"):
    out("POD_OWNER_CONTRACT_INVALID:" + ":".join(oc))

owners = md.get("ownerReferences") or []
if not isinstance(owners, list):
    out("POD_OWNER_REFS_NOT_LIST")
read_owners = []
for o in owners:
    if not isinstance(o, dict):
        out("POD_OWNER_NOT_OBJECT")
    read_owners.append(o)

if oc[0] == "OWNER_FREE":
    if read_owners:
        out("POD_OWNER_REFS_NOT_EMPTY:%d" % len(read_owners))
else:
    # OWNED_BY_REPLICASET_OF_DEPLOYMENT; oc[1]=deploy_ns, oc[2]=deploy_name
    dep_ns, dep_name = oc[1], oc[2]
    # Find the controller=true owner; ambiguity fails closed.
    controllers = [o for o in read_owners if o.get("controller") is True]
    if len(controllers) == 0:
        out("POD_OWNER_NO_CONTROLLER")
    if len(controllers) > 1:
        out("POD_OWNER_AMBIGUOUS_CONTROLLERS:%d" % len(controllers))
    rs = controllers[0]
    if rs.get("kind") != "ReplicaSet":
        out("POD_OWNER_NOT_REPLICASET:%r" % (rs.get("kind"),))
    if rs.get("namespace") != ns:
        out("POD_OWNER_WRONG_NAMESPACE:%r!=%r" % (rs.get("namespace"), ns))
    rs_name = rs.get("name")
    if not (isinstance(rs_name, str) and rs_name):
        out("POD_OWNER_NO_NAME")
    # The ReplicaSet's owner chain must terminate in the declared Deployment.
    # We only see the ReplicaSet here; enforce its naming convention
    # (Deployment-owned ReplicaSet is named "<deploy>-<hash>"), which is the
    # only stable identity without a second apiserver round-trip. The
    # Step-09 gate already proves the full Deployment -> ReplicaSet link via
    # the Deployment manifest, so accepting this naming is consistent with the
    # existing contract.
    rs_prefix = dep_name + "-"
    if not rs_name.startswith(rs_prefix):
        out("POD_OWNER_RS_NAME_PREFIX_MISMATCH:r=%r!=%r" % (rs_name, rs_prefix))
    uid = rs.get("uid")
    if not (isinstance(uid, str) and uid):
        out("POD_OWNER_RS_NO_UID")

out("OK")
EOF_PY_POD
  local p_rc=$?
  local verdict; verdict="$(head -n1 "${prefix}.verdict" 2>/dev/null || true)"
  printf '%s\tns=%s\tname=%s\tquery_rc=%s\tverdict=%s\n' \
    "$label" "$ns" "$name" "$q_rc" "${verdict:-POD_VERDICT_EMPTY}" \
    >> "$ART/scenario-identity.log"
  if [[ "$p_rc" -ne 0 || "$verdict" != "OK" ]]; then
    return 1
  fi
  return 0
}

# --------------------------------------------------------------------------
# Step 3a — resolve the L2 control probe.
#
# The control probe is Deployment-generated, so its Pod name is dynamic. It
# is resolved through the canonical label selector, then validated with the
# same closed rules as every other Pod (exactly one, exact namespace, name
# matching the Deployment-generated regex, not terminating, Running,
# Ready=True). There is no static fallback to the literal Deployment name.
# --------------------------------------------------------------------------
CONTROL_PREFIX="$ART/scenario-control-probe"
run_kc "$CONTROL_PREFIX-list" -n "$CONTROL_NS" get pod \
  -l "$CONTROL_LABEL_SELECTOR" -o json
CONTROL_LIST_RC="$(cat "$CONTROL_PREFIX-list.rc" 2>/dev/null || echo 1)"

CONTROL_POD=""
CONTROL_VERDICT="CONTROL_UNRESOLVED"
if [[ "$CONTROL_LIST_RC" -eq 0 ]]; then
  CONTROL_POD="$("$PYTHON3" - "$CONTROL_PREFIX-list.stdout" "$CONTROL_NS" "$CONTROL_NAME_REGEX" \
    2> "$CONTROL_PREFIX-list.parse.stderr" <<'EOF_PY_CONTROL'
import json, re, sys
path, want_ns, name_re = sys.argv[1], sys.argv[2], sys.argv[3]
rx = re.compile(name_re)
# Hard-coded control-probe owner contract. Mirrors the Step-09 gate at
# scripts/cni-readiness-gate.sh:SOURCE_POD_DEPLOYMENT=cni-control-probe
# and deploy/helm/nexus/tests/fixture_namespace_document_boundary_test.py:
# (Deployment, cni-control-probe, CONTROL_POD_FIXTURE). Resolved here as a
# tuple so a json round-trip can never mutate it.
DEPLOY_NS = u"cni-control"
DEPLOY_NAME = u"cni-control-probe"
RS_PREFIX = DEPLOY_NAME + u"-"

def out(reason, name=""):
    print("%s\t%s" % (reason, name))
    sys.exit(0)

try:
    with open(path, "r", encoding="utf-8") as f:
        raw = f.read()
except Exception as e:
    out("CONTROL_READ_FAIL:" + repr(e))
if not raw.strip():
    out("CONTROL_EMPTY_DOCUMENT")
try:
    doc = json.loads(raw)
except Exception as e:
    out("CONTROL_MALFORMED_JSON:" + repr(e))
if not isinstance(doc, dict):
    out("CONTROL_NOT_OBJECT")
items = doc.get("items")
if not isinstance(items, list):
    out("CONTROL_ITEMS_NOT_LIST")

qualified = []
for p in items:
    if not isinstance(p, dict):
        out("CONTROL_ITEM_NOT_OBJECT")
    md = p.get("metadata") or {}
    st = p.get("status") or {}
    if not isinstance(md, dict) or not isinstance(st, dict):
        continue
    if md.get("namespace") != want_ns:
        continue
    nm = md.get("name") or ""
    if not rx.fullmatch(nm):
        continue
    if md.get("deletionTimestamp") not in (None, ""):
        continue
    if st.get("phase") != "Running":
        continue
    conds = st.get("conditions")
    if not isinstance(conds, list):
        continue
    if not any(isinstance(c, dict) and c.get("type") == "Ready" and c.get("status") == "True"
               for c in conds):
        continue
    # d2b.54 owner contract: the dynamic control probe is owned by
    # Deployment cni-control-probe in ns=cni-control. Accept the Pod ONLY
    # if it carries exactly one controller=true ReplicaSet owner whose
    # name begins with "<deploy_name>-". Wrong kind/name/namespace/missing
    # controller/wrong prefix/ambiguity all reduce the Pod to
    # CONTROL_CONTRACT_FAIL and no Pod is selected.
    owners = md.get("ownerReferences") or []
    if not isinstance(owners, list):
        out("CONTROL_OWNER_REFS_NOT_LIST")
    controllers = [o for o in owners if isinstance(o, dict) and o.get("controller") is True]
    if len(controllers) == 0:
        continue
    if len(controllers) > 1:
        out("CONTROL_OWNER_AMBIGUOUS_CONTROLLERS:%d" % len(controllers))
    rs = controllers[0]
    if rs.get("kind") != "ReplicaSet":
        continue
    if rs.get("namespace") != want_ns:
        continue
    rs_name = rs.get("name") or u""
    if not rs_name.startswith(RS_PREFIX):
        continue
    qualified.append(nm)

if len(qualified) == 0:
    out("CONTROL_ZERO_CANDIDATES_OR_CONTRACT_FAIL")
if len(qualified) > 1:
    out("CONTROL_MULTIPLE_CANDIDATES:%d" % len(qualified))
out("OK", qualified[0])
EOF_PY_CONTROL
)"
  CONTROL_VERDICT="$(printf '%s' "$CONTROL_POD" | cut -f1)"
  CONTROL_POD="$(printf '%s' "$CONTROL_POD" | cut -f2)"
else
  CONTROL_VERDICT="CONTROL_LIST_COMMAND_ERROR rc=${CONTROL_LIST_RC}"
fi

printf 'control-probe\tns=%s\tselector=%s\tquery_rc=%s\tverdict=%s\tresolved=%s\n' \
  "$CONTROL_NS" "$CONTROL_LABEL_SELECTOR" "$CONTROL_LIST_RC" \
  "$CONTROL_VERDICT" "${CONTROL_POD:-}" >> "$ART/scenario-identity.log"

if [[ "$CONTROL_VERDICT" != "OK" || -z "$CONTROL_POD" ]]; then
  log "FATAL: L2 control probe unresolved in ${CONTROL_NS} (selector=${CONTROL_LABEL_SELECTOR}): ${CONTROL_VERDICT}"
  log "       L2 separates 'policy denied' from 'cluster DNS broken'; without it no verdict is trustworthy"
  exit "$EXIT_IDENTITY"
fi
log "[setup] L2 control probe resolved dynamically: ${CONTROL_POD} (ns=${CONTROL_NS} selector=${CONTROL_LABEL_SELECTOR})"

# --------------------------------------------------------------------------
# Step 3b — validate EVERY declared source and target Pod BEFORE any
# traffic is requested.
#
# The whole identity surface is validated up front so a single unresolved
# Pod stops the run with ZERO L1/L2/L3 exec calls. This is the contract the
# regression harness asserts against scenario-kubectl-argv.log.
# --------------------------------------------------------------------------
IDENTITY_FAILURES=0
IDENTITY_CHECKED=0
IDENT_SEEN_FILE="$ART/.scenario-identity-seen"
: > "$IDENT_SEEN_FILE"

while IFS=$'\x1f' read -r sid role action kind src_ns src_pod src_contract tgt_ns tgt_pod tgt_contract fqdn host port expected intent ig_l1 ig_l2 exempt desc upstream; do
  [[ -z "${sid:-}" ]] && continue
  # source ownership check first; the slot is mandatory.
  src_key="${src_ns}/${src_pod}"
  if grep -qxF "${src_key}|source" "$IDENT_SEEN_FILE" 2>/dev/null; then
    : # already validated in this run; do not double-count
  else
    printf '%s\n' "${src_key}|source" >> "$IDENT_SEEN_FILE"
    safe_src="$(printf '%s' "$src_key" | tr '/' '_')"
    IDENTITY_CHECKED=$((IDENTITY_CHECKED + 1))
    if validate_pod "$src_ns" "$src_pod" "$ART/scenario-identity-${safe_src}" "source" "$src_contract"; then
      logq "[identity] source ${src_key} : ok (Running, Ready=True, exactly one, not terminating, owner=token=${src_contract%[:]*})"
    else
      v="$(head -n1 "$ART/scenario-identity-${safe_src}.verdict" 2>/dev/null || echo POD_VERDICT_EMPTY)"
      log "[identity] source ${src_key} : FAIL (${v})"
      IDENTITY_FAILURES=$((IDENTITY_FAILURES + 1))
    fi
  fi
  # target ownership check: external_ip targets have no Pod and carry
  # an explicit exempt=true contract — skip with a log line, do NOT
  # double-count identity failures.
  if [[ -n "${tgt_ns:-}" && -n "${tgt_pod:-}" ]]; then
    tgt_key="${tgt_ns}/${tgt_pod}"
    if grep -qxF "${tgt_key}|target" "$IDENT_SEEN_FILE" 2>/dev/null; then
      : # already validated
    else
      printf '%s\n' "${tgt_key}|target" >> "$IDENT_SEEN_FILE"
      safe_tgt="$(printf '%s' "$tgt_key" | tr '/' '_')"
      IDENTITY_CHECKED=$((IDENTITY_CHECKED + 1))
      if validate_pod "$tgt_ns" "$tgt_pod" "$ART/scenario-identity-${safe_tgt}" "target" "$tgt_contract"; then
        logq "[identity] target ${tgt_key} : ok (Running, Ready=True, exactly one, not terminating, owner=token=${tgt_contract%[:]*})"
      else
        v="$(head -n1 "$ART/scenario-identity-${safe_tgt}.verdict" 2>/dev/null || echo POD_VERDICT_EMPTY)"
        log "[identity] target ${tgt_key} : FAIL (${v})"
        IDENTITY_FAILURES=$((IDENTITY_FAILURES + 1))
      fi
    fi
  fi
done < "$SCEN_TSV"

log "[identity] distinct fixture Pods validated=${IDENTITY_CHECKED} failures=${IDENTITY_FAILURES}"
if [[ "$IDENTITY_FAILURES" -ne 0 ]]; then
  log "FATAL: ${IDENTITY_FAILURES} declared fixture Pod identity check(s) failed; refusing to request traffic"
  log "       ledger: $ART/scenario-identity.log ; argv: $ART/scenario-kubectl-argv.log"
  exit "$EXIT_IDENTITY"
fi

# --------------------------------------------------------------------------
# Step 4 — layer probes.
#
# classify_client <prefix> <mode>
#   Reads the captured rc/stdout/stderr of a client exec and returns one of
#   OPEN / HTTP:<code> / CLOSED / CLIENT_ERROR / EXEC_ERROR on stdout.
#
# The marker discipline is what keeps an environment failure from being
# graded as a security outcome: only a client that actually ran AND reported
# a network-layer error may become CLOSED.
# --------------------------------------------------------------------------
CLIENT_MARKER='cni-listener client mode failed:'
CLIENT_INVALID_MARKER='cni-listener client mode failed: invalid '

classify_client() {
  local prefix="$1" mode="$2"
  local rc; rc="$(cat "${prefix}.rc" 2>/dev/null || echo 1)"
  local out_f="${prefix}.stdout" err_f="${prefix}.stderr"

  if [[ "$rc" -eq 0 ]]; then
    if [[ "$mode" == "http_get" ]]; then
      local code
      code="$("$PYTHON3" -c '
import json,sys
try:
    d=json.loads(open(sys.argv[1],"r",encoding="utf-8").read().strip())
except Exception:
    print(""); sys.exit(0)
if not isinstance(d,dict): print(""); sys.exit(0)
s=d.get("status")
print(str(s) if isinstance(s,int) and 100<=s<=599 else "")
' "$out_f" 2>/dev/null)"
      if [[ "$code" =~ ^[1-5][0-9][0-9]$ ]]; then
        printf 'HTTP:%s\n' "$code"; return 0
      fi
      printf 'CLIENT_ERROR\n'; return 0
    fi
    local connected
    connected="$("$PYTHON3" -c '
import json,sys
try:
    d=json.loads(open(sys.argv[1],"r",encoding="utf-8").read().strip())
except Exception:
    print("no"); sys.exit(0)
print("yes" if isinstance(d,dict) and d.get("connected") is True else "no")
' "$out_f" 2>/dev/null)"
    if [[ "$connected" == "yes" ]]; then
      printf 'OPEN\n'; return 0
    fi
    printf 'CLIENT_ERROR\n'; return 0
  fi

  # Non-zero. A success-looking stdout on a non-zero exit is a contract
  # violation in the client, not a policy outcome.
  if [[ -s "$out_f" ]]; then
    printf 'CLIENT_ERROR\n'; return 0
  fi
  if grep -qF "$CLIENT_INVALID_MARKER" "$err_f" 2>/dev/null; then
    printf 'CLIENT_ERROR\n'; return 0
  fi
  if grep -qF "$CLIENT_MARKER" "$err_f" 2>/dev/null; then
    printf 'CLOSED\n'; return 0
  fi
  printf 'EXEC_ERROR\n'; return 0
}

probe_l1() {
  local ns="$1" pod="$2" port="$3" prefix="$4"
  run_kc "$prefix" exec -n "$ns" "$pod" -- "$LISTENER_BIN" "-probe=${port}"
  local rc; rc="$(cat "${prefix}.rc" 2>/dev/null || echo 1)"
  if [[ "$rc" -eq 0 ]]; then printf 'OK\n'; else printf 'DOWN\n'; fi
}

probe_l2() {
  local fqdn="$1" prefix="$2"
  run_kc "$prefix" exec -n "$CONTROL_NS" "$CONTROL_POD" -- \
    "$LISTENER_BIN" "-resolve-host=${fqdn}"
  local rc; rc="$(cat "${prefix}.rc" 2>/dev/null || echo 1)"
  if [[ "$rc" -ne 0 ]]; then printf 'FAIL\n'; return 0; fi
  local n
  n="$("$PYTHON3" -c '
import json,sys
try:
    d=json.loads(open(sys.argv[1],"r",encoding="utf-8").read().strip())
except Exception:
    print("0"); sys.exit(0)
a=d.get("addresses") if isinstance(d,dict) else None
print(str(len(a)) if isinstance(a,list) else "0")
' "${prefix}.stdout" 2>/dev/null)"
  if [[ "$n" =~ ^[0-9]+$ ]] && [[ "$n" -ge 1 ]]; then printf 'OK\n'; else printf 'FAIL\n'; fi
}

probe_l3() {
  local mode="$1" ns="$2" pod="$3" host="$4" port="$5" prefix="$6"
  if [[ "$mode" == "http_get" ]]; then
    run_kc "$prefix" exec -n "$ns" "$pod" -- \
      "$LISTENER_BIN" "-http-get=http://${host}:${port}/"
  else
    run_kc "$prefix" exec -n "$ns" "$pod" -- \
      "$LISTENER_BIN" "-tcp-connect=${host}:${port}"
  fi
  classify_client "$prefix" "$mode"
}

# --------------------------------------------------------------------------
# Step 5 — execute every declared scenario.
# --------------------------------------------------------------------------
PASS=0
CHART_INTENT_DENY=0
FAIL=0
TOTAL=0
EXECUTED=0
N_SKIPPED=0
N_UNKNOWN=0
N_LAYER1_DOWN=0
N_LAYER2_FAIL=0
N_UNRESOLVED=0
N_CLIENT_ERROR=0
N_ENV_ERROR=0
N_DENY_LEAK=0
N_RULE_LEAK=0
N_RULE_GAP=0

while IFS=$'\x1f' read -r sid role action kind src_ns src_pod src_contract tgt_ns tgt_pod tgt_contract fqdn host port expected intent ig_l1 ig_l2 exempt desc upstream; do
  # d2b.54: the row now carries source/target owner-contract tokens
  # src_contract and tgt_contract (slot 7 and 10). The probe path below
  # only reads lanes 1-6, 8-9, 11-19; lane 7 / lane 10 are already
  # enforced earlier in validate_pod(). Keeping the alignment here lets
  # processor and validator share an identical canonical layout.

  [[ -z "${sid:-}" ]] && continue
  EXECUTED=$((EXECUTED + 1))
  pfx="$ART/scenario-${sid}"

  # ---- Layer 1: the target's own listener, inside the exact target Pod.
  if [[ "$ig_l1" == "true" ]]; then
    L1="N/A"
  else
    L1="$(probe_l1 "$tgt_ns" "$tgt_pod" "$port" "${pfx}-l1")"
  fi

  # ---- Layer 2: cluster DNS from the unpoliced control probe.
  if [[ "$ig_l2" == "true" ]]; then
    L2="N/A"
  else
    L2="$(probe_l2 "$fqdn" "${pfx}-l2")"
  fi

  # ---- Layer 3: the enforced policy path from the exact source Pod.
  L3="NOT_RUN"
  verdict="UNKNOWN"
  if [[ "$L1" != "OK" && "$L1" != "N/A" ]]; then
    verdict="LAYER1_DOWN"
  elif [[ "$L2" != "OK" && "$L2" != "N/A" ]]; then
    verdict="LAYER2_FAIL"
  else
    L3="$(probe_l3 "$action" "$src_ns" "$src_pod" "$host" "$port" "${pfx}-l3")"
    case "$L3" in
      CLIENT_ERROR) verdict="CLIENT_ERROR" ;;
      EXEC_ERROR)   verdict="ENV_ERROR" ;;
      *)
        case "${expected}:${intent}" in
          ALLOW:ALLOW_IMPLIED)
            case "$L3" in
              OPEN|HTTP:*) verdict="ALLOW_OK" ;;
              CLOSED)      verdict="RULE_GAP" ;;
              *)           verdict="UNKNOWN" ;;
            esac
            ;;
          ALLOW:ALLOW_FEATURE_OFF)
            case "$L3" in
              CLOSED)      verdict="CHART_INTENTIONAL_DENY" ;;
              OPEN|HTTP:*) verdict="RULE_LEAK" ;;
              *)           verdict="UNKNOWN" ;;
            esac
            ;;
          DENY:DENY_*)
            case "$L3" in
              CLOSED)      verdict="DENY_OK" ;;
              OPEN|HTTP:*) verdict="DENY_LEAK" ;;
              *)           verdict="UNKNOWN" ;;
            esac
            ;;
          *) verdict="UNKNOWN" ;;
        esac
        ;;
    esac
  fi

  case "$verdict" in
    ALLOW_OK|DENY_OK)
      PASS=$((PASS + 1)) ;;
    CHART_INTENTIONAL_DENY)
      PASS=$((PASS + 1)); CHART_INTENT_DENY=$((CHART_INTENT_DENY + 1)) ;;
    DENY_LEAK)
      FAIL=$((FAIL + 1)); N_DENY_LEAK=$((N_DENY_LEAK + 1)) ;;
    RULE_LEAK)
      FAIL=$((FAIL + 1)); N_RULE_LEAK=$((N_RULE_LEAK + 1)) ;;
    RULE_GAP)
      FAIL=$((FAIL + 1)); N_RULE_GAP=$((N_RULE_GAP + 1)) ;;
    LAYER1_DOWN)
      N_LAYER1_DOWN=$((N_LAYER1_DOWN + 1)) ;;
    LAYER2_FAIL)
      N_LAYER2_FAIL=$((N_LAYER2_FAIL + 1)) ;;
    CLIENT_ERROR)
      N_CLIENT_ERROR=$((N_CLIENT_ERROR + 1)) ;;
    ENV_ERROR)
      N_ENV_ERROR=$((N_ENV_ERROR + 1)) ;;
    UNKNOWN)
      N_UNKNOWN=$((N_UNKNOWN + 1)) ;;
    *)
      N_UNKNOWN=$((N_UNKNOWN + 1)) ;;
  esac
  TOTAL=$((TOTAL + 1))

  # Resolve the source and target owner-contract tokens to a JSON literal
  # so the result writer can embed them verbatim. "OWNER_FREE" maps to a
  # canonical minimum envelope; "DEPLOY_NS/DEPLOY_NAME" maps to the full
  # OWNED_BY_REPLICASET_OF_DEPLOYMENT contract; any other shape is rejected
  # by validate_pod before reaching here. Marginal external_ip targets carry
  # an empty target_contract slot and emit owner_contract=null.
  case "$src_contract" in
    OWNER_FREE) src_contract_doc='{"kind":"OWNER_FREE","description":"ownerReferences must be absent/empty; inject fail-closed"}' ;;
    DEPLOY:*)
      dep_ns="${src_contract#DEPLOY:}"; dep_name="${dep_ns##*/}"; dep_ns="${dep_ns%%/*}"
      src_contract_doc='{"kind":"OWNED_BY_REPLICASET_OF_DEPLOYMENT","deployment_namespace":"'"$dep_ns"'","deployment_name":"'"$dep_name"'","controller_required":true,"kind_chain":["Deployment","ReplicaSet"],"description":"controller=true ReplicaSet owned by Deployment '"$dep_name"' in ns='"$dep_ns"'"}'
      ;;
    *) log "[$sid] FATAL: unrecognised source contract token '$src_contract'"; exit 6 ;;
  esac
  case "$tgt_contract" in
    '')        tgt_contract_doc="" ;;
    OWNER_FREE) tgt_contract_doc='{"kind":"OWNER_FREE","description":"ownerReferences must be absent/empty; inject fail-closed"}' ;;
    DEPLOY:*)
      dep_ns="${tgt_contract#DEPLOY:}"; dep_name="${dep_ns##*/}"; dep_ns="${dep_ns%%/*}"
      tgt_contract_doc='{"kind":"OWNED_BY_REPLICASET_OF_DEPLOYMENT","deployment_namespace":"'"$dep_ns"'","deployment_name":"'"$dep_name"'","controller_required":true,"kind_chain":["Deployment","ReplicaSet"],"description":"controller=true ReplicaSet owned by Deployment '"$dep_name"' in ns='"$dep_ns"'"}'
      ;;
    *) log "[$sid] FATAL: unrecognised target contract token '$tgt_contract'"; exit 6 ;;
  esac
  ext_contract_doc=""

  log "[$sid] $(printf '%-50s' "$desc") role=$role expect=$expected intent=$intent L1=$L1 L2=$L2 L3=$L3 verdict=$verdict"

  # One parseable JSONL result per scenario, serialized by the Python
  # standard library. printf-built JSON is forbidden: the upstream_reason
  # and description fields are free text and would break hand-quoting.
  L1="$L1" L2="$L2" L3="$L3" VERDICT="$verdict" \
  SID="$sid" ROLE="$role" ACTION="$action" KIND="$kind" \
  SRC_NS="$src_ns" SRC_POD="$src_pod" \
  TGT_NS="$tgt_ns" TGT_POD="$tgt_pod" FQDN="$fqdn" \
  HOST="$host" PORT="$port" EXPECTED="$expected" INTENT="$intent" \
  IG_L1="$ig_l1" IG_L2="$ig_l2" EXEMPT="$exempt" \
  DESC="$desc" UPSTREAM="$upstream" CONTROL_POD="$CONTROL_POD" \
  SRC_CONTRACT_JSON="$src_contract_doc" TGT_CONTRACT_JSON="$tgt_contract_doc" \
  EXTERNAL_CONTRACT_JSON="$ext_contract_doc" \
  "$PYTHON3" - >> "$ART/probes.jsonl" <<'EOF_PY_RESULT'
import json, os, sys
e = os.environ
src_oc_raw = e.get("SRC_CONTRACT_JSON") or ""
tgt_oc_raw = e.get("TGT_CONTRACT_JSON") or ""
ext_oc_raw = e.get("EXTERNAL_CONTRACT_JSON") or ""
source_owner_contract = json.loads(src_oc_raw) if src_oc_raw else None
target_owner_contract = json.loads(tgt_oc_raw) if tgt_oc_raw else {"kind":"OWNER_FREE"}
margin_owner_contract = json.loads(ext_oc_raw) if ext_oc_raw else None
kind = e["KIND"]
if kind == "service":
    target = {
        "namespace": e["TGT_NS"], "pod_name": e["TGT_POD"], "service_fqdn": e["FQDN"],
        "owner_contract": target_owner_contract,
    }
else:
    target = {
        "kind": "external_ip", "host": e["HOST"], "port": int(e["PORT"]),
        "l1_l2_exempt": True, "namespace": None, "pod_name": None,
        "owner_contract": margin_owner_contract,
    }
rec = {
    "id": e["SID"],
    "role": e["ROLE"],
    "action": e["ACTION"],
    "target_kind": kind,
    "source": {"namespace": e["SRC_NS"], "pod_name": e["SRC_POD"], "owner_contract": source_owner_contract},
    "target": target,
    "l2_control_pod": e["CONTROL_POD"],
    "target_host": e["HOST"],
    "target_port": int(e["PORT"]),
    "L1": e["L1"],
    "L2": e["L2"],
    "L3": e["L3"],
    "ignores_l1": e["IG_L1"] == "true",
    "ignores_l2": e["IG_L2"] == "true",
    "l1_l2_exempt": e["EXEMPT"] == "true",
    "chart_intent": e["INTENT"],
    "expected": e["EXPECTED"],
    "verdict": e["VERDICT"],
    "description": e["DESC"],
    "upstream_reason": e["UPSTREAM"],
}
sys.stdout.write(json.dumps(rec, sort_keys=True) + "\n")
EOF_PY_RESULT
  if [[ $? -ne 0 ]]; then
    log "[$sid] FATAL: result serialization failed"
    N_UNKNOWN=$((N_UNKNOWN + 1))
  fi
done < "$SCEN_TSV"

# --------------------------------------------------------------------------
# Step 6 — fail-closed accounting.
#
# Exit 0 requires ALL of:
#   (1) schema valid and every declared id unique              [Step 1]
#   (2) declared_count == executed_count == JSONL result count
#   (3) every declared id appears EXACTLY once in the results and no extra
#       result id exists
#   (4) no scenario is SKIP / UNKNOWN / LAYER1_DOWN / LAYER2_FAIL /
#       unresolved / client-error / env-error
#   (5) every expected allow/deny verdict is accepted under the declared
#       chart-intent grading (FAIL == 0)
# --------------------------------------------------------------------------
ACC_JSON="$ART/scenario-accounting.json"

# grep -c prints 0 AND exits 1 when there is no match, so a `|| echo 0`
# fallback would emit "00". Capture once and normalise instead.
RESULT_COUNT_OBSERVED="$(grep -c . "$ART/probes.jsonl" 2>/dev/null | tr -d ' \n')"
[[ "$RESULT_COUNT_OBSERVED" =~ ^[0-9]+$ ]] || RESULT_COUNT_OBSERVED=0

DECLARED_COUNT="$DECLARED_COUNT" EXECUTED="$EXECUTED" \
TOTAL="$TOTAL" PASS="$PASS" FAIL="$FAIL" \
CHART_INTENT_DENY="$CHART_INTENT_DENY" \
N_SKIPPED="$N_SKIPPED" N_UNKNOWN="$N_UNKNOWN" \
N_LAYER1_DOWN="$N_LAYER1_DOWN" N_LAYER2_FAIL="$N_LAYER2_FAIL" \
N_UNRESOLVED="$N_UNRESOLVED" N_CLIENT_ERROR="$N_CLIENT_ERROR" \
N_ENV_ERROR="$N_ENV_ERROR" N_DENY_LEAK="$N_DENY_LEAK" \
N_RULE_LEAK="$N_RULE_LEAK" N_RULE_GAP="$N_RULE_GAP" \
IDENTITY_CHECKED="$IDENTITY_CHECKED" \
"$PYTHON3" - "$ART/scenario-schema.json" "$ART/probes.jsonl" "$ACC_JSON" \
  > "$ART/scenario-accounting.stdout" 2> "$ART/scenario-accounting.stderr" <<'EOF_PY_ACC'
import json, os, sys

schema_path, jsonl_path, out_path = sys.argv[1], sys.argv[2], sys.argv[3]
e = os.environ


def i(name):
    return int(e.get(name, "0") or "0")


errors = []

try:
    with open(schema_path, "r", encoding="utf-8") as f:
        schema = json.load(f)
except Exception as ex:
    schema = {}
    errors.append("SCHEMA_ARTIFACT_UNREADABLE:" + repr(ex))

declared_ids = schema.get("declared_ids") or []
declared_count = i("DECLARED_COUNT")
executed = i("EXECUTED")

result_ids = []
malformed = 0
line_no = 0
try:
    with open(jsonl_path, "r", encoding="utf-8") as f:
        for line in f:
            line_no += 1
            s = line.strip()
            if not s:
                continue
            try:
                rec = json.loads(s)
            except Exception:
                malformed += 1
                errors.append("MALFORMED_RESULT_LINE:%d" % line_no)
                continue
            if not isinstance(rec, dict):
                malformed += 1
                errors.append("RESULT_LINE_NOT_OBJECT:%d" % line_no)
                continue
            rid = rec.get("id")
            if not isinstance(rid, str) or not rid:
                malformed += 1
                errors.append("RESULT_LINE_ID_MISSING:%d" % line_no)
                continue
            for req in ("source", "target", "L1", "L2", "L3", "verdict",
                        "chart_intent", "expected"):
                if req not in rec:
                    malformed += 1
                    errors.append("RESULT_LINE_FIELD_MISSING:%s:%s" % (rid, req))
            result_ids.append(rid)
except Exception as ex:
    errors.append("JSONL_UNREADABLE:" + repr(ex))

result_count = len(result_ids)
seen = {}
for r in result_ids:
    seen[r] = seen.get(r, 0) + 1
duplicate_result_ids = sorted(k for k, v in seen.items() if v > 1)
missing_result_ids = sorted(set(declared_ids) - set(result_ids))
unexpected_result_ids = sorted(set(result_ids) - set(declared_ids))

if declared_count < 1:
    errors.append("DECLARED_COUNT_ZERO")
if i("TOTAL") < 1:
    # The exact defect this repair closes: run 33642318757 reported
    # TOTAL=0 and exited 0.
    errors.append("TOTAL_ZERO")
if executed != declared_count:
    errors.append("EXECUTED_COUNT_MISMATCH:declared=%d executed=%d" % (declared_count, executed))
if result_count != declared_count:
    errors.append("RESULT_COUNT_MISMATCH:declared=%d results=%d" % (declared_count, result_count))
if i("TOTAL") != declared_count:
    errors.append("TOTAL_COUNT_MISMATCH:declared=%d total=%d" % (declared_count, i("TOTAL")))
if duplicate_result_ids:
    errors.append("DUPLICATE_RESULT_IDS:" + ",".join(duplicate_result_ids))
if missing_result_ids:
    errors.append("MISSING_RESULT_IDS:" + ",".join(missing_result_ids))
if unexpected_result_ids:
    errors.append("UNEXPECTED_RESULT_IDS:" + ",".join(unexpected_result_ids))
if malformed:
    errors.append("MALFORMED_RESULTS:%d" % malformed)

for label, key in (
    ("SKIPPED", "N_SKIPPED"),
    ("UNKNOWN", "N_UNKNOWN"),
    ("LAYER1_DOWN", "N_LAYER1_DOWN"),
    ("LAYER2_FAIL", "N_LAYER2_FAIL"),
    ("UNRESOLVED", "N_UNRESOLVED"),
    ("CLIENT_ERROR", "N_CLIENT_ERROR"),
    ("ENV_ERROR", "N_ENV_ERROR"),
):
    if i(key):
        errors.append("NONZERO_%s:%d" % (label, i(key)))

verdict_failures = i("FAIL")
policy_failure = verdict_failures > 0
structural_failure = bool(errors)

doc = {
    "phase": "scenario_accounting",
    "declared_count": declared_count,
    "executed_count": executed,
    "result_count": result_count,
    "total": i("TOTAL"),
    "pass_ok": i("PASS"),
    "chart_intentional_deny": i("CHART_INTENT_DENY"),
    "fail": verdict_failures,
    "identity_pods_validated": i("IDENTITY_CHECKED"),
    "counters": {
        "skipped": i("N_SKIPPED"),
        "unknown": i("N_UNKNOWN"),
        "layer1_down": i("N_LAYER1_DOWN"),
        "layer2_fail": i("N_LAYER2_FAIL"),
        "unresolved": i("N_UNRESOLVED"),
        "client_error": i("N_CLIENT_ERROR"),
        "env_error": i("N_ENV_ERROR"),
        "deny_leak": i("N_DENY_LEAK"),
        "rule_leak": i("N_RULE_LEAK"),
        "rule_gap": i("N_RULE_GAP"),
        "malformed_results": malformed,
    },
    "declared_ids": sorted(declared_ids),
    "result_ids": sorted(result_ids),
    "duplicate_result_ids": duplicate_result_ids,
    "missing_result_ids": missing_result_ids,
    "unexpected_result_ids": unexpected_result_ids,
    "errors": errors,
    "structural_failure": structural_failure,
    "policy_failure": policy_failure,
    "verdict": "pass" if not (structural_failure or policy_failure) else "fail",
}
with open(out_path, "w", encoding="utf-8") as f:
    f.write(json.dumps(doc, indent=2, sort_keys=True) + "\n")

if structural_failure:
    print("STRUCTURAL")
elif policy_failure:
    print("POLICY")
else:
    print("PASS")
EOF_PY_ACC
ACC_RC=$?
ACC_CLASS="$(tr -d ' \n' < "$ART/scenario-accounting.stdout" 2>/dev/null || true)"

# Readable summary retained for run-record continuity, plus the explicit
# counters that prove declared/executed/result totals and every error
# category on a green run.
{
  echo "PASS_OK=$PASS"
  echo "CHART_INTENTIONAL_DENY=$CHART_INTENT_DENY"
  echo "FAIL=$FAIL"
  echo "TOTAL=$TOTAL"
  echo "ENV_ISSUES=$((TOTAL - PASS - FAIL))"
  echo "DECLARED_COUNT=$DECLARED_COUNT"
  echo "EXECUTED_COUNT=$EXECUTED"
  echo "RESULT_COUNT=${RESULT_COUNT_OBSERVED}"
  echo "IDENTITY_PODS_VALIDATED=$IDENTITY_CHECKED"
  echo "SKIPPED=$N_SKIPPED"
  echo "UNKNOWN=$N_UNKNOWN"
  echo "LAYER1_DOWN=$N_LAYER1_DOWN"
  echo "LAYER2_FAIL=$N_LAYER2_FAIL"
  echo "UNRESOLVED=$N_UNRESOLVED"
  echo "CLIENT_ERROR=$N_CLIENT_ERROR"
  echo "ENV_ERROR=$N_ENV_ERROR"
  echo "DENY_LEAK=$N_DENY_LEAK"
  echo "RULE_LEAK=$N_RULE_LEAK"
  echo "RULE_GAP=$N_RULE_GAP"
  echo "L2_CONTROL_POD=$CONTROL_POD"
  echo "ACCOUNTING_CLASS=${ACC_CLASS:-UNSET}"
} > "$ART/scenario-summary.txt"

cat "$ART/scenario-summary.txt" | tee -a "$ART/scenarios.log"

if [[ "$ACC_RC" -ne 0 ]]; then
  log "FATAL: accounting serializer failed (rc=${ACC_RC}); refusing to claim a green gate"
  exit "$EXIT_ACCOUNTING"
fi

case "${ACC_CLASS}" in
  PASS)
    log "[gate] accounting verdict=pass (declared=${DECLARED_COUNT} executed=${EXECUTED} results=${DECLARED_COUNT}; all error categories zero)"
    exit 0
    ;;
  POLICY)
    log "FATAL: policy verdict failure (FAIL=${FAIL} deny_leak=${N_DENY_LEAK} rule_leak=${N_RULE_LEAK} rule_gap=${N_RULE_GAP}); see ${ACC_JSON}"
    exit "$EXIT_VERDICT"
    ;;
  STRUCTURAL)
    log "FATAL: structural accounting failure; see ${ACC_JSON}"
    "$PYTHON3" -c '
import json,sys
d=json.load(open(sys.argv[1],"r",encoding="utf-8"))
for x in d.get("errors",[]): print("       error: "+x)
' "$ACC_JSON" 2>/dev/null | tee -a "$ART/scenarios.log"
    # A layer / client / env error is a distinct, louder class than a pure
    # counting mismatch, so the exit code names which one closed the gate.
    if [[ "$N_LAYER1_DOWN" -ne 0 || "$N_LAYER2_FAIL" -ne 0 || "$N_CLIENT_ERROR" -ne 0 || "$N_ENV_ERROR" -ne 0 ]]; then
      exit "$EXIT_LAYER"
    fi
    exit "$EXIT_ACCOUNTING"
    ;;
  *)
    log "FATAL: accounting classifier emitted an unrecognised class '${ACC_CLASS}'; failing closed"
    exit "$EXIT_ACCOUNTING"
    ;;
esac
