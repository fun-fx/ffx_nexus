# scripts/fixtures/integrationcni/control-netpol-gate.Dockerfile
#
# Build context: scripts/fixtures/integrationcni/
# (the directory this Dockerfile lives in)
#
# Source: cmd/cni-listener/main.go
#
# Why one image, not many:
#
#   The fixture used to pull five distinct
#   external images (ghcr.io/fun-fx/nexus-fixture:
#   httpecho, metrics, probe, gateway, worker).
#   Those tags were not pinned by digest; a pull
#   failure or a silent registry-side rewrite can
#   be misattributed to a chart-side regression.
#
#   This Dockerfile produces a single static
#   binary `cni-listener` that:
#
#     - listens on multiple TCP ports (-ports
#       flag, comma-separated)
#     - replies to /healthz and / with HTTP 200
#       body {"ok":true,"listen":"<addr>","target":"<pod>"}
#     - emits /readyz<port> as HTTP 200 when the
#       listening port can accept() a SYN
#     - writes a deterministic bootstrap line to
#       stderr so the readiness probe can read it
#       without HTTP. The pod's readinessProbe
#       uses an exec probe (`/cni-listener -probe
#       <port>`) which exits 0 only when the
#       listener accepts SYN locally.
#
#   The fixture references the image by
#   `@sha256:<digest>` so the registry cannot
#   substitute an updated tag underfoot.
#   `scripts/fixtures/integrationcni/build.sh`
#   computes that digest and is the only path
#   allowed to regenerate the fixture yamls.
#
# Why FROM scratch and not busybox/alpine:
#
#   busybox's /readyz does not exist; we already
#   learned that the league-breaking-run failure
#   was caused by curl on a busybox endpoint that
#   the busybox image does not serve. The fix is
#   not "stop checking" but to ship a binary that
#   defines /readyz itself, written in Go so the
#   binary is a single static binary -x and serves
#   exactly the probes the gate needs.
#
# Build contract:
#
#   GOOS=linux go build -trimpath -ldflags='-s -w' \
#     -o cni-listener ./cmd/cni-listener
#
# Output:
#   - /cni-listener binary, <5 MiB
FROM golang:1.26-alpine AS build
WORKDIR /src
# The build context is
# `scripts/fixtures/integrationcni/`. Bring the
# `cmd/` subtree directly and synthesize a
# go.mod at the WORKDIR root so `go build
# ./cmd/cni-listener` resolves the cmd
# package against the synthesized module root.
COPY cmd/ ./cmd/
RUN printf 'module github.com/fun-fx/ffx_nexus/scripts/fixtures/integrationcni/cmd\n\ngo 1.26\n' > ./go.mod \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags='-s -w' -o /out/cni-listener ./cmd/cni-listener
FROM scratch AS runtime
COPY --from=build /out/cni-listener /cni-listener
# Phase D-2b.29 hardened runtime contract. The
# runtime image is `scratch` so it has no /etc/passwd.
# USER 65534:65534 keeps the kubelet happy on the
# runtime UID and matches the Pod runAsUser 65534
# used in 03/04-control Pod fixtures. The cni-listener
# operand only binds non-privileged ports and does not
# write to disk in any code path the fixture covers,
# so non-root execution is the only contract.
USER 65534:65534
ENTRYPOINT ["/cni-listener"]
