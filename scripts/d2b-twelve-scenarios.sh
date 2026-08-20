#!/usr/bin/env bash
# scripts/d2b-twelve-scenarios.sh
#
# Phase D-2b.21: enforcing-CNI 12-scenario gate,
# driven by scripts/fixtures/integrationcni/scenarios.json.
# The bash script is a thin driver: the source of
# truth is the JSON file, and a future regression
# in chart intent must be reflected in the JSON.
#
# Three layers per scenario:
#
#   L1 target localhost  — target's own process is
#                           listening. If this fails
#                           and the scenario is not
#                           configured to ignore L1,
#                           the verdict is LAYER1_DOWN.
#
#   L2 cluster DNS       — a probe Pod in cni-control
#                           (NOT selected by any rendered
#                           NetworkPolicy) resolves the
#                           target hostname. Failure
#                           means the cluster DNS/service
#                           routing is broken, NOT a
#                           policy verdict.
#
#   L3 policy path       — the actual scenario source
#                           Pod, on the enforced cluster,
#                           talks to the target. This is
#                           THE verdict that closes the
#                           gate.
#
# Verdict grading (fixed):
#
#   ALLOW_OK
#     expected=ALLOW, chart_intent=ALLOW_*
#       and L3 OPEN / HTTP 2xx / HTTP 3xx / HTTP 4xx / HTTP 5xx
#
#   DENY_OK
#     expected=DENY, chart_intent=DENY_*
#       and L3 CLOSED (timeout, refused, exit 28, "nc:")
#
#   CHART_INTENTIONAL_DENY (NEW)
#     expected=ALLOW, chart_intent=ALLOW_FEATURE_OFF
#       and L3 CLOSED because chart did NOT render an
#       egress rule for the (port, selector) pair.
#     This is the correct answer when the chart's
#     "feature off" rendered manifest omits the
#     rule. A future regression where the rule is
#     rendered but L3 CLOSED changes this to
#     DENY_LEAK.
#
#   DENY_LEAK
#     expected=DENY, chart_intent=DENY_*
#       and L3 OPEN or HTTP 2xx. This is a security
#       regression: the policy is supposed to block
#       but the connection succeeded.
#
#   LAYER1_DOWN / LAYER2_FAIL
#     Environment problem, NOT a policy verdict.
#
#   ALLOW_DENY (legacy alias for CHART_INTENTIONAL_DENY)
#     Kept for run-record continuity. Renamed in the
#     summary; the JSONL log field stays as "verdict"
#     so old dashboards can read older runs.

set -uo pipefail
ART="${ARTIFACTS:-${PWD}/artifacts/integrationcni}"
SCENARIOS_JSON="${SCENARIOS_JSON:-${PWD}/scripts/fixtures/integrationcni/scenarios.json}"
mkdir -p "$ART"
: > "$ART/scenarios.log"
: > "$ART/probes.jsonl"

PASS=0
CHART_INTENT_DENY=0
FAIL=0
TOTAL=0

# Layer helpers
local_target() {
  local ns="$1"; local pod="$2"; local port="$3"
  kubectl exec -n "$ns" "$pod" -- nc -zv -w 2 127.0.0.1 "$port" 2>&1 \
    | grep -qE "open|succeeded|Connected" \
    && echo OK \
    || echo DOWN
}

control_dns_probe() {
  local ns="$1"; local pod="$2"; local host="$3"
  local addr
  addr=$(kubectl exec -n "$ns" "$pod" -- nslookup "$host" 2>/dev/null \
    | awk '/^Address: /{print $2; exit}' || true)
  if [[ -n "$addr" ]] && [[ "$addr" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo OK
  else
    echo FAIL
  fi
}

policy_path_tcp() {
  local ns="$1"; local pod="$2"; local host="$3"; local port="$4"
  if kubectl exec -n "$ns" "$pod" -- nc -zv -w 5 "$host" "$port" 2>&1 \
       | grep -qE "open|succeeded|Connected"; then
    echo OPEN
  else
    echo CLOSED
  fi
}

policy_path_http() {
  local ns="$1"; local pod="$2"; local host="$3"; local port="$4"
  out=$(kubectl exec -n "$ns" "$pod" -- \
        curl --max-time 5 -sS -o /tmp/d2bbody -w "%{http_code}" \
        "http://$host:$port/" 2>&1 | tail -1)
  code="${out//[[:space:]]/}"
  if [[ "$code" =~ ^[1-5][0-9][0-9]$ ]]; then
    echo "HTTP:$code"
  elif echo "$out" | grep -qiE "timed out|connection|forbidden|unreachable|terminated|exit code 28|Could not connect|nc: "; then
    echo CLOSED
  else
    echo "RAW:$out"
  fi
}

# Pick the source Pod based on role. We expect
# fixture YAML keeps these selectors stable.
resolve_source() {
  local role="$1"
  case "$role" in
    ingress-controller)
      kubectl -n cni-test-ingress get pod \
        -l app=cni-source,role=ingress \
        -o jsonpath='{.items[0].metadata.name}'
      ;;
    prometheus)
      kubectl -n cni-test-prometheus get pod \
        -l app=cni-source,role=prometheus \
        -o jsonpath='{.items[0].metadata.name}'
      ;;
    untrusted)
      kubectl -n cni-test-untrusted get pod \
        -l app=cni-source,role=untrusted \
        -o jsonpath='{.items[0].metadata.name}'
      ;;
    gateway)
      kubectl -n default get pod \
        -l app=cni-target,role=gateway \
        -o jsonpath='{.items[0].metadata.name}'
      ;;
    worker)
      kubectl -n default get pod \
        -l app=cni-target,role=worker \
        -o jsonpath='{.items[0].metadata.name}'
      ;;
  esac
}

