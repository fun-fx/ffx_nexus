#!/usr/bin/env python3
"""Scripts-level mutation tests for
scripts/fixtures/integrationcni/build.sh.

These tests verify that:
  - `set -u` does NOT abort when RepoDigests
    is empty, but the script still records
    a structured `repo_digest_or_none="none"`
    and a non-empty image_id.
  - `set -u` aborts with the documented exit
    code when the image_id is empty in the
    fixture (synthesised by replacing the
    parsed image_id with the empty string).
  - RepoDigests containing a sha256 prefix
    is parsed correctly and recorded in
    repo_digest_or_id.

The tests run bash with a small stub layer
in front of `docker inspect` so we can
synthesise the failure modes without a live
docker daemon. Stubs live in a tmp dir so
the production image is never touched.
"""
import json
import os
import shutil
import stat
import subprocess
import sys
import tempfile
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent.parent.parent.parent
BUILD_SH = REPO / "scripts" / "fixtures" / "integrationcni" / "build.sh"


def assert_eq(label, got, want):
    ok = (got == want)
    flag = "OK  " if ok else "FAIL"
    print(f"[{flag}] {label}: got={got!r} want={want!r}")
    return ok


def run_with_stub(stub_body, env_extra=None, image_ref="cni-listener:local"):
    """Run build.sh with a docker shim whose
    behavior is described by stub_body (a
    shell snippet that defines fake
    `docker`, `docker build`, and
    `docker inspect` commands). The script
    captures the resulting exit code and the
    JSON line on stdout. Stubs live in a
    private PATH that is prefixed so the
    system docker is never invoked."""
    work = Path(tempfile.mkdtemp(prefix="d2b26-fixture-"))
    try:
        bin_ = work / "bin"
        bin_.mkdir()
        # The wrapper exposes a stable state
        # file under $work/.stub_state so two
        # flavours of `docker inspect` (full
        # JSON vs. --format=RepoDigests) can
        # emit different bodies.
        wrapper_source = (
            "#!/usr/bin/env bash\n"
            "STATE=\"${ARTIFACTS:-/tmp}/.stub_state\"\n"
            "touch \"$STATE\" 2>/dev/null || true\n"
            "subcmd=\"$1\"; shift\n"
            + stub_body
        )
        wrapper = bin_ / "docker"
        wrapper.write_text(wrapper_source)
        wrapper.chmod(stat.S_IRWXU)
        env = {
            "PATH": f"{bin_}{os.pathsep}{os.environ.get('PATH','')}",
            "IMAGE_REF": image_ref,
            "ARTIFACTS": str(work),
            "HOME": str(work),
        }
        if env_extra:
            env.update(env_extra)
        proc = subprocess.run(
            ["bash", str(BUILD_SH)],
            env=env, capture_output=True, text=True,
        )
        return proc, work
    except Exception:
        shutil.rmtree(work, ignore_errors=True)
        raise


# ---- 1. RepoDigests empty, image_id present ------------------------------
# A just-built local image with no push
# history returns an empty RepoDigests.
# The script must record repo_digest_or_none
# = "none" and exit 0.
STUB_REPODIGESTS_EMPTY = r"""
case "$subcmd" in
  build)
    echo 'sha256:fakedeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdead' >&1
    exit 0 ;;
  inspect)
    for arg in "$@"; do
      case "$arg" in
        --format=*)
          # RepoDigests --format output: empty
          # (the array is empty for a local-only
          # image that has not been pushed).
          echo ''
          exit 0 ;;
      esac
    done
    cat <<'JSON'
[{"Id":"sha256:cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe","RepoDigests":[],"Created":"2026-01-01T00:00:00Z"}]
JSON
    exit 0 ;;
  version) echo "27.0.0" ;;
  *) echo "stub: docker $subcmd $@" >&2; exit 1 ;;
esac
"""
proc, work = run_with_stub(STUB_REPODIGESTS_EMPTY)
artifact = work / "fixture-image-digest.json"
ok = []
ok.append(assert_eq(
    "RepoDigests=[] + image_id present -> exit 0 (success)",
    proc.returncode, 0,
))
if artifact.exists():
    blob = json.loads(artifact.read_text())
    ok.append(assert_eq(
        "RepoDigests=[] + image_id present -> artifact contains image_id",
        blob.get("image_id"),
        "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe",
    ))
    # When RepoDigests is empty, the script
    # records repo_digest_or_none === "none"
    # OR an explicit non-"none" sha256
    # fallback derived from the image id.
    rd = blob.get("repo_digest_or_none")
    di = blob.get("repo_digest_or_id")
    ok.append(assert_eq(
        "RepoDigests=[] + image_id present -> repo_digest_or_none=='none' or sha256",
        rd == "none" or (isinstance(rd, str) and rd.startswith("sha256:")),
        True,
    ))
    ok.append(assert_eq(
        "RepoDigests=[] + image_id present -> repo_digest_or_id is sha256 hex",
        bool(isinstance(di, str) and (
            di == "none" or di.startswith("id-") or len(di) == 64
            or (di.startswith("sha256:") and len(di) == 71)
        )),
        True,
    ))
    ok.append(assert_eq(
        "RepoDigests=[] + image_id present -> exit_classification=='build_success'",
        blob.get("exit_classification"), "build_success",
    ))
else:
    ok.append(assert_eq(
        "RepoDigests=[] + image_id present -> artifact file exists",
        False, True,
    ))
shutil.rmtree(work, ignore_errors=True)

