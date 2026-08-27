#!/usr/bin/env python3
"""D-2b fixture Namespace/Service document-boundary regression.

The D-2b.38 dry-run failure on ``02-stub-deps.yaml`` (run 33043030973 /
job 98420689813) showed that ``spec.selector`` / ``spec.ports`` were
nested inside a ``Namespace`` document. The Kubernetes API server
correctly rejects that with "unknown field spec.ports, unknown field
spec.selector".

This test loads the integrationcni fixture corpus offline and asserts
the YAML documents are split with strict boundaries:

  * Every ``Namespace`` document MUST omit ``spec`` entirely. This is
    an intentional fixture-level rule for the integration CNI corpus
    that prevents accidental object-fragment attachment. Diagnose any
    Namespace whose ``spec`` is present (``{}``, ``None``, a dict,
    ``finalizers``, ``foo: bar``, anything).
  * ``spec.selector`` / ``spec.ports`` / ``spec.clusterIP`` / ``spec.type``
    under any Namespace is the historical defect and is rejected by
    name.
  * The Service/cni-arbitrary contract lives in its own document, in
    default namespace, with selector ``app.kubernetes.io/component ==
    arbitrary``, a single port named ``tcp`` with ``port`` and
    ``targetPort`` set to ``9090``, and matches the existing
    Pod/cni-mock-arbitrary named container port.

Ten controls run deep-copy mutations on parsed documents:

  1.  C1  pristine fixture: every documented invariant holds.
  2.  C2  document separation: across the entire fixture corpus, no
           Namespace document carries a ``spec`` key.
  3.  C3  exact historical mutation: cni-arbitrary Service's spec is
           moved under Namespace/cni-test-ingress.spec; validator
           rejects naming Namespace, spec, and the forbidden
           Service-only keys.
  4.  C4  orphaned-service mutation: cni-arbitrary selector mutated to
           a non-matching value; validator rejects.
  5.  C5  wrong-port mutation: cni-arbitrary ``targetPort`` changed
           to ``9091``; validator rejects.
  6.  C6  missing-service mutation: Service/cni-arbitrary removed;
           validator rejects.
  7.  C7  namespace integrity mutation: Namespace/cni-test-ingress
           renamed; validator rejects.
  8.  C8  parse-failure mutation: a malformed YAML file in a temp dir
           is fed to the same shared parser helper; the helper must
           raise / main() must report FAIL and exit nonzero. Never
           catch-and-continue.
  9.  C9  empty-spec mutation: ``ingress["spec"] = {}``; rejected.
 10.  C10 unknown-spec mutation: ``ingress["spec"] = {"foo": "bar"}``;
           rejected.

The test runs offline (no live cluster, no Helm, no kubectl, no shell,
no writes to source fixtures). Every fixture YAML parse is fail-closed:
a parse failure in any one fixture must produce a FAIL and exit 1,
naming the fixture file. No fixture is skipped on parse failure.
"""
import copy
import sys
import tempfile
from pathlib import Path

import yaml

REPO = Path(__file__).resolve().parent.parent.parent.parent.parent
FIXTURE_DIR = REPO / "scripts" / "fixtures" / "integrationcni"
# The D-2b.38 historical defect (run 33043030973, dry-run step D)
# was located inside ``02-stub-deps.yaml``. Validation, every control,
# and every mutation MUST be scoped to this single file. Other fixture
# files declare ingress-ns etc. for fixture apply pre-roll, but only
# ``02-stub-deps.yaml`` carries the Service/cni-arbitrary contract and
# is the one Kubernetes API server rejected.
TARGET_FIXTURE = FIXTURE_DIR / "02-stub-deps.yaml"
ALL_FIXTURE_FILES = sorted(p for p in FIXTURE_DIR.glob("*.yaml"))

# Service-only keys that must NEVER appear under a Namespace spec.
NAMESPACE_FORBIDDEN_SPEC_KEYS = ("selector", "ports", "clusterIP", "type")

ARBITRARY_NAME = "cni-arbitrary"
ARBITRARY_NAMESPACE = "default"
ARBITRARY_COMPONENT = "arbitrary"
ARBITRARY_PORT_NAME = "tcp"
ARBITRARY_PORT_NUMBER = 9090

