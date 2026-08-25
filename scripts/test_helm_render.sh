#!/usr/bin/env bash
# =============================================================================
# Helm chart render regression suite
# =============================================================================
# `helm lint` alone is not enough. It validated the chart happily while
# config.sso / config.email / config.metabase / config.runtime were documented
# one indent level too deep (under `metrics:`) than where the templates read
# them, so a customer could configure their IdP in values.yaml, install
# successfully, and get no SSO at all with zero diagnostics.
#
# This suite therefore asserts on the RENDERED output: the env vars an operator
# configured must actually appear in the ConfigMap/Secret. It also asserts the
# historical mis-indentation is now rejected outright.
#
# Usage:  scripts/test_helm_render.sh
# Requires: helm 3.x, python3
# =============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHART="${REPO_ROOT}/deploy/helm/nexus"
WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

pass=0
fail=0

ok()   { printf '  \033[32mPASS\033[0m %s\n' "$1"; pass=$((pass + 1)); }
bad()  { printf '  \033[31mFAIL\033[0m %s\n' "$1"; fail=$((fail + 1)); }
head_() { printf '\n\033[1m%s\033[0m\n' "$1"; }

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing dependency: $1" >&2; exit 127; }; }
need helm
need python3

# render <output-file> [extra helm args...]
render() {
  local out="$1"; shift
  helm template nexus-test "${CHART}" "$@" >"${out}" 2>"${out}.err"
}

# assert_contains <file> <literal> <description>
assert_contains() {
  if grep -qF -- "$2" "$1"; then ok "$3"; else
    bad "$3 (expected to find: $2)"
  fi
}

# assert_absent <file> <literal> <description>
assert_absent() {
  if grep -qF -- "$2" "$1"; then
    bad "$3 (unexpectedly found: $2)"
  else ok "$3"; fi
}

# ---------------------------------------------------------------------------
head_ "1. helm lint --strict"
# ---------------------------------------------------------------------------
if helm lint --strict "${CHART}" >"${WORK}/lint.txt" 2>&1; then
  ok "chart passes helm lint --strict"
else
  bad "helm lint --strict failed"; cat "${WORK}/lint.txt"
fi

# ---------------------------------------------------------------------------
head_ "2. Default values render (zero-dependency mode)"
# ---------------------------------------------------------------------------
# The chart default for `networkPolicy.enforcementAcknowledged` is `false`
# and the d2b-enterprise template requires an explicit acknowledgement in
# enterprise profile. The render suite therefore opts in via --set so the
# test exercises the rendered ConfigMap/Secret/etc. surface — chart defaults
# MUST NOT be relaxed to make the test green. A NEGATIVE acknowledgement
# case in section 5 holds the chart's fail-closed line.
if render "${WORK}/default.yaml" \
    --set networkPolicy.mode=enforce \
    --set networkPolicy.profile=enterprise \
    --set networkPolicy.enforcementAcknowledged=true; then
  ok "default values render (with explicit enterprise acknowledgement)"
  # Zero-dep default must not ship a scrape surface or an email transport.
  assert_absent "${WORK}/default.yaml" "NEXUS_METRICS_ADDR" \
    "default values do not enable a metrics listener"
  assert_absent "${WORK}/default.yaml" "kind: ServiceMonitor" \
    "default values do not emit a ServiceMonitor"
else
  bad "default values failed to render"; cat "${WORK}/default.yaml.err"
fi

# ---------------------------------------------------------------------------
head_ "3. No vendor domain survives as a production default"
# ---------------------------------------------------------------------------
# A self-hosted install must never send mail from, or point a browser at, a
# domain the vendor owns and the customer cannot authenticate for.
for pattern in "ffx.ai" "noreply@" "tenant-root" "tenant-nexus"; do
  assert_absent "${WORK}/default.yaml" "${pattern}" \
    "rendered default output is free of '${pattern}'"
done

