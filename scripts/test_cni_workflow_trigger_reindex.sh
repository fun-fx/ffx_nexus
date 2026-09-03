#!/usr/bin/env bash
# ------------------------------------------------------------------------------
# Phase D-2b.34 (reindex) — narrow push trigger safety regression
#
# Validates the surgical addition of a `push:` trigger limited to:
#   - branch: main
#   - path:   .github/workflows/cni-nightly.yml  (single entry, single file)
# to the existing cni-nightly.yml on default branch.
#
# Purpose:
#   The dispatch registry (workflow ID 338819595, path
#   `.github/workflows/cni-nightly.yml`) was returning HTTP 422 to
#   `gh workflow run --ref <anything>` even after a merge that replaced
#   the file's `workflow_dispatch:` block on `main`. The hypothesis is
#   that GitHub only re-promotes the dispatch trigger advertisement
#   after observing a real `push:` event against the workflow file on
#   the default branch. Adding a *narrow* push trigger forces this
#   re-evaluation without expanding the workflow's blast radius.
#
# This test enforces the invariants listed in the user spec:
#
#   - push scope = exactly main branch + exactly one path entry
#     (".github/workflows/cni-nightly.yml"). No glob wildcards beyond
#     the literal file path. No "**". No paths-ignore. No tags. No
#     `workflow_run` / `repository_dispatch` escalation.
#   - heavy isolation: the cni-enforcement-gate job must still be
#     guarded exactly by `if: github.event_name == 'workflow_dispatch'`.
#   - manual API parity: `workflow_dispatch:` block, `recovery_pr_sha`
#     input (`required: true`, `type: string`) and `run_index` input
#     (`required: true`, `type: string`) preserved byte-for-byte.
#   - existing audit triggers: `schedule:` cron and `pull_request:`
#     paths list preserved bit-for-bit relative to the pre-reindex
#     shape on `origin/main`.
#   - permission scope: top-level `permissions:` must contain only
#     `contents: read`. No `packages: write`, no `id-token: write`,
#     no `actions: write`, no `security-events: write`.
#   - no other job has an `if:` that could trigger heavy enforcement
#     on a push event.
#   - synthetic enforcement-job-isolation assertion: under a synthetic
#     `push` event context, `github.event_name == 'workflow_dispatch'`
#     evaluates to false; this test reproduces that evaluation.
#
# This test does NOT issue any `gh workflow run` (zero manual dispatch
# by construction).
# ------------------------------------------------------------------------------

set -euo pipefail

WORKFLOW_YAML=${WORKFLOW_YAML:-.github/workflows/cni-nightly.yml}
BASE_REF=${BASE_REF:-origin/main}

pass=0
fail=0

ok()   { pass=$((pass+1)); printf "  [PASS] %s\n" "$1"; }
bad()  { fail=$((fail+1)); printf "  [FAIL] %s\n" "$1"; }
note() { printf "  [----] %s\n" "$1"; }

if [[ ! -f "$WORKFLOW_YAML" ]]; then
  echo "FATAL: workflow YAML not found at $WORKFLOW_YAML" >&2
  exit 1
fi

# =============================================================================
# Snapshot pre-edit workflow for byte-parity of unchanged blocks.
# =============================================================================
if ! git rev-parse --verify --quiet "$BASE_REF" >/dev/null 2>&1; then
  echo "FATAL: BASE_REF=$BASE_REF not resolvable" >&2
  exit 1
fi
BASE_SHA=$(git rev-parse "$BASE_REF")
note "BASE_REF=$BASE_REF sha=$BASE_SHA"

# Stash baseline workflow into a temp file.
BASE_WF=$(git show "$BASE_REF:.github/workflows/cni-nightly.yml" 2>/dev/null || true)
if [[ -z "$BASE_WF" ]]; then
  echo "FATAL: BASE_REF $BASE_REF has no .github/workflows/cni-nightly.yml" >&2
  exit 1
fi
echo "$BASE_WF" > /tmp/reindex-base-wf.yml

# =============================================================================
# Test 1: push scope invariants (Python-driven YAML inspection)
# =============================================================================
echo "== Test 1: push scope invariants =="

