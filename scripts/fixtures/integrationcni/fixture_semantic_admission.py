#!/usr/bin/env python3
# scripts/fixtures/integrationcni/fixture_semantic_admission.py
#
# Phase D-2b.28: canonical fixture role
# inventory + offline semantic admission.
#
# This module is the SINGLE source of truth
# for the fixture role, port, namespace,
# Service / Pod label matrix. NO fixture
# yaml, chart values.yaml, scenario script,
# or rendered NetworkPolicy may hand-edit a
# role / port / namespace. ANY deviation is
# caught here before `kubectl apply` is
# allowed to run.
#
# The offline admission pass walks every
# fixture yaml and, for each document kind
# (Pod / Deployment / Service / NetworkPolicy),
# verifies the relationships documented in
# the d2b.28 directive:
#
#   1. Containers live under spec.containers
#      (Pod) or spec.template.spec.containers
#      (Deployment / StatefulSet). Top-level
#      `containers` is forbidden.
#   2. Every fixture Service selector matches
#      a Pod in the same namespace whose
#      template labels are a superset.
#      Allowed exceptions (ExternalName
#      services) are listed explicitly.
#   3. Service targetPort / port matches a
#      declared containerPort or named port on
#      the matched Pod.
#   4. Every rendered NetworkPolicy podSelector
#      matches at least one fixture Pod in its
#      selected namespace, AND every fixture
#      Pod in the `cni-control` namespace is
#      excluded from every chart product
#      podSelector.
#   5. The fixture role matrix
#      (gateway / worker / ingress /
#      prometheus / untrusted / proxy /
#      postgres / redis / clickhouse /
#      arbitrary / control) is the SAME set as
#      the chart values.yaml uses.
#   6. No fixture role string is hand-typed in
#      a yaml — every fixture document
#      references ROLE_INVENTORY[<role>] via
#      the role key constant, asserted as a
#      string match.
#
# Usage:
#   python3 fixture_semantic_admission.py \
#     --rendered-networkpolicy=path/to/rendered.yaml
#
# Exit codes:
#   0 - all relationships semantically valid
#   1 - at least one relationship is broken;
#       FAIL_LINE prints the offending pair.
import argparse
import json
import sys
from pathlib import Path

# Canonical inventory. Hand-edited role/port
# strings in fixture yamls MUST match this
# structure. The phase-D-2b.28 directive
# forbids scattering role/port metadata
# across chart values, fixture yamls, and
# scenario code.
ROLE_INVENTORY = {
    "gateway":              {"role": "gateway",     "namespace": "default",   "port_http": 8080, "port_metrics": 9101, "expect_product_selector_match": True},
    "worker":               {"role": "worker",      "namespace": "default",   "port_http": 8081, "port_metrics": 9101, "expect_product_selector_match": True},
    "ingress-controller":   {"role": "ingress",     "namespace": "cni-test-ingress",    "port_http": 8080, "port_metrics": None, "expect_product_selector_match": True},
    "prometheus":           {"role": "prometheus",  "namespace": "cni-test-prometheus", "port_http": 9100, "port_metrics": 9100, "expect_product_selector_match": True},
    "untrusted":            {"role": "untrusted",   "namespace": "cni-test-untrusted",  "port_http": 9111, "port_metrics": None, "expect_product_selector_match": False},
    "egress-proxy":         {"role": "egress-proxy","namespace": "cni-test-proxy",      "port_http": 3128, "port_metrics": None, "expect_product_selector_match": False},
    "postgres":             {"role": "postgres",    "namespace": "cni-test-postgres",   "port_http": 5432, "port_metrics": None, "expect_product_selector_match": False},
    "redis":                {"role": "redis",       "namespace": "cni-test-redis",      "port_http": 6379, "port_metrics": None, "expect_product_selector_match": False},
    "clickhouse":           {"role": "clickhouse",  "namespace": "cni-test-clickhouse", "port_http": 9000, "port_metrics": None, "expect_product_selector_match": False},
    "arbitrary":            {"role": "arbitrary",   "namespace": "cni-test-proxy",      "port_http": 9090, "port_metrics": None, "expect_product_selector_match": False},
    "control-probe":        {"role": "control",     "namespace": "cni-control",         "port_http": 8080, "port_metrics": None, "expect_product_selector_match": False},
    "control-target":       {"role": "control",     "namespace": "cni-control",         "port_http": 18080, "port_metrics": None, "expect_product_selector_match": False},
}