# ---------------------------------------------------------------------------
head_ "4. Customer example values render with the settings they configured"
# ---------------------------------------------------------------------------
# This is the core regression: every one of these env vars was reachable only
# through the correct config.* path. If a key is ever moved or mis-nested
# again, the corresponding assertion fails. The enterprise acknowledgement
# is set on the command line for the same reason as section 2 — chart defaults
# stay minimal-and-fail-closed.
for env in staging production; do
  vf="${CHART}/values-${env}.example.yaml"
  out="${WORK}/${env}.yaml"
  if [[ ! -f "${vf}" ]]; then bad "missing example values file: ${vf}"; continue; fi
  if ! render "${out}" -f "${vf}" \
      --set networkPolicy.mode=enforce \
      --set networkPolicy.profile=enterprise \
      --set networkPolicy.enforcementAcknowledged=true; then
    bad "${env} example failed to render"; cat "${out}.err"; continue
  fi
  ok "${env} example renders"

  for key in \
      NEXUS_PUBLIC_BASE_URL \
      NEXUS_PUBLIC_GATEWAY_URL \
      NEXUS_PUBLIC_WEB_ORIGINS \
      NEXUS_PUBLIC_GRAFANA_URL \
      NEXUS_SSO_ISSUER \
      NEXUS_SSO_CLIENT_ID \
      NEXUS_SSO_REDIRECT_URL \
      NEXUS_EMAIL_PROVIDER \
      NEXUS_EMAIL_FROM_ADDRESS \
      NEXUS_SMTP_HOST \
      NEXUS_METRICS_ADDR \
      NEXUS_MAX_CONCURRENT_PER_KEY \
      GOMEMLIMIT; do
    assert_contains "${out}" "${key}:" "${env}: ${key} is rendered"
  done

  # Examples reference an out-of-band Secret, so the chart must not render one.
  assert_absent "${out}" "kind: Secret" \
    "${env}: existingSecret means the chart renders no Secret of its own"
done

# ---------------------------------------------------------------------------
head_ "5. NEGATIVE: historical mis-indentation must be rejected"
# ---------------------------------------------------------------------------
# Reproduces the exact shape that shipped: application settings nested under
# `metrics:`. values.schema.json closes that object, so this must fail.
cat >"${WORK}/misnested.yaml" <<'YAML'
metrics:
  sso:
    issuer: "https://idp.customer.example/oauth2/default"
    clientId: "nexus-console"
  email:
    fromAddress: "Nexus <no-reply@customer.example>"
  metabase:
    url: "http://metabase.svc:3000"
  runtime:
    gomemlimit: "512MiB"
  maxConcurrentPerKey: 8
  failoverWebhook: "https://hooks.customer.example/nexus"
YAML
if render "${WORK}/misnested_out.yaml" -f "${WORK}/misnested.yaml"; then
  bad "mis-nested values were silently ACCEPTED (schema guard is not working)"
else
  if grep -q "Additional property" "${WORK}/misnested_out.yaml.err"; then
    ok "mis-nested values rejected by values.schema.json with a clear message"
  else
    bad "mis-nested values rejected, but not by the schema (unclear diagnostic)"
    cat "${WORK}/misnested_out.yaml.err"
  fi
fi

# A plain typo in a real path must also be caught.
cat >"${WORK}/typo.yaml" <<'YAML'
config:
  ssoo:
    issuer: "https://idp.customer.example"
YAML
if render "${WORK}/typo_out.yaml" -f "${WORK}/typo.yaml"; then
  bad "typo'd key config.ssoo was silently accepted"
else
  ok "typo'd key config.ssoo is rejected"
fi

# The enterprise profile MUST stay fail-closed: an operator who forgets to
# acknowledge enforcement must NOT be allowed to install a chart that
# silently starts enforcing NetworkPolicy. The chart default for
# networkPolicy.enforcementAcknowledged is `false`, so this case is the only
# thing standing between the test suite and a "make the test green by
# flipping the default" regression.
if render "${WORK}/no_ack_default.yaml" \
    --set networkPolicy.mode=enforce \
    --set networkPolicy.profile=enterprise; then
  bad "enterprise render WITHOUT acknowledgement succeeded; the chart's fail-closed line is open"
  cat "${WORK}/no_ack_default.yaml.err"
else
  if grep -q "enforcementAcknowledged=true" "${WORK}/no_ack_default.yaml.err"; then
    ok "enterprise render WITHOUT acknowledgement is rejected with an explicit pointer"
  else
    bad "enterprise render was rejected, but without naming enforcementAcknowledged (unclear diagnostic)"
    cat "${WORK}/no_ack_default.yaml.err"
  fi
fi
if render "${WORK}/false_ack.yaml" \
    --set networkPolicy.mode=enforce \
    --set networkPolicy.profile=enterprise \
    --set networkPolicy.enforcementAcknowledged=false; then
  bad "enforcementAcknowledged=false was accepted as ACK; an operator who reads it as a positive signal would install anyway"
else
  ok "enforcementAcknowledged=false is rejected (an explicit true signal is required)"
fi