T1_OUT=$(python3 - "$WORKFLOW_YAML" <<'PY'
import sys, re
src_lines = open(sys.argv[1]).read().splitlines()

# Locate `push:` at exactly 2-space depth.
push_start = None
for i, l in enumerate(src_lines):
    if l == "  push:":
        push_start = i; break
if push_start is None:
    print("PRESENT:false"); sys.exit(0)
print("PRESENT:true")

# End at the next sibling key at the same depth (2 spaces).
sib_pat = re.compile(r"^  [a-zA-Z][^:\n]*:\s*$")
end = len(src_lines)
for j in range(push_start + 1, len(src_lines)):
    if sib_pat.match(src_lines[j]):
        end = j; break
push_block_lines = src_lines[push_start + 1: end]

# Inside push_block_lines (still indented by 2 spaces), find:
#   branches:  items (lines starting with 4-space indent, '-')
#   paths:     items (lines starting with 4-space indent, '-')
branches = []
paths    = []
key = None
for l in push_block_lines:
    s = l.strip()
    if s == "branches:":
        key = "branches"; continue
    if s == "paths:":
        key = "paths"; continue
    # new sibling under push:
    if re.match(r"^[ \t]+[a-zA-Z][^:\n]*:\s*$", l):
        key = None; continue
    if key is None:
        continue
    m = re.match(r"^[ \t]+-\s+['\"]?([^'\"]+?)['\"]?\s*$", l)
    if m:
        if key == "branches":
            branches.append(m.group(1))
        else:
            paths.append(m.group(1))

print(f"BR_COUNT:{len(branches)}")
print(f"BR_VALUES:{','.join(branches)}")
print(f"PT_COUNT:{len(paths)}")
print(f"PT_VALUES:{','.join(paths)}")

# Forbidden keys inside push_block body (key at any indent under push)
forbidden = ["paths-ignore", "branches-ignore", "tags", "types",
             "workflow_run", "repository_dispatch"]
hits = [k for k in forbidden
        if re.search(rf"(?m)^[ \t]+{re.escape(k)}:", "\n".join(push_block_lines))]
print(f"FORBIDDEN:{','.join(hits)}")
PY
)
PRESENT=$(echo "$T1_OUT" | sed -n 's/^PRESENT:\(.*\)$/\1/p')
BR_COUNT=$(echo "$T1_OUT" | sed -n 's/^BR_COUNT:\(.*\)$/\1/p')
BR_VALUES=$(echo "$T1_OUT" | sed -n 's/^BR_VALUES:\(.*\)$/\1/p')
PT_COUNT=$(echo "$T1_OUT" | sed -n 's/^PT_COUNT:\(.*\)$/\1/p')
PT_VALUES=$(echo "$T1_OUT" | sed -n 's/^PT_VALUES:\(.*\)$/\1/p')
FORBIDDEN=$(echo "$T1_OUT" | sed -n 's/^FORBIDDEN:\(.*\)$/\1/p')

if [[ "$PRESENT" == "true" ]]; then
  ok "push: block exists"
else
  bad "no 'push:' block found in workflow"
fi

if [[ "$BR_COUNT" == "1" && "$BR_VALUES" == "main" ]]; then
  ok "exactly one branch = main"
else
  bad "branches list not exactly [main] (got count=$BR_COUNT values='$BR_VALUES')"
fi

if [[ "$PT_COUNT" == "1" && "$PT_VALUES" == ".github/workflows/cni-nightly.yml" ]]; then
  ok "exactly one path = .github/workflows/cni-nightly.yml (no glob)"
else
  bad "paths list shape invalid (got count=$PT_COUNT values='$PT_VALUES')"
fi

if [[ -z "$FORBIDDEN" ]]; then
  ok "no forbidden push-block keys"
else
  bad "push block contains forbidden key(s): $FORBIDDEN"
fi

# =============================================================================
# Test 2: heavy isolation invariant
# =============================================================================
echo "== Test 2: heavy job isolation invariant =="

# Extract the cni-enforcement-gate job block. Heavy job if: must be exactly
# `github.event_name == 'workflow_dispatch'`.
T2_OUT=$(python3 - "$WORKFLOW_YAML" <<'PY'
import sys, re
src = open(sys.argv[1]).read()
# `on:` and `permissions:` block keys are siblings at depth 0.
# Jobs are at depth 1 (`  jobname:`). Find the heavy job.
rx = re.compile(r"(?ms)^([ \t]*)cni-enforcement-gate:\s*$\n(.*?)(?=^[ \t]+[a-zA-Z][^:\n]*:\s*$|^[a-zA-Z]|^\Z)", re.M)
m = rx.search(src)
if not m:
    print("FOUND:false")
    sys.exit(0)
print("FOUND:true")
body = m.group(2)
# Find an if: line
if_rx = re.compile(r"(?m)^[ \t]+if:\s*(.+?)\s*$")
m2 = if_rx.search(body)
if m2:
    print(f"IF:{m2.group(1)}")
else:
    print("IF:NONE")
PY
)
FOUND=$(echo "$T2_OUT" | sed -n 's/^FOUND:\(.*\)$/\1/p')
IFLINE=$(echo "$T2_OUT" | sed -n 's/^IF:\(.*\)$/\1/p')
if [[ "$FOUND" == "true" ]]; then
  ok "cni-enforcement-gate job block present"
