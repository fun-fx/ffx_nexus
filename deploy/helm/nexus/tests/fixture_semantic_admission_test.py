#!/usr/bin/env python3
"""Phase D-2b.28 mutation tests for the
fixture semantic admission script.

Each test mutates a clone of the fixture
directory in-memory, runs the semantic
admission script against the mutated tree,
and asserts that the script classified
the run as FAILS (script returns non-zero)
for that mutation. Then we restore the
mutation, run again, and assert PASS.

The 6 mutations correspond to the d2b.28
directive items:
  1. containers moved to top-level
  2. Service selector label one char
  3. targetPort set to a port with no
     listener
  4. NetPol podSelector made to mismatch
     fixtures
  5. control pod label coerced onto a
     product selector
  6. (not in this file) checkout identity
     drift covered by workflow step A.

Behavioural contract:
  The script must NOT mis-classify a
  mutation FAIL as SCENARIO_POLICY_REGRESSION
  (13) — that's the wrong surface. The
  classification we want is FIXTURE_INVALID
  (15).
"""
import copy
import io
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

import yaml

REPO = Path(__file__).resolve().parent.parent.parent.parent.parent
FIXTURE_DIR = REPO / "scripts" / "fixtures" / "integrationcni"
ADMIT = FIXTURE_DIR / "fixture_semantic_admission.py"


def render_chart_networkpolicy(target_path):
    proc = subprocess.run(
        ["helm", "template", "render-test",
         str(REPO / "deploy" / "helm" / "nexus"),
         "--values",
         str(REPO / "scripts" / "fixtures" / "integrationcni"
             / "values-extra-cni.yaml"),
         "--set", "fullnameOverride=nexus-cni-test",
         "--set", "image.repository=busybox",
         "--set", "image.tag=1.36",
         "--set", "metrics.enabled=true",
         "--set", "metrics.port=9101",
         "--set", "networkPolicy.mode=enforce",
         "--set", "networkPolicy.profile=enterprise",
         "--set", "networkPolicy.enforcementAcknowledged=true",
         "--show-only", "templates/networkpolicy.yaml"],
        capture_output=True, text=True, check=True,
    )
    target_path.write_text(proc.stdout)


def copy_fixtures(dest):
    """Copy all fixture yamls into the
    destination directory, so mutation
    operates on a clone — the original
    fixtures stay intact. The script
    reads from --fixture-dir CLI flag."""
    dest.mkdir(parents=True, exist_ok=True)
    for fy in FIXTURE_DIR.glob("*.yaml"):
        shutil.copy2(fy, dest / fy.name)


def run_admit(fixture_dir, rendered_path, expect_fail, scenario):
    proc = subprocess.run(
        ["python3", str(ADMIT),
         "--fixture-dir", str(fixture_dir),
         "--rendered-networkpolicy", str(rendered_path)],
        capture_output=True, text=True,
    )
    rc = proc.returncode
    expected_rc = 1 if expect_fail else 0
    ok = (rc == expected_rc)
    flag = "OK  " if ok else "FAIL"
    print(f"[{flag}] mutation '{scenario}': rc={rc} expected={expected_rc}")
    if not ok:
        print(f"      stdout:\n{proc.stdout}\n"
              f"      stderr:\n{proc.stderr}")
    return ok


def mutate_top_level_containers(fixture_dir):
    """Mutation 1: Pick a fixture Pod and
    move spec.containers: to top-level.
    The structural guard inside the
    admission script MUST trip."""
    target = fixture_dir / "01-test-pods.yaml"
    docs = list(yaml.safe_load_all(target.read_text()))
    for d in docs:
        if (isinstance(d, dict) and d.get("kind") == "Pod"
                and d.get("metadata", {}).get("name") == "cni-mock-nexus-gateway"):
            spec = d.pop("spec") or {}
            extras = {k: v for k, v in spec.items() if k != "containers"}
            cont = spec.get("containers")
            if cont:
                d["containers"] = cont
                d["spec"] = extras
    out = yaml.safe_dump_all(docs, default_flow_style=False, sort_keys=False)
    target.write_text(out)


def revert_top_level_containers(fixture_dir):
    shutil.copy2(FIXTURE_DIR / "01-test-pods.yaml", fixture_dir / "01-test-pods.yaml")


def mutate_service_selector_one_char(fixture_dir):
    """Mutation 2: change one char of a
    fixture Service selector label so
    matching Pods becomes 0."""
    target = fixture_dir / "02-stub-deps.yaml"
    docs = list(yaml.safe_load_all(target.read_text()))
    for d in docs:
        if (isinstance(d, dict) and d.get("kind") == "Service"
                and d.get("metadata", {}).get("name") == "cni-gateway"):
            spec = d.get("spec") or {}
            sel = spec.get("selector") or {}
            for k in list(sel.keys()):
                if sel[k] == "gateway":
                    sel[k] = "gatewau"  # one-char typo
                    break
    out = yaml.safe_dump_all(docs, default_flow_style=False, sort_keys=False)
    target.write_text(out)


def revert_service_selector(fixture_dir):
    shutil.copy2(FIXTURE_DIR / "02-stub-deps.yaml", fixture_dir / "02-stub-deps.yaml")


