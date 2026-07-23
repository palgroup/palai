# syntax=docker/dockerfile:1
# Locally-built dev image for the packaged local stack (doctor labels it "locally-built";
# release digest pinning is E18). Build context is the repo root (see compose.yaml).
FROM golang:1.26.4 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
# CGO off => a fully static binary that runs on the musl-based alpine runtime.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -o /out/palai-control-plane ./apps/control-plane/cmd/palai-control-plane

FROM alpine:3.21
# No USER: compose bind-mounts file-secrets with the host's 0600 perms and ignores the
# secret uid/gid/mode (Context7 /docker/compose), so only root can read /run/secrets/*
# and the mounted .palai CA. The control-plane is trusted infrastructure; the untrusted
# engine is isolated separately by the Task 8 OCI driver.
COPY deploy/compose/control-plane-entrypoint.sh /usr/local/bin/entrypoint.sh
# The production posture guard (E14 T1). Baked into the SAME image; the local profile keeps
# the default ENTRYPOINT below, and production.yml overrides `entrypoint:` to run this first.
COPY deploy/compose/production-entrypoint.sh /usr/local/bin/production-entrypoint.sh
COPY --from=build /out/palai-control-plane /usr/local/bin/palai-control-plane
RUN chmod +x /usr/local/bin/entrypoint.sh /usr/local/bin/production-entrypoint.sh /usr/local/bin/palai-control-plane
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