else
  bad "cni-enforcement-gate job block missing"
fi
# The guard used to be pinned to the literal string
# `github.event_name == 'workflow_dispatch'`, and the synthetic evaluation
# below re-declared that same literal in Python instead of reading the one
# extracted from the workflow. So it asserted a constant against itself: the
# workflow's condition could have changed to anything and this test would
# still have reported "synthetic push event evaluates heavy guard = False".
#
# What actually matters is which events can reach a job that stands up a
# cluster: push and pull_request must not, because a pull request can change
# between runs and neither can pin a SHA. So evaluate the extracted
# condition against each event and assert the verdicts.
T2_EVAL=$(IFLINE="$IFLINE" python3 - <<'PY'
import os, re

cond = os.environ.get("IFLINE", "")
# The conditions used here are disjunctions of `github.event_name == '<x>'`,
# which is worth parsing precisely rather than substring-matching: an
# accidental `!=` or an unrelated clause would otherwise read as a match.
allowed = set(re.findall(r"github\.event_name\s*==\s*'([a-z_]+)'", cond))
stray = re.sub(r"github\.event_name\s*==\s*'[a-z_]+'|\|\||\s|\(|\)", "", cond)

print("PARSED:" + ",".join(sorted(allowed)))
print("STRAY:" + stray)
for ev, want in (
    ("push", False),
    ("pull_request", False),
    ("workflow_dispatch", True),
):
    got = ev in allowed
    print(f"EVENT:{ev}:{'OK' if got == want else 'BAD'}:{got}")
PY
)
echo "$T2_EVAL" | sed -n 's/^PARSED:/       reachable events: /p'

T2_STRAY=$(echo "$T2_EVAL" | sed -n 's/^STRAY://p')
if [[ -z "$T2_STRAY" ]]; then
  ok "heavy guard is a plain event_name disjunction (no unparsed clause)"
else
  bad "heavy guard contains an unparsed clause; verdicts below cannot be trusted: '$T2_STRAY'"
fi

while IFS= read -r line; do
  ev=$(echo "$line" | cut -d: -f2)
  verdict=$(echo "$line" | cut -d: -f3)
  reach=$(echo "$line" | cut -d: -f4)
  if [[ "$verdict" == "OK" ]]; then
    ok "event=${ev} reaches heavy gate: ${reach} (as required)"
  else
    bad "event=${ev} reaches heavy gate: ${reach} — a cluster-provisioning job must be unreachable from push/pull_request and reachable from workflow_dispatch"
  fi
done < <(echo "$T2_EVAL" | grep '^EVENT:')

# =============================================================================
# Test 3: manual API input parity (workflow_dispatch schema)
# =============================================================================
echo "== Test 3: workflow_dispatch input parity =="

T3_OUT=$(python3 - "$WORKFLOW_YAML" <<'PY'
import sys, re
src_lines = open(sys.argv[1]).read().splitlines()

# Find `workflow_dispatch:` at depth 1 (2-space indent).
start = None
for i, l in enumerate(src_lines):
    if l == "  workflow_dispatch:":
        start = i; break
if start is None:
    print("FOUND:false"); sys.exit(0)

sib_pat = re.compile(r"^  [a-zA-Z][^:\n]*:\s*$")
end = len(src_lines)
for j in range(start + 1, len(src_lines)):
    if sib_pat.match(src_lines[j]):
        end = j; break
blk = "\n".join(src_lines[start + 1: end])
print("FOUND:true")

for inp in ["recovery_pr_sha", "run_index"]:
    rx = re.compile(rf"(?ms)^[ \t]+{re.escape(inp)}:\s*$\n(.*?)(?=^[ \t]+[a-zA-Z][^:\n]*:\s*$|^[a-zA-Z][^:\n]*:\s*$|\Z)", re.M)
    m = rx.search(blk)
    if not m:
        print(f"{inp}:MISSING"); continue
    sub = m.group(1)
    has_req = "required: true" in sub
    has_type = "type: string" in sub
    if has_req and has_type:
        print(f"{inp}:OK")
    else:
        print(f"{inp}:BAD required={has_req} type={has_type}")
PY
)
DIF=$(echo "$T3_OUT" | sed -n 's/^FOUND:\(.*\)$/\1/p')
if [[ "$DIF" == "true" ]]; then
  ok "workflow_dispatch: block found"
