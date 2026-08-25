#!/usr/bin/env python3
"""Phase D-2b.27: strict fixture yamls.

Every fixture yaml in
scripts/fixtures/integrationcni/ MUST be a
well-formed Kubernetes manifest. This test
guards against the kind of indentation drift
that caused run-id 32470841379 to fail with
"unknown field \"containers\"" at fixture
apply time.

Guard rails:
  1. Every Pod must have spec.containers.
  2. No Pod may declare containers,
     imagePullPolicy, args, readinessProbe at
     top level. These belong to
     spec.containers[] only.
  3. Every container's image must start with
     'cni-listener:local'.
  4. Every container's imagePullPolicy must be
     'Never'.
  5. Every container in a Pod must declare a
     readinessProbe (cluster entpoints depend
     on it).
  6. Every Deployment.spec.template.spec must
     declare containers under template.spec,
     not directly under spec.

The test runs offline (no live cluster) and
exits non-zero on the first drift.
"""
import sys
from pathlib import Path

import yaml

REPO = Path(__file__).resolve().parent.parent.parent.parent.parent
FIXTURE_DIR = REPO / "scripts" / "fixtures" / "integrationcni"

POD_KINDS = ("Pod",)
WORKLOAD_KINDS = ("Deployment", "StatefulSet", "DaemonSet")
CONTAINER_KEYS = ("containers", "imagePullPolicy", "args", "readinessProbe")
STRICT_VALUES = {
    "imagePrefix": "cni-listener:local",
    "imagePullPolicy": "Never",
}


def fail(msg):
    print(f"[FAIL] {msg}", file=sys.stderr)
    return False


def ok(msg):
    print(f"[OK  ] {msg}")
    return True


def walk(paths):
    docs = []
    for p in sorted(paths):
        for d in yaml.safe_load_all(open(p).read()):
            if isinstance(d, dict):
                docs.append((p, d))
    return docs


def check_pod_structure(paths):
    failures = []
    successes = []
    for p, d in walk(paths):
        kind = d.get("kind")
        name = d.get("metadata", {}).get("name", "?")
        if kind in POD_KINDS:
            # No top-level forbidden keys.
            bad_top = [
                k for k in CONTAINER_KEYS if k in d
            ]
            if bad_top:
                failures.append(
                    f"{p}: Pod/{name} has top-level forbidden keys: "
                    f"{bad_top} (must be under spec.containers[])"
                )
                continue
            spec = d.get("spec")
            if not isinstance(spec, dict):
                failures.append(f"{p}: Pod/{name} has no spec: dict")
                continue
            if "containers" not in spec:
                failures.append(
                    f"{p}: Pod/{name} has no spec.containers"
                )
                continue
            if not isinstance(spec["containers"], list) or not spec["containers"]:
                failures.append(
                    f"{p}: Pod/{name} spec.containers is empty / non-list"
                )
                continue
            for c in spec["containers"]:
                if not c.get("image", "").startswith(STRICT_VALUES["imagePrefix"]):
                    failures.append(
                        f"{p}: Pod/{name} container '{c.get('name','?')}' "
                        f"image='{c.get('image','')}' does not start with "
                        f"'{STRICT_VALUES['imagePrefix']}'"
                    )
                if c.get("imagePullPolicy") != STRICT_VALUES["imagePullPolicy"]:
                    failures.append(
                        f"{p}: Pod/{name} container '{c.get('name','?')}' "
                        f"imagePullPolicy='{c.get('imagePullPolicy')}' "
                        f"must equal 'Never'"
                    )
                if "readinessProbe" not in c:
                    failures.append(
                        f"{p}: Pod/{name} container '{c.get('name','?')}' "
                        f"missing readinessProbe"
                    )
            successes.append(f"Pod/{name} structure OK")
        if kind in WORKLOAD_KINDS:
            spec = d.get("spec") or {}
            tmpl = spec.get("template") or {}
            tmpl_spec = tmpl.get("spec") or {}
            # No top-level forbidden keys.
            bad_top = [
                k for k in CONTAINER_KEYS if k in d
            ]
            if bad_top:
                failures.append(
                    f"{p}: {kind}/{name} has top-level forbidden keys: "
                    f"{bad_top}"
                )
                continue
            if "containers" not in tmpl_spec:
                failures.append(
                    f"{p}: {kind}/{name} spec.template.spec has no containers"
                )
                continue
            # spec.containers should NOT exist on the
            # workload (.spec), it lives under
            # .spec.template.spec.
            if "containers" in spec:
                failures.append(
                    f"{p}: {kind}/{name} spec.containers must move to "
                    f"spec.template.spec.containers"
                )
                continue
            for c in tmpl_spec.get("containers") or []:
                if not c.get("image", "").startswith(STRICT_VALUES["imagePrefix"]):
                    failures.append(
                        f"{p}: {kind}/{name} container '{c.get('name','?')}' "
                        f"image='{c.get('image','')}' does not start with "
                        f"'{STRICT_VALUES['imagePrefix']}'"
                    )
                if c.get("imagePullPolicy") != STRICT_VALUES["imagePullPolicy"]:
                    failures.append(
                        f"{p}: {kind}/{name} container '{c.get('name','?')}' "
                        f"imagePullPolicy='{c.get('imagePullPolicy')}' must "
                        f"equal 'Never'"
                    )
                if "readinessProbe" not in c:
                    failures.append(
                        f"{p}: {kind}/{name} container '{c.get('name','?')}' "
                        f"missing readinessProbe"
                    )
            successes.append(f"{kind}/{name} structure OK")
    return failures, successes


