#!/usr/bin/env python3
"""D-2b fixture Namespace/Service document-boundary regression +
prerequisite-coverage regression for the integration CNI corpus.

The D-2b.38 dry-run failure on ``02-stub-deps.yaml`` (run 33043030973 /
job 98420689813) showed that ``spec.selector`` / ``spec.ports`` were
nested inside a ``Namespace`` document. PR #278 fixed that.

The D-2b.42 dry-run failure on the fixture preflight (run 33051554620,
heavy job 98448060803) exposed a second, independent ordering defect in
``scripts/install-nexus-test.sh``'s Step B → Step D sequence:

  * Step B persists ``00-prereq-namespaces.yaml`` via ``kubectl apply``.
  * Steps D/F validate later fixtures (01..05) via
    ``kubectl apply --dry-run=server --validate=strict``.
  * Strict server dry-run does NOT persist ``kind: Namespace``
    documents it encounters. So a Namespace declared inside
    ``03-control-pod.yaml`` or ``04-control-service.yaml`` is created
    for the dry-run call but is NOT visible to the next dry-run call's
    namespace-resolution machinery.
  * Result: ``kubectl apply --dry-run=server --validate=strict`` is
    forced to fail with ``Error from server (NotFound): ... namespaces
    \"cni-control\" not found``.

This test enforces both invariants offline:

Section A — D-2b.38 Namespace/Service document-boundary invariants
==============================================================
  * Every ``Namespace`` document MUST omit ``spec`` entirely.
  * ``spec.selector`` / ``spec.ports`` / ``spec.clusterIP`` / ``spec.type``
    under any Namespace is rejected by name.
  * Service/cni-arbitrary lives in its own cni-test-proxy-namespace
    document with selector ``component=arbitrary`` and TCP 9090 port
    matching Pod/cni-mock-arbitrary's named container port.

Section B — D-2b.42 prerequisite-coverage invariants
====================================================
  * The parsed 00-prereq-namespaces.yaml MUST contain all of:
    cni-test-ingress, cni-test-prometheus, cni-test-untrusted,
    database, cni-test-proxy, cni-control (with the documented
    label set, no spec). The dependency stubs share `database`
    (stateful deps, matching the chart's own
    networkPolicy.postgres.selector.namespace default) and
    `cni-test-proxy` (network-path deps); none may sit in the
    release namespace, where default-deny denies all ingress.
  * The parsed full corpus MUST contain exactly one
    Namespace/cni-control, only in 00-prereq-namespaces.yaml.
  * 03, 04, 05 MUST contain zero kind: Namespace documents.
  * For every namespaced object in the Step D file set
    [01, 02, 03, 04, 05] whose namespace is not ``default`` and not
    a system namespace (``kube-system``, ``kube-public``,
    ``default``, ``kube-node-lease``), the namespace MUST exist in
    the parsed 00 prerequisite set. This proves a namespaced dry-run
    would resolve its namespace.
  * The control objects must remain in ``cni-control``:
    Deployment/cni-control-probe, Service/cni-control-probe-svc,
    Pod/cni-control-target, Service/cni-control-target-svc,
    ServiceAccount/cni-control, NetworkPolicy/cni-control-allow-probe-to-target.

Ten controls (C1-C10) below cover Section A's parsed document-boundary
fixtures. Four additional controls (C11-C14) cover Section B's
prerequisite-coverage invariants on deep-copied mutations.
"""
import copy
import sys
import tempfile
from pathlib import Path

import yaml

REPO = Path(__file__).resolve().parent.parent.parent.parent.parent
FIXTURE_DIR = REPO / "scripts" / "fixtures" / "integrationcni"
TARGET_FIXTURE = FIXTURE_DIR / "02-stub-deps.yaml"
ALL_FIXTURE_FILES = sorted(p for p in FIXTURE_DIR.glob("*.yaml"))

PREREQ_FIXTURE = "00-prereq-namespaces.yaml"
CONTROL_POD_FIXTURE = "03-control-pod.yaml"
CONTROL_SVC_FIXTURE = "04-control-service.yaml"
CONTROL_POLICY_FIXTURE = "05-control-policy.yaml"

# The exact ordering used by install-nexus-test.sh for Step D.
STEP_D_FIXTURE_ORDER = [
    "01-test-pods.yaml",
    "02-stub-deps.yaml",
    "03-control-pod.yaml",
    "04-control-service.yaml",
    "05-control-policy.yaml",
]
SYSTEM_NAMESPACES = {"default", "kube-system", "kube-public", "kube-node-lease"}

# Pre-existing CNI test namespaces. cni-control is appended in 00 by
# the D-2b.42 prerequisite repair.
EXPECTED_PREREQ_NAMESPACES = {
    "cni-test-ingress",
    "cni-test-prometheus",
    "cni-test-untrusted",
    "database",
    "cni-test-proxy",
    "cni-control",
}

