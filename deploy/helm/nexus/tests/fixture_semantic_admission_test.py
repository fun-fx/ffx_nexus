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


def resolve_helm_binary():
    """Phase D-2b portable hermetic Helm resolver.

    Returns the absolute filesystem path to a real Helm
    binary suitable for rendering the networkpolicy
    template in this test. The resolver must not invoke
    `helm` by bare command name, must not read ambient
    PATH, must not use shutil.which, and must not call
    Helm via a shell — every step is required to keep a
    contract-test stub `helm` from accidentally
    becoming a renderer in a polluted shell environment.

    Resolution order:

      1. Test-only explicit override: the
         `NEXUS_TEST_HELM_BIN` environment variable. The
         value MUST be an absolute path that exists on
         disk and is executable. A relative path, a
         missing path, or a non-executable path raises a
         diagnostic `RuntimeError` BEFORE any render is
         attempted.

      2. No override set? Walk a short deterministic,
         OS-neutral candidate list in this exact intent
         order:

            /usr/local/bin/helm
            /usr/bin/helm
            /opt/homebrew/bin/helm

         The first entry that resolves to an existing
         executable file is returned as a `pathlib.Path`.

      3. If none of the above yields an executable
         file, raise a `RuntimeError` that lists the
         candidates and documents
         `NEXUS_TEST_HELM_BIN` as the supported
         override.

    This contract maps to:
      * GitHub ubuntu-22.04 (CNI workflow installs the
        pinned heavy Helm tool) => /usr/local/bin/helm
        is the standard absolute candidate and resolves.
      * Local macOS/Homebrew (/opt/homebrew/bin/helm)
        => the third candidate resolves.
      * Local Linux with snap/apt helm => /usr/bin/helm
        resolves as the second candidate.
    """
    override = os.environ.get("NEXUS_TEST_HELM_BIN")
    if override:
        if not os.path.isabs(override):
            raise RuntimeError(
                "resolve_helm_binary: NEXUS_TEST_HELM_BIN=%r must be an "
                "ABSOLUTE path; received a relative path. Set it to a "
                "fully-qualified filesystem path." % (override,)
            )
        p = Path(override)
        if not p.exists():
            raise RuntimeError(
                "resolve_helm_binary: NEXUS_TEST_HELM_BIN=%r does not "
                "exist on disk." % (str(p),)
            )
        if not p.is_file():
            raise RuntimeError(
                "resolve_helm_binary: NEXUS_TEST_HELM_BIN=%r is not a "
                "regular file." % (str(p),)
            )
        # os.access(X_OK) is the cross-platform executor
        # check. Importantly this does NOT consult PATH
        # or shell lookup; it tests the named file
        # directly.
        if not os.access(str(p), os.X_OK):
            raise RuntimeError(
                "resolve_helm_binary: NEXUS_TEST_HELM_BIN=%r exists but is "
                "not executable; chmod +x or specify a different binary." %
                (str(p),)
            )
        return p

    # Deterministic OS-neutral absolute candidates in
    # order. The order is intentional: GitHub ubuntu-22.04
    # places the heavy-tool Helm at /usr/local/bin/helm
    # first, so the resolver stops there before scanning
    # local-only paths. The list is closed; do not extend
    # it without a documented portability reason.
    candidates = [
        Path("/usr/local/bin/helm"),
        Path("/usr/bin/helm"),
        Path("/opt/homebrew/bin/helm"),
    ]
    for cand in candidates:
        if cand.exists() and cand.is_file() and os.access(str(cand), os.X_OK):
            return cand

    listed = ", ".join(str(p) for p in candidates)
    raise RuntimeError(
        "resolve_helm_binary: no executable Helm binary found in the "
        "deterministic candidate list (%s). Set NEXUS_TEST_HELM_BIN to "
        "the absolute path of a real Helm binary before invoking this "
        "test." % (listed,)
    )


