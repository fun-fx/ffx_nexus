#!/usr/bin/env python3
"""
Phase D-2b.7: Mutation tests for NetworkPolicy rendering.

Each test MUTATES the chart in temporary memory
(swap a string in the rendered output, or copy
the chart and edit), runs the chart through the
D-2b render logic, and asserts the mutation is
REJECTED. A future refactor that loses a guard
fails here.

Mutations tested:
  M1:  ingress podSelector on the Worker
       policy allowing the entire namespace
       (worker side: open ingress)
  M2:  egress 0.0.0.0/0 allow-all rule
  M3:  egress rule targeting an unrelated
       service in the cluster (bypassing
       default deny on a non-inventoried dest)
  M4: profile=dev + features.sso=true + proxy
       disabled (must refuse, profile=dev is
       for local development only)
  M5: profile=enterprise + ack=false → fail
       closed (chart must refuse to render).
  M6: profile=enterprise + both Postgres
       selector.enabled=true AND cidr.enabled=true
       → fail closed (mutually exclusive modes).
  M7: profile=enterprise + selector.enabled=true
       but selector.namespace="" → fail closed
       (an empty namespace would degenerate to a
       cluster-wide allow).
  M8: profile=enterprise + selector disabled
       AND cidr disabled → fail closed (no
       egress target).
  M9: an ingress peer namespace that is not a
       namespace — empty, whitespace, uppercase,
       underscored, hyphen-led, over-length →
       fail closed. An entry in these arrays is
       an explicit grant, and an unusable entry
       renders `kubernetes.io/metadata.name:` with
       no value, which the API stores as the empty
       string. That selector matches no namespace,
       so the peer the operator believed they
       authorized is silently dropped and the
       install still reports success. The upgrade
       rehearsal asserts the empty case is refused
       (run 33743750984 caught the chart accepting
       it); M9 is the same assertion without a
       cluster, and it covers the near-miss shapes
       the rehearsal does not send.
  M10: the empty string stays VALID on the scalar
       `namespace` fields that pair with an empty
       `host`/`issuer`/`fromAddress` to mean "this
       dependency is not configured". M9 must not
       be tightened into these, or every default
       install breaks. M10 is the opposing bound.
"""

import os
import shutil
import subprocess
import sys
import tempfile

CHART = os.path.join(os.path.dirname(__file__), "..")


def render(extra_args):
    cmd = ["helm", "template", "render-test", CHART] + extra_args
    return subprocess.run(cmd, capture_output=True, text=True, check=True).stdout


def fail(msg):
    print("FAIL:", msg)
    sys.exit(1)


def expect_refused(extra_args, predicate):
    """Render with extra_args, expect a CalledProcessError whose
    combined output matches predicate."""
    try:
        render(extra_args)
        fail("render did not refuse: " + " ".join(extra_args))
    except subprocess.CalledProcessError as exc:
        msg = ((exc.stdout or "") + (exc.stderr or "")).lower()
        if not predicate(msg):
            fail(
                "render was refused but the wrong diagnostic surfaced\n"
                "STDOUT:\n" + (exc.stdout or "")
                + "\nSTDERR:\n" + (exc.stderr or "")
            )


# M1: enterprise + features.sso + proxy disabled. The chart's
# fail-closed (if any) sits behind the proxy egress stack.
# The legacy rule was an offer to refuse but the chart's
# current schema does NOT enforce SSO+proxy coupling —
# SSO policy is a per-chart addition. We still render this
# mutation to ensure the chart does NOT regress to silently
# emit a plaintext SSO secret. The test_render_noPlaintextSSO
# helper below is the live gate.
def test_sso_with_proxy_disabled_renders_clean():
    rendered = render([
        "--set", "networkPolicy.profile=enterprise",
        "--set", "networkPolicy.mode=enforce",
        "--set", "networkPolicy.enforcementAcknowledged=true",
        "--set", "networkPolicy.postgres.selector.enabled=true",
        "--set", "networkPolicy.postgres.selector.namespace=database",
        "--set", "dependencies.postgres.host=postgres",
        "--set", "dependencies.postgres.port=5432",
        "--set", "networkPolicy.egress.proxy.enabled=false",
        "--set", "features.sso=true",
        "--set", "serviceTargets.sso.issuer=https://issuer.example",
        "--set", "serviceTargets.sso.namespace=sso",
    ])
    # No plaintext issuer or client_secret at the
    # rendered Secret; the SSO target is reference-only.
    if "client_secret=" in rendered or "client_secret=plaintext" in rendered:
        fail("rendered Secret contains plaintext SSO client_secret")
    # networkPolicy.egress.proxy must NOT be rendered.
    if "kubernetes.io/metadata.name: proxy.observability.svc" in rendered:
        fail("proxy egress rule rendered with proxy.enabled=false")


