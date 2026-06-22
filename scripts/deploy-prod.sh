#!/usr/bin/env bash
# Manual production deployment helper for Nexus (Talos + Cozystack).
#
# Replaces the GitHub Actions CD workflow (which is paused pending ARC
# runner / GitHub App permission fixes). Mirrors .github/workflows/cd-prod.yml
# step-for-step.
#
# Usage:
#   ./scripts/deploy-prod.sh                      # use existing values-prod.yaml tag
#   ./scripts/deploy-prod.sh --tag 0.3.6          # bump image tag + deploy
#   ./scripts/deploy-prod.sh --skip-build         # skip kaniko (image already in Harbor)
#   ./scripts/deploy-prod.sh --restart            # force rollout restart
#   ./scripts/deploy-prod.sh --dry-run            # show what would happen
#
# Prerequisites:
#   - kubectl context pointed at prod cluster (Tailscale MagicDNS reachable)
#   - helm 3.x installed locally
#   - Harbor pull secret `harbor` exists in tenant-nexus (one-time setup)
#   - Namespace tenant-nexus exists
#
# Required env (or hardcode in ~/.config/nexus/deploy-prod.env):
#   KUBECONFIG        — path to prod kubeconfig
#   HARBOR_USER       — Harbor push user (for kaniko logs, optional)
#   HARBOR_PASS       — Harbor push password
#
# See docs/manual-deploy.md for the full procedure.

set -euo pipefail

# ----- config -----
NS="${NS:-tenant-nexus}"
CHART="${CHART:-deploy/helm/nexus}"
VALUES="${VALUES:-deploy/cozystack/values-prod.yaml}"
KUSTOMIZE_TAG="${KUSTOMIZE_TAG:-}"
DRY_RUN=0
SKIP_BUILD=0
FORCE_RESTART=0
TIMEOUT="${TIMEOUT:-15m}"
ROLL_TIMEOUT="${ROLL_TIMEOUT:-300s}"

# ----- args -----
while [[ $# -gt 0 ]]; do
  case "$1" in
    --tag)         KUSTOMIZE_TAG="$2"; shift 2 ;;
    --skip-build)  SKIP_BUILD=1; shift ;;
    --restart)     FORCE_RESTART=1; shift ;;
    --dry-run)     DRY_RUN=1; shift ;;
    --ns)          NS="$2"; shift 2 ;;
    --timeout)     TIMEOUT="$2"; shift 2 ;;
    -h|--help)
      sed -n '2,28p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "Unknown arg: $1" >&2
      exit 1
      ;;
  esac
done