# ---------------------------------------------------------------------------
head_ "6. ConfigMap data has no duplicate keys"
# ---------------------------------------------------------------------------
# NEXUS_METRICS_ADDR used to be emitted twice (once from config.metricsAddr,
# once from metrics.enabled). A duplicate key in a ConfigMap's data map means
# the effective value depends on YAML parse order.
render "${WORK}/dup.yaml" \
  --set metrics.enabled=true \
  --set config.metricsAddr=":9999" \
  --set networkPolicy.mode=enforce \
  --set networkPolicy.profile=enterprise \
  --set networkPolicy.enforcementAcknowledged=true >/dev/null 2>&1 || true
python3 - "${WORK}/dup.yaml" <<'PY'
import collections, sys, yaml

path = sys.argv[1]
dupes = []


class DupCatcher(yaml.SafeLoader):
    pass


def mapping(loader, node, deep=False):
    counts = collections.Counter(
        loader.construct_object(k, deep=True) for k, _ in node.value
    )
    dupes.extend(k for k, n in counts.items() if n > 1)
    return yaml.SafeLoader.construct_mapping(loader, node, deep)


DupCatcher.add_constructor(
    yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, mapping
)

with open(path) as fh:
    list(yaml.load_all(fh, Loader=DupCatcher))

if dupes:
    print(f"  \033[31mFAIL\033[0m duplicate keys in rendered manifests: {sorted(set(dupes))}")
    sys.exit(1)
print("  \033[32mPASS\033[0m rendered manifests contain no duplicate mapping keys")
PY
if [[ $? -eq 0 ]]; then pass=$((pass + 1)); else fail=$((fail + 1)); fi

# metrics.enabled must win, and it must appear exactly once.
n="$(grep -c "NEXUS_METRICS_ADDR:" "${WORK}/dup.yaml" || true)"
if [[ "${n}" == "1" ]]; then
  assert_contains "${WORK}/dup.yaml" 'NEXUS_METRICS_ADDR: ":9100"' \
    "metrics.enabled takes precedence over config.metricsAddr"
else
  bad "NEXUS_METRICS_ADDR rendered ${n} times (expected exactly 1)"
fi

# ---------------------------------------------------------------------------
head_ "7. ServiceMonitor honours its own enable flag"
# ---------------------------------------------------------------------------
# The CRD may not exist on the customer's cluster, so emitting the CR whenever
# metrics are on would fail the whole release.
render "${WORK}/sm_off.yaml" --set metrics.enabled=true \
  --set metrics.serviceMonitor.enabled=false \
  --set networkPolicy.mode=enforce \
  --set networkPolicy.profile=enterprise \
  --set networkPolicy.enforcementAcknowledged=true >/dev/null 2>&1 || true
assert_absent "${WORK}/sm_off.yaml" "kind: ServiceMonitor" \
  "metrics.enabled without serviceMonitor.enabled emits no ServiceMonitor"

render "${WORK}/sm_on.yaml" --set metrics.enabled=true \
  --set metrics.serviceMonitor.enabled=true \
  --set networkPolicy.mode=enforce \
  --set networkPolicy.profile=enterprise \
  --set networkPolicy.enforcementAcknowledged=true >/dev/null 2>&1 || true
assert_contains "${WORK}/sm_on.yaml" "kind: ServiceMonitor" \
  "serviceMonitor.enabled emits a ServiceMonitor"

# ---------------------------------------------------------------------------
head_ "8. Migration hook Job"
# ---------------------------------------------------------------------------
# The Job must exist, run as a pre-install/pre-upgrade hook so Helm aborts the
# release on failure, use the SAME image and env sources as the Deployment, and
# keep a failed pod around for its logs.
render "${WORK}/mig.yaml" \
  --set dependencies.postgres.enabled=true \
  --set networkPolicy.mode=enforce \
  --set networkPolicy.profile=enterprise \
  --set networkPolicy.enforcementAcknowledged=true >/dev/null 2>&1 || true
assert_contains "${WORK}/mig.yaml" "kind: Job" \
  "a migration Job is rendered when a datastore is enabled"
assert_contains "${WORK}/mig.yaml" '"helm.sh/hook": pre-install,pre-upgrade' \
  "Job runs as a pre-install/pre-upgrade hook (Helm aborts the release on failure)"
assert_contains "${WORK}/mig.yaml" "- migrate" \
  "Job invokes the migrate subcommand"
assert_contains "${WORK}/mig.yaml" "restartPolicy: Never" \
  "Job pod does not restart in place"

# A failed hook must NOT be auto-deleted, or the logs vanish with it.
if grep -A2 'hook-delete-policy' "${WORK}/mig.yaml" | grep -q "hook-failed"; then
  bad "delete policy includes hook-failed; a failed migration's logs would be deleted"
else
  ok "failed migration Jobs are retained for their logs"
fi

