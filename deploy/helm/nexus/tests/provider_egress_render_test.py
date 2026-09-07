"""Provider-egress mode contract: chart rejects configurations whose
intent cannot be inferred from the values.

Background
----------
The chart lets an operator declare `networkPolicy.providerEgress.mode`:

  proxy            — external providers (and external EvalPlugin
                   vendors, Resend/SMTP, SSO issuer, etc.) go
                   through the cluster-local proxy peer.
  in_cluster_only  — public destinations are refused in the
                   runtime; the chart only renders cluster-internal
                   Service peers.

`mode=""` is not silently treated as one of the above. On
profile=enterprise & mode=enforce, the chart's render-time guard
refuses to render, because the two intents must produce different
NetworkPolicy peers and the chart cannot infer which one you meant.

Why an automated test, not just a render-time fail in helm
---------------------------------------------------------
A render-time fail alone is enough to gate installs, but a fail that
nobody has seen fail can pass for the wrong reason: rendering might
have stopped because of an unrelated bug, the fail message might
have been a typo that doesn't explain the actual cause, or someone
might add a fourth mode silently. Each case below mutates a copy of
the chart (or the --set values) and asserts the diagnostic that
surfaces names the actual cause.

Truth table (see the docstring in
`deploy/helm/nexus/templates/networkpolicy.yaml` for the equivalent
in-source description):

  Case                                | Expected outcome
  ------------------------------------+---------------------------------
  enterprise+enforce, mode empty       | reject — names providerEgress.mode=""
  enterprise+enforce, mode unknown     | schema reject — lists enum
  enterprise+enforce, mode=proxy,
    proxy.enabled false                | reject — names proxy.enabled
  enterprise+enforce, mode=proxy,
    enabled but missing host/namespace | reject — names proxy peer null
  enterprise+enforce, mode=in_cluster_only,
    allowedServiceTargets empty        | reject — names empty+deadlock
  enterprise+enforce, both legacy
    egress.proxy and new providerEgress
    proxy configured enabled           | reject — names alias duplication
  development or mode=disabled         | mode=anything renders
"""

import os
import subprocess
import sys


CHART = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
RELEASE = "provider-egress-mode-test"

_BASE = [
    "--set", "networkPolicy.profile=enterprise",
    "--set", "networkPolicy.mode=enforce",
    "--set", "networkPolicy.enforcementAcknowledged=true",
    # Dependency egress (required for the chart's data path) is
    # declared below so the fail-closed-under-test is the
    # provider-egress gate, not a tangential dependency gate.
    "--set", "dependencies.postgres.enabled=true",
    "--set", "dependencies.postgres.host=p.db.svc.cluster.local",
    "--set", "dependencies.postgres.namespace=database",
]


def render(extra):
    """Return (returncode, combined_stdout_stderr)."""
    cmd = ["helm", "template", RELEASE, CHART] + _BASE + (extra or [])
    proc = subprocess.run(cmd, capture_output=True, text=True)
    return proc.returncode, (proc.stdout or "") + (proc.stderr or "")


def case_rejected(label, extra, needle):
    rc, out = render(extra)
    if rc == 0:
        sys.exit("  MISS " + label + ": expected refusal; got rc=0\n"
                 "  --- output ---\n" + out)
    if needle.lower() not in out.lower():
        sys.exit("  MISS " + label
                 + ": refused but the diagnostic did not name "
                 + repr(needle) + "\n"
                 + "  --- output ---\n" + out)
    print("  OK   " + label)


def case_rendered(label, extra):
    rc, out = render(extra)
    if rc != 0:
        sys.exit("  MISS " + label + ": expected render; got rc="
                 + str(rc) + "\n" + "  --- output ---\n" + out)
    print("  OK   " + label)