CONTROL_NAMESPACE = "cni-control"
EXPECTED_CONTROL_OBJECTS = (
    ("Deployment", "cni-control-probe", CONTROL_POD_FIXTURE),
    ("Service", "cni-control-probe-svc", CONTROL_SVC_FIXTURE),
    ("Pod", "cni-control-target", CONTROL_SVC_FIXTURE),
    ("Service", "cni-control-target-svc", CONTROL_SVC_FIXTURE),
    ("ServiceAccount", "cni-control", CONTROL_SVC_FIXTURE),
    ("NetworkPolicy", "cni-control-allow-probe-to-target", CONTROL_POLICY_FIXTURE),
)

NAMESPACE_FORBIDDEN_SPEC_KEYS = ("selector", "ports", "clusterIP", "type")

ARBITRARY_NAME = "cni-arbitrary"
ARBITRARY_NAMESPACE = "cni-test-proxy"
ARBITRARY_COMPONENT = "arbitrary"
ARBITRARY_PORT_NAME = "tcp"
ARBITRARY_PORT_NUMBER = 9090

INGRESS_NAME = "cni-test-ingress"
INGRESS_LABEL_KEY = "kubernetes.io/metadata.name"
INGRESS_LABEL_VALUE = "cni-test-ingress"

ARBITRARY_POD_NAME = "cni-mock-arbitrary"
ARBITRARY_POD_NAMESPACE = "cni-test-proxy"

HISTORICAL_DEFECT_CONTEXT = (
    "selector/ports nested under Namespace is the d2b.38 "
    "run 33043030973 defect"
)
PREREQ_DEFECT_CONTEXT = (
    "namespaced fixture references namespace that is not in "
    "the parsed 00 prerequisite set; Step B only persists "
    "00-prereq-namespaces.yaml, so Steps D/F dry-run fails "
    "with 'namespaces \"<name>\" not found' (this is the "
    "d2b.42 run 33051554620 defect)"
)


def _fail(control, msg):
    print(f"[FAIL] {control}: {msg}", file=sys.stderr)
    return False


def _ok(control, msg):
    print(f"[OK]   {control}: {msg}")
    return True


# ---------------------------------------------------------------------------
# Shared parser helper (fail-closed)
# ---------------------------------------------------------------------------
def _load_documents(path):
    """Parse every YAML document from ``path`` and return deep copies.

    Fail-closed on any parse or IO error. Never catches-and-continues.
    Every fixture YAML must parse cleanly; otherwise this function
    raises ``RuntimeError`` naming the file and the underlying YAML
    error message. The caller is responsible for turning that into a
    FAIL verdict.
    """
    try:
        raw = list(yaml.safe_load_all(path.read_text(encoding="utf-8")))
    except (OSError, yaml.YAMLError) as exc:
        raise RuntimeError(
            f"fixture YAML parse failed: path={path}: {exc!r}"
        ) from exc
    return [copy.deepcopy(doc) for doc in raw if doc is not None]


def _find_one(docs, kind, **meta_kv):
    matches = []
    for d in docs:
        if not isinstance(d, dict) or d.get("kind") != kind:
            continue
        meta = d.get("metadata") or {}
        if all(meta.get(k) == v for k, v in meta_kv.items()):
            matches.append(d)
    if len(matches) != 1:
        raise AssertionError(
            f"expected exactly one {kind} matching {meta_kv}, "
            f"found {len(matches)}"
        )
    return matches[0]


def _find_all(docs, kind, **meta_kv):
    out = []
    for d in docs:
        if not isinstance(d, dict) or d.get("kind") != kind:
            continue
        meta = d.get("metadata") or {}
        if all(meta.get(k) == v for k, v in meta_kv.items()):
            out.append(d)
    return out