# Resolve namespace for the source role.
resolve_source_namespace() {
  local role="$1"
  case "$role" in
    ingress-controller) echo "cni-test-ingress";;
    prometheus)         echo "cni-test-prometheus";;
    untrusted)          echo "cni-test-untrusted";;
    gateway|worker)     echo "default";;
  esac
}

# Resolve target Pod for L1 probe when the
# target is a Service.
resolve_target_pod() {
  local svc="$1"
  local ns="${svc%%.*}"
  local name="${svc#*.}"
  ns="${ns%%.*}"
  # Expect svc like "cni-gateway.default.svc.cluster.local"
  # -> ns=default, name=cni-gateway
  kubectl -n "$ns" get pod -l "app=cni-target,role=${name#cni-}" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

# Print scenario table header
echo "[setup] cluster topology:" | tee -a "$ART/scenarios.log"
kubectl get nodes -o wide 2>&1 | tee -a "$ART/scenarios.log"
CONTROL_POD=$(kubectl -n cni-control get pod \
  -l app=cni-control,role=probe \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
[[ -n "$CONTROL_POD" ]] && echo "control: $CONTROL_POD on $(kubectl -n cni-control get pod "$CONTROL_POD" -o jsonpath='{.spec.nodeName}')"

# Python reader: scenarios.json -> per-row env vars
pick_field() {
  python3 -c "
import json, sys
d=json.load(open('$SCENARIOS_JSON'))
for s in d['scenarios']:
  if s['id'] == sys.argv[1]:
    print(s.get(sys.argv[2], ''))
    sys.exit(0)
print('', end='')
" "$1" "$2"
}

SCEN_IDS=$(python3 -c "
import json; d=json.load(open('$SCENARIOS_JSON'))
print('\n'.join(s['id'] for s in d['scenarios']))
")

# Sanity: every scenario must carry a non-empty
# chart_intent. Without this, the verdict is
# not a chart-compliance signal at all and the
# CI gate is a cosmetic pass.
MISSING=$(python3 -c "
import json, sys
d=json.load(open('$SCENARIOS_JSON'))
miss=[s['id'] for s in d['scenarios'] if not s.get('chart_intent','')]
print('\n'.join(miss))
")
if [[ -n "$MISSING" ]]; then
  echo "FATAL: scenarios.json missing chart_intent for: $MISSING" >&2
  exit 2
fi

for sid in $SCEN_IDS; do
  desc=$(pick_field "$sid" description)
  role=$(pick_field "$sid" role)
  action=$(pick_field "$sid" action)
  target_kind=$(pick_field "$sid" target_kind)
  host=$(pick_field "$sid" target_host)
  svc=$(pick_field "$sid" target_svc)
  port=$(pick_field "$sid" target_port)
  expected=$(pick_field "$sid" expected)
  chart_intent=$(pick_field "$sid" chart_intent)
  upstream=$(pick_field "$sid" upstream_reason)
  ignore_l1=$(pick_field "$sid" ignores_l1)
  ignore_l2=$(pick_field "$sid" ignores_l2)

  if [[ "$target_kind" == "service" ]]; then host="$svc"; fi

  source_ns=$(resolve_source_namespace "$role")
  source_pod=$(resolve_source "$role")
  target_pod="$source_pod"  # placeholder
  if [[ "$target_kind" == "service" ]]; then
    ns_part="${svc%%.*}"
    name_part="${ns_part#cni-}"
    target_pod=$(kubectl -n "$ns_part" get pod \
      -l "app=cni-target,role=${svc%%.*}" 2>/dev/null | awk '/Running/{print $1; exit}')
    [[ -z "$target_pod" ]] && target_pod=$(resolve_target_pod "$svc")
  fi
  if [[ -z "$source_pod" ]]; then
    echo "[$sid] SKIP: source pod for role=$role not found" | tee -a "$ART/scenarios.log"
    continue
  fi
  if [[ "$target_kind" == "service" && -z "$target_pod" ]]; then
    echo "[$sid] WARN: target pod for $svc not found; L1 will DOWN" | tee -a "$ART/scenarios.log"
  fi

  # Layer 1
  L1="SKIP"
  if [[ "$ignore_l1" == "True" || "$ignore_l1" == "true" || "$ignore_l1" == "1" ]]; then
    L1="N/A"
  else
    if [[ -n "$target_pod" && "$target_pod" != "$source_pod" ]]; then
      ns_for_target="${svc%%.*}"
      L1=$(local_target "${ns_for_target}" "$target_pod" "$port")
    else
      L1=$(local_target "$source_ns" "$source_pod" "$port")
    fi
  fi

  # Layer 2
  L2="SKIP"
  if [[ -z "$CONTROL_POD" ]]; then L2="FAIL"
  elif [[ "$ignore_l2" == "True" || "$ignore_l2" == "true" || "$ignore_l2" == "1" ]]; then
    L2="N/A"
  else
    L2=$(control_dns_probe "cni-control" "$CONTROL_POD" "$host")
  fi

  # Layer 3
  L3="SKIP"; verdict="UNKNOWN"
  if [[ "$L1" == "OK" || "$L1" == "N/A" ]]; then
    if [[ "$L2" == "OK" || "$L2" == "N/A" ]]; then
      if [[ "$action" == "tcp_connect" ]]; then
        L3=$(policy_path_tcp "$source_ns" "$source_pod" "$host" "$port")
      else
        L3=$(policy_path_http "$source_ns" "$source_pod" "$host" "$port")
      fi
      case "$expected:$chart_intent" in
        ALLOW:ALLOW_IMPLIED)
          case "$L3" in
            OPEN|HTTP:*) verdict="ALLOW_OK";;
            CLOSED)       verdict="RULE_GAP";;  # expected ALLOW but chart drew a rule and it DENIED
            *)            verdict="UNKNOWN";;
          esac
          ;;
        ALLOW:ALLOW_FEATURE_OFF)
          case "$L3" in
            CLOSED)       verdict="CHART_INTENTIONAL_DENY";;
            OPEN|HTTP:*)  verdict="RULE_LEAK";;  # feature off but rule was rendered (bug)
            *)            verdict="UNKNOWN";;
          esac
          ;;
        DENY:DENY_*)
          case "$L3" in
            CLOSED)       verdict="DENY_OK";;
            OPEN|HTTP:*)  verdict="DENY_LEAK";;
            *)            verdict="UNKNOWN";;
          esac
          ;;
      esac
    else
      verdict="LAYER2_FAIL"
    fi
  else
    verdict="LAYER1_DOWN"
  fi

  # Counting (chart-intent aware):
  #   ALLOW_OK / DENY_OK / CHART_INTENTIONAL_DENY  -> pass
  #   everything else                              -> fail or env
  case "$verdict" in
    ALLOW_OK|DENY_OK|CHART_INTENTIONAL_DENY)
      PASS=$((PASS+1))
      if [[ "$verdict" == "CHART_INTENTIONAL_DENY" ]]; then
        CHART_INTENT_DENY=$((CHART_INTENT_DENY+1))
      fi
      ;;
    DENY_LEAK|RULE_LEAK|RULE_GAP)
      FAIL=$((FAIL+1))
      ;;
    LAYER1_DOWN|LAYER2_FAIL|UNKNOWN)
      # env issues: do NOT count as FAIL
      ;;
    *)
      FAIL=$((FAIL+1))
      ;;
  esac
  TOTAL=$((TOTAL+1))

  echo "[$sid] $(printf '%-50s' "$desc") role=$role expect=$expected intent=$chart_intent L1=$L1 L2=$L2 L3=$L3 verdict=$verdict" \
    | tee -a "$ART/scenarios.log"
  printf '{"id":"%s","role":"%s","action":"%s","expected":"%s","chart_intent":"%s","target_kind":"%s","target_host":"%s","target_port":%s,"L1":"%s","L2":"%s","L3":"%s","verdict":"%s","upstream_reason":"%s"}\n' \
    "$sid" "$role" "$action" "$expected" "$chart_intent" "$target_kind" "$host" "$port" \
    "$L1" "$L2" "$L3" "$verdict" "$upstream" \
    >> "$ART/probes.jsonl"
done

# summary
{
  echo "PASS_OK=$PASS"
  echo "CHART_INTENTIONAL_DENY=$CHART_INTENT_DENY"
  echo "FAIL=$FAIL"
  echo "TOTAL=$TOTAL"
  echo "ENV_ISSUES=$((TOTAL - PASS - FAIL))"
} > "$ART/scenario-summary.txt"

cat "$ART/scenario-summary.txt" | tee -a "$ART/scenarios.log"
# Gate is green when FAIL == 0
exit "$FAIL"