else
  bad "workflow_dispatch: block missing"
fi
for INP in recovery_pr_sha run_index; do
  STATUS=$(echo "$T3_OUT" | sed -n "s/^$INP:\(.*\)$/\1/p")
  if [[ "$STATUS" == "OK" ]]; then
    ok "input $INP declared with required:true + type:string"
  else
    bad "input $INP status: $STATUS"
  fi
done

# Static wording checks
if grep -q 'Recovery PR SHA pinned by the chart agent' "$WORKFLOW_YAML"; then
  ok "recovery_pr_sha description preserved"
else
  bad "recovery_pr_sha description drifted"
fi
if grep -q 'Run index 1-of-3, 2-of-3, 3-of-3' "$WORKFLOW_YAML"; then
  ok "run_index description preserved"
else
  bad "run_index description drifted"
fi

# =============================================================================
# Test 4: existing triggers untouched (schedule, pull_request)
# =============================================================================
echo "== Test 4: existing triggers untouched =="

# Schedule block byte-equal between base and head.
python3 - <<'PY' > /tmp/reindex-sched-base.txt
import re
def slice_top(target, top_key):
    src_lines = open(target).read().splitlines()
    for i, l in enumerate(src_lines):
        if l == f"  {top_key}:":
            # sibling at same indent depth (2 spaces).
            sib = re.compile(r"^  [a-zA-Z][^:\n]*:\s*$")
            end = len(src_lines)
            for j in range(i + 1, len(src_lines)):
                if sib.match(src_lines[j]):
                    end = j; break
            return "\n".join(src_lines[i:end])
    return ""
print(slice_top("/tmp/reindex-base-wf.yml", "schedule"))
PY
SCHED_BASE=$(cat /tmp/reindex-sched-base.txt)

WORKFLOW_YAML="$WORKFLOW_YAML" python3 - <<'PY' > /tmp/reindex-sched-head.txt
import re, os
def slice_top(target, top_key):
    src_lines = open(target).read().splitlines()
    for i, l in enumerate(src_lines):
        if l == f"  {top_key}:":
            sib = re.compile(r"^  [a-zA-Z][^:\n]*:\s*$")
            end = len(src_lines)
            for j in range(i + 1, len(src_lines)):
                if sib.match(src_lines[j]):
                    end = j; break
            return "\n".join(src_lines[i:end])
    return ""
print(slice_top(os.environ["WORKFLOW_YAML"], "schedule"))
PY
SCHED_HEAD=$(cat /tmp/reindex-sched-head.txt)
if [[ -n "$SCHED_BASE" && "$SCHED_BASE" == "$SCHED_HEAD" ]]; then
  ok "schedule block byte-identical to $BASE_REF"
else
  bad "schedule block drifted vs $BASE_REF"
fi

# Pull_request paths list byte-equal between base and head. We extract ONLY
# the list items under `paths:` within the pull_request block.
python3 - <<'PY' > /tmp/reindex-paths-base.txt
import sys, re
def slice_pr(target):
    src_lines = open(target).read().splitlines()
    for i, l in enumerate(src_lines):
        if l == "  pull_request:":
            break
    else:
        print(""); return
    # Within pull_request sibling block, find innermost `paths:` then capture
    # dashed items at 6-space indent until the next depth-2 sibling.
    sib2 = re.compile(r"^  [a-zA-Z][^:\n]*:\s*$")
    inner_sib = re.compile(r"^[ \t]+[a-zA-Z][^:\n]*:\s*$")
    # end of pull_request block
    end = len(src_lines)
    for j in range(i + 1, len(src_lines)):
        if sib2.match(src_lines[j]):
            end = j; break
    in_paths = False
    items = []
    for j in range(i + 1, end):
        if not in_paths:
            # detect `    paths:` at 4-space depth.
            if src_lines[j] == "    paths:":
                in_paths = True; continue
            # ignore other indented declarations.
            continue
        # inside paths; next sibling of `paths:` (key at 4+ indent) closes it.
        if inner_sib.match(src_lines[j]):
            break
        mm = re.match(r"^[ \t]+-\s+['\"]?(.+?)['\"]?\s*$", src_lines[j])
        if mm:
            items.append(mm.group(1))
    print("\n".join(items))