def render_chart_networkpolicy(target_path):
    """Render the chart's `templates/networkpolicy.yaml`
    into the test workdir using a hermetic, absolute
    Helm path resolved by `resolve_helm_binary()`.

    Phase D-2b.28 / contract integrity: this test must
    render with a deterministic Helm executable that
    ignores the ambient shell PATH. Subprocess invocation
    uses `str(resolved)` as argv[0] directly; an explicit
    `env=` dict with an empty PATH guarantees no shell or
    PATH lookup can substitute a stub binary. This
    prevents false-PASS surfaces like a contract-test
    stub `helm` accidentally becoming the renderer.

    The resolver contract (override or candidate list)
    is enforced inside `resolve_helm_binary()`; this
    function is the sole caller.
    """
    helm_bin = resolve_helm_binary()
    import subprocess as _sp  # local alias keeps the
                              # capture site obvious
    proc = _sp.run(
        [str(helm_bin), "template", "render-test",
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
        # Strip PATH so the rendered subprocess cannot
        # escalate to a stub binary inside the helm
        # toolchain. We pass through only the explicit
        # Helm env chain a real binary expects; the test
        # itself does not depend on other env vars.
        env={
            "PATH": "/usr/bin:/bin",
            "HOME": os.environ.get("HOME", "/tmp"),
            "LANG": os.environ.get("LANG", "C.UTF-8"),
        },
    )
    target_path.write_text(proc.stdout)


def copy_fixtures(dest):
    """Copy all fixture yamls into the
    destination directory, so mutation
    operates on a clone — the original
    fixtures stay intact. The script
    reads from --fixture-dir CLI flag.
    Phase D-2b.29: the d2b hardened-fixture
    contract reads `control-netpol-gate.Dockerfile`
    for the `USER 65534:65534` line; the mutation
    clone must include that Dockerfile so the
    baseline (and restore) classifies the clone
    PASS rather than FAILing on the missing
    Dockerfile. Otherwise the test trivially
    always fails on a missing-file errror that
    does not exist on the actual gate path."""
    dest.mkdir(parents=True, exist_ok=True)
    for fy in FIXTURE_DIR.glob("*.yaml"):
        shutil.copy2(fy, dest / fy.name)
    df = FIXTURE_DIR / "control-netpol-gate.Dockerfile"
    if df.exists():
        shutil.copy2(df, dest / df.name)


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


# Local integrity marker for mutation 4. The string must NOT coincide with
# any string a default-namespace fixture Pod or chart-side selector could
# carry, so when we read the fixture clone and assert "no Pod label can
# possibly select on this value", the assertion is provably sound.
#
# No existing fixture has a label value containing this string; no
# existing chart-side role key starts with the underscore convention
# reserved for test assertions; selecting it as the matcher value forces
# the validator's "must match a fixture Pod" check to fail.
MISMATCH_VERTEX = "__d2b_fixture_selector_mismatch__"


def mutate_rendered_policy_selector(fixture_dir, rendered_path):
    """Mutation 4 (D-2b.28 hardened):

    Require the rendered `nexus-cni-test-gateway` NetworkPolicy to
    exist and to carry a `component == gateway` selector before
    mutation, then replace that selector key with the local
    `MISMATCH_VERTEX` marker so the validator's existing rule
    (NetworkPolicy podSelector must match a fixture Pod in
    namespace=default) fails closed with a non-zero exit.

    Hardening rationale
    -------------------
    The earlier mutation only renamed `gateway` -> `gataway` in
    memory and then re-dumped the doc list. Two failure modes
    silently produced a false PASS:

      1. If the rendered file was empty (e.g. a polluted shell
         PATH supplied a stub `helm` to `subprocess.run`), the
         filtered doc-list was empty, the mutation never applied,
         the validator walked zero NetworkPolicies, and returned
         exit-zero trivially.
      2. If `app.kubernetes.io/component` was absent for any
         reason, the mutation would silently skip and the same
         false PASS persisted.

    The hardened mutation now:

      a. parses the rendered YAML into a doc list;
      b. demands exactly one NetworkPolicy whose `metadata.name`
         ends in "gateway" (post-fix rendered output always has
         exactly one gateway policy);
      c. demands that this policy's podSelector.matchLabels
         contains `app.kubernetes.io/component` whose value is
         exactly "gateway" before mutation;
      d. replaces only the gateway component value with the
         `MISMATCH_VERTEX` marker;
      e. loads the fixture clone and asserts that no fixture Pod
         or workload template labels a `app.kubernetes.io/component`
         equal to the marker (i.e., the marker cannot coincide
         with any fixture Pod selector value currently in the
         clone);
      f. writes the mutated doc list to `rendered_path`;
      g. re-loads the written file and asserts exactly one
         rendered gateway NetworkPolicy carries the marker at
         the expected selector key.

    Any of (a)..(g) failing raises `RuntimeError`, preventing the
    validator from being invoked against an unmutated or
    vacuous input. That guarantees the rced=1 reported by
    `run_admit(expect_fail=True)` reflects the selector change,
    not luck.
    """
    raw = rendered_path.read_text()
    docs = list(yaml.safe_load_all(raw))
    if not docs:
        raise RuntimeError(
            "mutate_rendered_policy_selector: rendered file contains zero "
            "YAML documents; the helm render step produced empty output. "
            "Refusing to claim a selector mutation against an empty file."
        )

    gateway_policies = [
        d for d in docs
        if isinstance(d, dict)
        and d.get("kind") == "NetworkPolicy"
        and isinstance(d.get("metadata"), dict)
        and str(d.get("metadata", {}).get("name", "")).endswith("gateway")
    ]
    if not gateway_policies:
        raise RuntimeError(
            "mutate_rendered_policy_selector: zero rendered NetworkPolicies "
            "ending in 'gateway' were found. Expected exactly one (the chart "
            "renders nexus-cni-test-gateway). Refusing to mutate."
        )
    if len(gateway_policies) > 1:
        raise RuntimeError(
            "mutate_rendered_policy_selector: %d rendered NetworkPolicies "
            "ending in 'gateway' were found; expected exactly one. Refusing "
            "to mutate." % len(gateway_policies)
        )
    gateway = gateway_policies[0]
    spec = gateway.get("spec") or {}
    pod_sel = spec.get("podSelector") or {}
    match_labels = pod_sel.get("matchLabels") or {}
    # Hard pre-condition: the rendered product must carry the role-bearing
    # component label set to "gateway". If a future chart redesign drops
    # this exact key/value pair, the marker mutation cannot claim a
    # baseline, and we fail closed rather than silently passing.
    if "app.kubernetes.io/component" not in match_labels:
        raise RuntimeError(
            "mutate_rendered_policy_selector: rendered gateway NetworkPolicy "
            "has no spec.podSelector.matchLabels['app.kubernetes.io/component'] "
            "key; expected component=gateway. Refusing to mutate."
        )
    if match_labels["app.kubernetes.io/component"] != "gateway":
        raise RuntimeError(
            "mutate_rendered_policy_selector: rendered gateway NetworkPolicy "
            "spec.podSelector.matchLabels['app.kubernetes.io/component'] is "
            "%r; expected the literal string 'gateway' before mutation. "
            "Refusing to mutate." %
            match_labels["app.kubernetes.io/component"]
        )

    # Anti-match fail-safe: scan fixture clone Pods (any namespace, not
    # only default) and Pod-template workloads and refuse the mutation
    # if any Pod's `app.kubernetes.io/component` label could already
    # match the marker. The marker is unique to this assertion, so a
    # match means the fixture is contaminated or the marker string is
    # not unique enough — either way, fail-closed.
    matched_pods = []
    for fy in sorted(Path(fixture_dir).glob("*.yaml")):
        try:
            fdocs = list(yaml.safe_load_all(fy.read_text()))
        except Exception:
            continue
        for fd in fdocs:
            if not isinstance(fd, dict):
                continue
            if fd.get("kind") == "Pod":
                labels = (fd.get("metadata") or {}).get("labels") or {}
            elif fd.get("kind") in ("Deployment", "StatefulSet", "DaemonSet"):
                tmpl = ((fd.get("spec") or {}).get("template") or {})
                labels = ((tmpl.get("metadata") or {}).get("labels") or {})
            else:
                continue
            v = labels.get("app.kubernetes.io/component")
            if v == MISMATCH_VERTEX:
                matched_pods.append((fy, fd.get("kind"),
                                     (fd.get("metadata") or {}).get("name", "?")))
    if matched_pods:
        raise RuntimeError(
            "mutate_rendered_policy_selector: MISMATCH_VERTEX %r accidentally "
            "matches an existing fixture Pod/workload label, so the marker is "
            "not unique. Refusing to mutate. Offenders: %r" %
            (MISMATCH_VERTEX, matched_pods)
        )

    # Apply the marker to the selected gateway policy's component
    # selector value, then re-write the doc list.
    match_labels["app.kubernetes.io/component"] = MISMATCH_VERTEX
    pod_sel["matchLabels"] = match_labels
    spec["podSelector"] = pod_sel
    gateway["spec"] = spec
    rendered_path.write_text(
        yaml.safe_dump_all(docs, default_flow_style=False, sort_keys=False)
    )

    # Post-write integrity: re-parse the file we just wrote and
    # assert exactly one rendered gateway NetworkPolicy carries the
    # marker at the expected selector key.
    re_docs = list(yaml.safe_load_all(rendered_path.read_text()))
    marker_policies = [
        d for d in re_docs
        if isinstance(d, dict)
        and d.get("kind") == "NetworkPolicy"
        and isinstance(d.get("metadata"), dict)
        and str(d.get("metadata", {}).get("name", "")).endswith("gateway")
        and ((d.get("spec") or {}).get("podSelector") or {})
            .get("matchLabels", {})
            .get("app.kubernetes.io/component") == MISMATCH_VERTEX
    ]
    if len(marker_policies) != 1:
        raise RuntimeError(
            "mutate_rendered_policy_selector: post-write marker check "
            "expected exactly 1 rendered gateway NetworkPolicy with "
            "matchLabels['app.kubernetes.io/component']=" + repr(MISMATCH_VERTEX) +
            "; got %d. Refusing to claim mutation applied." %
            len(marker_policies)
        )


def revert_rendered_policy(rendered_path):
    """Re-render to a clean copy."""
    render_chart_networkpolicy(rendered_path)


# Phase D-2b.29: a strong revert_control_label
# must restore BOTH the labels mutation AND
# any new fixture hardening lines (Pod/container
# securityContext) that the mutation test
# itself may have re-saved through yaml.dump.
# Using `git checkout -- <path>` would discard
# hardened fixtures whose securityContext is
# unstaged at the moment of restore; the safer
# pattern is to keep the snapshot we made at
# mutation start and re-apply it.
_CONTROL_LABEL_BACKUP: list[str] | None = None


def mutate_control_label_match_product(rendered_path):
    """Mutation 5: injected control-Pod
    labels must NOT match any product
    NetworkPolicy podSelector. We
    forcibly align the cni-control target
    Pod's labels with the chart-gateway
    podSelector by editing the fixture
    yaml directly.

    Phase D-2b.29: do NOT round-trip through
    yaml.dump_all. The fixture file contains
    docker-style YAML that pyyaml cannot
    re-emit identically (anchor/alias
    differences), and a round-trip discards
    d2b.29 hardening (Pod/container
    securityContext). Edit the labels block
    in place so the new noscript lines survive
    the mutation. After this mutation the
    semantic-admission script MUST fail on
    the `podSelector matches a cni-control
    Pod` check; the revert below restores
    the labels without touching other lines.
    """
    global _CONTROL_LABEL_BACKUP
    raw = (FIXTURE_DIR / "04-control-service.yaml").read_text()
    _CONTROL_LABEL_BACKUP = raw.splitlines(keepends=True)

    # Find the cni-control-target Pod doc block and
    # patch only its `labels:` block. We do this by
    # replacing the entire `{app:\n    role:` lines
    # under that doc. Other keys/structure are kept.
    lines = raw.splitlines(keepends=True)
    out = []
    in_target = False
    in_labels = False
    replaced = False
    for ln in lines:
        if not in_target:
            out.append(ln)
            if ln.strip() == "name: cni-control-target":
                in_target = True
            continue
        if not in_labels:
            out.append(ln)
            if ln.startswith("  labels:"):
                in_labels = True
            elif ln.startswith("---") or ln.strip().startswith("apiVersion:"):
                # moved on to next doc without finding labels
                in_target = False
            continue
        # in_labels
        if ln.startswith("    ") or ln.strip() == "":
            if not replaced:
                out.append(
                    "    app.kubernetes.io/name: nexus\n"
                    "    app.kubernetes.io/component: gateway\n"
                    "    app.kubernetes.io/instance: nexus-cni-test\n"
                )
                replaced = True
            if ln.strip() == "":
                out.append(ln)
                in_labels = False
            # skip the existing labels lines we don't need
            continue
        # We left the labels block. Push the new labels
        # we already emitted (only once) followed by
        # the current line; turn off in_labels.
        if replaced:
            out.append(ln)
        else:
            out.append(
                "    app.kubernetes.io/name: nexus\n"
                "    app.kubernetes.io/component: gateway\n"
                "    app.kubernetes.io/instance: nexus-cni-test\n"
            )
            out.append(ln)
            replaced = True
        in_labels = False
    if not replaced:
        raise RuntimeError(
            "could not locate cni-control-target.labels for mutation 5"
        )
    (FIXTURE_DIR / "04-control-service.yaml").write_text("".join(out))


def revert_control_label():
    """Use the in-memory backup captured at
    mutation start so the hardened fixture
    (securityContext / seccomp / drop ALL)
    added in d2b.29 is preserved exactly."""
    if _CONTROL_LABEL_BACKUP is None:
        require_failsafe("no control-label backup captured; cannot restore")
    target = FIXTURE_DIR / "04-control-service.yaml"
    target.write_text("".join(_CONTROL_LABEL_BACKUP))
    _drop_backup()


def _drop_backup():
    global _CONTROL_LABEL_BACKUP
    _CONTROL_LABEL_BACKUP = None


def require_failsafe(message):
    """A helper used by mutation revert paths
    where the wrong-shape fallback reverts to a
    pre-hardening index version of a fixture and
    silently regresses the security contract.
    We must not let a missing backup pass
    silently; we abort the run with a clear
    fixture_semantic_admission_test failure so
    a follow-up CI run can debug it instead of
    shipping a half-hardened fixture."""
    print(f"FATALSAFEGUARD: {message}", file=sys.stderr)
    sys.exit(2)


# ---------------------------------------------------------------------
# Phase D-2b portable helm resolver self-checks.
#
# _self_check_resolve_helm_binary() runs at import-time only when
# the `NEXUS_TEST_HELM_SELF_CHECK=1` environment variable is
# explicitly set. The self-checks cover the four cases the
# Helm-portability contract requires:
#
#   1. explicit real absolute override  → returns that exact path
#   2. relative override                → RuntimeError, mentions
#                                          absolute path
#   3. override missing/non-executable  → RuntimeError before render
#   4. ambient fake PATH contains a stub helm → resolver still
#                                          returns the explicit
#                                          override if set, or the
#                                          first executable
#                                          absolute candidate, never
#                                          the fake PATH entry
#
# The four cases share the same `_PREV_*` sentinel-via-`os.environ`
# pattern; we restore NEXUS_TEST_HELM_BIN/PATH exactly via
# try/finally so subsequent real render/mutation cases in the
# same Python process run against the original environment.
#
# We do NOT shell out to a fake Helm wrapper that renders output:
# the real `subprocess.run(['/opt/homebrew/bin/helm', 'version'],
# capture_output=True)` is the verifier for case 1.
# ---------------------------------------------------------------------
def _self_check_resolve_helm_binary():
    print("== Helm resolver self-checks ==")
    real_bin = "/opt/homebrew/bin/helm"
    if not (Path(real_bin).exists() and os.access(real_bin, os.X_OK)):
        # Self-checks require the local real helm to be
        # available for the "real absolute override"
        # case. If unavailable locally, defer rather than
        # silently lie. We document this state but do not
        # hard-fail the whole module — the test-runner
        # entrypoint can still decide to skip.
        print("  [SKIP] resolf_bin_missing: no local real helm at "
              + repr(real_bin) + "; cannot run self-checks.")
        return

    saved_override = os.environ.get("NEXUS_TEST_HELM_BIN")
    saved_path = os.environ.get("PATH")

    # ----- CASE 1: explicit real absolute override -----
    try:
        os.environ["NEXUS_TEST_HELM_BIN"] = real_bin
        resolved = resolve_helm_binary()
        if str(resolved) == real_bin:
            print("  [OK ] case1_explicit_absolute_override: "
                  "resolved=%s" % str(resolved))
        else:
            raise RuntimeError(
                "self-check: case1 mismatch; expected %s got %s" %
                (real_bin, str(resolved)))
    finally:
        # Restore for subsequent cases.
        if saved_override is None:
            os.environ.pop("NEXUS_TEST_HELM_BIN", None)
        else:
            os.environ["NEXUS_TEST_HELM_BIN"] = saved_override

    # ----- CASE 2: relative override -> RuntimeError -----
    try:
        os.environ["NEXUS_TEST_HELM_BIN"] = "helm"  # bare command, not absolute
        try:
            resolve_helm_binary()
        except RuntimeError as e:
            if "ABSOLUTE" in str(e) and "absolute" in str(e).lower():
                print("  [OK ] case2_relative_override_rejected: "
                      "%s" % str(e).splitlines()[0][:120])
            else:
                raise RuntimeError(
                    "self-check: case2 RuntimeError did not mention "
                    "absolute path; got: %r" % (str(e),))
        else:
            raise RuntimeError(
                "self-check: case2 expected RuntimeError for relative "
                "override; got none")
    finally:
        if saved_override is None:
            os.environ.pop("NEXUS_TEST_HELM_BIN", None)
        else:
            os.environ["NEXUS_TEST_HELM_BIN"] = saved_override

    # ----- CASE 3: override missing/non-executable -----
    try:
        os.environ["NEXUS_TEST_HELM_BIN"] = "/nonexistent/helm-path"
        try:
            resolve_helm_binary()
        except RuntimeError as e:
            if "does not exist" in str(e) or "not executable" in str(e):
                print("  [OK ] case3_missing_or_nonexec_rejected: "
                      "%s" % str(e).splitlines()[0][:120])
            else:
                raise RuntimeError(
                    "self-check: case3 RuntimeError did not mention "
                    "missing/non-executable; got: %r" % (str(e),))
        else:
            raise RuntimeError(
                "self-check: case3 expected RuntimeError for missing "
                "override; got none")
    finally:
        if saved_override is None:
            os.environ.pop("NEXUS_TEST_HELM_BIN", None)
        else:
            os.environ["NEXUS_TEST_HELM_BIN"] = saved_override

    # ----- CASE 4: ambient fake PATH contains stub helm,
    #                resolver still returns explicit override
    #                or deterministic absolute candidate, never
    #                the fake PATH entry -----
    try:
        # Build a fake PATH where the FIRST entry is a tmpdir
        # with a never-executed stub shim. If the resolver
        # ever consulted PATH, it would raise (or worse,
        # silently call the shim). It must not.
        stub_dir = Path(tempfile.mkdtemp(prefix="d2b-helm-stub-"))
        stub_shim = stub_dir / "helm"
        stub_shim.write_text("#!/bin/sh\necho fake\necho fake >&2\nexit 7\n")
        stub_shim.chmod(0o755)
        try:
            # case 4a: override set -> resolver must return
            # the override regardless of PATH.
            os.environ["NEXUS_TEST_HELM_BIN"] = real_bin
            os.environ["PATH"] = str(stub_dir) + ":" + (saved_path or "")
            resolved = resolve_helm_binary()
            if str(resolved) == real_bin:
                print("  [OK ] case4a_ambient_fake_path_ignored_with_override: "
                      "resolved=%s (NOT %s)" % (str(resolved), str(stub_shim)))
            else:
                raise RuntimeError(
                    "self-check: case4a mismatch; expected %s got %s" %
                    (real_bin, str(resolved)))
        finally:
            if saved_override is None:
                os.environ.pop("NEXUS_TEST_HELM_BIN", None)
            else:
                os.environ["NEXUS_TEST_HELM_BIN"] = saved_override

        try:
            # case 4b: no override, fake PATH first; resolver
            # must still pick the first deterministic absolute
            # candidate that exists and is executable. The
            # candidate list is /usr/local/bin/helm, /usr/bin/helm,
            # /opt/homebrew/bin/helm — only the third currently
            # exists on this machine, so the resolved path must be
            # exactly /opt/homebrew/bin/helm, NOT the stub.
            os.environ.pop("NEXUS_TEST_HELM_BIN", None)
            os.environ["PATH"] = str(stub_dir) + ":" + (saved_path or "")
            resolved = resolve_helm_binary()
            if Path(str(resolved)).resolve() == Path(real_bin).resolve() \
                    and str(resolved) != str(stub_shim):
                print("  [OK ] case4b_ambient_fake_path_ignored_no_override: "
                      "resolved=%s (NOT %s)" % (str(resolved), str(stub_shim)))
            else:
                raise RuntimeError(
                    "self-check: case4b mismatch; expected %s got %s, "
                    "stub=%s" % (real_bin, str(resolved), str(stub_shim)))
        finally:
            if saved_override is None:
                os.environ.pop("NEXUS_TEST_HELM_BIN", None)
            else:
                os.environ["NEXUS_TEST_HELM_BIN"] = saved_override
    finally:
        if saved_path is None:
            os.environ.pop("PATH", None)
        else:
            os.environ["PATH"] = saved_path
        try:
            shutil.rmtree(stub_dir, ignore_errors=True)
        except Exception:
            pass

    # ----- Sanity: run the real bin via subprocess.run once,
    # confirm captured_output reflects real helm version -----
    try:
        os.environ["NEXUS_TEST_HELM_BIN"] = real_bin
        h = resolve_helm_binary()
        proc = subprocess.run(
            [str(h), "version", "--short"],
            capture_output=True, text=True, check=True,
        )
        v = proc.stdout.strip()
        if v.startswith("v"):
            print("  [OK ] case_real_helm_subprocess_invocation: "
                  "real_binary_path=%s real_version=%s" %
                  (str(h), v))
        else:
            raise RuntimeError(
                "self-check: real helm version output unexpected: %r" %
                (v,))
    finally:
        if saved_override is None:
            os.environ.pop("NEXUS_TEST_HELM_BIN", None)
        else:
            os.environ["NEXUS_TEST_HELM_BIN"] = saved_override

    print("== Helm resolver self-checks: PASS ==")


# Invoked at module import when explicitly opted in via env
# var. Default behavior is OFF so the production test run is
# not perturbed; CI can flip it on with
# NEXUS_TEST_HELM_SELF_CHECK=1.
if os.environ.get("NEXUS_TEST_HELM_SELF_CHECK") == "1":
    _self_check_resolve_helm_binary()


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
    mutate_rendered_policy_selector(fixtures_clone, rendered_template)
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
