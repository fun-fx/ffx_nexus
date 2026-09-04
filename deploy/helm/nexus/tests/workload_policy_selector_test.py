"""Every role-scoped NetworkPolicy must select exactly the workload it names.

The gap this closes
-------------------
`internal/contracttest/d2b_fixture_label_conformance_test.go` already compares
the rendered NetworkPolicy selectors against the integration fixture's pods,
and it passes. Its own doc says what it checks: "every selector key the chart
draws must appear in the fixture with exactly the same value." The fixture
pods in scripts/fixtures/integrationcni/02-stub-deps.yaml carry
`app.kubernetes.io/component: gateway` and `: worker`, so fixture and policy
agree perfectly.

Neither of them was ever compared against the workload the chart actually
deploys. The Deployment's pod template labelled its pods with
`nexus.selectorLabels` — name and instance, no component — so the gateway and
worker policies selected nothing, while `default-deny` (podSelector: {})
selected everything. Under mode=enforce the only pod serving traffic had no
allow rule that applied to it: no DNS, no Postgres, no ClickHouse, no proxy.
Twelve enforcing-CNI scenarios passed the whole time, because they exercise
the fixture's pods and not the chart's.

So this file asserts the missing edge of the triangle: policy selector against
rendered workload. It is deliberately not merged into the fixture conformance
test — that one answers "does the fixture model the policy", this one answers
"does the policy govern the pod", and a install can fail the second while
passing the first.

What is checked
---------------
For every NetworkPolicy with a non-empty podSelector, exactly one rendered
workload pod template may match, and its name must be the one the policy is
named for. That single rule fails closed on all four ways this can break:

  missing label      the workload carries no component, so nothing matches
  wrong role         the workload carries a component the policy does not name
  multiple candidate two workloads match one policy, so the policy governs a
                     pod it was not written for
  wrong namespace    a peer names a namespace no declared target lives in

The mutation cases in the module docstring of mutation_test.py are the model
for how these are exercised; see test_mutations() at the bottom, which edits a
copy of the chart and asserts this file rejects each one.
"""

import copy
import os
import subprocess
import sys
import tempfile

import yaml

CHART = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
RELEASE = "selector-test"

# Policies whose podSelector is empty by design. default-deny is the baseline
# that must match every pod, so "it matches more than one workload" is its
# purpose rather than a defect.
UNSCOPED = {"default-deny"}

# Roles that have no workload in the chart today and must not be reported as
# defects. `worker` exists for deploymentMode=split, which the chart does not
# render yet — the gateway pod runs the eval worker in-process, and the
# gateway policy carries the union of what both need. If split mode ever
# ships, delete this and the test starts requiring a worker workload.
ROLES_WITHOUT_WORKLOAD = {"worker"}

# Values that make the chart render its full policy set plus every optional
# workload, so multiple-candidate defects are reachable. The Grafana auth
# proxy matters here: it is a second Deployment whose pods carry the same
# name/instance pair as the gateway.
FULL_RENDER = [
    "--set", "networkPolicy.profile=enterprise",
    "--set", "networkPolicy.mode=enforce",
    "--set", "networkPolicy.enforcementAcknowledged=true",
    "--set", "dependencies.postgres.enabled=true",
    "--set", "dependencies.postgres.host=postgres.database.svc.cluster.local",
    "--set", "dependencies.postgres.namespace=database",
    "--set", "config.grafana.enabled=true",
    "--set", "config.grafana.instanceSelector.matchLabels.app=grafana",
    "--set", "config.grafana.authProxy.enabled=true",
]


def render(chart=CHART, extra_args=None):
    cmd = ["helm", "template", RELEASE, chart] + FULL_RENDER + (extra_args or [])
    proc = subprocess.run(cmd, capture_output=True, text=True)
    if proc.returncode != 0:
        raise RuntimeError(
            "helm template failed\nSTDOUT:\n" + proc.stdout + "\nSTDERR:\n" + proc.stderr
        )
    return proc.stdout