# ---------------------------------------------------------------------------
# Section A — D-2b.38 Namespace/Service document-boundary validator
# ---------------------------------------------------------------------------
def validate_target(docs, scope_label="02-stub-deps.yaml"):
    """Accept the post-d2b.38 repaired 02-stub-deps.yaml documents.

    Returns a list of human-readable violation strings. An empty list
    means acceptance. The function must accept the post-d2b.38 repaired
    fixture and reject the nine structural mutations C1..C10 had
    pre-cursor scope for.
    """
    issues = []

    ingress_nss = _find_all(docs, "Namespace", name=INGRESS_NAME)
    if len(ingress_nss) != 1:
        issues.append(
            f"{scope_label}: Namespace/{INGRESS_NAME}: expected "
            f"exactly 1 document, found {len(ingress_nss)}"
        )
        return issues

    arbitrary_services = _find_all(
        docs, "Service", name=ARBITRARY_NAME, namespace=ARBITRARY_NAMESPACE
    )
    if len(arbitrary_services) != 1:
        issues.append(
            f"{scope_label}: Service/{ARBITRARY_NAME} in namespace "
            f"{ARBITRARY_NAMESPACE}: expected exactly 1 document, "
            f"found {len(arbitrary_services)}"
        )
        return issues

    arbitrary_pods = _find_all(
        docs, "Pod", name=ARBITRARY_POD_NAME, namespace=ARBITRARY_POD_NAMESPACE
    )
    if len(arbitrary_pods) != 1:
        issues.append(
            f"{scope_label}: Pod/{ARBITRARY_POD_NAME} in namespace "
            f"{ARBITRARY_POD_NAMESPACE}: expected exactly 1 document, "
            f"found {len(arbitrary_pods)}"
        )
        return issues

    ingress_ns = ingress_nss[0]
    svc = arbitrary_services[0]
    pod = arbitrary_pods[0]

    igr_labels = (ingress_ns.get("metadata") or {}).get("labels") or {}
    if igr_labels.get(INGRESS_LABEL_KEY) != INGRESS_LABEL_VALUE:
        issues.append(
            f"{scope_label}: Namespace/{INGRESS_NAME}: required label "
            f"{INGRESS_LABEL_KEY}={INGRESS_LABEL_VALUE!r}; "
            f"got {igr_labels.get(INGRESS_LABEL_KEY)!r}"
        )

    if "spec" in ingress_ns:
        spec_repr = repr(ingress_ns["spec"])
        spec_kind_bad = sorted(
            k for k in (ingress_ns.get("spec") or {}).keys()
            if k in NAMESPACE_FORBIDDEN_SPEC_KEYS
        )
        issues.append(
            f"{scope_label}: Namespace/{INGRESS_NAME}: 'spec' must be "
            f"absent (got type={type(ingress_ns['spec']).__name__}, "
            f"value={spec_repr}"
            + (
                f", forbidden Service-only keys={spec_kind_bad}"
                if spec_kind_bad
                else ""
            )
            + f"); {HISTORICAL_DEFECT_CONTEXT}"
        )

    s_meta = svc.get("metadata") or {}
    s_labels = s_meta.get("labels") or {}
    s_selector = (svc.get("spec") or {}).get("selector") or {}
    s_ports = (svc.get("spec") or {}).get("ports")
    part_of = s_labels.get("app.kubernetes.io/part-of")
    sel_comp = s_selector.get("app.kubernetes.io/component")

    if part_of != "nexus":
        issues.append(
            f"{scope_label}: Service/{ARBITRARY_NAME}: required label "
            f"app.kubernetes.io/part-of='nexus'; got {part_of!r}"
        )
    if sel_comp != ARBITRARY_COMPONENT:
        issues.append(
            f"{scope_label}: Service/{ARBITRARY_NAME}: selector "
            f"app.kubernetes.io/component must equal "
            f"{ARBITRARY_COMPONENT!r}; got {sel_comp!r}"
        )
    if not isinstance(s_ports, list) or len(s_ports) != 1:
        issues.append(
            f"{scope_label}: Service/{ARBITRARY_NAME}: expected exactly "
            f"1 port entry; got {s_ports!r}"
        )
    else:
        p0 = s_ports[0]
        if (
            p0.get("name") != ARBITRARY_PORT_NAME
            or p0.get("port") != ARBITRARY_PORT_NUMBER
            or p0.get("targetPort") != ARBITRARY_PORT_NUMBER
            or p0.get("protocol") != "TCP"
        ):
            issues.append(
                f"{scope_label}: Service/{ARBITRARY_NAME}: port entry "
                f"must be name={ARBITRARY_PORT_NAME!r}, "
                f"port={ARBITRARY_PORT_NUMBER}, "
                f"targetPort={ARBITRARY_PORT_NUMBER}, "
                f"protocol='TCP'; got {p0!r}"
            )

    p_meta = pod.get("metadata") or {}
    p_labels = p_meta.get("labels") or {}
    p_comp = p_labels.get("app.kubernetes.io/component")
    if p_comp != ARBITRARY_COMPONENT:
        issues.append(
            f"{scope_label}: Pod/{ARBITRARY_POD_NAME}: label "
            f"app.kubernetes.io/component must equal "
            f"{ARBITRARY_COMPONENT!r}; got {p_comp!r}"
        )
    containers = (pod.get("spec") or {}).get("containers") or []
    named_port_ok = False
    if len(containers) < 1:
        issues.append(
            f"{scope_label}: Pod/{ARBITRARY_POD_NAME}: requires at "
            f"least one container"
        )
    else:
        for c in containers:
            for port in c.get("ports") or []:
                if (
                    port.get("name") == ARBITRARY_PORT_NAME
                    and port.get("containerPort") == ARBITRARY_PORT_NUMBER
                ):
                    named_port_ok = True
                    break
    if not named_port_ok:
        issues.append(
            f"{scope_label}: Pod/{ARBITRARY_POD_NAME}: must declare a "
            f"named container port {ARBITRARY_PORT_NAME!r} with "
            f"containerPort {ARBITRARY_PORT_NUMBER}"
        )

    return issues


# ---------------------------------------------------------------------------
# Section B — D-2b.42 prerequisite-coverage validators
# ---------------------------------------------------------------------------
def _load_fixture(name):
    return _load_documents(FIXTURE_DIR / name)