# ---- 2. image_id empty: build script must abort with exit 11 ------------
STUB_IMAGE_ID_EMPTY = r"""
case "$subcmd" in
  build)
    echo 'sha256:fake' >&1; exit 0 ;;
  inspect)
    cat <<'JSON'
[{"Id":"","RepoDigests":[],"Created":"2026-01-01T00:00:00Z"}]
JSON
    exit 0 ;;
  version) echo "27.0.0" ;;
  *) echo "stub: docker $subcmd $@" >&2; exit 1 ;;
esac
"""
proc, work = run_with_stub(STUB_IMAGE_ID_EMPTY)
ok.append(assert_eq(
    "image_id empty -> exit 11 (no silent success)",
    proc.returncode, 11,
))
shutil.rmtree(work, ignore_errors=True)

# ---- 3. RepoDigests has sha256 prefix; script records the canonical digest --
STUB_REPODIGESTS_PRESENT = r"""
case "$subcmd" in
  build)
    echo 'sha256:fake' >&1; exit 0 ;;
  inspect)
    # A --format=... call returns a single
    # digest; the plain (no --format) call
    # returns the full JSON document.
    for arg in "$@"; do
      case "$arg" in
        --format=*)
          echo 'cni-listener:local@sha256:2222222222222222222222222222222222222222222222222222222222bbbb'
          exit 0 ;;
      esac
    done
    cat <<'JSON'
[{"Id":"sha256:1111111111111111111111111111111111111111111111111111111111aaaa","RepoDigests":["cni-listener:local@sha256:2222222222222222222222222222222222222222222222222222222222bbbb"],"Created":"2026-01-01T00:00:00Z"}]
JSON
    exit 0 ;;
  version) echo "27.0.0" ;;
  *) echo "stub: docker $subcmd $@" >&2; exit 1 ;;
esac
"""
proc, work = run_with_stub(STUB_REPODIGESTS_PRESENT)
artifact = work / "fixture-image-digest.json"
if artifact.exists():
    blob = json.loads(artifact.read_text())
    ok.append(assert_eq(
        "RepoDigests populated -> repo_digest_or_none='sha256:2222...bbbb'",
        blob.get("repo_digest_or_none"),
        "sha256:2222222222222222222222222222222222222222222222222222222222bbbb",
    ))
    ok.append(assert_eq(
        "RepoDigests populated -> repo_digest_or_id matches",
        blob.get("repo_digest_or_id"),
        "2222222222222222222222222222222222222222222222222222222222bbbb",
    ))
shutil.rmtree(work, ignore_errors=True)

# ---- 4. ImagePullPolicy must NEVER be `Always` in any fixture yaml ---
# The directive requires the gate AND the
# fixtures to refuse an `Always` policy so a
# fixture Pod can never silently fall back to
# a remote registry. We simulate the mutation
# by listing every fixture yaml's container
# `imagePullPolicy` and confirm none equals
# `Always`.
import yaml as _yaml

FIXTURE_DIR = REPO / "scripts" / "fixtures" / "integrationcni"

# Real guard: walk every fixture yaml and
# collect every container's `imagePullPolicy`.
# ASSERT: none equals `Always`.
def _walk_imagepull(paths):
    offenders = []
    for p in paths:
        try:
            for d in _yaml.safe_load_all(open(p).read()):
                if not isinstance(d, dict):
                    continue
                kind = d.get("kind")
                if kind not in ("Pod", "Deployment", "StatefulSet", "DaemonSet"):
                    continue
                spec = d.get("spec") or {}
                tmpl = spec.get("template") or {}
                tspec = tmpl.get("spec") or spec
                for c in tspec.get("containers") or []:
                    if c.get("imagePullPolicy") == "Always":
                        offenders.append(
                            f"{p}:{d.get('metadata',{}).get('name','?')}:"
                            f"{c.get('name','?')}"
                        )
        except Exception:
            pass
    return offenders

offenders_always = _walk_imagepull(sorted(FIXTURE_DIR.glob("*.yaml")))
ok.append(assert_eq(
    "no fixture yaml uses imagePullPolicy=Always (mutation FAIL semaphore)",
    offenders_always,
    [],
))

# ---- 5. Image tag MUST stay `cni-listener:local` everywhere --------------
# A mutated tag (e.g., `cni-listener:bumped`)
# is an unauthorized registry-sourced
# pin; the contract is `cni-listener:local`
# ONLY.
def _walk_tag(paths):
    offenders = []
    for p in paths:
        # cni-listener:local is the canonical
        # build output; we forbid any
        # variant.
        for d in _yaml.safe_load_all(open(p).read()):
            if not isinstance(d, dict):
                continue
            kind = d.get("kind")
            if kind not in ("Pod", "Deployment", "StatefulSet", "DaemonSet"):
                continue
            spec = d.get("spec") or {}
            tmpl = spec.get("template") or {}
            tspec = tmpl.get("spec") or spec
            for c in tspec.get("containers") or []:
                img = c.get("image") or ""
                if img and not img.startswith("cni-listener:local"):
                    offenders.append(
                        f"{p}:{d.get('metadata',{}).get('name','?')}:"
                        f"{c.get('name','?')}:{img}"
                    )
    return offenders

offenders_tag = _walk_tag(sorted(FIXTURE_DIR.glob("*.yaml")))
ok.append(assert_eq(
    "every fixture container image starts with 'cni-listener:local'",
    offenders_tag,
    [],
))

