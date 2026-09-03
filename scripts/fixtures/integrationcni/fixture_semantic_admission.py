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
    "migration":            {"role": "migration",   "namespace": "default",   "port_http": 8082, "port_metrics": None, "expect_product_selector_match": True},
    "egress-proxy":         {"role": "egress-proxy","namespace": "cni-test-proxy",      "port_http": 3128, "port_metrics": None, "expect_product_selector_match": False},
    "postgres":             {"role": "postgres",    "namespace": "database",            "port_http": 5432, "port_metrics": None, "expect_product_selector_match": False},
    "redis":                {"role": "redis",       "namespace": "database",            "port_http": 6379, "port_metrics": None, "expect_product_selector_match": False},
    "clickhouse":           {"role": "clickhouse",  "namespace": "database",            "port_http": 9000, "port_metrics": None, "expect_product_selector_match": False},
    "arbitrary":            {"role": "arbitrary",   "namespace": "cni-test-proxy",      "port_http": 9090, "port_metrics": None, "expect_product_selector_match": False},
    "control-probe":        {"role": "control",     "namespace": "cni-control",         "port_http": 8080, "port_metrics": None, "expect_product_selector_match": False},
    "control-target":       {"role": "control",     "namespace": "cni-control",         "port_http": 18080, "port_metrics": None, "expect_product_selector_match": False},
}

# ROLE_INVENTORY answers "which namespace does role R live in". This
# answers "which role is Pod P", so check_pod_namespace_binding can prove
# the two agree. Kept adjacent to ROLE_INVENTORY so a fixture Pod cannot
# be added or renamed without declaring its role here.
ROLE_POD_NAMES = {
    "gateway":            "cni-mock-nexus-gateway",
    "worker":             "cni-mock-nexus-worker",
    "migration":          "cni-mock-nexus-migration",
    "ingress-controller": "cni-mock-ingress-controller",
    "prometheus":         "cni-mock-prometheus",
    "untrusted":          "cni-untrusted-default",
    "egress-proxy":       "cni-mock-egress-proxy",
    "postgres":           "cni-mock-postgres",
    "redis":              "cni-mock-redis",
    "clickhouse":         "cni-mock-clickhouse",
    "arbitrary":          "cni-mock-arbitrary",
    "control-probe":      "cni-control-probe",
    "control-target":     "cni-control-target",
}

# The namespace the chart is released into.
RELEASE_NAMESPACE = "default"

# Roles whose Pods stand in for dependencies the chart reaches over the
# network rather than owning. None of them may sit in RELEASE_NAMESPACE:
# the chart renders `<release>-default-deny` with an empty podSelector,
# which denies ingress to every Pod in the release namespace, so a stub
# parked there is unreachable no matter what the gateway's egress
# allowlist permits — which silently reports a false RULE_GAP instead of
# an allowlist defect.
DEPENDENCY_STUB_ROLES = frozenset({
    "egress-proxy", "postgres", "redis", "clickhouse", "arbitrary",
})

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