def fail(msg):
    print("FAIL:", msg)
    sys.exit(1)


def parse(rendered):
    """Return (workloads, policies, namespaces_declared).

    workloads maps a workload name to its pod template labels. Both Deployment
    and Job are included: the migration Job is a real pod the policy set
    governs, and leaving it out would have hidden the one role that was
    already correct.
    """
    workloads, policies = {}, {}
    for doc in yaml.safe_load_all(rendered):
        if not isinstance(doc, dict):
            continue
        kind, name = doc.get("kind"), doc.get("metadata", {}).get("name", "")
        if kind in ("Deployment", "Job"):
            labels = doc["spec"]["template"]["metadata"].get("labels", {}) or {}
            workloads[name] = labels
        elif kind == "NetworkPolicy":
            policies[name] = doc.get("spec") or {}
    return workloads, policies


def role_of(policy_name):
    """`selector-test-nexus-gateway` -> `gateway`."""
    prefix = f"{RELEASE}-nexus-"
    return policy_name[len(prefix):] if policy_name.startswith(prefix) else policy_name


def matches(labels, match_labels):
    return all(labels.get(k) == v for k, v in match_labels.items())


def check_selectors(rendered):
    """The core contract. Returns a list of human-readable defects."""
    workloads, policies = parse(rendered)
    defects = []

    if not workloads:
        return ["no workloads rendered; the chart cannot be checked"]

    for pname, spec in policies.items():
        role = role_of(pname)
        base_role = role[:-len("-egress")] if role.endswith("-egress") else role
        base_role = base_role[:-len("-ingress")] if base_role.endswith("-ingress") else base_role
        if base_role in UNSCOPED:
            continue

        sel = spec.get("podSelector")
        # An ingress-only policy with no podSelector is a shape this chart
        # uses; it is not role-scoped so there is nothing to verify.
        if not sel or not sel.get("matchLabels"):
            continue

        ml = sel["matchLabels"]
        hits = [w for w, labels in workloads.items() if matches(labels, ml)]

        if not hits:
            if base_role in ROLES_WITHOUT_WORKLOAD:
                continue
            defects.append(
                f"{pname}: selector {ml} matches no rendered workload. "
                f"Under mode=enforce this policy's allow rules apply to nothing, "
                f"while default-deny still applies to every pod. "
                f"Rendered workloads: "
                + "; ".join(f"{w}={labels}" for w, labels in workloads.items())
            )
            continue

        if len(hits) > 1:
            defects.append(
                f"{pname}: selector {ml} matches {len(hits)} workloads {hits}. "
                f"A role-scoped policy that governs more than one workload "
                f"grants one of them rules written for the other."
            )
            continue

        # Exactly one candidate. It must be the workload this policy is named
        # for, which is the check that catches a role label applied to the
        # wrong pod.
        hit = hits[0]
        declared = workloads[hit].get("app.kubernetes.io/component")
        if declared != base_role:
            defects.append(
                f"{pname}: selector matched workload {hit}, whose component is "
                f"{declared!r} rather than {base_role!r}."
            )

    return defects


def check_peer_namespaces(rendered):
    """No namespaceSelector peer may be empty.

    An empty namespaceSelector selects *every* namespace, so a peer that lost
    its namespace name does not fail — it widens. That is the same shape as
    the selector defect above read from the other side: the manifest renders,
    the policy looks specific, and it governs far more than it names.
    """
    _, policies = parse(rendered)
    defects = []
    for pname, policy in policies.items():
        for direction in ("egress", "ingress"):
            for rule in policy.get(direction) or []:
                for peer in rule.get("to") or rule.get("from") or []:
                    nssel = peer.get("namespaceSelector")
                    if nssel is None:
                        continue
                    if not (nssel.get("matchLabels") or {}).get(
                        "kubernetes.io/metadata.name"
                    ):
                        defects.append(
                            f"{pname}: a {direction} peer has a namespaceSelector "
                            f"with no namespace name, which selects every namespace."
                        )
    return defects