# The Job and the Deployment must never disagree about image or database.
job_img="$(python3 - "${WORK}/mig.yaml" <<'PY'
import sys, yaml
imgs = {}
for doc in yaml.safe_load_all(open(sys.argv[1])):
    if not doc:
        continue
    if doc.get("kind") in ("Job", "Deployment"):
        spec = doc["spec"]["template"]["spec"]["containers"][0]
        imgs[doc["kind"]] = spec["image"]
print("MATCH" if len(set(imgs.values())) == 1 and len(imgs) == 2 else f"MISMATCH {imgs}")
PY
)"
if [[ "${job_img}" == "MATCH" ]]; then
  ok "migration Job and Deployment use the identical image tag"
else
  bad "migration Job / Deployment image mismatch: ${job_img}"
fi

# Disabling it must remove it entirely (for DBA-gated change processes).
render "${WORK}/mig_off.yaml" \
  --set dependencies.postgres.enabled=true \
  --set migrations.enabled=false \
  --set networkPolicy.mode=enforce \
  --set networkPolicy.profile=enterprise \
  --set networkPolicy.enforcementAcknowledged=true >/dev/null 2>&1 || true
assert_absent "${WORK}/mig_off.yaml" "kind: Job" \
  "migrations.enabled=false renders no Job"

# No datastore means nothing to migrate.
assert_absent "${WORK}/default.yaml" "kind: Job" \
  "no Job when no datastore is enabled"

# ---------------------------------------------------------------------------
head_ "9. Every config.* key declared in values.yaml is read by a template"
# ---------------------------------------------------------------------------
# Catches the other half of the drift: a documented knob nothing consumes.
python3 - "${CHART}" <<'PY'
import pathlib, re, sys

chart = pathlib.Path(sys.argv[1])
templates = "\n".join(
    f.read_text() for f in (chart / "templates").rglob("*") if f.is_file()
)

import yaml
values = yaml.safe_load((chart / "values.yaml").read_text())

# Leaf-ish check: a top-level config.<key> must be mentioned somewhere.
unread = [
    k for k in values.get("config", {})
    if f".Values.config.{k}" not in templates
]
if unread:
    print(f"  \033[31mFAIL\033[0m config keys declared but never read: {unread}")
    sys.exit(1)
print(f"  \033[32mPASS\033[0m all {len(values.get('config', {}))} config.* keys are read by templates")
PY
if [[ $? -eq 0 ]]; then pass=$((pass + 1)); else fail=$((fail + 1)); fi

# ---------------------------------------------------------------------------
head_ "10. Outbound destination policy"
# ---------------------------------------------------------------------------
# The default must be the restrictive one. If this key renders with no value
# set, an operator upgrading would silently widen the policy for every
# destination an org admin can configure.
assert_absent "${WORK}/default.yaml" "NEXUS_EGRESS_TENANT_ALLOWED_CIDRS" \
  "no egress allowlist is rendered by default (API-configured destinations reach public internet only)"

# A values file rather than --set: the value is a comma-separated CIDR list and
# --set reads commas as list separators.
cat >"${WORK}/egress_values.yaml" <<'YAML'
config:
  egressTenantAllowedCidrs: "10.44.0.0/16,10.45.1.7"
YAML
if render "${WORK}/egress.yaml" \
    -f "${WORK}/egress_values.yaml" \
    --set networkPolicy.mode=enforce \
    --set networkPolicy.profile=enterprise \
    --set networkPolicy.enforcementAcknowledged=true; then
  assert_contains "${WORK}/egress.yaml" 'NEXUS_EGRESS_TENANT_ALLOWED_CIDRS: "10.44.0.0/16,10.45.1.7"' \
    "an explicit egress allowlist is passed through verbatim"
else
  bad "an egress allowlist failed to render"; cat "${WORK}/egress.yaml.err"
fi

# A misspelling under config.* must not be silently accepted, which is what the
# values schema is for.
cat >"${WORK}/egress_typo_values.yaml" <<'YAML'
config:
  egressTenantAllowedCIDRs: "10.0.0.0/8"
YAML
if render "${WORK}/egress_typo.yaml" -f "${WORK}/egress_typo_values.yaml"; then
  bad "config.egressTenantAllowedCIDRs (wrong case) rendered successfully; a typo in the egress allowlist would be silently ignored"
else
  ok "a misspelled egress allowlist key is rejected by the values schema"
fi

# ---------------------------------------------------------------------------
printf '\n\033[1m=== helm render suite: %d passed, %d failed ===\033[0m\n' "${pass}" "${fail}"
[[ "${fail}" -eq 0 ]] || exit 1
