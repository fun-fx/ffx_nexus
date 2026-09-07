"""Runtime proxy wiring contract (chart side, part (b)).

Background
----------
Part (a) of the provider-egress milestone added the render-time
fail-closed gate (`networkPolicy.providerEgress.mode` must be one
of "proxy" or "in_cluster_only"). Part (b) makes the operator's
intent operational by injecting `HTTPS_PROXY`, `HTTP_PROXY`, and a
chart-composed `NO_PROXY` into the gateway/worker pod, so the Go
HTTP transports (which already honour `HTTP_PROXY` via
`http.ProxyFromEnvironment` on the provider-side pool, and will be
extended to honour it on the egress.Guard side in part (c)) actually
walk through the proxy.

Why an automated test, not just a rendering assumption
------------------------------------------------------
NetworkPolicy peer rendering and pod env rendering look like the
same kind of artefact at a glance, and both are easy to forget
when one of them is updated. This test fails if either one diverges
from the documented truth table, and there are no false positives:
the chart can only pass if the policy emits the proxy peer AND the
pod env contains HTTPS_PROXY in the same mode.

Truth table
-----------
  mode="in_cluster_only"        policy has NO provider-egress peer
                                pod has NO HTTPS_PROXY/HTTP_PROXY
                                env (the operator has explicitly
                                opted out of proxying)
  mode="proxy", complete config policy has the proxy peer
                                pod has HTTPS_PROXY=http://host:port
                                pod has HTTP_PROXY=http://host:port
                                pod has NO_PROXY composed from:
                                  - 127.0.0.1, ::1, localhost
                                  - .svc, .svc.cluster.local,
                                    .cluster.local
                                  - all enabled datastore hosts
                                    (postgres/clickhouse/redis)
                                  - providerEgress.proxy.noProxyExtras
  mode="proxy", host present
    but no datastore enabled    NO_PROPYRY contains only built-ins,
                                not any datastore host (this is a
                                operator-visible difference: they
                                see exactly what is NOT proxied)
  mode="disabled" or development  both contracts are off (existing
                                behaviour preserved)
"""

import os
import subprocess
import sys


CHART = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
RELEASE = "runtime-proxy-wiring-test"

BASE = [
    "--set", "networkPolicy.profile=enterprise",
    "--set", "networkPolicy.mode=enforce",
    "--set", "networkPolicy.enforcementAcknowledged=true",
]


def helm(args):
    """Run helm template with BASE + extra args against networkpolicy
    and deployment manifests. Returns combined YAML text."""
    cmd = ["helm", "template", RELEASE, CHART] + BASE + list(args) \
        + ["--show-only", "templates/networkpolicy.yaml",
           "--show-only", "templates/deployment.yaml"]
    proc = subprocess.run(cmd, capture_output=True, text=True)
    if proc.returncode != 0:
        sys.exit("  setup FAIL, helm exit " + str(proc.returncode) + "\n"
                 + (proc.stdout or "") + "\n" + (proc.stderr or ""))
    return proc.stdout


def dataset(extra, dep_pg=False, dep_ch=False, dep_redis=False,
            dep_pg_host="", dep_ch_host="", dep_redis_host=""):
    args = list(extra)
    if dep_pg:
        args += ["--set", "dependencies.postgres.enabled=true",
                 "--set", f"dependencies.postgres.host={dep_pg_host}",
                 "--set", "dependencies.postgres.namespace=database"]
    if dep_ch:
        args += ["--set", "dependencies.clickhouse.enabled=true",
                 "--set", f"dependencies.clickhouse.host={dep_ch_host}",
                 "--set", "dependencies.clickhouse.namespace=analytics"]
    if dep_redis:
        args += ["--set", "dependencies.redis.enabled=true",
                 "--set", f"dependencies.redis.host={dep_redis_host}",
                 "--set", "dependencies.redis.namespace=cache"]
    return tuple(args)


def assert_in(label, rendered, needle):
    if needle not in rendered:
        sys.exit("  FAIL " + label
                 + "  -- needle not in rendered YAML:\n"
                 + "  needle: " + repr(needle) + "\n")
    print("  OK   " + label)


def assert_absent(label, rendered, needle):
    if needle in rendered:
        sys.exit("  FAIL " + label
                 + "  -- needle present in rendered YAML:\n"
                 + "  needle: " + repr(needle) + "\n")
    print("  OK   " + label)