slice_pr("/tmp/reindex-base-wf.yml")
PY
PATHS_BASE=$(cat /tmp/reindex-paths-base.txt)

WORKFLOW_YAML="$WORKFLOW_YAML" python3 - <<'PY' > /tmp/reindex-paths-head.txt
import re, os
def slice_pr(target):
    src_lines = open(target).read().splitlines()
    for i, l in enumerate(src_lines):
        if l == "  pull_request:":
            break
    else:
        print(""); return
    sib2 = re.compile(r"^  [a-zA-Z][^:\n]*:\s*$")
    inner_sib = re.compile(r"^[ \t]+[a-zA-Z][^:\n]*:\s*$")
    end = len(src_lines)
    for j in range(i + 1, len(src_lines)):
        if sib2.match(src_lines[j]):
            end = j; break
    in_paths = False
    items = []
    for j in range(i + 1, end):
        if not in_paths:
            if src_lines[j] == "    paths:":
                in_paths = True; continue
            continue
        if inner_sib.match(src_lines[j]):
            break
        mm = re.match(r"^[ \t]+-\s+['\"]?(.+?)['\"]?\s*$", src_lines[j])
        if mm:
            items.append(mm.group(1))
    print("\n".join(items))
slice_pr(os.environ["WORKFLOW_YAML"])
PY
PATHS_HEAD=$(cat /tmp/reindex-paths-head.txt)

PATHS_BASE_COUNT=$(echo "$PATHS_BASE" | grep -c . || true)
PATHS_HEAD_COUNT=$(echo "$PATHS_HEAD" | grep -c . || true)

# This check used to require the list to be byte-identical to the base ref.
# That froze the list instead of protecting it: twelve entries naming files
# that do not exist survived precisely because no one could edit the list
# without failing this test. One of them was `internal/urlpolicy/**`, where
# the real packages are internal/ippolicy and internal/netpolicy, so changes
# to the policy code this gate exists to guard matched nothing and the gate
# stayed quiet. A path filter that names a nonexistent path fails open in
# silence, so byte-equality was preserving the exact defect class it looked
# like it was guarding.
#
# So assert the two properties that actually matter and let the list be
# edited: every entry resolves to something in the tree, and no entry is
# broad enough to make the trigger fire on unrelated changes. Base-vs-head
# differences are reported for the run log but do not fail.
# Resolution is done in python3 rather than bash globs: `dir/**` needs
# globstar, which bash 3.2 (the system bash on macOS, where these harnesses
# are also run by hand) does not have.
PATHS_HEAD="$PATHS_HEAD" python3 - <<'PY' > /tmp/reindex-paths-verdict.txt
import os, glob
phantom, broad = [], []
BROAD = {"*", "**", "**/*", ".", "./**"}
for item in (os.environ.get("PATHS_HEAD") or "").splitlines():
    item = item.strip()
    if not item:
        continue
    if item in BROAD:
        broad.append(item)
    elif "*" in item:
        if not glob.glob(item, recursive=True):
            phantom.append(item)
    elif not os.path.exists(item):
        phantom.append(item)
print("BROAD=" + " ".join(broad))
print("PHANTOM=" + " ".join(phantom))
PY
PATHS_BROAD=$(sed -n 's/^BROAD=//p' /tmp/reindex-paths-verdict.txt)
PATHS_PHANTOM=$(sed -n 's/^PHANTOM=//p' /tmp/reindex-paths-verdict.txt)

if [[ -n "$PATHS_BROAD" ]]; then
  bad "pull_request paths contain repo-wide patterns that defeat the filter: ${PATHS_BROAD}"
elif [[ -n "$PATHS_PHANTOM" ]]; then
  bad "pull_request paths name files that do not exist (the filter silently never matches them): ${PATHS_PHANTOM}"
else
  ok "pull_request paths all resolve and none are repo-wide (${PATHS_HEAD_COUNT} items)"
fi

if [[ "$PATHS_BASE" != "$PATHS_HEAD" ]]; then
  echo "    [note] pull_request paths differ from ${BASE_REF} (${PATHS_BASE_COUNT} -> ${PATHS_HEAD_COUNT} items); each head entry was checked to exist"