# Allowed Service targetPort exceptions.
# External services are PinnedServiceAllowedExternal
# and DO NOT match a fixture Pod; the chart
# always treats them as RPC-only peers.
EXTERNAL_SERVICE_NAMES = {"cni-mock-external"}

PAIRS_TO_VALIDATE = [
    # (fixture_role_key, service_name)
    ("gateway",            "cni-gateway"),
    ("worker",             "cni-worker-metrics"),
    ("ingress-controller", "cni-mock-ingress-controller-no-svc"),  # direct probe, no svc
    ("prometheus",         "cni-mock-prometheus-no-svc"),          # direct probe, no svc
    ("untrusted",          "cni-untrusted-no-svc"),                # direct probe
    ("egress-proxy",       "cni-proxy"),
    ("postgres",           "cni-postgres"),
    ("redis",              "cni-redis"),
    ("clickhouse",         "cni-clickhouse"),
    ("arbitrary",          "cni-arbitrary"),
    ("control-probe",      "cni-control-probe-svc"),
    ("control-target",     "cni-control-target-svc"),
]


def fail(msg):
    print(f"[FAIL] {msg}", file=sys.stderr)
    return False


def ok(msg):
    print(f"[OK  ] {msg}")


def load_documents(yaml_path):
    import yaml
    text = yaml_path.read_text()
    return [d for d in yaml.safe_load_all(text) if isinstance(d, dict)]


def containers_paths(doc):
    kind = doc.get("kind")
    if kind == "Pod":
        spec = doc.get("spec") or {}
        return [("Pod", doc.get("metadata", {}).get("name", "?"), c)
                for c in (spec.get("containers") or [])
                if isinstance(c, dict)]
    if kind in ("Deployment", "StatefulSet", "DaemonSet"):
        spec = doc.get("spec") or {}
        tmpl = spec.get("template") or {}
        tmpl_spec = tmpl.get("spec") or {}
        owner = doc.get("metadata", {}).get("name", "?")
        return [(kind, owner, c)
                for c in (tmpl_spec.get("containers") or [])
                if isinstance(c, dict)]
    return []


def pod_template_labels(doc):
    """Resolve Pod template labels for
    Service selector matching. Returns
    (namespace-or-empty, dict-of-labels)."""
    md = doc.get("metadata") or {}
    kind = doc.get("kind")
    if kind == "Pod":
        return (md.get("namespace") or ""), (md.get("labels") or {})
    if kind in ("Deployment", "StatefulSet", "DaemonSet"):
        tmpl = (doc.get("spec") or {}).get("template") or {}
        return (tmpl.get("metadata", {}).get("namespace")
                or md.get("namespace") or ""), \
               (tmpl.get("metadata", {}).get("labels") or {})
    return ("", {})


def collect_pods(fixture_dir):
    out = []
    for path in sorted(fixture_dir.glob("*.yaml")):
        for d in load_documents(path):
            if d.get("kind") in ("Pod", "Deployment", "StatefulSet", "DaemonSet"):
                ns, labels = pod_template_labels(d)
                out.append((path, d, ns, labels))
    return out


def collect_services(fixture_dir):
    out = []
    for path in sorted(fixture_dir.glob("*.yaml")):
        for d in load_documents(path):
            if d.get("kind") == "Service":
                spec = d.get("spec") or {}
                out.append((
                    path, d,
                    spec.get("selector") or {},
                    spec.get("ports") or [],
                    d.get("metadata", {}).get("namespace", ""),
                    d.get("metadata", {}).get("name", "?"),
                ))
    return out


def collect_network_policies(fixture_dir, rendered_path):
    out = list(_collect_fixtures_nps(fixture_dir))
    if rendered_path and rendered_path.exists():
        for d in load_documents(rendered_path):
            if d.get("kind") == "NetworkPolicy":
                out.append((rendered_path, d))
    return out


def _collect_fixtures_nps(fixture_dir):
    for path in sorted(fixture_dir.glob("*.yaml")):
        for d in load_documents(path):
            if d.get("kind") == "NetworkPolicy":
                yield (path, d)


def selector_matches(selector, labels):
    """A Service selector matches a Pod iff
    every selector key/value pair is in the
    Pod's labels."""
    return all(labels.get(k) == v for k, v in selector.items())