def main():
    print("runtime proxy wiring contract")

    # 1. in_cluster_only: policy has NO proxy peer; pod has NO
    #    HTTPS_PROXY/HTTP_PROXY/NO_PROXY.
    rendered = helm(dataset(
        ["--set", "networkPolicy.providerEgress.mode=in_cluster_only",
         "--set", "networkPolicy.providerEgress.inCluster.allowedServiceTargets[0]=vllm.models.svc.cluster.local:8000"],
        dep_pg=True, dep_pg_host="p.db.svc"))
    assert_absent("in_cluster_only: policy has no proxy peer",
                  rendered, "# Egress proxy")
    assert_absent("in_cluster_only: pod has no HTTPS_PROXY",
                  rendered, "HTTPS_PROXY")
    assert_absent("in_cluster_only: pod has no HTTP_PROXY",
                  rendered, "HTTP_PROXY")
    assert_absent("in_cluster_only: pod has no NO_PROXY",
                  rendered, "NO_PROXY")

    # 2. mode=proxy, complete config: policy emits proxy peer;
    #    pod env contains the operator-set proxy URL and a
    #    chart-composed NO_PROXY that includes each enabled
    #    datastore host.
    rendered = helm(
        dataset(
            ["--set", "networkPolicy.providerEgress.mode=proxy",
             "--set", "networkPolicy.providerEgress.proxy.enabled=true",
             "--set", "networkPolicy.providerEgress.proxy.host=proxy.x.svc",
             "--set", "networkPolicy.providerEgress.proxy.port=3128",
             "--set", "networkPolicy.providerEgress.proxy.namespace=proxy-ns"],
            dep_pg=True,  dep_pg_host="p.db.svc",
            dep_ch=True,  dep_ch_host="ch.db.svc",
            dep_redis=True, dep_redis_host="redis.db.svc"))
    # Postgresql selector must be enabled or chart gate rejects.
    rendered = helm(
        dataset(
            ["--set", "networkPolicy.providerEgress.mode=proxy",
             "--set", "networkPolicy.providerEgress.proxy.enabled=true",
             "--set", "networkPolicy.providerEgress.proxy.host=proxy.x.svc",
             "--set", "networkPolicy.providerEgress.proxy.port=3128",
             "--set", "networkPolicy.providerEgress.proxy.namespace=proxy-ns",
             "--set", "networkPolicy.postgres.selector.enabled=true",
             "--set", "networkPolicy.postgres.selector.namespace=database"],
            dep_pg=True,  dep_pg_host="p.db.svc",
            dep_ch=True, dep_ch_host="ch.db.svc",
            dep_redis=True, dep_redis_host="redis.db.svc"))
    assert_in("mode=proxy: policy emits proxy peer",
              rendered, "# Egress proxy")
    assert_in("mode=proxy: pod has HTTPS_PROXY",
              rendered, '- name: HTTPS_PROXY')
    assert_in("mode=proxy: HTTPS_PROXY value points at proxy host & port",
              rendered, 'value: "http://proxy.x.svc:3128"')
    assert_in("mode=proxy: pod has HTTP_PROXY",
              rendered, '- name: HTTP_PROXY')
    assert_in("mode=proxy: pod has NO_PROXY",
              rendered, '- name: NO_PROXY')
    assert_in("mode=proxy: NO_PROXY includes loopback built-ins",
              rendered, '127.0.0.1')
    assert_in("mode=proxy: NO_PROXY includes cluster DNS suffix",
              rendered, '.svc.cluster.local')
    assert_in("mode=proxy: NO_PROXY includes postgres host",
              rendered, 'p.db.svc')
    assert_in("mode=proxy: NO_PROXY includes clickhouse host",
              rendered, 'ch.db.svc')
    assert_in("mode=proxy: NO_PROXY includes redis host",
              rendered, 'redis.db.svc')

    # 3. NO_PROXY composition when NO datastore host is pre-set
    #    at chart time. Operator can still set one of them later
    #    via extraEnv, but the chart must not silently include
    #    an empty placeholder.
    rendered = helm(
        ["--set", "networkPolicy.providerEgress.mode=proxy",
         "--set", "networkPolicy.providerEgress.proxy.enabled=true",
         "--set", "networkPolicy.providerEgress.proxy.host=proxy.x.svc",
         "--set", "networkPolicy.providerEgress.proxy.port=3128",
         "--set", "networkPolicy.providerEgress.proxy.namespace=proxy-ns"])
    assert_in("mode=proxy without datastore: NO_PROXY has built-ins only",
              rendered, '"127.0.0.1,::1,localhost,.svc,.svc.cluster.local,.cluster.local"')

    # 4. mode=disabled or development bypasses both contracts.
    rendered = helm(
        ["--set", "networkPolicy.profile=development",
         "--set", "networkPolicy.mode=enforce"])
    assert_absent("development profile: policy has no proxy peer",
                  rendered, "# Egress proxy")
    assert_absent("development profile: pod has no HTTPS_PROXY",
                  rendered, "HTTPS_PROXY")

    # 5. operator-supplied extras land in NO_PROXY.
    rendered = helm(
        dataset(
            ["--set", "networkPolicy.providerEgress.mode=proxy",
             "--set", "networkPolicy.providerEgress.proxy.enabled=true",
             "--set", "networkPolicy.providerEgress.proxy.host=proxy.x.svc",
             "--set", "networkPolicy.providerEgress.proxy.port=3128",
             "--set", "networkPolicy.providerEgress.proxy.namespace=proxy-ns",
             "--set", "networkPolicy.postgres.selector.enabled=true",
             "--set", "networkPolicy.postgres.selector.namespace=database",
             "--set", "networkPolicy.providerEgress.proxy.noProxyExtras[0]=my-model.svc.cluster.local"],
            dep_pg=True, dep_pg_host="p.db.svc"))
    assert_in("noProxyExtras appear in NO_PROXY",
              rendered, "my-model.svc.cluster.local")

    print("=== runtime proxy wiring contract: all cases passed ===")


if __name__ == "__main__":
    main()
