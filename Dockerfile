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
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/nexus \
    ./cmd/nexus

# --- Runtime stage ---
FROM alpine:3.20
# ca-certificates.crt bundle is needed by Go's TLS stack (Keycloak OIDC
# discovery, etc.). alpine 3.20's ca-certificates package no longer runs
# update-ca-certificates as an install hook, so we call it explicitly
# to populate the bundle that the runtime links against.
RUN apk add --no-cache ca-certificates tzdata \
    && update-ca-certificates \
    && adduser -D -H -u 65532 nexus
COPY --from=build /out/nexus /usr/local/bin/nexus

USER nexus
EXPOSE 8080 8081

# Gateway :8080, console :8081. Configure via NEXUS_* env vars.
ENTRYPOINT ["nexus"]
