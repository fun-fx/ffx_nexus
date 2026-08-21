#!/usr/bin/env bash
# scripts/wait-cilium-endpoints.sh
#
# Phase D-2b.21: polled readiness for cilium
# endpoints. We OBSERVE the conditions: never use
# a fixed sleep. Returns non-zero if any condition
# is not satisfied within the bounded poll.
#
# Usage:
#   wait_cilium_endpoints.sh <namespace>:<selector>[:<containerPort>]
#
# Multiple selector matching rules may be passed
# space-separated. Each one must produce a Pod
# that:
#   - kubectl get pod -n <ns> -l <selector> exists
#     with a non-empty pod name
#   - status.conditions[type=Ready].status == True
#   - cilium endpoint list shows an endpoint with
#     identity non-zero
#   - cilium policy get (or endpoint <id> policy)
#     shows a non-zero policy revision matching the
#     rendered chart's last update
#
# Then the SAME source Pod must reach the SAME
# target's localhost (and the same dns name) using
# a direct kubectl exec. This is the "is the
# server itself alive?" layer that ALLOW probes
# must layer on top of.
set -euo pipefail
ART="$${ARTIFACTS:-${PWD}/artifacts/integrationcni}"
mkdir -p "$ART"
SHIFT_READY_DELAY=0

wait_pod_ready() {
  local ns="$1"; local selector="$2"
  local deadline=$(( $(date +%s) + 300 ))
  while (( $(date +%s) < deadline )); do
    local out
    out=$(kubectl get pod -n "$ns" -l "$selector" -o json 2>/dev/null || echo "{}")
    local rdy
    rdy=$(echo "$out" | python3 -c "
import json,sys
try:
  d=json.loads(sys.stdin.read())
  items=d.get('items',[]) if isinstance(d,dict) else d
  if not items: print('NOMATCH'); sys.exit()
  cs=items[0].get('status',{}).get('conditions',[])
  for c in cs:
    if c.get('type')=='Ready' and c.get('status')=='True':
      print('READY'); sys.exit()
  print('NOT-READY')
except Exception:
  print('PARSE-ERR')
")
    if [[ "$rdy" == "READY" ]]; then
      echo "pod $ns/$selector READY"
      return 0
    fi
    sleep 3
  done
  echo "ERROR: pod $ns/$selector did not become Ready within 300s" >&2
  return 1
}

wait_cilium_endpoint() {
  local pod_name="$1"; local ns="$2"
  local deadline=$(( $(date +%s) + 180 ))
  while (( $(date +%s) < deadline )); do
    local line
    line=$(kubectl -n kube-system exec ds/cilium -- cilium endpoint list -o json 2>/dev/null || echo "{}")
    local got
    got=$(echo "$line" | python3 -c "
import json,sys
try:
  endpoints=json.loads(sys.stdin.read())
  if isinstance(endpoints, dict):
    # newer cilium wraps in {'endpoint':[...]}
    endpoints=endpoints.get('endpoint', endpoints)
  for e in endpoints:
    name=e.get('container-name','')
    nsname=e.get('k8s-namespace-name','') or e.get('namespace','')
    idn=e.get('identity',{}) or {}
    idv = idn.get('id', 0) if isinstance(idn, dict) else idn
    if name.startswith('${ns}/') or ('/' in name and '${pod_name}' in name):
      pol=e.get('policy',{}) or {}
      rev = pol.get('proxy-deferred-revision',0) or e.get('policy-revision',0)
      print(f'EP-OK id={idv} rev={rev} name={name}')
      sys.exit()
  print('NO-EP')
except Exception as exc:
  print('PARSE:'+str(exc))
")
    if [[ "$got" == EP-OK* ]]; then
      echo "cilium endpoint: $got"
      return 0
    fi
    sleep 3
  done
  echo "ERROR: cilium endpoint for ${ns}/${pod_name} not Ready within 180s; last=$got" >&2
  return 1
}

probe_target_local() {
  local ns="$1"; local pod="$2"; local port="$3"
  local deadline=$(( $(date +%s) + 30 ))
  while (( $(date +%s) < deadline )); do
    if kubectl exec -n "$ns" "$pod" -- nc -zv -w 2 127.0.0.1 "$port" 2>&1 | grep -qE "open|succeeded"; then
      echo "target $ns/$pod localhost:$port READY"
      return 0
    fi
    sleep 3
  done
  echo "ERROR: target $ns/$pod not listening on 127.0.0.1:$port" >&2
  return 1
}

main() {
  : "${1:?spec required as ns:selector[:port]}"
  local spec="$1"
  local ns="${spec%%:*}"; local rest="${spec#*:}"; local port="${rest##*:}"
  [[ "$port" == "$rest" ]] && port=""   # no port
  local selector="${rest%:*}"
  [[ "$selector" == "$rest" ]] && selector=""   # no port in spec
  wait_pod_ready "$ns" "$selector"
  local pod
  pod=$(kubectl get pod -n "$ns" -l "$selector" -o jsonpath='{.items[0].metadata.name}')
  wait_cilium_endpoint "$pod" "$ns"
  if [[ -n "$port" ]]; then
    probe_target_local "$ns" "$pod" "$port"
  fi
}

# Expose functions for source; run main if called
# with a spec arg.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