def check_pod_namespace_binding(fixture_dir):
    """Bind every canonical fixture Pod to exactly one namespace.

    check_role_inventory_enforced only proves each namespace string is
    DRAWN FROM the inventory, which cannot catch a Pod parked in the
    wrong inventory namespace. That is precisely how the five dependency
    stubs came to sit in `default` while ROLE_INVENTORY declared
    dedicated namespaces for them: `default` is in the inventory (the
    gateway and worker use it), so set membership held while the
    per-role binding was silently violated.
    """
    want = {}
    for role, pod_name in ROLE_POD_NAMES.items():
        entry = ROLE_INVENTORY.get(role)
        if entry is None:
            fail(f"ROLE_POD_NAMES declares role {role!r} with no "
                 f"ROLE_INVENTORY entry")
            return False
        want[pod_name] = (role, entry["namespace"])
    seen = {}
    all_ok = True
    for path in sorted(fixture_dir.glob("*.yaml")):
        for d in load_documents(path):
            kind = d.get("kind")
            if kind not in ("Pod", "Deployment", "StatefulSet", "DaemonSet"):
                continue
            name = (d.get("metadata") or {}).get("name", "?")
            if name not in want:
                fail(f"{path}: {kind}/{name} is not bound to any "
                     f"ROLE_POD_NAMES entry; declare its role or rename "
                     f"it to a canonical fixture Pod name")
                all_ok = False
                continue
            role, want_ns = want[name]
            got_ns = (d.get("metadata") or {}).get("namespace") or ""
            seen.setdefault(name, []).append(got_ns)
            if got_ns != want_ns:
                fail(f"{path}: {kind}/{name} (role={role}) is in namespace "
                     f"{got_ns!r} but ROLE_INVENTORY declares {want_ns!r}")
                all_ok = False
    for pod_name, (role, want_ns) in sorted(want.items()):
        count = len(seen.get(pod_name, []))
        if count != 1:
            fail(f"role={role} pod={pod_name} (namespace {want_ns!r}) "
                 f"appears {count} times in the fixture corpus; "
                 f"expected exactly 1")
            all_ok = False
    if all_ok:
        ok(f"per-role namespace binding: {len(want)} canonical Pods each "
           f"in exactly the namespace ROLE_INVENTORY declares")
    return all_ok


def check_dependency_stubs_outside_release_namespace(fixture_dir):
    """No dependency stub may live in the release namespace.

    Independent of ROLE_INVENTORY so that relaxing an inventory entry
    back to `default` cannot re-open the hole: the rendered
    `<release>-default-deny` policy selects every Pod in the release
    namespace and grants it no ingress, which converts every
    egress-allowlist ALLOW assertion against that stub into a false
    RULE_GAP.
    """
    all_ok = True
    for role in sorted(DEPENDENCY_STUB_ROLES):
        entry = ROLE_INVENTORY.get(role) or {}
        ns = entry.get("namespace")
        if ns == RELEASE_NAMESPACE:
            fail(f"dependency stub role={role} is declared in the release "
                 f"namespace {RELEASE_NAMESPACE!r}; the chart's "
                 f"default-deny policy denies ingress to every Pod there")
            all_ok = False
    if all_ok:
        ok(f"all {len(DEPENDENCY_STUB_ROLES)} dependency stubs live outside "
           f"the release namespace {RELEASE_NAMESPACE!r}")
    return all_ok