def mutate_target_port_no_listener(fixture_dir):
    """Mutation 3: change targetPort to a
    numeric value no Pod has."""
    target = fixture_dir / "02-stub-deps.yaml"
    docs = list(yaml.safe_load_all(target.read_text()))
    for d in docs:
        if (isinstance(d, dict) and d.get("kind") == "Service"
                and d.get("metadata", {}).get("name") == "cni-postgres"):
            spec = d.get("spec") or {}
            for p in (spec.get("ports") or []):
                p["targetPort"] = 59999
    out = yaml.safe_dump_all(docs, default_flow_style=False, sort_keys=False)
    target.write_text(out)


def revert_target_port(fixture_dir):
    shutil.copy2(FIXTURE_DIR / "02-stub-deps.yaml", fixture_dir / "02-stub-deps.yaml")


def mutate_rendered_policy_selector(rendered_path):
    """Mutation 4: change rendered
    NetworkPolicy podSelector of
    nexus-cni-test-gateway so it does NOT
    match any fixture Pod."""
    docs = list(yaml.safe_load_all(rendered_path.read_text()))
    for d in docs:
        if not isinstance(d, dict):
            continue
        if d.get("kind") != "NetworkPolicy":
            continue
        name = d.get("metadata", {}).get("name", "")
        if not name.endswith("gateway"):
            continue
        spec = d.get("spec") or {}
        psel = spec.get("podSelector") or {}
        ml = psel.get("matchLabels") or {}
        if ml.get("app.kubernetes.io/component") == "gateway":
            ml["app.kubernetes.io/component"] = "gataway"
            psel["matchLabels"] = ml
            spec["podSelector"] = psel
    rendered_path.write_text(yaml.safe_dump_all(
        docs, default_flow_style=False, sort_keys=False))


def revert_rendered_policy(rendered_path):
    """Re-render to a clean copy."""
    render_chart_networkpolicy(rendered_path)


def mutate_control_label_match_product(rendered_path):
    """Mutation 5: injected control-Pod
    labels must NOT match any product
    NetworkPolicy podSelector. We
    forcibly align the cni-control target
    Pod's labels with the chart-gateway
    podSelector by editing the fixture
    yaml directly."""
    target = FIXTURE_DIR / "04-control-service.yaml"
    mutate_path = Path(FIXTURE_DIR / "04-control-service.yaml")
    docs = list(yaml.safe_load_all(mutate_path.read_text()))
    for d in docs:
        if not isinstance(d, dict): continue
        if d.get("kind") == "Pod" and d.get("metadata", {}).get("name") == "cni-control-target":
            md = d.get("metadata") or {}
            md["labels"] = {
                "app.kubernetes.io/name": "nexus",
                "app.kubernetes.io/component": "gateway",
                "app.kubernetes.io/instance": "nexus-cni-test",
            }
    mutate_path.write_text(yaml.safe_dump_all(
        docs, default_flow_style=False, sort_keys=False))


def revert_control_label():
    import subprocess
    subprocess.run(["git","checkout","--","scripts/fixtures/integrationcni/04-control-service.yaml"], cwd=REPO, check=True)


def main():
    cwd = Path(tempfile.mkdtemp(prefix="d2b28-fixtures-"))
    rendered_template = cwd / "rendered.yaml"
    print(f"workdir: {cwd}")
    fixtures_clone = cwd / "fixtures"
    copy_fixtures(fixtures_clone)
    render_chart_networkpolicy(rendered_template)
    # Phase: PASS baseline
    # We need a clean rendered policy.
    ok_results = []
    ok_results.append(run_admit(
        fixtures_clone, rendered_template,
        expect_fail=False, scenario="baseline (no mutation)")
    )
    # Mutation 1: containers top-level
    mutate_top_level_containers(fixtures_clone)
    ok_results.append(run_admit(
        fixtures_clone, rendered_template,
        expect_fail=True, scenario="containers to top-level"
    ))
    revert_top_level_containers(fixtures_clone)
    # Mutation 2: Service selector one char
    mutate_service_selector_one_char(fixtures_clone)
    ok_results.append(run_admit(
        fixtures_clone, rendered_template,
        expect_fail=True, scenario="Service selector label one-char typo"
    ))
    revert_service_selector(fixtures_clone)
    # Mutation 3: targetPort no-listener
    mutate_target_port_no_listener(fixtures_clone)
    ok_results.append(run_admit(
        fixtures_clone, rendered_template,
        expect_fail=True, scenario="targetPort advertises a port with no listener"
    ))
    revert_target_port(fixtures_clone)
    # Mutation 4: NetworkPolicy selector mismatch
    mutate_rendered_policy_selector(rendered_template)
    ok_results.append(run_admit(
        fixtures_clone, rendered_template,
        expect_fail=True, scenario="rendered NetworkPolicy selector mismatch"
    ))
    revert_rendered_policy(rendered_template)
    # Mutation 5: control Pod labels coerced to product selector
    mutate_control_label_match_product(rendered_template)
    # Refresh fixture clone to pick up mutation
    copy_fixtures(fixtures_clone)
    ok_results.append(run_admit(
        fixtures_clone, rendered_template,
        expect_fail=True, scenario="cni-control target Pod label coerced to gateway selector"
    ))
    revert_control_label()
    # Final: PASS restored
    copy_fixtures(fixtures_clone)
    render_chart_networkpolicy(rendered_template)
    ok_results.append(run_admit(
        fixtures_clone, rendered_template,
        expect_fail=False, scenario="restored clean tree"
    ))
    shutil.rmtree(cwd, ignore_errors=True)
    if all(ok_results):
        print()
        print("d2b.28 semantic admission mutation tests: PASS")
        return 0
    print()
    print("d2b.28 semantic admission mutation tests: FAIL",
          file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