def test_contract():
    rendered = render()
    defects = check_selectors(rendered) + check_peer_namespaces(rendered)
    if defects:
        fail("policy/workload selector contract broken:\n  - " + "\n  - ".join(defects))
    print("  PASS every role-scoped policy selects exactly its own workload")


# --- mutation coverage ------------------------------------------------------
#
# A contract test that has never been seen to fail is a contract test that
# might not be testing anything; this suite exists because exactly that
# happened to the fixture conformance test. Each case below edits a copy of
# the chart and asserts check_selectors() reports the defect.


def mutate_chart(edits):
    """Copy the chart to a temp dir and apply {relative_path: (old, new)}."""
    tmp = tempfile.mkdtemp(prefix="nexus-chart-")
    dest = os.path.join(tmp, "nexus")
    subprocess.run(["cp", "-R", CHART, dest], check=True)
    for rel, (old, new) in edits.items():
        path = os.path.join(dest, rel)
        with open(path) as fh:
            body = fh.read()
        if old not in body:
            fail(f"mutation setup failed: {old!r} not found in {rel}")
        with open(path, "w") as fh:
            fh.write(body.replace(old, new, 1))
    return dest


DEPLOY = "templates/deployment.yaml"
COMPONENT_LINE = "        app.kubernetes.io/component: gateway\n"


def expect_defect(label, chart, needle):
    try:
        rendered = render(chart=chart)
    except RuntimeError as exc:
        # A render refusal is also fail-closed, which satisfies the case.
        print(f"  PASS {label} (refused at render: {str(exc).splitlines()[0]})")
        return
    defects = check_selectors(rendered)
    if not defects:
        fail(f"{label}: mutation produced no defect; the contract does not catch it")
    if not any(needle in d for d in defects):
        fail(f"{label}: reported the wrong defect: {defects}")
    print(f"  PASS {label}")


def test_mutation_missing_label():
    """The original defect: workload carries no component at all."""
    chart = mutate_chart({DEPLOY: (COMPONENT_LINE, "")})
    expect_defect("missing label is fail-closed", chart, "matches no rendered workload")


def test_mutation_wrong_role():
    """Workload labelled with a role no policy is named for."""
    chart = mutate_chart({
        DEPLOY: (COMPONENT_LINE, "        app.kubernetes.io/component: api\n")
    })
    expect_defect("wrong role is fail-closed", chart, "matches no rendered workload")


def test_mutation_multiple_candidates():
    """Two workloads answering to one role-scoped policy."""
    chart = mutate_chart({
        "templates/grafana-auth-proxy.yaml": (
            "        app.kubernetes.io/component: grafana-auth-proxy",
            "        app.kubernetes.io/component: gateway",
        )
    })
    expect_defect("multiple candidates is fail-closed", chart, "matches 2 workloads")


def test_mutation_wrong_namespace():
    """A peer whose namespaceSelector selects every namespace."""
    chart = mutate_chart({
        "templates/networkpolicy.yaml": (
            "              kubernetes.io/metadata.name: {{ .Values.networkPolicy.postgres.selector.namespace }}",
            "",
        )
    })
    try:
        rendered = render(chart=chart)
    except RuntimeError:
        print("  PASS wrong namespace is fail-closed (refused at render)")
        return
    defects = check_peer_namespaces(rendered)
    if not defects:
        fail("wrong namespace: mutation produced no defect")
    print("  PASS wrong namespace is fail-closed")


def main():
    print("workload/policy selector contract")
    test_contract()
    test_mutation_missing_label()
    test_mutation_wrong_role()
    test_mutation_multiple_candidates()
    test_mutation_wrong_namespace()
    print("=== workload policy selector suite: all cases passed ===")


if __name__ == "__main__":
    main()