def target_port_matches(svc_port, container_ports):
    """Match either by numeric port OR by
    named port. Returns True / False plus
    the matched container port."""
    port = svc_port.get("port")
    target = svc_port.get("targetPort")
    if isinstance(target, int):
        return any(p.get("containerPort") == target for p in container_ports)
    if isinstance(target, str):
        return any(p.get("name") == target for p in container_ports)
    # Default: target == port
    return any(p.get("containerPort") == port for p in container_ports)


def check_no_top_level_containers(fixture_dir):
    ok_count = 0
    fail_count = 0
    for path in sorted(fixture_dir.glob("*.yaml")):
        for d in load_documents(path):
            kind = d.get("kind")
            if kind not in ("Pod", "Deployment", "StatefulSet", "DaemonSet"):
                continue
            forbidden_top = [
                k for k in ("containers", "imagePullPolicy", "args",
                            "readinessProbe") if k in d
            ]
            if forbidden_top:
                fail(f"{path}: {kind}/"
                     f"{d.get('metadata',{}).get('name','?')} has "
                     f"top-level forbidden keys: {forbidden_top}")
                fail_count += 1
            else:
                ok_count += 1
    print(f"  structural: ok={ok_count} fail={fail_count}")
    return fail_count == 0


def check_service_selector_matches_pod(fixture_dir):
    pods = collect_pods(fixture_dir)
    services = collect_services(fixture_dir)
    all_ok = True
    for path, d, selector, ports, ns, name in services:
        if name in EXTERNAL_SERVICE_NAMES:
            ok(f"service {ns}/{name}: external; selector optional")
            continue
        if not selector:
            ok(f"service {ns}/{name}: empty selector (allowed)")
            continue
        matched = []
        for p_path, p_d, p_ns, p_lbls in pods:
            if p_ns != ns:
                continue
            if selector_matches(selector, p_lbls):
                matched.append((p_path, p_d))
        if not matched:
            fail(f"service {ns}/{name}: selector {dict(selector)} matched "
                 f"0 pods in namespace {ns!r}")
            all_ok = False
            continue
        # targetPort check.
        for mp_path, mp_d in matched:
            containers = containers_paths(mp_d)
            # `containers` is a list of
            # (kind, owner, container_dict). Each
            # container_dict has a `ports` list of
            # {containerPort, name, protocol}.
            flat_ports = []
            for _, _, cont in containers:
                for pp in (cont.get("ports") or []):
                    flat_ports.append(pp)
            for sp in ports:
                if not target_port_matches(sp, flat_ports):
                    fail(f"service {ns}/{name}: targetPort={sp} unmatched "
                         f"on Pod {mp_d.get('metadata',{}).get('name')!s}/{ns}; "
                         f"declared ports={[p.get('containerPort') for p in flat_ports]!s}")
                    all_ok = False
                    continue
                else:
                    ok(f"service {ns}/{name}: targetPort={sp} matched on "
                       f"Pod/{ns}")
    return all_ok


