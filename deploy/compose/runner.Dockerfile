# syntax=docker/dockerfile:1
# Locally-built dev image for the private runner (doctor labels it "locally-built").
# Build context is the repo root (see compose.yaml).
FROM golang:1.26.4 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -o /out/palai-runner ./cmd/runner

FROM alpine:3.21
# No USER: the runner reads its 0600 CA cert and one-use token from host bind-mounts, and
# drives the Docker socket to launch the hardened engine sandbox (Task 8). Root is
# required for the socket; the engine workload itself receives none of this.
COPY deploy/compose/runner-entrypoint.sh /usr/local/bin/entrypoint.sh
COPY --from=build /out/palai-runner /usr/local/bin/palai-runner
RUN chmod +x /usr/local/bin/entrypoint.sh /usr/local/bin/palai-runner
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