INGRESS_NAME = "cni-test-ingress"
INGRESS_LABEL_KEY = "kubernetes.io/metadata.name"
INGRESS_LABEL_VALUE = "cni-test-ingress"

ARBITRARY_POD_NAME = "cni-mock-arbitrary"
ARBITRARY_POD_NAMESPACE = "default"

# Historical context the d2b.38 defect invoked at run creation.
HISTORICAL_DEFECT_CONTEXT = (
    "selector/ports nested under Namespace is the d2b.38 "
    "run 33043030973 defect"
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


def _load_target_docs():
    """Load ``02-stub-deps.yaml`` (fail-closed)."""
    if not TARGET_FIXTURE.exists():
        raise RuntimeError(
            f"fixture YAML parse failed: path={TARGET_FIXTURE}: missing"
        )
    return _load_documents(TARGET_FIXTURE)


def _load_all_namespace_docs_across_fixtures():
    """Yield every (path, Namespace document) across the fixture corpus.

    Fail-closed: a parse error in any single fixture aborts the scan
    with a diagnostic that names the file. The corpus is partitioned
    into the target fixture (which the validator inspects) and the
    broader fixture set (which C2 audits for Namespace-spec presence
    + Service-only keys without exemption).
    """
    for path in ALL_FIXTURE_FILES:
        for d in _load_documents(path):
            if isinstance(d, dict) and d.get("kind") == "Namespace":
                yield path, d


def _find_one(docs, kind, **meta_kv):
    """Find the single document matching ``kind`` + exact metadata kv."""
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
# Validator
# ---------------------------------------------------------------------------
def validate(docs, scope_label="02-stub-deps.yaml"):
    """Validate the parsed ``02-stub-deps.yaml`` documents.

    Returns a list of human-readable violation strings. An empty list
    means acceptance. The function must accept the post-d2b.38 repaired
    fixture and reject the nine structural mutations and the two
    presence-based spec mutations (C9 / C10).

    Namespace rule (intentional fixture-level rule): every kind:
    Namespace document must omit ``spec`` entirely. ``"spec" in doc``
    is the rejection criterion, regardless of value (empty dict,
    unknown keys, forbidden Service keys, None, list, scalar).
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

    # Namespace integrity (label mirror == name).
    igr_labels = (ingress_ns.get("metadata") or {}).get("labels") or {}
    if igr_labels.get(INGRESS_LABEL_KEY) != INGRESS_LABEL_VALUE:
        issues.append(
            f"{scope_label}: Namespace/{INGRESS_NAME}: required label "
            f"{INGRESS_LABEL_KEY}={INGRESS_LABEL_VALUE!r}; "
            f"got {igr_labels.get(INGRESS_LABEL_KEY)!r}"
        )

    # Namespace presence rule: spec must be absent entirely. This is
    # the intentional fixture-level rule for the integration CNI
    # corpus; it prevents accidental object-fragment attachment that
    # caused run 33043030973. ``"spec" in ingress_ns`` is the test.
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
    elif isinstance(ingress_ns.get("spec", None), dict):
        # Belt-and-suspenders: even an empty dict would be a violation
        # via the ``"spec" in ingress_ns`` rule above, but we keep this
        # branch so the validator never accidentally accepts a Nesting-
        # edge case where ``spec`` is set to a non-mapping.
        issues.append(
            f"{scope_label}: Namespace/{INGRESS_NAME}: 'spec' must be "
            f"absent; {HISTORICAL_DEFECT_CONTEXT}"
        )

    # Service/cni-arbitrary contract.
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

    # Pod/cni-mock-arbitrary must select arbitrary and expose named
    # container port ``tcp`` on 9090.
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
# Controls
# ---------------------------------------------------------------------------
def run_control_pristine():
    """C1: pristine fixture passes the validator (no exception path)."""
    docs = _load_target_docs()
    issues = validate(docs)
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
    """C2: across the corpus, no Namespace document carries spec."""
    bad = []
    try:
        for path, d in _load_all_namespace_docs_across_fixtures():
            if "spec" in d:
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
    docs = _load_target_docs()
    ingress = _find_one(docs, "Namespace", name=INGRESS_NAME)
    svc = _find_one(
        docs, "Service", name=ARBITRARY_NAME, namespace=ARBITRARY_NAMESPACE
    )

    moved = copy.deepcopy(svc["spec"])
    ingress.setdefault("spec", {})
    ingress["spec"]["selector"] = moved.get("selector")
    ingress["spec"]["ports"] = moved.get("ports")

    issues = validate(docs)
    if not issues:
        return _fail(
            "C3-exact-historical",
            "expected validator to reject moved-Namespace defect "
            "(selector/ports nested under Namespace spec)",
        )
    # Must name Namespace AND 'spec'; historical forbidden-key
    # detection must also be visible.
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
        "forbidden Service-only keys=" in m and "selector" in m and "ports" in m
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
    """C4: cni-arbitrary selector mutated to nonmatching -> reject."""
    docs = _load_target_docs()
    svc = _find_one(
        docs, "Service", name=ARBITRARY_NAME, namespace=ARBITRARY_NAMESPACE
    )
    svc.setdefault("spec", {}).setdefault("selector", {})
    svc["spec"]["selector"]["app.kubernetes.io/component"] = "rogue-component"
    issues = validate(docs)
    if not issues:
        return _fail(
            "C4-orphaned-service",
            "expected validator to reject orphaned (nonmatching) "
            "selector",
        )
    if not any(
        "Service/cni-arbitrary" in m and "selector" in m
        and ARBITRARY_COMPONENT in m
        for m in issues
    ):
        return _fail(
            "C4-orphaned-service",
            "validator rejected orphan but did not name selector: "
            + "; ".join(issues),
        )
    return _ok(
        "C4-orphaned-service",
        "validator rejected orphaned selector as expected",
    )


def run_control_wrong_port():
    """C5: targetPort 9090 -> 9091 -> reject."""
    docs = _load_target_docs()
    svc = _find_one(
        docs, "Service", name=ARBITRARY_NAME, namespace=ARBITRARY_NAMESPACE
    )
    ports = svc.setdefault("spec", {}).setdefault("ports", [])
    if not isinstance(ports, list) or len(ports) != 1:
        return _fail("C5-wrong-port", "fixture inverse invariant broken")
    ports[0]["targetPort"] = 9091
    issues = validate(docs)
    if not issues:
        return _fail(
            "C5-wrong-port",
            "expected validator to reject wrong targetPort",
        )
    if not any("port entry must be" in m for m in issues):
        return _fail(
            "C5-wrong-port",
            "validator rejected wrong port but did not name port entry; "
            f"got: {'; '.join(issues)}",
        )
    return _ok("C5-wrong-port", "validator rejected wrong targetPort")


def run_control_missing_service():
    """C6: remove Service/cni-arbitrary -> reject."""
    docs = _load_target_docs()
    survivors = [
        i for i, d in enumerate(docs)
        if isinstance(d, dict)
        and d.get("kind") == "Service"
        and (d.get("metadata") or {}).get("name") == ARBITRARY_NAME
        and (d.get("metadata") or {}).get("namespace") == ARBITRARY_NAMESPACE
    ]
    for i in survivors:
        del docs[i]
    issues = validate(docs)
    if not issues:
        return _fail(
            "C6-missing-service",
            "expected validator to reject a fixture missing "
            "Service/cni-arbitrary",
        )
    if not any(
        "Service/cni-arbitrary" in m and "expected exactly 1" in m
        for m in issues
    ):
        return _fail(
            "C6-missing-service",
            "validator rejected but did not name missing Service; "
            f"got: {'; '.join(issues)}",
        )
    return _ok(
        "C6-missing-service",
        "validator rejected missing Service/cni-arbitrary",
    )


def run_control_namespace_integrity():
    """C7: Namespace label or name drift -> reject."""
    docs = _load_target_docs()
    ingress = _find_one(docs, "Namespace", name=INGRESS_NAME)
    ing_meta = ingress.setdefault("metadata", {})
    ing_meta["name"] = "cni-test-ingress-renamed"
    issues = validate(docs)
    if not issues:
        return _fail(
            "C7-namespace-integrity",
            "expected validator to reject a Namespace name drift",
        )
    if not any(
        "Namespace/cni-test-ingress" in m
        and ("exactly 1" in m or "required label" in m)
        for m in issues
    ):
        return _fail(
            "C7-namespace-integrity",
            "validator rejected but did not name Namespace drift; "
            f"got: {'; '.join(issues)}",
        )
    return _ok(
        "C7-namespace-integrity",
        "validator rejected renamed Namespace/cni-test-ingress",
    )


def run_control_parse_failure():
    """C8: malformed YAML fed to the shared parser must raise/FAIL."""
    with tempfile.TemporaryDirectory() as tmp:
        bad_path = Path(tmp) / "bad-fixture.yaml"
        bad_path.write_text(
            "apiVersion: v1\n"
            "kind: Namespace\n"
            "metadata:\n"
            "  name: cni-arbitrary\n"
            "spec: [unterminated\n",  # broken YAML
            encoding="utf-8",
        )
        # The shared parser must raise RuntimeError; main() maps that
        # to a FAIL verdict and exits 1.
        try:
            _load_documents(bad_path)
            return _fail(
                "C8-parse-failure",
                "shared parser returned success on a deliberately "
                "malformed YAML; fail-closed invariant broken "
                "(fixture YAML parse failed must raise)",
            )
        except RuntimeError as exc:
            msg = str(exc)
            if "fixture YAML parse failed" not in msg or str(bad_path) not in msg:
                return _fail(
                    "C8-parse-failure",
                    f"shared parser raised but did not name file/path "
                    f"correctly; got: {msg}",
                )
            return _ok(
                "C8-parse-failure",
                f"shared parser raised fail-closed on malformed YAML: "
                f"{msg}",
            )


def run_control_namespace_empty_spec():
    """C9: spec={} on the Ingress NS -> reject (presence rule)."""
    docs = _load_target_docs()
    ingress = _find_one(docs, "Namespace", name=INGRESS_NAME)
    ingress["spec"] = {}
    issues = validate(docs)
    if not issues:
        return _fail(
            "C9-empty-spec",
            "expected validator to reject Namespace spec={}",
        )
    if not any(
        f"Namespace/{INGRESS_NAME}" in m and "'spec' must be absent" in m
        for m in issues
    ):
        return _fail(
            "C9-empty-spec",
            "validator rejected but did not name Namespace/spec; "
            f"got: {'; '.join(issues)}",
        )
    return _ok(
        "C9-empty-spec",
        "validator rejected Namespace/empty-spec by the absence rule",
    )


def run_control_namespace_unknown_spec():
    """C10: spec={'foo':'bar'} on the Ingress NS -> reject (presence rule)."""
    docs = _load_target_docs()
    ingress = _find_one(docs, "Namespace", name=INGRESS_NAME)
    ingress["spec"] = {"foo": "bar"}
    issues = validate(docs)
    if not issues:
        return _fail(
            "C10-unknown-spec",
            "expected validator to reject Namespace spec={'foo':'bar'}",
        )
    if not any(
        f"Namespace/{INGRESS_NAME}" in m and "'spec' must be absent" in m
        for m in issues
    ):
        return _fail(
            "C10-unknown-spec",
            "validator rejected but did not name Namespace/spec; "
            f"got: {'; '.join(issues)}",
        )
    return _ok(
        "C10-unknown-spec",
        "validator rejected Namespace/unknown-spec by the absence rule",
    )


CONTROLS = [
    ("C1-pristine", run_control_pristine),
    ("C2-doc-separation", run_control_document_separation),
    ("C3-exact-historical", run_control_exact_historical_mutation),
    ("C4-orphaned-service", run_control_orphaned_service),
    ("C5-wrong-port", run_control_wrong_port),
    ("C6-missing-service", run_control_missing_service),
    ("C7-namespace-integrity", run_control_namespace_integrity),
    ("C8-parse-failure", run_control_parse_failure),
    ("C9-empty-spec", run_control_namespace_empty_spec),
    ("C10-unknown-spec", run_control_namespace_unknown_spec),
]


def main():
    results = []
    for name, fn in CONTROLS:
        try:
            ok = fn()
        except AssertionError as e:
            ok = _fail(name, f"control precondition broke: {e}")
        except RuntimeError as e:
            # Fail-closed parser raised during the control body.
            ok = _fail(name, f"fail-closed parser raised: {e}")
        except Exception as e:
            ok = _fail(name, f"unexpected exception: {e!r}")
        results.append(ok)
    print(f"results={[int(r) for r in results]}")
    if not all(results):
        sys.exit(1)
    print("d2b.38 fixture Namespace/Service document-boundary: PASS")


if __name__ == "__main__":
    main()