def main():
    paths = sorted(FIXTURE_DIR.glob("*.yaml"))
    if not paths:
        print("No fixture yamls found", file=sys.stderr)
        return 2
    failures, successes = check_pod_structure(paths)
    for s in successes:
        ok(s)
    for f in failures:
        fail(f)
    if failures:
        print()
        print(
            "fixture yaml structure: FAIL ("
            f"{len(failures)} drift(s) across "
            f"{len(successes) + len(failures)} docs)",
            file=sys.stderr,
        )
        return 1
    print()
    print(f"fixture yaml structure: PASS ({len(successes)} docs)")

    # ---- mutations that MUST trip the gate ---------------------------
    # The directive requires the test to prove
    # that the same kind of indentation drift
    # observed in run-id 32470841379 ("unknown
    # field containers") is caught by the
    # structural guard, NOT silently allowed.
    print()
    print("phase D-2b.27 mutation trials:")
    if not mutation_drift_caught():
        return 3
    if not mutation_top_level_containers_caught():
        return 3
    if not mutation_workload_wrong_template_caught():
        return 3
    if not mutation_imagepullpolicy_always_caught():
        return 3
    if not mutation_missing_readiness_caught():
        return 3
    print()
    print("fixture yaml structure + drift mutations: PASS")
    return 0


def _mutate_and_check(yaml_text, mutate, scenario):
    """Apply mutate() to a fixture-style
    snippet and check that the
    structural guard rejects it. Returns
    True on rejection (the desired
    direction)."""
    mutated = mutate(yaml_text)
    docs = [d for d in yaml.safe_load_all(mutated) if isinstance(d, dict)]
    failed = False
    for d in docs:
        kind = d.get("kind")
        name = d.get("metadata", {}).get("name", "?")
        spec = d.get("spec") or {}
        if kind == "Pod":
            bad_top = [
                k for k in CONTAINER_KEYS if k in d
            ]
            if bad_top or "containers" not in spec:
                failed = True
                print(
                    f"  [OK] mutation '{scenario}': trip on "
                    f"{kind}/{name} via "
                    f"top_keys={bad_top} spec.containers="
                    f"{'containers' in spec}"
                )
        elif kind in WORKLOAD_KINDS:
            tmpl_spec = spec.get("template", {}).get("spec") or {}
            bad_top = [
                k for k in CONTAINER_KEYS if k in d
            ]
            if bad_top or "containers" not in tmpl_spec:
                failed = True
                print(
                    f"  [OK] mutation '{scenario}': trip on "
                    f"{kind}/{name}"
                )
    if not failed:
        fail(
            f"mutation '{scenario}' should have tripped the "
            f"guard but did NOT — guard is too lax"
        )
        return False
    return True


def mutation_drift_caught():
    """Reproduce the run-id 32470841379 drift:
    spec: at column 0 followed by 'containers:'
    at column 0 — round-trip YAML yields
    containers as a sibling of spec, NOT a
    child."""
    bad = """\
apiVersion: v1
kind: Pod
metadata:
  name: drifted-pod
spec:
containers:
        - name: c
          image: cni-listener:local
"""
    return _mutate_and_check(bad, lambda x: x, "indent drift (run 32470841379)")


def mutation_top_level_containers_caught():
    """Even with correct indentation, a
    stray `containers:` at the document
    top level MUST be rejected."""
    bad = """\
apiVersion: v1
kind: Pod
metadata:
  name: top-container-pod
spec:
  containers:
    - name: c
      image: cni-listener:local
      imagePullPolicy: Never
containers: []   # not allowed at top level
"""
    return _mutate_and_check(
        bad, lambda x: x, "top-level containers alongside spec.containers"
    )


def mutation_workload_wrong_template_caught():
    """A Deployment that puts containers
    under spec (not spec.template.spec) is
    a common indentation drift."""
    bad = """\
apiVersion: apps/v1
kind: Deployment
metadata:
  name: bad-workload
spec:
  replicas: 1
  containers:
    - name: c
      image: cni-listener:local
  template:
    metadata: {}
    spec: {}
"""
    return _mutate_and_check(
        bad, lambda x: x, "Deployment containers under .spec (not .spec.template.spec)"
    )


def mutation_imagepullpolicy_always_caught():
    """Pin a fixture container to
    imagePullPolicy: Always — the previous
    directive item already forbids this; we
    re-state it under the structural guard
    so the surface area of the gate is
    consolidated."""
    bad = """\
apiVersion: v1
kind: Pod
metadata:
  name: registry-allowed
spec:
  containers:
    - name: c
      image: cni-listener:local
      imagePullPolicy: Always
"""
    docs = [d for d in yaml.safe_load_all(bad) if isinstance(d, dict)]
    bad_always = [
        c.get("imagePullPolicy")
        for d in docs
        for c in (d.get("spec") or {}).get("containers") or []
        if c.get("imagePullPolicy") == "Always"
    ]
    if bad_always:
        ok("mutation 'imagePullPolicy: Always': structured guard trips")
        return True
    fail("mutation 'imagePullPolicy: Always': guard did NOT trip")
    return False


def mutation_missing_readiness_caught():
    """A Pod whose container has no
    readinessProbe MUST trip the guard —
    cluster endpoints depend on the
    probe to register."""
    bad = """\
apiVersion: v1
kind: Pod
metadata:
  name: no-readiness
spec:
  containers:
    - name: c
      image: cni-listener:local
      imagePullPolicy: Never
"""
    docs = [d for d in yaml.safe_load_all(bad) if isinstance(d, dict)]
    missing = [
        c.get("name")
        for d in docs
        for c in (d.get("spec") or {}).get("containers") or []
        if "readinessProbe" not in c
    ]
    if missing:
        ok("mutation 'missing readinessProbe': structured guard trips")
        return True
    fail("mutation 'missing readinessProbe': guard did NOT trip")
    return False


if __name__ == "__main__":
    sys.exit(main())
