# syntax=docker/dockerfile:1

# --- Web build stage: compile the dashboard SPA so it can be embedded ---
FROM node:20-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# --- Go build stage ---
FROM golang:1.26-alpine AS build
RUN apk add --no-cache ca-certificates git
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Overlay the freshly built dashboard assets for the go:embed in web/embed.go.
COPY --from=web /web/dist ./web/dist
# Inject the source commit into the binary so the X-Nexus-Build
# header on every response tells the operator where their binary
# came from. SOURCE_COMMIT is wired by the CD pipeline; the ARG has
# a fallback ("dev") so a local `docker build .` still produces a
# usable binary.
ARG SOURCE_COMMIT=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.nexusBuildTag=${SOURCE_COMMIT}" \
    -o /out/nexus \
    ./cmd/nexus

# --- Runtime stage ---
FROM alpine:3.20
# ca-certificates.crt bundle is needed by Go's TLS stack (Keycloak OIDC
# discovery, etc.). alpine 3.20's ca-certificates package no longer runs
# update-ca-certificates as an install hook, so we call it explicitly
# to populate the bundle that the runtime links against.
#
# postgresql16 is here for `serve --local` (the CMD below). Without it the
# gateway would download a Postgres runtime on first start, which turns
# `docker run` into something that needs the network for a database and fails
# on an air-gapped host. Alpine puts the server binaries in
# /usr/libexec/postgresql16; internal/localdb expects a directory with a bin/
# subdirectory, hence the symlink. The version must match the pgVersion
# constant in internal/localdb, or the runner re-runs initdb on a data
# directory it thinks belongs to another major.
#
# A pod never runs any of this: the Helm chart pins args to ["serve"].
RUN apk add --no-cache ca-certificates tzdata postgresql16 \
    && update-ca-certificates \
    && adduser -D -H -u 65532 nexus \
    && mkdir -p /opt/postgres \
    && ln -s /usr/libexec/postgresql16 /opt/postgres/bin
COPY --from=build /out/nexus /usr/local/bin/nexus

# Bundled documentation tree. The console serves everything under
# /etc/nexus/docs verbatim so the operator sees the same markdown
# that the binary was built against. A Helm overlay at /etc/nexus/docs
# is the supported way to extend or replace this tree at deploy time
# without rebuilding the image.
COPY --from=build /src/docs /etc/nexus/docs
# Files inside /etc/nexus/docs must remain world-readable; the
# runtime CDK drops privileges to UID 65532 (nexus) with no group
# write access, so a more restrictive mode here would silent-fail
# the walk() at boot and present an empty /api/docs response.
RUN chmod -R a+rX /etc/nexus/docs

# State for `serve --local`: the Postgres data directory and the generated
# master key. One `-v $(pwd)/data:/app/data` keeps a console's configuration
# across container replacements; without it the credentials the user entered
# go away with the container.
RUN mkdir -p /app/data && chown -R 65532:65532 /app
ENV NEXUS_LOCAL_STATE_DIR=/app/data \
    NEXUS_LOCAL_DB_BINARIES=/opt/postgres \
    NEXUS_DOCS_DIR=/etc/nexus/docs

USER nexus
WORKDIR /app
EXPOSE 8080 8081

# Gateway :8080, console :8081. Configure via NEXUS_* env vars.
#
# The default CMD runs a container-local Postgres so `docker run` reaches a
# working console rather than one that answers 503 to every write. Anything
# with an external database overrides it — the Helm chart pins args to
# ["serve"], and `docker run ... nexus serve` does the same by hand.
ENTRYPOINT ["nexus"]
CMD ["serve", "--local"]