def main():
    print("provider-egress mode contract")

    # ----- chart refuses --------------------------------------------
    case_rejected(
        "enterprise+enforce, mode empty -> reject + name providerEgress.mode",
        [],
        'networkPolicy.providerEgress.mode',
    )
    case_rejected(
        "enterprise+enforce, mode unknown -> schema reject + enum list",
        ["--set", "networkPolicy.providerEgress.mode=in-cluster-only"],
        'must be one of the following: "", "proxy", "in_cluster_only"',
    )
    case_rejected(
        "mode=proxy, proxy.enabled=false -> reject + name proxy.enabled",
        [
            "--set", "networkPolicy.providerEgress.mode=proxy",
        ],
        "providerEgress.proxy.enabled must be true",
    )
    case_rejected(
        "mode=proxy, enabled but no host/namespace -> reject + peer null",
        [
            "--set", "networkPolicy.providerEgress.mode=proxy",
            "--set", "networkPolicy.providerEgress.proxy.enabled=true",
        ],
        "providerEgress.proxy needs",
    )
    case_rejected(
        "mode=in_cluster_only, allowedServiceTargets=[] -> reject + deadlock",
        ["--set", "networkPolicy.providerEgress.mode=in_cluster_only"],
        "allowedServiceTargets must be a non-empty list",
    )
    case_rejected(
        "deprecated egress.proxy + new providerEgress.proxy both enabled -> reject + alias duplication",
        [
            "--set", "networkPolicy.providerEgress.mode=proxy",
            "--set", "networkPolicy.providerEgress.proxy.enabled=true",
            "--set", "networkPolicy.providerEgress.proxy.host=p.x.svc",
            "--set", "networkPolicy.providerEgress.proxy.port=3128",
            "--set", "networkPolicy.providerEgress.proxy.namespace=proxy-ns",
            "--set", "networkPolicy.egress.proxy.enabled=true",
            "--set", "networkPolicy.egress.proxy.host=p.x.svc",
            "--set", "networkPolicy.egress.proxy.port=3128",
            "--set", "networkPolicy.egress.proxy.namespace=proxy-ns",
        ],
        "Pick one",
    )

    # ----- chart renders --------------------------------------------
    # No provider-egress mode contract applies on profile=development
    # (the chart's whole NetworkPolicy logic is opt-in there), so any
    # mode or no mode at all should render.
    case_rendered("development profile: no providerEgress mode renders",
                  ["--set", "networkPolicy.profile=development",
                   "--set", "networkPolicy.mode=enforce"])
    case_rendered("development profile: providerEgress.proxy renders",
                  ["--set", "networkPolicy.profile=development",
                   "--set", "networkPolicy.mode=enforce",
                   "--set", "networkPolicy.providerEgress.mode=proxy"])
    # mode=disabled under profile=enterprise is a SEPARATE
    # existing chart fail-closed (line 25 of networkpolicy.yaml
    # rejects "enterprise wants enforcement but mode=disabled
    # would render no NetworkPolicy at all"). That is not the
    # provider-egress contract; this test asserts the provider-
    # egress contract applies specifically to enterprise+enforce
    # — mode=disabled on enterprise is a different contract
    # handled by the existing test_helm_render.sh suite.
    case_rendered("mode=in_cluster_only with declared targets renders",
                  ["--set", "networkPolicy.providerEgress.mode=in_cluster_only",
                   "--set", "networkPolicy.providerEgress.inCluster.allowedServiceTargets[0]=vllm.models.svc.cluster.local:8000"])
    case_rendered("mode=proxy with complete proxy identity renders",
                  ["--set", "networkPolicy.providerEgress.mode=proxy",
                   "--set", "networkPolicy.providerEgress.proxy.enabled=true",
                   "--set", "networkPolicy.providerEgress.proxy.host=proxy.x.svc",
                   "--set", "networkPolicy.providerEgress.proxy.port=3128",
                   "--set", "networkPolicy.providerEgress.proxy.namespace=proxy-ns"])

    print("=== provider-egress mode contract: all cases passed ===")


if __name__ == "__main__":
    main()