def validate_prereq(prereq_docs):
    """Validate 00-prereq-namespaces.yaml alone.

    Returns a list of violation strings.
    """
    issues = []
    namespaces = [
        d for d in prereq_docs
        if isinstance(d, dict) and d.get("kind") == "Namespace"
    ]
    seen = set()
    for d in namespaces:
        nm = (d.get("metadata") or {}).get("name")
        if not isinstance(nm, str) or not nm:
            issues.append(
                f"{PREREQ_FIXTURE}: Namespace without a name field"
            )
            continue
        if "spec" in d:
            issues.append(
                f"{PREREQ_FIXTURE}: Namespace/{nm}: 'spec' must be absent; "
                f"got type={type(d['spec']).__name__}, value={repr(d['spec'])}; "
                f"{HISTORICAL_DEFECT_CONTEXT}"
            )
        if nm in seen:
            issues.append(
                f"{PREREQ_FIXTURE}: duplicate Namespace/{nm} in same file"
            )
        seen.add(nm)
    missing = EXPECTED_PREREQ_NAMESPACES - seen
    if missing:
        issues.append(
            f"{PREREQ_FIXTURE}: missing expected prerequisite namespaces: "
            f"{sorted(missing)}; {PREREQ_DEFECT_CONTEXT}"
        )

    ctl = next((d for d in namespaces if (d.get("metadata") or {}).get("name") == CONTROL_NAMESPACE), None)
    if ctl is not None:
        labels = (ctl.get("metadata") or {}).get("labels") or {}
        if labels.get("kubernetes.io/metadata.name") != CONTROL_NAMESPACE:
            issues.append(
                f"{PREREQ_FIXTURE}: Namespace/{CONTROL_NAMESPACE}: required "
                f"label kubernetes.io/metadata.name={CONTROL_NAMESPACE!r}; "
                f"got {labels.get('kubernetes.io/metadata.name')!r}"
            )
        if labels.get("app.kubernetes.io/part-of") != CONTROL_NAMESPACE:
            issues.append(
                f"{PREREQ_FIXTURE}: Namespace/{CONTROL_NAMESPACE}: required "
                f"label app.kubernetes.io/part-of={CONTROL_NAMESPACE!r}; "
                f"got {labels.get('app.kubernetes.io/part-of')!r}"
            )
    return issues


def validate_corpus_namespace_uniqueness(all_parsed_by_name):
    """Across the whole corpus, Namespace/cni-control must appear
    exactly once, only in 00-prereq-namespaces.yaml.
    """
    issues = []
    occurrences = []
    for fname, docs in all_parsed_by_name.items():
        for d in docs:
            if isinstance(d, dict) and d.get("kind") == "Namespace":
                nm = (d.get("metadata") or {}).get("name")
                if nm == CONTROL_NAMESPACE:
                    occurrences.append(fname)
    if len(occurrences) == 0:
        issues.append(
            f"{CONTROL_NAMESPACE}: Namespace/{CONTROL_NAMESPACE} not declared "
            f"anywhere; {PREREQ_DEFECT_CONTEXT}"
        )
    elif len(occurrences) > 1:
        issues.append(
            f"{CONTROL_NAMESPACE}: Namespace/{CONTROL_NAMESPACE} appears "
            f"{len(occurrences)} times (files: {occurrences}); "
            f"{PREREQ_DEFECT_CONTEXT}; only {PREREQ_FIXTURE} may declare it"
        )
    elif occurrences[0] != PREREQ_FIXTURE:
        issues.append(
            f"{CONTROL_NAMESPACE}: Namespace/{CONTROL_NAMESPACE} declared in "
            f"{occurrences[0]!r}; only {PREREQ_FIXTURE} may declare it; "
            f"{PREREQ_DEFECT_CONTEXT}"
        )
    return issues, occurrences


def _forbid_ns_kind_in(fixture_name, all_parsed_by_name):
    """03, 04, 05 must not declare any kind: Namespace documents."""
    issues = []
    docs = all_parsed_by_name.get(fixture_name, [])
    for idx, d in enumerate(docs):
        if isinstance(d, dict) and d.get("kind") == "Namespace":
            nm = (d.get("metadata") or {}).get("name")
            issues.append(
                f"{fixture_name}:doc[{idx}]: unexpected Kind/Namespace "
                f"document name={nm!r}; only {PREREQ_FIXTURE} may declare "
                f"Namespaces; {PREREQ_DEFECT_CONTEXT}"
            )
    return issues


def validate_step_d_namespace_coverage(all_parsed_by_name, prereq_names):
    """For every namespaced object in [01, 02, 03, 04, 05] whose
    namespace is not a system namespace, the namespace must exist in
    the parsed 00 prerequisite set.
    """
    issues = []
    covered = set(prereq_names) | SYSTEM_NAMESPACES
    for fname in STEP_D_FIXTURE_ORDER:
        docs = all_parsed_by_name.get(fname, [])
        for idx, d in enumerate(docs):
            if not isinstance(d, dict):
                continue
            kind = d.get("kind")
            if kind in (None, "Namespace"):
                continue
            meta = d.get("metadata") or {}
            ns = meta.get("namespace")
            if ns is None:
                continue
            if ns in covered:
                continue
            issues.append(
                f"{fname}:doc[{idx}]: kind={kind} name={meta.get('name')!r} "
                f"references namespace={ns!r} that is NOT in the parsed "
                f"00 prerequisite set {sorted(prereq_names)}; "
                f"{PREREQ_DEFECT_CONTEXT}"
            )
    return issues


