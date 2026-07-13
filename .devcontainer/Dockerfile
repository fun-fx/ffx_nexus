# syntax=docker/dockerfile:1.7
# Dev container for ffx_nexus.
# - Go 1.26 (matches go.mod)
# - Docker-in-Docker socket so docker compose can be run from inside
# - Compose CLI already provided by docker-in-docker feature
# - Preinstalled CLI tools: curl, jq, gh (best-effort)
ARG GO_VERSION=1.26.3

FROM golang:${GO_VERSION}-bookworm

ARG DEBIAN_FRONTEND=noninteractive

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        dnsutils \
        git \
        gnupg \
        iproute2 \
        jq \
        lsb-release \
        make \
        netcat-openbsd \
        openssh-client \
        postgresql-client \
        redis-tools \
        ripgrep \
        sqlite3 \
        unzip \
        vim-tiny \
        xz-utils; \
    install -m 0755 -d /etc/apt/keyrings; \
    curl -fsSL https://download.docker.com/linux/debian/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg; \
    chmod a+r /etc/apt/keyrings/docker.gpg; \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/debian bookworm stable" > /etc/apt/sources.list.d/docker.list; \
    apt-get update; \
    apt-get install -y --no-install-recommends docker-ce-cli docker-compose-plugin; \
    rm -rf /var/lib/apt/lists/*

WORKDIR /workspace

# Pre-warm go module download cache layer for the most common deps.
# (Real deps still fetch on first build via bind mount; this just primes the
# cache volume for faster rebuilds.)
COPY go.mod go.sum* ./
RUN go mod download || true

COPY .devcontainer/post-create.sh /usr/local/bin/post-create.sh
RUN chmod +x /usr/local/bin/post-create.sh

LABEL org.opencontainers.image.title="ffx_nexus dev" \
      org.opencontainers.image.source="https://github.com/fun-fx/ffx_nexus" \
      devcontainer.metadata="{\"dockerComposeFile\": \"../deploy/docker-compose.yml\"}"

CMD ["sleep", "infinity"]