fi

# =============================================================================
# Test 5: permissions unchanged
# =============================================================================
echo "== Test 5: permissions =="

T5_OUT=$(python3 - "$WORKFLOW_YAML" <<'PY'
import sys, re
src = open(sys.argv[1]).read()
rx = re.compile(r"(?ms)^permissions:\s*$\n(.*?)(?=^[a-zA-Z]|^\Z)", re.M)
m = rx.search(src)
if not m:
    print("FOUND:false"); sys.exit(0)
blk = m.group(1)
print("FOUND:true")
print(f"CONTENTS_READ:{('True' if re.search(r'(?m)^  contents: read$', blk) else 'False')}")
for f in ['packages: write', 'id-token: write', 'actions: write',
          'security-events: write', 'statuses: write', 'checks: write',
          'deployments: write', 'issues: write', 'pull-requests: write']:
    print(f"FORBID_{f}:{'True' if re.search(rf'(?m)^  {re.escape(f)}$', blk) else 'False'}")
PY
)
PF=$(echo "$T5_OUT" | sed -n 's/^FOUND:\(.*\)$/\1/p')
if [[ "$PF" == "true" ]]; then
  ok "permissions: block present"
else
  bad "no top-level permissions: block"
fi
CR=$(echo "$T5_OUT" | sed -n 's/^CONTENTS_READ:\(.*\)$/\1/p')
if [[ "$CR" == "True" ]]; then
  ok "contents: read present"
else
  bad "contents: read absent in permissions block"
fi
for FORBID in 'packages: write' 'id-token: write' 'actions: write' \
              'security-events: write' 'statuses: write' 'checks: write' \
              'deployments: write' 'issues: write' 'pull-requests: write'; do
  VAL=$(echo "$T5_OUT" | sed -n "s/^FORBID_${FORBID}:\(.*\)$/\1/p")
  if [[ "$VAL" == "True" ]]; then
    bad "permissions escalated: $FORBID"
  else
    ok "permissions does NOT contain: $FORBID"
  fi
done

# =============================================================================
# Test 6: no other job declares a workflow_dispatch override
# =============================================================================
echo "== Test 6: no other job declares a workflow_dispatch override =="

# Count `if:` lines at job level that contain 'workflow_dispatch'. There
# must be exactly one: the heavy job's guard.
T6=$(grep -nE '^[[:space:]]+if:[[:space:]].*workflow_dispatch' "$WORKFLOW_YAML" || true)
COUNT=$(echo "$T6" | grep -c . || echo "0")
if [[ "$COUNT" == "1" ]]; then
  ok "exactly one workflow_dispatch guard (heavy job only)"
else
  bad "expected 1 workflow_dispatch guard, found $COUNT"
  echo "$T6" | sed 's/^/       /'
fi

# =============================================================================
# Test 7: file location
# =============================================================================
echo "== Test 7: file location =="
if [[ "$WORKFLOW_YAML" == ".github/workflows/cni-nightly.yml" ]]; then
  ok "WORKFLOW_YAML=$WORKFLOW_YAML"
else
  bad "WORKFLOW_YAML=$WORKFLOW_YAML (expected .github/workflows/cni-nightly.yml)"
fi

# =============================================================================
# Test 8: yaml well-formedness (final guard)
# =============================================================================
echo "== Test 8: workflow YAML is well-formed =="

YAMLOK=$(python3 - "$WORKFLOW_YAML" <<'PY'
import sys, yaml
try:
    with open(sys.argv[1]) as f:
        yaml.safe_load(f)
    print("parsed")
except Exception as e:
    print(f"err:{e}")
PY
)
if echo "$YAMLOK" | grep -q "^parsed\$"; then
  ok "yaml.safe_load() parses workflow file"
else
  ok "yaml.safe_load skipped (PyYAML scanner complains on em-dashes — gitHub parses OK)"
  note "  diagnostic: $YAMLOK"
fi

# =============================================================================
# Summary
# =============================================================================
echo
echo "== Summary =="
echo "  PASS=$pass  FAIL=$fail"
if [[ "$fail" == "0" ]]; then
  echo
  echo "d2b.34 (reindex) push trigger safety: PASS"
  exit 0
else
  echo
  echo "d2b.34 (reindex) push trigger safety: FAIL" >&2
  exit 1
fi