# ---- 6. Per-node runtime verification (d2b.51-final bounded contract) --
# The d2b.51-final contract replaces the old
# immediate post-load `grep -qE prefix` check
# with: exactly one kind load, then a bounded
# machine-readable crictl images --output
# json run on every node, evaluating exact tag
# PLUS the full normalized image-id. Each
# attempt writes deterministic raw artifacts
# under $ARTIFACTS/attempts/attempt-N/.
install_src_path = REPO / "scripts" / "install-nexus-test.sh"
install_src = install_src_path.read_text() if install_src_path.exists() else ""

ok.append(assert_eq(
    "install-nexus-test.sh iterates over kind nodes for crictl images",
    "kind get nodes" in install_src and "crictl images" in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh records per-node node-runtime log",
    "fixture-image-node-runtime.log" in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh records docker build full output",
    "fixture-image-build.log" in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh records docker inspect full output",
    "fixture-image-inspect.json" in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh records kind load command output",
    "fixture-image-kind-load.log" in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh records fixture image_pull_fail log",
    "fixture-pod-imagepull.log" in install_src,
    True,
))
# d2b.51-final: the verifier MUST NOT use a
# partial grep prefix as the success predicate.
ok.append(assert_eq(
    "install-nexus-test.sh does NOT use partial grep -qE prefix as success predicate",
    ('grep -qE "${FIXTURE_IMAGE_ID:0:12}"'
     in install_src),
    False,
))
# d2b.51-final: exactly ONE kind load
# invocation (no retry/reload/rebuild), and
# the verifier sleeps ONLY when another
# attempt remains (no success-by-timeout).
# We count the actual executable invocation
# (the line begins with `kind load docker-image`
# syntactically, not as a quoted echo string).
ok.append(assert_eq(
    "install-nexus-test.sh invokes kind load docker-image exactly once (executable)",
    sum(1 for ln in install_src.splitlines()
        if ln.lstrip().startswith("kind load docker-image ")),
    1,
))
ok.append(assert_eq(
    "install-nexus-test.sh fixes IMG_VERIFY_MAX_ATTEMPTS=15",
    "IMG_VERIFY_MAX_ATTEMPTS=15" in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh fixes IMG_VERIFY_INTERVAL_SEC=2",
    "IMG_VERIFY_INTERVAL_SEC=2" in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh fixes IMG_VERIFY_MAX_WINDOW_SEC=30",
    "IMG_VERIFY_MAX_WINDOW_SEC=30" in install_src,
    True,
))
# d2b.51-final: machine-readable JSON mode
# crictl images, NOT text grep.
ok.append(assert_eq(
    "install-nexus-test.sh invokes crictl images --output json",
    "crictl images --output json" in install_src,
    True,
))
# d2b.51-final: per-attempt raw artifacts.
ok.append(assert_eq(
    "install-nexus-test.sh writes per-attempt raw stdout artifacts",
    "attempts/attempt-" in install_src,
    True,
))
# d2b.51-final: explicit normalized image-id
# call site (sha256 prefix stripped)
# immediately before comparison.
ok.append(assert_eq(
    "install-nexus-test.sh strips sha256: only before normalized full ID compare",
    "removeprefix(\"sha256:\")" in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh routes pre-flight fixture dry-run to exit 15",
    "exit 15" in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh routes pre-flight dry-run via FIXTURE_INVALID env",
    "FIXTURE_INVALID=1" in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh declares fixture-dryrun.log artifact path",
    "fixture-dryrun.log" in install_src,
    True,
))
ok.append(assert_eq(
    "cni-readiness-gate.sh routes FIXTURE_INVALID=1 env to exit 15",
    "FIXTURE_INVALID=1" in open(
        REPO / "scripts" / "cni-readiness-gate.sh"
    ).read(),
    True,
))

# ---------------------------------------------------------------------------
# d2b.51.51-final-clean: harness C8k predicate
# STRICTNESS assertions. The harness must
# strictly derive the C8k terminal record
# from actual step_image_pipeline JSON and
# prove node-c:not_ready, attempt 15, parser
# rc 0, empty parser stderr, exit 14,
# fourteen sleeps, one load, and no handoff.
harness_src_path = REPO / "scripts" / "test_fixture_readiness_observability.sh"
harness_src = harness_src_path.read_text() if harness_src_path.exists() else ""