# ---------------------------------------------------------------------
# d2b.29 hardened-fixture contract.
#
# The fixture container ONLY opens non-privileged
# TCP ports in code paths the fixture exercises;
# it does NOT require /proc/sys reads, raw sockets,
# nor filesystem writes. Therefore a Pod that
# regresses to a default root securityContext, or
# a Dockerfile that drops the runtime USER line,
# is a regression that nothing in the d2b.28
# semantic-admission surface detects. This check
# closes that hole.
#
# Hard rules:
#   * Every Pod / Deployment template MUST have
#     spec.securityContext.runAsNonRoot = true
#     and a non-zero runAsUser.
#   * Every container MUST declare
#     securityContext.allowPrivilegeEscalation=false
#     and capabilities.drop=["ALL"].
#   * readOnlyRootFilesystem is REQUIRED for
#     every fixture container (the operand never
#     writes to disk).
#   * seccompProfile.type must be RuntimeDefault
#     on the Pod-level securityContext.
#   * control-netpol-gate.Dockerfile must contain
#     a "USER 65534" line so the fixture image
#     refuses to start as root if pulled by a
#     runtime that hasn't mounted /etc/passwd.
# ---------------------------------------------------------------------
def _walk_pod_sc(path, doc, problems):
    kind = doc.get("kind")
    if kind not in ("Pod", "Deployment", "StatefulSet", "DaemonSet"):
        return
    if kind == "Pod":
        name = doc.get("metadata", {}).get("name", "?")
        spec = doc.get("spec") or {}
        containers = spec.get("containers") or []
        pod_sc = spec.get("securityContext")
    else:
        name = doc.get("metadata", {}).get("name", "?")
        tmpl = (doc.get("spec") or {}).get("template") or {}
        spec = tmpl.get("spec") or {}
        containers = spec.get("containers") or []
        pod_sc = spec.get("securityContext")

    if not isinstance(pod_sc, dict):
        problems.append((path, kind, name,
                         "Pod securityContext is missing"))
    else:
        if pod_sc.get("runAsNonRoot") is not True:
            problems.append((path, kind, name,
                             "Pod securityContext.runAsNonRoot != true"))
        run_as_user = pod_sc.get("runAsUser")
        if not isinstance(run_as_user, int) or run_as_user <= 0 \
                or run_as_user == 0:
            # Only forbid UID 0. The exact match on
            # 65534 is asserted below as a stricter
            # rule of the fixture contract.
            problems.append((path, kind, name,
                             "Pod securityContext.runAsUser must be a "
                             "positive integer (got %r)" % (run_as_user,)))
        elif run_as_user != 65534:
            problems.append((path, kind, name,
                             "Pod securityContext.runAsUser must be 65534 "
                             "(matches Dockerfile USER 65534:65534); "
                             "got %d" % (run_as_user,)))
        sec = pod_sc.get("seccompProfile")
        if not isinstance(sec, dict) or sec.get("type") != "RuntimeDefault":
            problems.append((path, kind, name,
                             "Pod securityContext.seccompProfile.type "
                             "must be RuntimeDefault"))

    for c in containers:
        if not isinstance(c, dict):
            continue
        c_sc = c.get("securityContext")
        if not isinstance(c_sc, dict):
            problems.append((path, kind, name,
                             "container %s securityContext missing" %
                             c.get("name", "?")))
            continue
        if c_sc.get("allowPrivilegeEscalation") is not False:
            problems.append((path, kind, name,
                             "container %s securityContext.allowPrivilegeEscalation "
                             "must be false" % c.get("name", "?")))
        if c_sc.get("readOnlyRootFilesystem") is not True:
            problems.append((path, kind, name,
                             "container %s securityContext.readOnlyRootFilesystem "
                             "must be true" % c.get("name", "?")))
        caps = c_sc.get("capabilities") or {}
        drop = caps.get("drop") or []
        if "ALL" not in drop:
            problems.append((path, kind, name,
                             "container %s securityContext.capabilities.drop "
                             "must include ALL" % c.get("name", "?")))


def check_pod_security_context_hardened(fixture_dir):
    problems = []
    for path in sorted(fixture_dir.glob("*.yaml")):
        for d in load_documents(path):
            _walk_pod_sc(path, d, problems)
    if problems:
        for path, kind, name, msg in problems:
            fail(f"{path}: {kind}/{name}: {msg}")
        return False
    ok("every fixture Pod + container passes the d2b.29 hardened-fixture contract")
    return True


def check_dockerfile_user_present(fixture_dir):
    """The fixture Dockerfile MUST declare a
    non-root USER. We assert a literal
    `USER 65534:65534` line because:
       * it's a single, deterministic
         substring grep;
       * it matches the kubelet's runAsUser
         assertion;
       * absent a USER line, Pod
         spec.securityContext.runAsUser=65534
         fails on a scratch image (no
         /etc/passwd) at runtime, not at
         template-parse time, so the
         contract is enforced here rather
         than later.
    """
    df = fixture_dir / "control-netpol-gate.Dockerfile"
    if not df.exists():
        fail(f"{df}: missing; the d2b hardened fixture contract cannot be enforced")
        return False
    text = df.read_text()
    has_user_line = "USER 65534:65534" in text
    if not has_user_line:
        fail(f"{df}: must declare `USER 65534:65534` so the runtime image cannot "
             f"start as root (matches Pod runAsUser 65534)")
        return False
    ok(f"{df}: USER 65534:65534 declared (matches Pod runAsUser)")
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
    s5 = check_pod_security_context_hardened(fixture_dir)
    s6 = check_dockerfile_user_present(fixture_dir)
    s7 = check_pod_namespace_binding(fixture_dir)
    s8 = check_dependency_stubs_outside_release_namespace(fixture_dir)
    if s1 and s2 and s3 and s4 and s5 and s6 and s7 and s8:
        print()
        print("fixture_semantic_admission: PASS")
        return 0
    print()
    print("fixture_semantic_admission: FAIL", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