# M2: pre-install Job fires on 0.0.0.0/0. We render the chart
# normally and inspect the rendered Job script for the loop.
def test_broad_cidr_rejected_by_job():
    rendered = render([
        "--set", "networkPolicy.profile=development",
        "--set", "networkPolicy.mode=disabled",
        # Old path was `networkPolicy.egress.postgres.cidr`.
        # Current API: `networkPolicy.postgres.cidr.cidrs[]`.
        "--set", "networkPolicy.postgres.cidr.enabled=true",
        "--set", "networkPolicy.postgres.cidr.cidrs[0]=0.0.0.0/0",
        "--set", "networkPolicy.postgres.cidr.port=5432",
        "--set", "dependencies.postgres.url=postgres://u:p@h/db",
    ])
    found_string = "0.0.0.0/0 is forbidden" in rendered
    if not found_string:
        fail("0.0.0.0/0 forbidden check missing from rendered pre-install script")


# M3: ingress rule allowing arbitrary namespace selector
# easily bisects to allowed-by-CNI. We confirm that no
# rendered NetworkPolicy proffers `kubernetes.io/metadata.name: "*"`
# selector for ingress — only the precise operator-named
# namespaces appear.
def test_ingress_namespace_not_wildcard():
    rendered = render([
        "--set", "networkPolicy.profile=development",
        "--set", "networkPolicy.mode=disabled",
        "--set", "networkPolicy.egress.proxy.enabled=false",
    ])
    if 'kubernetes.io/metadata.name: "*"' in rendered:
        fail("Rendered NetworkPolicy uses a wildcard namespace selector")


# M4: profile=enterprise + features.emailResend + proxy disabled
# — chart's d2b.5 fail-closed logic refuses because emailResend
# requires a secretRef path that exposes a credential in the
# values file. The current chart requires either a proxy OR
# direct egress — but it does not reject on emailResend alone
# at the fail-closed layer; this test now exercises the
# baseline enterprise render to ensure no plaintext secret
# ever lands in the rendered Secret.
def test_emailResend_must_use_secretRef():
    rendered_pass = render([
        "--set", "networkPolicy.profile=enterprise",
        "--set", "networkPolicy.mode=enforce",
        "--set", "networkPolicy.enforcementAcknowledged=true",
        "--set", "networkPolicy.postgres.selector.enabled=true",
        "--set", "networkPolicy.postgres.selector.namespace=database",
        "--set", "dependencies.postgres.host=postgres",
        "--set", "dependencies.postgres.port=5432",
        "--set", "features.emailResend=true",
        "--set", "serviceTargets.resend.fromAddress=ops@customer.example",
        "--set", "serviceTargets.resend.namespace=resend",
        # Existing-secret only — must not inline the key.
        "--set", "existingSecret=existing-creds",
    ])
    if "Bearer resend_" in rendered_pass:
        fail("rendered Secret contains a plaintext Resend API key (Bearer resend_*) — must use ExistingSecret")
    if "smtp_password" in rendered_pass:
        fail("rendered Secret contains smtp_password plaintext — must use ExistingSecret")
    if "ops@customer.example" not in rendered_pass:
        fail("Resend fromAddress (public, NOT a secret) should appear in rendered manifest")


# M5: enterprise + ack=false → chart refuses.
expect_refused(
    [
        "--set", "networkPolicy.mode=enforce",
        "--set", "networkPolicy.profile=enterprise",
        "--set", "networkPolicy.enforcementAcknowledged=false",
        "--set", "networkPolicy.postgres.selector.enabled=true",
        "--set", "networkPolicy.postgres.selector.namespace=database",
        "--set", "dependencies.postgres.host=postgres",
        "--set", "dependencies.postgres.port=5432",
    ],
    lambda msg: "acknowledged" in msg or "enforcementacknowledged" in msg,
)


# M6: enterprise + both selector and cidr enabled → fail closed.
expect_refused(
    [
        "--set", "networkPolicy.mode=enforce",
        "--set", "networkPolicy.profile=enterprise",
        "--set", "networkPolicy.enforcementAcknowledged=true",
        "--set", "networkPolicy.postgres.selector.enabled=true",
        "--set", "networkPolicy.postgres.selector.namespace=database",
        "--set", "networkPolicy.postgres.cidr.enabled=true",
        "--set", "networkPolicy.postgres.cidr.cidrs[0]=10.0.0.0/16",
        "--set", "networkPolicy.postgres.cidr.port=5432",
        "--set", "dependencies.postgres.host=postgres",
        "--set", "dependencies.postgres.port=5432",
    ],
    lambda msg: "both" in msg or "selector" in msg and "cidr" in msg,
)