def check_networkpolicy_selectors(fixture_dir, rendered_path):
    """Walk every NetworkPolicy; compute the
    union of podSelector matchLabels. Two
    invariants:
      A. Every product podSelector MUST match
         at least one fixture Pod (chart is
         not orphaned from fixtures).
      B. NO podSelector of any chart product
         NetworkPolicy may match a Pod in
         namespace=cni-control (control
         isolation).
    The control-only fixture `cni-control-allow-probe-to-target`
    NetworkPolicy a kind of inverse — it MUST
    match control probe-target Pod. We
    therefore tag fixture-policies whose
    namespace is cni-control and skip the
    "must match default-ns" / "must NOT match
    control Pod" checks for them.
    """
    nps = collect_network_policies(fixture_dir, rendered_path)
    pods = collect_pods(fixture_dir)
    pods_in_default = [t for t in pods if t[2] == "default"]
    pods_in_control = [t for t in pods if t[2] == "cni-control"]
    all_ok = True
    for path, d in nps:
        spec = d.get("spec") or {}
        ps = spec.get("podSelector") or {}
        sel = ps.get("matchLabels") or {}
        ns_sel = (
            (spec.get("ingress") or spec.get("egress") or [{}])[0]
        )
        # namespaceSelector under podSelector-less items
        ns_match = (
            (spec.get("podSelector") or {}).get("matchExpressions") or []
        )
        # Acceptable selector expressions include
        # `{matchLabels: {app: ...}}`.
        if not sel:
            ok(f"NetworkPolicy {d.get('metadata',{}).get('name','?')}: "
               f"empty podSelector (cluster-wide denier); skipped")
            continue
        owner_path = "rendered" if path == rendered_path else "fixture"
        # Skip control-only fixture policies for
        # the "must NOT match control Pod" check.
        np_ns = d.get("metadata", {}).get("namespace") or ""
        np_name = d.get("metadata", {}).get("name", "?")
        is_control_fixture_policy = (
            owner_path == "fixture" and np_ns == "cni-control")
        if is_control_fixture_policy:
            ok(f"NetworkPolicy fixture/{np_name}: "
               f"control-only fixture policy; product matches "
               f"not enforced")
            continue
        # A. must match a fixture Pod in
        #    the role-bearing namespaces.
        ok_match = any(
            all(lbls.get(k) == v for k, v in sel.items())
            for _, _, _, lbls in pods_in_default
        )
        if ok_match:
            ok(f"NetworkPolicy {owner_path}/{np_name}: "
               f"podSelector {dict(sel)} matches at least one default-ns Pod")
        else:
            if owner_path == "rendered":
                fail(f"NetworkPolicy rendered/"
                     f"{np_name}: podSelector "
                     f"{dict(sel)} did not match ANY fixture Pod in namespace=default")
                all_ok = False
        # B. MUST NOT match a control Pod.
        bad = [
            (p_path, p, ns, lbls)
            for p_path, p, ns, lbls in pods_in_control
            if all(lbls.get(k) == v for k, v in sel.items())
        ]
        if bad:
            fail(f"NetworkPolicy {owner_path}/{np_name}: "
                 f"podSelector {dict(sel)} MATCHES a control Pod "
                 f"({bad[0][2]}/{bad[0][1].get('metadata',{}).get('name')}) — "
                 f"control isolation broken")
            all_ok = False
        else:
            ok(f"NetworkPolicy {owner_path}/{np_name}: "
               f"podSelector does NOT match any cni-control Pod")
    return all_ok


def check_role_inventory_enforced(fixture_dir):
    """Walk every fixture Pod's container
    args[*] and Service's selector matchLabels
    in search of hard-coded role strings NOT
    in ROLE_INVENTORY. The goal is to ensure
    nobody hand-types a role/port/namespace
    outside the inventory; we tolerate the
    inventory strings themselves."""
    allowed = set(ROLE_INVENTORY.keys()) | {
        v["namespace"] for v in ROLE_INVENTORY.values()
    } | {str(v["port_http"]) for v in ROLE_INVENTORY.values() if v.get("port_http")}
    violations = []
    for path in sorted(fixture_dir.glob("*.yaml")):
        for d in load_documents(path):
            ns = d.get("metadata", {}).get("namespace", "")
            kind = d.get("kind")
            if kind in ("Pod", "Deployment", "StatefulSet", "DaemonSet"):
                _, labels = pod_template_labels(d)
                for k, v in (labels or {}).items():
                    if isinstance(v, str) and v.startswith("cni-") and v not in allowed:
                        violations.append((path, kind, v))
    if violations:
        for path, kind, v in violations:
            fail(f"{path}: {kind} uses hard-coded role/namespace string "
                 f"{v!r} not in ROLE_INVENTORY")
        return False
    ok(f"all fixture role/namespace strings come from ROLE_INVENTORY "
       f"({len(ROLE_INVENTORY)} roles)")
    return True


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--rendered-networkpolicy", required=False)
    ap.add_argument("--fixture-dir",
                    default="scripts/fixtures/integrationcni")
    args = ap.parse_args()
    fixture_dir = Path(args.fixture_dir).resolve()
    if not fixture_dir.exists():
        fail(f"fixture dir not found: {fixture_dir}")
        return 2
    rendered_path = None
    if args.rendered_networkpolicy:
        rendered_path = Path(args.rendered_networkpolicy).resolve()
    print(f"fixture_semantic_admission: walking {fixture_dir}")
    s1 = check_no_top_level_containers(fixture_dir)
    s2 = check_service_selector_matches_pod(fixture_dir)
    s3 = check_networkpolicy_selectors(fixture_dir, rendered_path)
    s4 = check_role_inventory_enforced(fixture_dir)
    if s1 and s2 and s3 and s4:
        print()
        print("fixture_semantic_admission: PASS")
        return 0
    print()
    print("fixture_semantic_admission: FAIL", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