def validate_control_object_locations(all_parsed_by_name):
    """All six expected control objects must remain in namespace cni-control
    in their declaring fixture files."""
    issues = []
    for kind, name, fname in EXPECTED_CONTROL_OBJECTS:
        docs = all_parsed_by_name.get(fname, [])
        matches = _find_all(docs, kind, name=name, namespace=CONTROL_NAMESPACE)
        if len(matches) != 1:
            issues.append(
                f"control-object-invariant: {kind}/{name} in "
                f"{fname}: expected exactly 1 in namespace "
                f"{CONTROL_NAMESPACE!r}, found {len(matches)}"
            )
    return issues


def _gather_prereq_names(prereq_docs):
    out = set()
    for d in prereq_docs:
        if isinstance(d, dict) and d.get("kind") == "Namespace":
            nm = (d.get("metadata") or {}).get("name")
            if isinstance(nm, str) and nm:
                out.add(nm)
    return out


# ---------------------------------------------------------------------------
# Mutation helpers (run on parsed deep copies)
# ---------------------------------------------------------------------------
def _all_parsed_baseline():
    """Read every fixture file once into a mapping keyed by filename."""
    return {
        p.name: _load_documents(p)
        for p in sorted(FIXTURE_DIR.glob("*.yaml"))
    }


def _setprereq(per_file, fixture_name, name, replacement_docs,
               only_kind="Namespace"):
    """Helper to rebuild per_file's documents for a mutation."""
    clones = {fn: copy.deepcopy(docs) for fn, docs in per_file.items()}
    if replacement_docs is not None:
        # Replace only the targeted kind documents in the fixture.
        surviving = [
            d for d in clones[fixture_name]
            if not (isinstance(d, dict) and d.get("kind") == only_kind
                    and (d.get("metadata") or {}).get("name") == name)
        ]
        clones[fixture_name] = surviving + [
            copy.deepcopy(d) for d in replacement_docs
        ]
    else:
        clones[fixture_name] = [
            d for d in clones[fixture_name]
            if not (isinstance(d, dict) and d.get("kind") == only_kind
                    and (d.get("metadata") or {}).get("name") == name)
        ]
    return clones


def _mut_remove_cni_control_from_prereq(per_file):
    prereq = _load_fixture(PREREQ_FIXTURE)
    new_prereq = [
        d for d in prereq
        if not (isinstance(d, dict) and d.get("kind") == "Namespace"
                and (d.get("metadata") or {}).get("name") == CONTROL_NAMESPACE)
    ]
    return _setprereq(per_file, PREREQ_FIXTURE, CONTROL_NAMESPACE, new_prereq)


def _mut_move_cni_control_to_pod(per_file):
    """Move cni-control declaration from 00 to 03 (out of order)."""
    # Remove from 00
    per_file = _mut_remove_cni_control_from_prereq(per_file)
    # Add a fresh Namespace/cni-control to 03 with labels and no spec
    clone = copy.deepcopy(per_file)
    fresh = {
        "apiVersion": "v1",
        "kind": "Namespace",
        "metadata": {
            "name": CONTROL_NAMESPACE,
            "labels": {
                "kubernetes.io/metadata.name": CONTROL_NAMESPACE,
                "app.kubernetes.io/part-of": CONTROL_NAMESPACE,
            },
        },
    }
    clone[CONTROL_POD_FIXTURE] = [fresh] + clone[CONTROL_POD_FIXTURE]
    return clone


def _mut_duplicate_cni_control_into_service(per_file):
    """Add a second Namespace/cni-control to 04 (duplicate declaration)."""
    clone = {fn: copy.deepcopy(docs) for fn, docs in per_file.items()}
    fresh = {
        "apiVersion": "v1",
        "kind": "Namespace",
        "metadata": {
            "name": CONTROL_NAMESPACE,
            "labels": {
                "kubernetes.io/metadata.name": CONTROL_NAMESPACE,
                "app.kubernetes.io/part-of": CONTROL_NAMESPACE,
            },
        },
    }
    clone[CONTROL_SVC_FIXTURE] = [fresh] + clone[CONTROL_SVC_FIXTURE]
    return clone


def _mut_undeclared_namespace(per_file):
    """Change every control object's namespace to 'cni-control-missing'."""
    clone = {fn: copy.deepcopy(docs) for fn, docs in per_file.items()}
    targets = {fname for (_k, _n, fname) in EXPECTED_CONTROL_OBJECTS}
    targeted_name_to_kind = {
        n: k for (k, n, _f) in EXPECTED_CONTROL_OBJECTS
    }
    for fname in targets:
        for d in clone[fname]:
            if not isinstance(d, dict):
                continue
            meta = d.get("metadata") or {}
            name = meta.get("name")
            kind = d.get("kind")
            if kind in {"Deployment", "Service", "Pod",
                        "ServiceAccount", "NetworkPolicy"} and (
                targeted_name_to_kind.get(name) == kind
            ):
                meta["namespace"] = "cni-control-missing"
    return clone