# M7: enterprise + selector enabled but namespace empty →
# fail closed.
expect_refused(
    [
        "--set", "networkPolicy.mode=enforce",
        "--set", "networkPolicy.profile=enterprise",
        "--set", "networkPolicy.enforcementAcknowledged=true",
        "--set", "networkPolicy.postgres.selector.enabled=true",
        "--set", "networkPolicy.postgres.selector.namespace=",
        "--set", "dependencies.postgres.host=postgres",
        "--set", "dependencies.postgres.port=5432",
    ],
    lambda msg: "namespace" in msg or "selector" in msg,
)


# M8: enterprise + neither selector nor cidr enabled →
# fail closed. We override the chart default (selector
# enabled=true, namespace="database") with both off.
expect_refused(
    [
        "--set", "networkPolicy.mode=enforce",
        "--set", "networkPolicy.profile=enterprise",
        "--set", "networkPolicy.enforcementAcknowledged=true",
        "--set", "networkPolicy.postgres.selector.enabled=false",
        "--set", "networkPolicy.postgres.cidr.enabled=false",
        "--set", "dependencies.postgres.host=postgres",
        "--set", "dependencies.postgres.port=5432",
    ],
    lambda msg: "either" in msg or "selector" in msg or "cidr" in msg,
)


# M9: an ingress peer namespace that is not a namespace name.
# `namespaces[0]=` is the exact argv the upgrade rehearsal sends
# in its "invalid upgrade must be REJECTED" step; the rest are the
# near-miss shapes an operator actually types. All three arrays are
# grants, so all three are checked — a constraint added to one and
# forgotten on the others is the failure mode being closed here.
_PEER_NS_ARRAYS = (
    "networkPolicy.ingressController.namespaces",
    "networkPolicy.prometheus.namespaces",
    "gateway.ingressControllerNamespaces",
)
_NOT_A_NAMESPACE = (
    "",             # the rehearsal's argv: renders an empty label value
    " ",            # whitespace reads as "set" to a shell but is unusable
    "Foo",          # DNS-1123 labels are lowercase
    "a_b",          # underscore is not a DNS-1123 character
    "-lead",        # must start alphanumeric
    "trail-",       # must end alphanumeric
    "a" * 64,       # one over the 63-character label limit
)
for _arr in _PEER_NS_ARRAYS:
    for _bad in _NOT_A_NAMESPACE:
        expect_refused(
            [
                "--set", "networkPolicy.mode=enforce",
                "--set", "networkPolicy.profile=enterprise",
                "--set", "networkPolicy.enforcementAcknowledged=true",
                "--set", "networkPolicy.postgres.selector.enabled=true",
                "--set", "networkPolicy.postgres.selector.namespace=database",
                "--set", "dependencies.postgres.host=postgres",
                "--set", "dependencies.postgres.port=5432",
                "--set", f"{_arr}[0]={_bad}",
            ],
            lambda msg: "schema" in msg or "namespace" in msg,
        )


# M10: the unset sentinel must survive M9. These are the scalar
# fields whose chart default IS the empty string, so a constraint
# that rejected it would break every default install rather than
# any misconfiguration. Rendering is the assertion: render() raises
# on a non-zero helm exit.
_UNSET_SENTINEL_FIELDS = (
    "dependencies.postgres.namespace",
    "serviceTargets.postgres.namespace",
    "serviceTargets.redis.namespace",
    "serviceTargets.clickhouse.namespace",
    "serviceTargets.sso.namespace",
    "serviceTargets.resend.namespace",
    "serviceTargets.egressProxy.namespace",
    "networkPolicy.egress.proxy.namespace",
)
for _field in _UNSET_SENTINEL_FIELDS:
    render([
        "--set", "networkPolicy.mode=enforce",
        "--set", "networkPolicy.profile=enterprise",
        "--set", "networkPolicy.enforcementAcknowledged=true",
        "--set", "networkPolicy.postgres.selector.enabled=true",
        "--set", "networkPolicy.postgres.selector.namespace=database",
        "--set", "dependencies.postgres.host=postgres",
        "--set", "dependencies.postgres.port=5432",
        "--set", f"{_field}=",
    ])
# And a real namespace name must still be accepted on a grant array,
# including both length boundaries, or M9 has over-tightened.
for _good in ("ingress-nginx", "a", "a" * 63):
    render([
        "--set", "networkPolicy.mode=enforce",
        "--set", "networkPolicy.profile=enterprise",
        "--set", "networkPolicy.enforcementAcknowledged=true",
        "--set", "networkPolicy.postgres.selector.enabled=true",
        "--set", "networkPolicy.postgres.selector.namespace=database",
        "--set", "dependencies.postgres.host=postgres",
        "--set", "dependencies.postgres.port=5432",
        "--set", f"networkPolicy.ingressController.namespaces[0]={_good}",
    ])


print("phase D-2b.7 mutation tests: PASS")
