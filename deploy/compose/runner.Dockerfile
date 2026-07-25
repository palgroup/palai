# syntax=docker/dockerfile:1
# Image for the private runner (doctor labels a local build "locally-built"). Build context is the
# repo root (see compose.yaml). Digest-pinned bases, cross-compiling build stage, hermetic compile:
# see deploy/compose/control-plane.Dockerfile for the rationale of each (same recipe, one binary).
FROM --platform=$BUILDPLATFORM golang:1.26.4@sha256:f96cc555eb8db430159a3aa6797cd5bae561945b7b0fe7d0e284c63a3b291609 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
# The release version stamp (E15 T2): build.sh passes it so the runner advertises its git-describe stamp
# in the enroll handshake for the §48.2 window (OPS-008). Empty leaves it unstamped (VCS/"dev") — a plain
# `local up` build is unchanged. PALAI_VERSION env at run time still overrides this (drills/ops pinning).
ARG PALAI_VERSION_STAMP=""
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH="$TARGETARCH" GOPROXY=off GOFLAGS=-mod=readonly \
    go build -trimpath -buildvcs=false \
      -ldflags "-s -w -buildid= -X github.com/palgroup/palai/packages/version.Stamp=${PALAI_VERSION_STAMP}" \
      -o /out/palai-runner ./cmd/runner

FROM alpine:3.21@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d
# No USER: the runner reads its 0600 CA cert and one-use token from host bind-mounts, and
# drives the Docker socket to launch the hardened engine sandbox (Task 8). Root is
# required for the socket; the engine workload itself receives none of this.
COPY --chmod=0755 deploy/compose/runner-entrypoint.sh /usr/local/bin/entrypoint.sh
COPY --chmod=0755 --from=build /out/palai-runner /usr/local/bin/palai-runner
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