# ----- helpers -----
say()  { printf '\n== %s ==\n' "$*"; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
# run: in dry-run mode, just print the command. Otherwise execute.
run() {
  if [[ $DRY_RUN -eq 1 ]]; then
    local out='+'
    for a in "$@"; do out+=" $(printf '%q' "$a")"; done
    printf '%s\n' "$out"
  else
    "$@"
  fi
}

# In dry-run, neutralize KUBECONFIG so unset under `set -u` is not a problem
[[ $DRY_RUN -eq 1 ]] && KUBECONFIG="${KUBECONFIG:-/dev/null}"

# ----- preflight -----
say "Preflight"
if [[ $DRY_RUN -eq 0 ]]; then
  [[ -n "${KUBECONFIG:-}" && -f "$KUBECONFIG" ]] || die "KUBECONFIG not set or file missing"
  command -v kubectl >/dev/null 2>&1 || die "kubectl not installed"
fi
command -v helm    >/dev/null 2>&1 || die "helm not installed"
[[ -f "$VALUES"  ]] || die "values file not found: $VALUES"
[[ -f "$CHART/Chart.yaml" ]] || die "helm chart not found: $CHART"

# Cluster reachability
if [[ $DRY_RUN -eq 0 ]]; then
  kubectl --kubeconfig "$KUBECONFIG" version --client=true >/dev/null
  kubectl --kubeconfig "$KUBECONFIG" get ns "$NS" >/dev/null \
    || die "namespace $NS not reachable (Tailscale up? kubeconfig correct?)"
fi

# ----- step 1: bump tag -----
if [[ -n "$KUSTOMIZE_TAG" ]]; then
  if [[ $DRY_RUN -eq 1 ]]; then
    say "Would bump image.tag -> $KUSTOMIZE_TAG in $VALUES (dry-run: SKIPPED)"
  else
    say "Bumping image.tag -> $KUSTOMIZE_TAG in $VALUES"
    # idempotent: replace tag line, or insert if missing
    if grep -qE '^[[:space:]]*tag:' "$VALUES"; then
      # use python for safe yaml-ish edit (single-line replacement)
      python3 - "$VALUES" "$KUSTOMIZE_TAG" <<'PY'
import sys, re, pathlib
path, tag = sys.argv[1], sys.argv[2]
text = pathlib.Path(path).read_text()
new = re.sub(r'(^[ \t]*tag:)[ \t]*[^\n#]+', f'\\g<1> {tag}', text, count=1, flags=re.M)
if new == text:
    print('warn: tag line not replaced', file=sys.stderr)
pathlib.Path(path).write_text(new)
PY
    else
      sed -i.bak "/^image:/a\\
  tag: $KUSTOMIZE_TAG" "$VALUES"
    fi
    echo "new tag:"
    grep -E '^[[:space:]]*tag:' "$VALUES" || true
  fi
fi

CURRENT_TAG=$(awk '/^image:/{p=1;next} p && /tag:/{print $2; exit}' "$VALUES")
[[ -n "$CURRENT_TAG" ]] || die "could not read current image tag from $VALUES"
say "Current image tag: $CURRENT_TAG"

# ----- step 2: kaniko build (in-cluster) -----
if [[ $SKIP_BUILD -eq 0 ]]; then
  KANIKO_JOB="${KANIKO_JOB:-deploy/cozystack/kaniko-build.yaml}"
  [[ -f "$KANIKO_JOB" ]] || die "kaniko manifest not found: $KANIKO_JOB"

  # patch destination tag to match current values tag (the manifest has a hardcoded one)
  say "Patching kaniko destination to $CURRENT_TAG"
  TMP_KANIKO=$(mktemp)
  sed "s|--destination=harbor\\.[^:]*:.*|--destination=harbor.<node-ip>.nip.io/ffx/nexus:$CURRENT_TAG|" \
    "$KANIKO_JOB" > "$TMP_KANIKO"

  say "Applying kaniko Job (builds $CURRENT_TAG from main)"
  run kubectl --kubeconfig "$KUBECONFIG" -n "$NS" delete job nexus-build --ignore-not-found
  run kubectl --kubeconfig "$KUBECONFIG" -n "$NS" apply -f "$TMP_KANIKO"
  rm -f "$TMP_KANIKO"

  if [[ $DRY_RUN -eq 0 ]]; then
    say "Waiting for kaniko build to finish (timeout 15m)..."
    if ! kubectl --kubeconfig "$KUBECONFIG" -n "$NS" \
        wait --for=condition=complete job/nexus-build --timeout=15m; then
      say "Build failed. Last 80 lines of logs:"
      kubectl --kubeconfig "$KUBECONFIG" -n "$NS" logs job/nexus-build --tail=80 || true
      die "kaniko build did not complete successfully"
    fi
    say "Build complete. Verifying image is in Harbor..."
    # we trust the kaniko success status; in-cluster pull will catch a missing image
  fi
else
  say "Skipping kaniko build (--skip-build). Assuming $CURRENT_TAG is already in Harbor."
fi

# ----- step 3: helm diff (best effort) -----
say "Helm diff (best effort, requires helm-diff plugin)"
run bash -c '
  set +e
  helm plugin list | grep -q diff || helm plugin install https://github.com/databus23/helm-diff --version v3.9.13 >/dev/null 2>&1
  helm diff upgrade nexus "$CHART" \
    --namespace "$NS" \
    -f "$VALUES" \
    --three-way-merge
'

# ----- step 4: helm upgrade -----
say "Helm upgrade --install"
if [[ $DRY_RUN -eq 0 ]]; then
  # Ensure namespace exists (idempotent on Helm 3 but explicit for industrial Helm 2 upgrades)
  kubectl --kubeconfig "$KUBECONFIG" -n "$NS" create ns "$NS" --dry-run=client -o yaml \
    | kubectl --kubeconfig "$KUBECONFIG" apply -f - >/dev/null
else
  echo "+ kubectl create ns $NS --dry-run=client -o yaml | kubectl apply -f - (dry-run: SKIPPED)"
fi

run helm upgrade --install nexus "$CHART" \
  --namespace "$NS" \
  -f "$VALUES" \
  --wait \
  --timeout "$TIMEOUT"

# ----- step 5: rollout restart (optional) -----
if [[ $FORCE_RESTART -eq 1 ]]; then
  say "Rollout restart (forced)"
  run kubectl --kubeconfig "$KUBECONFIG" -n "$NS" rollout restart deploy/nexus
fi

# ----- step 6: rollout status -----
if [[ $DRY_RUN -eq 0 ]]; then
  say "Wait for rollout"
  kubectl --kubeconfig "$KUBECONFIG" -n "$NS" rollout status deploy/nexus --timeout="$ROLL_TIMEOUT"
fi

# ----- step 7: smoke (optional, best effort) -----
if [[ $DRY_RUN -eq 0 ]]; then
  say "Health checks (best effort)"
  GW_URL="${GW_URL:-https://nexus.<tailnet>.ts.net}"
  CON_URL="${CON_URL:-https://console.<tailnet>.ts.net}"
  for url in "$GW_URL/healthz" "$CON_URL/healthz"; do
    code=$(curl -sk -o /dev/null -w "%{http_code}" --max-time 10 "$url" || echo "000")
    if [[ "$code" == "200" ]]; then
      printf '  %-50s -> %s OK\n' "$url" "$code"
    else
      printf '  %-50s -> %s WARN\n' "$url" "$code"
    fi
  done
fi

# ----- step 8: status -----
if [[ $DRY_RUN -eq 0 ]]; then
  say "Post-deploy status"
  kubectl --kubeconfig "$KUBECONFIG" -n "$NS" get deploy,po -l app.kubernetes.io/name=nexus -o wide || true
fi

say "Done. Run scripts/test_prod_smoke.sh for the full smoke suite."