# ---------------------------------------------------------------------------
# Section A controls (C1..C10) — preserved unchanged from PR #278
# ---------------------------------------------------------------------------
def run_control_pristine():
    """C1: 02-stub-deps.yaml passes the d2b.38 invariant."""
    docs = _load_fixture("02-stub-deps.yaml")
    issues = validate_target(docs)
    if issues:
        return _fail(
            "C1-pristine",
            "validator rejected pristine fixture: " + "; ".join(issues),
        )
    return _ok(
        "C1-pristine",
        "02-stub-deps.yaml: 1 Ingress NS without spec, 1 "
        "Svc/cni-arbitrary in default, 1 Pod/cni-mock-arbitrary in "
        "default; selector/port/named-container invariants hold",
    )


def run_control_document_separation():
    """C2: across the corpus, no Namespace document carries spec.

    Fail-closed on any fixture YAML parse error."""
    bad = []
    try:
        for path in ALL_FIXTURE_FILES:
            for d in _load_documents(path):
                if isinstance(d, dict) and d.get("kind") == "Namespace" and "spec" in d:
                    nm = (d.get("metadata") or {}).get("name")
                    spec_repr = repr(d["spec"])
                    bad.append(
                        f"{path.name}:Namespace/{nm}: 'spec' is present "
                        f"(type={type(d['spec']).__name__}, value={spec_repr})"
                    )
    except RuntimeError as exc:
        return _fail(
            "C2-doc-separation",
            f"fail-closed parser raised during fixture scan: {exc}",
        )
    if bad:
        return _fail(
            "C2-doc-separation",
            "Namespace documents carry a 'spec' key: " + "; ".join(bad),
        )
    return _ok(
        "C2-doc-separation",
        "all Namespace documents across the fixture corpus omit "
        "'spec' entirely; failures during fail-closed scan would "
        "have raised above this line",
    )


def run_control_exact_historical_mutation():
    """C3: replicate the d2b.38 defect; validator rejects with name."""
    docs = _load_fixture("02-stub-deps.yaml")
    ingress = _find_one(docs, "Namespace", name=INGRESS_NAME)
    svc = _find_one(
        docs, "Service", name=ARBITRARY_NAME, namespace=ARBITRARY_NAMESPACE
    )
    moved = copy.deepcopy(svc["spec"])
    ingress.setdefault("spec", {})
    ingress["spec"]["selector"] = moved.get("selector")
    ingress["spec"]["ports"] = moved.get("ports")
    issues = validate_target(docs)
    if not issues:
        return _fail(
            "C3-exact-historical",
            "expected validator to reject moved-Namespace defect "
            "(selector/ports nested under Namespace spec)",
        )
    if not any(
        f"Namespace/{INGRESS_NAME}" in m and "'spec' must be absent" in m
        for m in issues
    ):
        return _fail(
            "C3-exact-historical",
            "validator rejected but did not name Namespace/spec "
            f"violation; got: {issues}",
        )
    if not any(
        "forbidden Service-only keys=" in m
        and "selector" in m and "ports" in m
        for m in issues
    ):
        return _fail(
            "C3-exact-historical",
            "validator rejected but did not name selector/ports "
            f"forbidden-key violation; got: {issues}",
        )
    diag = next(
        m for m in issues
        if f"Namespace/{INGRESS_NAME}" in m and "'spec' must be absent" in m
    )
    return _ok("C3-exact-historical", "validator rejected: " + diag)


def run_control_orphaned_service():
    """C4: cni-arbitrary selector mutated to nonmatching."""
    docs = _load_fixture("02-stub-deps.yaml")
    svc = _find_one(
        docs, "Service", name=ARBITRARY_NAME, namespace=ARBITRARY_NAMESPACE
    )
    svc.setdefault("spec", {}).setdefault("selector", {})
    svc["spec"]["selector"]["app.kubernetes.io/component"] = "rogue-component"
    issues = validate_target(docs)
    if not issues or not any(
        "Service/cni-arbitrary" in m and "selector" in m
        and ARBITRARY_COMPONENT in m for m in issues
    ):
        return _fail(
            "C4-orphaned-service",
            "validator did not reject orphaned (nonmatching) selector: "
            + "; ".join(issues),
        )
    return _ok("C4-orphaned-service", "validator rejected orphaned selector as expected")


def run_control_wrong_port():
    """C5: targetPort 9090 -> 9091."""
    docs = _load_fixture("02-stub-deps.yaml")
    svc = _find_one(
        docs, "Service", name=ARBITRARY_NAME, namespace=ARBITRARY_NAMESPACE
    )
    ports = svc.setdefault("spec", {}).setdefault("ports", [])
    if not isinstance(ports, list) or len(ports) != 1:
        return _fail("C5-wrong-port", "fixture inverse invariant broken")
    ports[0]["targetPort"] = 9091
    issues = validate_target(docs)
    if not issues or not any("port entry must be" in m for m in issues):
        return _fail(
            "C5-wrong-port",
            "validator did not reject wrong targetPort: " + "; ".join(issues),
        )
    return _ok("C5-wrong-port", "validator rejected wrong targetPort")