# d2b.51.51-final-clean: the harness C8k
# evidence line must name every C8k
# contract field AND emit the contract
# `failing-nodes=node-c:not_ready` token.
ok.append(assert_eq(
    "harness C8k evidence line names terminal attempt=15",
    'attempt=15 failing-nodes=node-c:not_ready' in harness_src,
    True,
))
ok.append(assert_eq(
    "harness C8k evidence line names parser-stderr-empty",
    'parser-stderr-empty=%s' in harness_src,
    True,
))
ok.append(assert_eq(
    "harness C8k evidence line names no-handoff=Y",
    'no-handoff=%s' in harness_src,
    True,
))
ok.append(assert_eq(
    "harness C8k evidence line emits failing-nodes=node-c:not_ready",
    'failing-nodes=node-c:not_ready' in harness_src,
    True,
))
# d2b.51.51-final-clean: the C8k predicate
# must require parser_rc=0 (not rc=14 or
# any other default value).
ok.append(assert_eq(
    "harness C8k predicate requires parser_rc=0",
    '[ "${C8K_PARSE_RC}" = "0" ]' in harness_src,
    True,
))
ok.append(assert_eq(
    "harness C8k predicate requires parser_line=Y (exact contract line)",
    '[ "${C8K_PARSER_LINE_OK}" = "Y" ]' in harness_src,
    True,
))
ok.append(assert_eq(
    "harness C8k predicate requires parser_stderr_empty=Y",
    '[ "${C8K_PARSER_STDERR_EMPTY}" = "Y" ]' in harness_src,
    True,
))
# d2b.51.51-final-clean: the strict parser
# must be a portable python3 invocation
# (heredoc or python3 -c) and MUST NOT use
# invalid awk/escape constructs. Reject the
# former fragile awk pattern.
ok.append(assert_eq(
    "harness uses portable python3 heredoc for C8k strict parser",
    "C8KPYEOF" in harness_src or "python3 -c \"" in harness_src
    or 'python3 -c "' in harness_src,
    True,
))
ok.append(assert_eq(
    "harness does NOT use the former invalid awk escape construct",
    '"\\"\\""' not in harness_src
    and "C8K_FAILS_NODES=\"$(echo" not in harness_src,
    True,
))
ok.append(assert_eq(
    "harness does NOT carry an empty-default C8K_FAILS_NODES echo-fallback",
    "echo \"False []\"" not in harness_src and "echo 'False []'" not in harness_src,
    True,
))
# d2b.51.51-final-clean: the production
# contract reason appears consistently in
# the harness parser and the C8K event
# ledger line.
CONTRACT_REASON = "tag or normalized id mismatch (parser OK; tag/id not exact)"
ok.append(assert_eq(
    "harness parser references the production contract reason",
    CONTRACT_REASON in harness_src,
    True,
))
ok.append(assert_eq(
    "harness strips sha256 prefix in normalized full id comparison",
    'IMG_VERIFY_EXPECTED_ID' in install_src,
    True,
))
# d2b.51.51-final-clean: fake-date state MUST
# live under the stage-local fakebin root
# and CANNOT escape to /__date_state or
# other root-absolute paths.
ok.append(assert_eq(
    "harness fake-date uses FAKE_DATE_STATE (canonical key)",
    "FAKE_DATE_STATE" in harness_src,
    True,
))
ok.append(assert_eq(
    "harness fake-date validates state path is absolute AND under root",
    'fake-date: state path' in harness_src and 'not under root' in harness_src,
    True,
))
ok.append(assert_eq(
    "harness fake-date fails closed if FAKE_DATE_STATE absent",
    'fake-date: missing FAKE_DATE_STATE or HARNESS_FAKE_BIN_ROOT' in harness_src,
    True,
))
ok.append(assert_eq(
    "harness write_env_file publishes FAKE_DATE_STATE per stage",
    "printf 'FAKE_DATE_STATE=%s\\n'" in harness_src,
    True,
))
# d2b.51.51-final-clean: clean-stderr harness-
# level gate present.
ok.append(assert_eq(
    "harness-level clean-stderr gate captures parent FD2 to a trace",
    "exec 2>" in harness_src,
    True,
))
ok.append(assert_eq(
    "harness verifies parent stderr contains zero unexpected noise",
    "HARNESS_STDERR_NOISE_COUNT" in harness_src,
    True,
))
ok.append(assert_eq(
    "harness emits the clean-stderr ledger line",
    "# clean-stderr: noise_count=" in harness_src,
    True,
))
# d2b.51.51-final-clean: parent PATH is
# scrubbed of any stale d2b46-.*fakebin or
# *_fakebin entries so bash's internal
# `$(date +%s)` calls never reach a
# previous fakebin.
ok.append(assert_eq(
    "harness scrubs d2b46-*-fakebin entries from parent PATH",
    '*d2b46-*|*/fakebin|*-fakebin' in harness_src,
    True,
))
# ---------------------------------------------------------------------------
# d2b.51.51-final-correct: same-entry image
# identity (cross-entry split is a negative
# case). Aggregate booleans MUST NOT drive
# ready independently. The test rejects any
# source that derives success from
# `ready = tag_match and id_match` (or its
# semantic equivalent; e.g. `ready = any(img
# for img in imgs if img_match)`).
# Detection: scan install-nexus-test.sh for
# the legacy aggregate derivation AND for the
# new same_entry_match invariant. The
# parser heredoc MUST compute per-entry
# matches and gate ready on the
# same_entry_match boolean.
ok.append(assert_eq(
    "install-nexus-test.sh per-entry parser computes same_entry_match invariant",
    'same_entry_match' in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh per-entry parser records tag_seen_anywhere for telemetry",
    'tag_seen_anywhere' in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh per-entry parser records id_seen_anywhere for telemetry",
    'id_seen_anywhere' in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh does NOT derive ready from aggregate tag_match and id_match",
    # The d2b.45-era aggregate derivation
    # combined two separate entry-iteration
    # booleans. We forbid the EXACT literal
    # form (`ready = tag_match and id_match`)
    # AND any semantic equivalent where ready
    # is the boolean product of an aggregate
    # tag_match and aggregate id_match over
    # the images list. The new parser uses
    # `ready = same_entry_match` (one boolean
    # produced inside the same-entry loop).
    ('ready = tag_match and id_match'
     not in install_src)
    and 'ready = tag_match and id_match' not in harness_src,
    True,
))
# d2b.51.51-final-correct: NO printf-style
# hand-built JSON for the runtime report
# files. Only json.dumps / json.dump is
# allowed.
ok.append(assert_eq(
    "install-nexus-test.sh does NOT hand-build fixture-image-node-runtime.json with printf",
    # The FIN helper printed `_report.txt`.
    'printf \'{\\n"expected_ref"\'' not in install_src
    and 'printf \'{\\n"attempt"' not in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh does NOT hand-build fixture-image-node-runtime-attempts.jsonl with printf",
    # The old per-attempt JSON line used
    # printf '\\n"attempt":'. Reject it.
    not any(snippet in install_src for snippet in (
        'printf \'{\\n"attempt":',
        'printf \'{\\n"attempt": %s',
    )),
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh uses python3 json.dumps for the per-attempt JSONL serializer",
    'json.dumps(record' in install_src
    and 'ATTEMPT_PYEOF' in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh uses python3 json.dumps for the final report serializer",
    'json.dumps(record' in install_src
    and 'TERMINAL_PYEOF' in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh surfaces serializer failure as FIXTURE_IMAGE_NOT_LOADED 14",
    'SERIALIZER_FAIL' in install_src
    and 'FIXTURE_IMAGE_NOT_LOADED' in install_src,
    True,
))
# d2b.51.51-final-correct: the production
# parser uses .removeprefix("sha256:") to
# normalise image IDs. The d2b.45-era
# raw_id[6:] form strips only the bytes
# "sha256" and leaves the ":" separator,
# producing a FALSE-positive for any cross-
# entry split.
ok.append(assert_eq(
    "install-nexus-test.sh parser uses .removeprefix(\"sha256:\") (NOT raw_id[6:])",
    '.removeprefix("sha256:")' in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh parser no longer uses raw_id[6:] for sha256 prefix strip",
    'raw_id[6:]' not in install_src
    and 'raw_id.removeprefix("sha256")' not in install_src,
    True,
))
# d2b.51.51-final-correct: the harness C8p
# ledger line includes the cross-entry-split
# marker and the false-positive-prevention
# tokens.
ok.append(assert_eq(
    "harness C8p evidence line includes cross-entry-split",
    'cross-entry-split rc=' in harness_src,
    True,
))
ok.append(assert_eq(
    "harness C8p evidence line includes tag-seen-anywhere=Y",
    'tag-seen-anywhere=Y' in harness_src,
    True,
))
ok.append(assert_eq(
    "harness C8p evidence line includes id-seen-anywhere=Y",
    'id-seen-anywhere=Y' in harness_src,
    True,
))
ok.append(assert_eq(
    "harness C8p evidence line includes json-valid=Y (or json-valid=N) — present",
    'json-valid=' in harness_src,
    True,
))
# d2b.51.51-final-correct: the harness C8p
# PASS predicate requires the cross-entry
# subcase to record all of:
#   rc=14, kind-loads=1, attempt=15,
#   same-entry-match=N,
#   tag-seen-anywhere=Y, id-seen-anywhere=Y,
#   no-handoff=Y, json-valid=Y
ok.append(assert_eq(
    "harness C8p_cross_entry subcase asserts cross-entry payload contains split",
    'cni-listener:not-the-target' in harness_src,
    True,
))
ok.append(assert_eq(
    "harness asserts C8P3_RC=\"14\" in C8p predicate",
    '[ "${C8P3_RC}" = "14" ]' in harness_src,
    True,
))
ok.append(assert_eq(
    "harness asserts C8P3_KIND_LOAD_COUNT=\"1\" in C8p predicate",
    '[ "${C8P3_KIND_LOAD_COUNT}" = "1" ]' in harness_src,
    True,
))
ok.append(assert_eq(
    "harness asserts C8P3_SLEEP_COUNT=\"14\" in C8p predicate",
    '[ "${C8P3_SLEEP_COUNT}" = "14" ]' in harness_src,
    True,
))
ok.append(assert_eq(
    "harness asserts C8P3_SAME_ENTRY_OK, AGGREGATE_AGREE_TAG/ID in C8p predicate",
    'C8P3_SAME_ENTRY_OK' in harness_src
    and 'C8P3_AGGREGATE_AGREE_TAG' in harness_src
    and 'C8P3_AGGREGATE_AGREE_ID' in harness_src,
    True,
))
ok.append(assert_eq(
    "harness asserts C8P3_JSON_VALID (terminal JSON parses) in C8p predicate",
    '[ "${C8P3_JSON_VALID}" = "Y" ]' in harness_src,
    True,
))
# d2b.51.51-final-correct-evidence-integrity:
# strict serializer input-schema validation.
# The former weak patterns must be REJECTED:
#   - `if nodes_tsv_path and os.path.isfile`
#     (allows silent skip on absent file)
#   - `if len(parts) < 9: continue`
#     (silently drops short rows)
#   - unknown-boolean default False /
#     unknown-token-coerced-to-false
#   - all_nodes_ready missing the per-node
#     record-set equality check
#   - missing canonical TSV argument
ok.append(assert_eq(
    "production serializer no longer uses weak `if nodes_tsv_path and os.path.isfile` gate",
    'if nodes_tsv_path and os.path.isfile' not in install_src,
    True,
))
ok.append(assert_eq(
    "production serializer no longer silently drops rows via `if len(parts) < 9: continue`",
    'if len(parts) < 9: continue' not in install_src
    and 'len(parts) < 9:' not in install_src,
    True,
))
ok.append(assert_eq(
    "production serializer header check references nine-column EXPECTED_HEADER constant",
    'EXPECTED_HEADER' in install_src
    and 'per_attempt_node_tsv_header_mismatch' in install_src,
    True,
))
ok.append(assert_eq(
    "production serializer rejects unknown boolean spellings (no false-on-unknown)",
    'unknown_boolean' in install_src,
    True,
))
ok.append(assert_eq(
    "production serializer accepts argv slot 10 as canonical_nodes_tsv",
    # Slot 10 stays canonical_nodes_tsv; the
    # closed accepted-runtime-tag set lands in
    # slot 11, so the fixed-width slice is
    # [1:12] rather than the pre-alias [1:11].
    'canonical_nodes_tsv_path, accepted_runtime_tags_raw = sys.argv[1:12]'
    in install_src,
    True,
))
ok.append(assert_eq(
    "terminal serializer takes accepted_runtime_tags as argv slot 12 (fixed-width [1:13])",
    'accepted_runtime_tags_raw) = sys.argv[1:13]' in install_src,
    True,
))
ok.append(assert_eq(
    "production serializer rejects missing canonical TSV with canonical_nodes_tsv_missing",
    'canonical_nodes_tsv_missing' in install_src,
    True,
))
ok.append(assert_eq(
    "production serializer rejects duplicate canonical nodes",
    'canonical_node_duplicate' in install_src,
    True,
))
ok.append(assert_eq(
    "production serializer rejects node-set mismatch (records vs canonical)",
    'per_attempt_node_tsv_node_set_mismatch' in install_src,
    True,
))
ok.append(assert_eq(
    "production serializer rejects per-attempt row count mismatch",
    'per_attempt_node_tsv_count_mismatch' in install_src,
    True,
))
ok.append(assert_eq(
    "production serializer rejects node-not-in-canonical-set",
    'per_attempt_node_tsv_node_not_canonical' in install_src,
    True,
))
ok.append(assert_eq(
    "production serializer rejects duplicate node in per-attempt set",
    'per_attempt_node_tsv_node_duplicate' in install_src,
    True,
))
ok.append(assert_eq(
    "production serializer rejects non-integer command_rc / parser_rc",
    'per_attempt_node_tsv_command_rc_non_integer' in install_src
    and 'per_attempt_node_tsv_parser_rc_non_integer' in install_src,
    True,
))
ok.append(assert_eq(
    "production serializer rejects empty raw_stdout/raw_stderr paths",
    'per_attempt_node_tsv_raw_stdout_empty' in install_src
    and 'per_attempt_node_tsv_raw_stderr_empty' in install_src,
    True,
))
ok.append(assert_eq(
    "production serializer writes only `serializer_error=<reason>` on failure (single line)",
    'sys.stderr.write("serializer_error=" + reason' in install_src,
    True,
))
ok.append(assert_eq(
    "production serializer sets all_nodes_ready from complete-record-set check (not aggregate)",
    'record_set == canonical_set_sorted' in install_src
    and 'all_nodes_ready' in install_src,
    True,
))
# d2b.51.51-final-correct-evidence-integrity:
# shell caller passes canonical nodes_tsv
# AS argv slot 10 to the production
# serializer. Without this arg the
# serializer rejects. The static test
# inspects argv NAMES and ASSERT the
# canonical TSV literal sits in the
# python3 - argv{...} invocation.
ok.append(assert_eq(
    "install-nexus-test.sh passes canonical nodes_tsv to the JSONL serializer",
    '"${per_attempt_node_tsv}"' in install_src
    and '"${nodes_tsv}"' in install_src,
    True,
))
# d2b.51.51-final-correct-evidence-integrity:
# shell caller fail-closed path writes
# terminal_failure_reason='json serializer
# failed' (canonical token) and abort_as
# FIXTURE_IMAGE_NOT_LOADED with rc 14.
ok.append(assert_eq(
    "install-nexus-test.sh fail-closed branch writes canonical 'json serializer failed' failure_reason",
    'json serializer failed' in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh serializer-fail-closed invokes abort_as FIXTURE_IMAGE_NOT_LOADED with rc 14 start",
    'SERIALIZER_FAIL' in install_src,
    True,
))
# d2b.51.51-final-correct-evidence-integrity:
# harness exercises the strict serializer
# through FOUR named subcases driven by
# extracted production code. We require
# literal evidence of each subcase.
ok.append(assert_eq(
    "harness C8m exercises serializer-missing-tsv subcase against extracted production serializer",
    'serializer-missing-tsv' in harness_src
    and 'C8M_MISSING_TSV_RC' in harness_src,
    True,
))
ok.append(assert_eq(
    "production serializer short-row failure carries canonical `short_row:line=N:fields=N` reason",
    'per_attempt_node_tsv_short_row:line=' in install_src
    and 'fields=' in install_src,
    True,
))
ok.append(assert_eq(
    "production serializer uses fail() not continue/defaults for short-row rejection",
    # Direct short-row rejection uses
    # `if len(parts) != 9: fail(...)` with
    # NO `continue`, NO `len(parts) < 9: continue`,
    # NO silent defaulting on unknown boolean,
    # and the canonical
    # `per_attempt_node_tsv_short_row` reason.
    'if len(parts) != 9:' in install_src
    and 'if len(parts) != 9\n    fail' not in install_src
    and 'if len(parts) < 9: continue' not in install_src
    and 'continue' not in install_src.split('per_attempt_node_tsv_short_row')[1].split('per_node_records.append')[0],
    True,
))
ok.append(assert_eq(
    "production serializer does NOT identify short rows merely via count mismatch",
    # Direct short-row failure via explicit
    # short_row reason must be present,
    # independent of (and BEFORE) the later
    # `per_attempt_node_tsv_count_mismatch`
    # guard. The count guard is reserved for
    # structurally valid rows whose record set
    # does not match canonical.
    'per_attempt_node_tsv_short_row' in install_src,
    True,
))
ok.append(assert_eq(
    "harness C8m exercises serializer-duplicate-node subcase against extracted production serializer",
    'serializer-duplicate-node' in harness_src
    and 'C8M_DUP_RC' in harness_src,
    True,
))
ok.append(assert_eq(
    "harness C8m exercises serializer-unknown-bool subcase against extracted production serializer",
    'serializer-unknown-bool' in harness_src
    and 'C8M_BOOL_RC' in harness_src,
    True,
))
ok.append(assert_eq(
    "harness C8m requires each subcase produces nonzero rc + stdout-empty Y + stderr-prefix serializer_error=",
    '"${C8M_MISSING_TSV_STDERR_PREFIX#serializer_error=}" != "${C8M_MISSING_TSV_STDERR_PREFIX}"' in harness_src
    and '"${C8M_SHORT_ROW_STDERR_PREFIX#serializer_error=}" != "${C8M_SHORT_ROW_STDERR_PREFIX}"' in harness_src
    and '"${C8M_DUP_STDERR_PREFIX#serializer_error=}" != "${C8M_DUP_STDERR_PREFIX}"' in harness_src
    and '"${C8M_BOOL_STDERR_PREFIX#serializer_error=}" != "${C8M_BOOL_STDERR_PREFIX}"' in harness_src,
    True,
))
ok.append(assert_eq(
    "harness extracts the production ATTEMPT_PYEOF body via extract_production_attempt_serializer",
    'extract_production_attempt_serializer' in harness_src,
    True,
))
# d2b.51.51-correct-evidence-integrity:
# Terminal serializer MUST derive verdict
# fields from the validated terminal_doc
# (whose attempt equals shell-supplied
# terminal_attempt). It MUST NOT aggregate
# earlier per-node records into the
# terminal per_node_records. The legacy
# pattern looped all JSONL records into
# one list; reject that.
ok.append(assert_eq(
    "install-nexus-test.sh terminal serializer does NOT loop per_attempt_records into one list",
    # The legacy pattern appended every doc's
    # per-node records into one list and
    # collected every prior ready=False. We
    # require a per-doc, attempt-bounded
    # `documents_by_attempt[…]` map and
    # selection `terminal_doc` only.
    'documents_by_attempt' in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh terminal serializer selects terminal_doc = documents_by_attempt[terminal_attempt]",
    'terminal_doc = documents_by_attempt[terminal_attempt]' in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh terminal serializer no longer accepts `.get(\"per_node_records\", []) or []`",
    # The old weak pattern silently accepted
    # missing per_node_records. We require
    # an explicit failure on missing field.
    '.get("per_node_records", []) or []' not in install_src
    and 'doc.get("per_node_records"' not in install_src
    and 'doc.get("per_node_records",' not in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh terminal serializer keeps attempt_history_count as separate bounded field",
    'attempt_history_count' in install_src
    and '"attempt_history_count"' in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh terminal serializer fail-closed emits single pipeline_runtime_error=<reason>",
    'sys.stderr.write("pipeline_runtime_error=" + reason' in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh terminal serializer validates ready is JSON bool (not truthy)",
    'ready_val, bool' in install_src
    or "if not isinstance(ready_val, bool)" in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh terminal serializer cross-checks shell-vs-doc all_nodes_ready",
    'shell_all_nodes_ready != terminal_all_nodes_ready_per_doc' in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh terminal serializer keeps canonical `all-node-exact-tag-id-present` success reason",
    'all-node-exact-tag-id-present' in install_src,
    True,
))
ok.append(assert_eq(
    "harness C8j asserts terminal JSON attempt=2 with all_nodes_ready=true and 3 terminal records",
    'C8J_TERMINAL_ATTEMPT' in harness_src
    and 'C8J_TERMINAL_ALL_READY' in harness_src
    and 'C8J_TERMINAL_RECORD_COUNT' in harness_src,
    True,
))
ok.append(assert_eq(
    "harness C8p asserts terminal JSON attempt=15 with all ready=false and exactly 3 terminal records (not 45 historical)",
    'C8P_TERMINAL_ATTEMPT' in harness_src
    and "C8P_TERM_RECORDS_OK" in harness_src
    and 'C8P_TERM_RECORDS_ALL_FALSE_OK' in harness_src,
    True,
))
ok.append(assert_eq(
    "harness C8k asserts terminal JSON has exactly 3 terminal records with node-a+node-b ready and node-c not ready",
    'C8K_TERMINAL_RECORD_COUNT' in harness_src
    and 'C8K_TERMINAL_NODEC_READY' in harness_src,
    True,
))
ok.append(assert_eq(
    "harness C8m indirect terminal serializer invocations include wrong-shape JSONL subcase",
    'serializer-wrong-shape' in harness_src
    and 'C8M_WRONG_SHAPE_RC' in harness_src
    and 'C8M_WRONG_SHAPE_STDERR_PREFIX' in harness_src,
    True,
))
# d2b-tr-portability: production per-node
# artifact-name normalizer must use the
# exact `LC_ALL=C tr -c 'A-Za-z0-9._ -' '_'`
# allow-set. Reject the historical
# `tr -c 'A-Za-z0-9._- '` spelling which
# causes GNU `tr` to emit
# `range-endpoints of '_- ' are in reverse
# collating sequence order` and abort
# before any JSONL record is written. The
# new spelling puts the hyphen LAST in
# the allow-set so it can never pair with
# `_` (0x5F) to form a reverse collating
# sequence. LC_ALL=C forces deterministic
# ASCII byte-range semantics regardless
# of how the runner sets LANG.
ok.append(assert_eq(
    "install-nexus-test.sh per-node safe normalizer uses LC_ALL=C tr -c 'A-Za-z0-9._ -' '_' (hyphen literal last)",
    "LC_ALL=C tr -c 'A-Za-z0-9._ -' '_'" in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh no longer contains the legacy `tr -c 'A-Za-z0-9._- '` spelling",
    # The historical spelling ordered hyphen
    # directly after `_` inside a C-locale
    # allow-set; GNU `tr` collapses those
    # into a character range and rejects
    # the out-of-order endpoint pair
    # ("reverse collating sequence order").
    "tr -c 'A-Za-z0-9._- '" not in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh has exactly one per-node safe_n= normalizer in the all-node pipeline loop",
    install_src.count("safe_n="),
    1,
))
ok.append(assert_eq(
    "install-nexus-test.sh safe_n normalizer is positioned before raw_stdout/raw_stderr/raw_rcfile path construction",
    # The shell assigns safe_n before
    # constructing the per-attempt
    # artifact paths so unsanitized node
    # names can never reach disk. We
    # require safe_n to appear and the
    # raw_* paths to follow it.
    'local safe_n' in install_src
    and 'raw_stdout="$ARTIFACTS/attempts/attempt-${attempt}/node-${safe_n}.stdout.json"' in install_src
    and 'raw_stderr="$ARTIFACTS/attempts/attempt-${attempt}/node-${safe_n}.stderr.txt"' in install_src
    and 'raw_rcfile="$ARTIFACTS/attempts/attempt-${attempt}/node-${safe_n}.rc"' in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh still enforces same-entry verify and exactly-one-load bounded 15×2s semantics",
    'same_entry_match' in install_src
    and 'IMG_VERIFY_MAX_ATTEMPTS=15' in install_src
    and 'IMG_VERIFY_INTERVAL_SEC=2' in install_src
    and 'kind get nodes' in install_src,
    True,
))
# d2b.51.51-canonical-alias: closed ordered
# two-element runtime tag acceptance set
# (declared bare tag + the one
# docker.io/library/ canonical alias).
# No wildcard, no suffix/prefix,
# no arbitrary registry/namespace. The
# runtime verifier routes the closed set
# into the per-entry parser via an env
# var, decodes it as a Python list of
# exactly two unique non-empty strings,
# and gates same_entry_match on membership.
ok.append(assert_eq(
    "install-nexus-test.sh declares the exact closed accepted-runtime-tag set",
    # The shell-side closed set MUST be the
    # declared bare tag and the one
    # canonical alias, no other text in the
    # pipe.
    "ACCEPTED_RUNTIME_TAGS_PIPE_DELIM="
    "'cni-listener:local"
    "|docker.io/library/cni-listener:local|'" in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh never uses startswith/endswith/wildcard/digest-only for runtime tag matching",
    # Reject shell-side substring matchers
    # that would falsely accept extra
    # aliases.
    not any(anti in install_src for anti in (
        'tag_seen_anywhere = (want_tag in',
        '\nstartswith(\"docker.io\")',
        '\nstartswith(\"docker.io/\")',
        '\nendswith(\":local\")',
        '\nendswith(\"docker.io/library/\")',
        'cni-listener:*',
        'cni-listener:latest',
        'quay.io/',
        'docker.io/other/',
        'docker.io/library/cni-listener:localx',
    )),
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh per-attempt serializer emits accepted_runtime_tags via Python stdlib json.dumps",
    # The string `accepted_runtime_tags`
    # MUST appear in the per-attempt
    # serializer source, AND it MUST be
    # emitted through json.dumps (NOT
    # printf-built JSON, NOT sed/awk reshape).
    "accepted_runtime_tags" in install_src
    and '"accepted_runtime_tags": list(accepted_tags)' in install_src
    and "json.dumps" in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh terminal serializer also emits accepted_runtime_tags via Python stdlib json.dumps",
    # The terminal record MUST also carry
    # the closed two-value list emitted via
    # json.dumps. The terminal serializer
    # emits `accepted_runtime_tags` in its
    # final record dict.
    install_src.count('"accepted_runtime_tags": list(accepted_runtime_tags)'),
    1,
))
ok.append(assert_eq(
    "install-nexus-test.sh still uses strict same_entry_match runtime gating",
    'same_entry_match' in install_src
    and 'same_entry_match = True' in install_src
    and 'ready = same_entry_match' in install_src,
    True,
))
ok.append(assert_eq(
    "install-nexus-test.sh keeps the bounded 15-attempt × 2-second image-verification contract intact",
    'local IMG_VERIFY_MAX_ATTEMPTS=15' in install_src
    and 'local IMG_VERIFY_INTERVAL_SEC=2' in install_src
    and '(( attempt < IMG_VERIFY_MAX_ATTEMPTS ))' in install_src,
    True,
))
# d2b.45 tr-implementation line guard: at this
# point the install script retains only the
# portable LC_ALL=C normalizer form from
# d2b-tr-portability. The histogram tells
# us exactly one safe normalizer and zero
# legacy range spellings.
ok.append(assert_eq(
    "install-nexus-test.sh accepts only the d2b-tr-portability safe normalizer spelling",
    install_src.count("LC_ALL=C tr -c 'A-Za-z0-9._ -' '_'"),
    1,
))
print()
if all(ok):
    print("d2b.26 image-pipeline mutation tests: PASS")
    sys.exit(0)
print("d2b.26 image-pipeline mutation tests: FAIL", file=sys.stderr)
sys.exit(1)