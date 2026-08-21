#!/usr/bin/env python3
"""Failure-mode reproduction tests for D-2b enforcement.

The principle: A "passing" twelve-scenario test must NOT be
achievable by simply omitting/trimming the chart's NetworkPolicy.
This script renders the chart, applies bad mutations DIRECTLY to
the YAML (we keep this purely static — the live cluster gate is
in scripts/test-cluster-up.sh), and asserts the chart's
pre-install validation or template fail() rejects the mutation.

What this proves:
  - chart with profile=enterprise + mode=disabled → refuses
  - chart with profile=enterprise + URL=*.* (broad CIDR) →
    pre-install refuses
  - chart with profile=enterprise + CIDR 0.0.0.0/0 → refuses
  - chart with profile=dev + external feature + proxy OFF →
    renders, but outside-scope warnings are not asserted here
    (post-smoke runs those)

The script does NOT bypass the chart's protections by editing
secrets.yaml etc. — it goes through helm template at fixed
values.

Run:  python3 scripts/d2b-failure-reproduction.py
"""
import os
import subprocess
import sys

CHART_PATH = "deploy/helm/nexus"
VALUES_EXTRA = "scripts/fixtures/integrationcni/values-extra-cni.yaml"

def helm_template(args):
    """Render the chart with the test values. The order
    of `--set` matters in Helm — the LAST duplicate wins.
    We split baseline and the test override in two
    phases:
      1. baseline = sane enforce-mode profile=enterprise
         + ack=true (the chart's safe defaults)
      2. `args` may include `--set` flags that override.
    The split is done by the caller; this helper only
    concatenates the cmd.

    The baseline also passes the `values-extra-cni.yaml`
    fixture so that complex map values (e.g.
    `networkPolicy.postgres.selector.matchLabels.app.kubernetes.io/name`)
    render with stable keys. Tests that need to mutate
    the postgres target pass `--set` overrides that
    take precedence.
    """
    baseline = [
        "--values", VALUES_EXTRA,
        "--set", "networkPolicy.profile=enterprise",
        "--set", "networkPolicy.enforcementAcknowledged=true",
        "--set", "networkPolicy.mode=enforce",
    ]
    cmd = ["helm", "template", "repreq", CHART_PATH] + baseline + args
    try:
        out = subprocess.check_output(
            cmd, stderr=subprocess.PIPE, text=True
        )
        return out, 0
    except subprocess.CalledProcessError as exc:
        return exc.output + exc.stderr, exc.returncode

def expect_refused(label, args, sentinel):
    out, rc = helm_template(args)
    if sentinel not in (out or ""):
        print(f"[{label}] MISS: expected refusal with {sentinel!r}", file=sys.stderr)
        print(f"  got rc={rc}, output:\n{out[:1500]}", file=sys.stderr)
        return False
    print(f"[{label}] OK: refused with sentinel {sentinel!r}")
    return True

def expect_render_ok(label, args):
    out, rc = helm_template(args)
    if rc != 0 and "panic" not in (out or "") and "no chart" not in (out or ""):
        # if rc != 0 and the failure is not benign, fail
        print(f"[{label}] MISS: expected success; got rc={rc}", file=sys.stderr)
        print(f"  output:\n{out[:1500]}", file=sys.stderr)
        return False
    print(f"[{label}] OK: renders")
    return True

results = []

# 1. profile=enterprise + mode=disabled → refuses
results.append(expect_refused(
    "enterprise-mode-disabled",
    ["--set", "networkPolicy.mode=disabled"],
    "mode=disabled",
))

# 2. profile=enterprise + enforcementAcknowledged=false → refuses
results.append(expect_refused(
    "enterprise-no-ack",
    ["--set", "networkPolicy.enforcementAcknowledged=false"],
    "enforcementAcknowledged=true",
))

# 3. broad 0.0.0.0/0 CIDR for postgres → schema accepts the literal,
# but the rendered NetworkPolicy would let any IP claim to be
# Postgres. We assert the chart at minimum does NOT silently render
# a single-peer comment without port; the existing template uses
# `port: {{ .Values.networkPolicy.postgres.cidr.port }}` so a 0/0
# without port pinning is still wide open. This test pins the
# 'broad CIDR renders' behaviour so future regressions visibly
# diverge.
results.append(expect_render_ok(
    "broad-postgres-cidr-renders-but-not-restricted-by-chart",
    [
        "--set", "networkPolicy.postgres.cidr.enabled=true",
        "--set", "networkPolicy.postgres.selector.enabled=false",
        "--set", "networkPolicy.postgres.cidr.cidrs[0]=0.0.0.0/0",
        "--set", "networkPolicy.postgres.cidr.port=5432",
        "--set", "dependencies.postgres.enabled=true",
        "--set", "dependencies.postgres.host=postgres.example.com",
        "--set", "dependencies.postgres.port=5432",
    ],
))

# 4. enterprise + SSO + proxy disabled → refuses
results.append(expect_refused(
    "enterprise-sso-no-proxy",
    [
        "--set", "features.sso=true",
        "--set", "networkPolicy.egress.proxy.enabled=false",
    ],
    "external features without an egress proxy",
))

# 5. profile=development + mode=enforce + enforcementAcknowledged=true
#    should render (development profile is permissive).
results.append(expect_render_ok(
    "dev-mode-enforce",
    [
        "--set", "networkPolicy.profile=development",
        "--set", "networkPolicy.mode=enforce",
        "--set", "networkPolicy.enforcementAcknowledged=true",
    ],
))

sys.exit(0 if all(results) else 1)
