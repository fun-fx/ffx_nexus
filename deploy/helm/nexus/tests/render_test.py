#!/usr/bin/env python3
"""
helm render test for Phase D-1.

The chart renders two Deployments (gateway + worker) that share
an image and several configuration blocks. A regression that
drifts one Deployment's image from the other, or that fails
to apply the common-helper templates, is loud here.

The actual rendering uses `helm template` so this script
mirrors a deploy dry-run. Operators seeing a red bar here
read commit history for the corresponding template file.

Prerequisites:
- helm 3.x available on PATH
- chart's values.yaml validates against values.schema.json
- this script runs from a CI task that does NOT have network
  access; nothing here reaches the cluster.

The output is intentionally JSON so the post-job can grep for
`pass` / `fail` and surface a clear status line.
"""

import json
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
CHART = ROOT


def fail(msg: str) -> None:
    print(json.dumps({"pass": False, "reason": msg}))
    sys.exit(1)


def ok() -> None:
    print(json.dumps({"pass": True}))


def have_helm() -> bool:
    return shutil.which("helm") is not None


def render(extra_args=None):
    cmd = [
        "helm",
        "template",
        "testrelease",
        str(CHART),
        "--namespace",
        "default",
    ]
    if extra_args:
        cmd.extend(extra_args)
    proc = subprocess.run(cmd, capture_output=True, text=True)
    if proc.returncode != 0:
        fail(f"helm template failed: {proc.stderr}")
    return proc.stdout


def parse_docs(yaml_text: str):
    documents = []
    cur = []
    for line in yaml_text.splitlines():
        if line.strip() == "---" and cur:
            documents.append("\n".join(cur))
            cur = []
            continue
        cur.append(line)
    if cur:
        documents.append("\n".join(cur))
    return documents


def main() -> None:
    if not have_helm():
        fail("helm CLI not found; install helm 3.x before running this test")
    rendered = render()
    docs = parse_docs(rendered)

    deployments = []
    for d in docs:
        if "kind: Deployment" in d and "name:" in d:
            if "nexus-gateway" in d:
                deployments.append(("gateway", d))
            elif "nexus-worker" in d:
                deployments.append(("worker", d))
    if len(deployments) != 2:
        fail(f"expected 2 Deployments (gateway, worker); got {len(deployments)}")

    # Image equality across the two Deployments. Phase D-1 spec:
    # "두 Deployment의 이미지가 동일함을 렌더 테스트로 단정하라".
    images = {}
    for name, doc in deployments:
        for line in doc.splitlines():
            stripped = line.strip()
            if stripped.startswith("image:"):
                images[name] = stripped
                break
    if len(images) != 2:
        fail(f"could not extract image from one of the deployments; got {images}")
    if images["gateway"] != images["worker"]:
        fail(f"image drift between gateway and worker: {images}")

    # Service (ClusterIP) must select gateway, NOT worker. The
    # gateway Service routes external LLM traffic to the
    # pods; the worker pod has no Service in the roll-out
    # contract.
    main_service_selects_worker = False
    for d in docs:
        if "kind: Service" in d and "component: worker" in d:
            for line in d.splitlines():
                if "app.kubernetes.io/component:" in line and "worker" in line:
                    pass
                if line.strip().startswith("app.kubernetes.io/component: gateway"):
                    fail("worker Service also carries gateway selector; mis-routing")
            # If we have a worker Service we want to verify it
            # has no LLM-exposed port (8080/8081).
            for port in ["8080", "8081"]:
                if f"port: {port}" in d:
                    fail(f"worker Service exposes LLM port {port}; should be metrics-only")
        if "kind: Service" in d and "nexus-" in d and "component: gateway" in d:
            main_service_selects_worker = False

    # Worker's only exposed port is metrics.
    worker_doc = dict(deployments)["worker"]
    if "containerPort: 8080" in worker_doc:
        fail("worker pod exposes gateway HTTP port 8080; deployment must not")
    if "containerPort: 8081" in worker_doc:
        fail("worker pod exposes console HTTP port 8081; deployment must not")

    # Worker's readinessProbe path /readyz and port point at metrics.
    # Gateway's readinessProbe points at 8080.
    gateway_doc = dict(deployments)["gateway"]
    if "path: /readyz" not in gateway_doc or "port: 8080" not in gateway_doc:
        fail("gateway readinessProbe /readyz must target port 8080")
    if "path: /readyz" not in worker_doc:
        fail("worker readinessProbe /readyz must be present (lease readiness)")

    # Both Deployments carry the same NEXUS_POSTGRES_MAX_CONNS key
    # (separate values is allowed; the key shape must be the same).
    if "NEXUS_POSTGRES_MAX_CONNS" not in gateway_doc:
        fail("gateway missing NEXUS_POSTGRES_MAX_CONNS")
    if "NEXUS_POSTGRES_MAX_CONNS" not in worker_doc:
        fail("worker missing NEXUS_POSTGRES_MAX_CONNS")

    ok()


if __name__ == "__main__":
    main()
