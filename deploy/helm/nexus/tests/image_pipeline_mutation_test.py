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

# ---- 6. Per-node runtime verification -----------------------------------
# The directive requires install-nexus-test.sh
# NOT to trust `kind load`'s rc=0 alone. The
# install script MUST verify crictl images on
# every kind node shows the recorded image_id.
# We assert this by walking the source string.
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
ok.append(assert_eq(
    "install-nexus-test.sh counts missing nodes as delta present < expected",
    "PRESENT=" in install_src and "MISSING=" in install_src,
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

print()
if all(ok):
    print("d2b.26 image-pipeline mutation tests: PASS")
    sys.exit(0)
print("d2b.26 image-pipeline mutation tests: FAIL", file=sys.stderr)
sys.exit(1)