def run_control_missing_service():
    """C6: remove Service/cni-arbitrary."""
    docs = [
        d for d in _load_fixture("02-stub-deps.yaml")
        if not (isinstance(d, dict) and d.get("kind") == "Service"
                and (d.get("metadata") or {}).get("name") == ARBITRARY_NAME)
    ]
    issues = validate_target(docs)
    if not issues or not any(
        "Service/cni-arbitrary" in m and "expected exactly 1" in m
        for m in issues
    ):
        return _fail(
            "C6-missing-service",
            "validator did not reject missing Service/cni-arbitrary: "
            + "; ".join(issues),
        )
    return _ok("C6-missing-service", "validator rejected missing Service/cni-arbitrary")


def run_control_namespace_integrity():
    """C7: Namespace/cni-test-ingress renamable."""
    docs = _load_fixture("02-stub-deps.yaml")
    ingress = _find_one(docs, "Namespace", name=INGRESS_NAME)
    ing_meta = ingress.setdefault("metadata", {})
    ing_meta["name"] = "cni-test-ingress-renamed"
    issues = validate_target(docs)
    if not issues or not any(
        "Namespace/cni-test-ingress" in m
        and ("exactly 1" in m or "required label" in m)
        for m in issues
    ):
        return _fail(
            "C7-namespace-integrity",
            "validator did not reject Namespace rename: " + "; ".join(issues),
        )
    return _ok("C7-namespace-integrity",
               "validator rejected renamed Namespace/cni-test-ingress")


def run_control_parse_failure():
    """C8: malformed YAML fed to shared parser must raise/FAIL."""
    with tempfile.TemporaryDirectory() as tmp:
        bad_path = Path(tmp) / "bad-fixture.yaml"
        bad_path.write_text(
            "apiVersion: v1\n"
            "kind: Namespace\n"
            "metadata:\n"
            "  name: cni-arbitrary\n"
            "spec: [unterminated\n",
            encoding="utf-8",
        )
        try:
            _load_documents(bad_path)
            return _fail(
                "C8-parse-failure",
                "shared parser returned success on a deliberately malformed YAML",
            )
        except RuntimeError as exc:
            msg = str(exc)
            if "fixture YAML parse failed" not in msg or str(bad_path) not in msg:
                return _fail(
                    "C8-parse-failure",
                    f"shared parser raised but did not name path; got: {msg}",
                )
            return _ok(
                "C8-parse-failure",
                f"shared parser raised fail-closed on malformed YAML: {msg}",
            )


def run_control_namespace_empty_spec():
    """C9: spec={} on the Ingress NS -> reject (presence rule)."""
    docs = _load_fixture("02-stub-deps.yaml")
    ingress = _find_one(docs, "Namespace", name=INGRESS_NAME)
    ingress["spec"] = {}
    issues = validate_target(docs)
    if not issues or not any(
        f"Namespace/{INGRESS_NAME}" in m and "'spec' must be absent" in m
        for m in issues
    ):
        return _fail(
            "C9-empty-spec",
            "validator did not reject Namespace spec={}: ".format
            () + "; ".join(issues),
        )
    return _ok("C9-empty-spec", "validator rejected Namespace/empty-spec")


def run_control_namespace_unknown_spec():
    """C10: spec={'foo':'bar'} on the Ingress NS -> reject (presence rule)."""
    docs = _load_fixture("02-stub-deps.yaml")
    ingress = _find_one(docs, "Namespace", name=INGRESS_NAME)
    ingress["spec"] = {"foo": "bar"}
    issues = validate_target(docs)
    if not issues or not any(
        f"Namespace/{INGRESS_NAME}" in m and "'spec' must be absent" in m
        for m in issues
    ):
        return _fail(
            "C10-unknown-spec",
            "validator did not reject Namespace spec={'foo':'bar'}: "
            + "; ".join(issues),
        )
    return _ok("C10-unknown-spec", "validator rejected Namespace/unknown-spec")


# ---------------------------------------------------------------------------
# Section B controls (C11..C14) — D-2b.42 prerequisite coverage
# ---------------------------------------------------------------------------
def _combine_issues(*lists):
    out = []
    for L in lists:
        out.extend(L)
    return out


def _run_prereq_suite(per_file):
    """Run Section B validators on a snapshot dict of per-file docs."""
    prereq_docs = per_file.get(PREREQ_FIXTURE, [])
    prereq_issues = validate_prereq(prereq_docs)
    prereq_names = _gather_prereq_names(prereq_docs)
    uniq_issues, _ = validate_corpus_namespace_uniqueness(per_file)
    forbid_03 = _forbid_ns_kind_in(CONTROL_POD_FIXTURE, per_file)
    forbid_04 = _forbid_ns_kind_in(CONTROL_SVC_FIXTURE, per_file)
    forbid_05 = _forbid_ns_kind_in(CONTROL_POLICY_FIXTURE, per_file)
    dep_issues = validate_step_d_namespace_coverage(per_file, prereq_names)
    control_obj_issues = validate_control_object_locations(per_file)
    return _combine_issues(
        prereq_issues, uniq_issues, forbid_03, forbid_04, forbid_05,
        dep_issues, control_obj_issues,
    )


def run_control_prereq_baseline():
    """C11 positive + negative: cni-control missing in 00 must reject."""
    per_file = _all_parsed_baseline()
    # Sanity: the unmutated baseline must pass.
    baseline_issues = _run_prereq_suite(per_file)
    if baseline_issues:
        return _fail(
            "C11-prereq-missing",
            "baseline fixture is already failing Section B invariants: "
            + "; ".join(baseline_issues),
        )
    # Mutate: drop cni-control from prereq; expect rejection.
    mutated = _mut_remove_cni_control_from_prereq(per_file)
    issues = _run_prereq_suite(mutated)
    if not issues:
        return _fail(
            "C11-prereq-missing",
            "expected validator to reject removal of cni-control from 00",
        )
    if not any("cni-control" in m and "prerequisite namespaces" in m for m in issues):
        return _fail(
            "C11-prereq-missing",
            "validator rejected removal but did not name cni-control/"
            "prerequisite; got: " + "; ".join(issues),
        )
    diag = next(
        m for m in issues
        if "cni-control" in m and "prerequisite namespaces" in m
    )
    return _ok("C11-prereq-missing", "validator rejected: " + diag)


def run_control_declaration_out_of_order():
    """C12: move cni-control from 00 to 03 (declaration out of order)."""
    mutated = _mut_move_cni_control_to_pod(_all_parsed_baseline())
    issues = _run_prereq_suite(mutated)
    if not issues:
        return _fail(
            "C12-declaration-out-of-order",
            "expected validator to reject cni-control declared in 03 instead of 00",
        )
    if not any("cni-control" in m and (
        "expected" in m or "prerequisite namespaces" in m
        or "may declare" in m or "out of order" in m
        or "unexpected Kind/Namespace" in m
    ) for m in issues):
        return _fail(
            "C12-declaration-out-of-order",
            "validator rejected but did not name cni-control/"
            "prerequisite/out-of-order; got: " + "; ".join(issues),
        )
    diag = next(m for m in issues if "cni-control" in m)
    return _ok("C12-declaration-out-of-order", "validator rejected: " + diag)


def run_control_duplicate_declaration():
    """C13: a second Namespace/cni-control in 04 must reject."""
    mutated = _mut_duplicate_cni_control_into_service(_all_parsed_baseline())
    issues = _run_prereq_suite(mutated)
    if not issues:
        return _fail(
            "C13-duplicate-declaration",
            "expected validator to reject duplicate Namespace/cni-control in 04",
        )
    if not any(
        "cni-control" in m and ("duplicate" in m or "appears" in m)
        for m in issues
    ):
        return _fail(
            "C13-duplicate-declaration",
            "validator rejected but did not name duplicate; got: "
            + "; ".join(issues),
        )
    diag = next(m for m in issues if "cni-control" in m and ("duplicate" in m or "appears" in m))
    return _ok("C13-duplicate-declaration", "validator rejected: " + diag)


def run_control_undeclared_namespace_use():
    """C14: every control object's namespace flipped to cni-control-missing."""
    mutated = _mut_undeclared_namespace(_all_parsed_baseline())
    issues = _run_prereq_suite(mutated)
    if not issues:
        return _fail(
            "C14-undeclared-namespace",
            "expected validator to reject namespace=cni-control-missing",
        )
    if not any(
        "cni-control-missing" in m and ("prerequisite" in m or "NOT in the parsed" in m)
        for m in issues
    ):
        return _fail(
            "C14-undeclared-namespace",
            "validator rejected but did not name cni-control-missing/"
            "prerequisite; got: " + "; ".join(issues),
        )
    diag = next(
        m for m in issues
        if "cni-control-missing" in m
        and ("prerequisite" in m or "NOT in the parsed" in m)
    )
    return _ok("C14-undeclared-namespace", "validator rejected: " + diag)


CONTROLS = [
    ("C1-pristine",          run_control_pristine),
    ("C2-doc-separation",    run_control_document_separation),
    ("C3-exact-historical",  run_control_exact_historical_mutation),
    ("C4-orphaned-service",  run_control_orphaned_service),
    ("C5-wrong-port",        run_control_wrong_port),
    ("C6-missing-service",   run_control_missing_service),
    ("C7-namespace-integrity", run_control_namespace_integrity),
    ("C8-parse-failure",     run_control_parse_failure),
    ("C9-empty-spec",        run_control_namespace_empty_spec),
    ("C10-unknown-spec",     run_control_namespace_unknown_spec),
    ("C11-prereq-missing",   run_control_prereq_baseline),
    ("C12-declaration-out-of-order", run_control_declaration_out_of_order),
    ("C13-duplicate-declaration",    run_control_duplicate_declaration),
    ("C14-undeclared-namespace",     run_control_undeclared_namespace_use),
]


def main():
    results = []
    for name, fn in CONTROLS:
        try:
            ok = fn()
        except AssertionError as e:
            ok = _fail(name, f"control precondition broke: {e}")
        except RuntimeError as e:
            ok = _fail(name, f"fail-closed parser raised: {e}")
        except Exception as e:
            ok = _fail(name, f"unexpected exception: {e!r}")
        results.append(ok)
    print(f"results={[int(r) for r in results]}")
    if not all(results):
        sys.exit(1)
    print("d2b.38+N fixture Namespace document-boundary + "
          "d2b.42 prerequisite coverage: PASS")


if __name__ == "__main__":
    main()
