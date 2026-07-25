# syntax=docker/dockerfile:1
# Image for the packaged local stack and the E18 release matrix (doctor labels a local build
# "locally-built"). Build context is the repo root (see compose.yaml).
#
# Bases are pinned by DIGEST (E18 T1, design invariant §2 "digest everywhere, mutable tag
# nowhere"): the tag is kept for humans, the digest is what the build resolves, and a tag move
# cannot change a pinned build. Both digests are the multi-arch INDEX digest, so amd64 and arm64
# resolve from the same pin. scripts/release/hermetic_test.go rejects any unpinned FROM and any
# golang tag that drifts from go.mod's `toolchain` directive.
#
# The build stage runs on the BUILD platform and CROSS-compiles to $TARGETARCH: the Go compiler
# never runs under qemu, so an amd64 image builds at native speed on an arm64 host (and the final
# stage runs no commands at all — see COPY --chmod below).
FROM --platform=$BUILDPLATFORM golang:1.26.4@sha256:f96cc555eb8db430159a3aa6797cd5bae561945b7b0fe7d0e284c63a3b291609 AS build
WORKDIR /src
COPY go.mod go.sum ./
# The ONLY step allowed to reach the module proxy. Inputs are pinned by go.sum, and the compile
# step below runs with GOPROXY=off against this warmed cache — so a missing/changed module fails
# the build instead of being fetched. (Vendoring is the other lever; a warmed cache keeps the tree
# free of a vendor/ copy.)
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
# The release version stamp (E15 T2): scripts/release/build.sh passes it as a build arg so the image's
# binary reports the git-describe stamp for the §48.2 support window + schema_revisions.applied_by. Empty
# (a plain `local up` build) leaves it unstamped, so version.Resolve falls back to VCS/"dev" — unchanged.
ARG PALAI_VERSION_STAMP=""
ARG TARGETARCH
# CGO off => a fully static binary that runs on the musl-based alpine runtime.
# Reproducibility (binary level): -trimpath removes the build path, -buildvcs=false keeps the git
# state out, and -buildid= zeroes the toolchain build id, so the same commit + same toolchain +
# same stamp yields a BIT-IDENTICAL binary. GOPROXY=off + GOFLAGS=-mod=readonly make the compile
# hermetic against the cache warmed above.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH="$TARGETARCH" GOPROXY=off GOFLAGS=-mod=readonly \
    go build -trimpath -buildvcs=false \
      -ldflags "-s -w -buildid= -X github.com/palgroup/palai/packages/version.Stamp=${PALAI_VERSION_STAMP}" \
      -o /out/palai-control-plane ./apps/control-plane/cmd/palai-control-plane

FROM alpine:3.21@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d
# No USER: compose bind-mounts file-secrets with the host's 0600 perms and ignores the
# secret uid/gid/mode (Context7 /docker/compose), so only root can read /run/secrets/*
# and the mounted .palai CA. The control-plane is trusted infrastructure; the untrusted
# engine is isolated separately by the Task 8 OCI driver.
# COPY --chmod instead of a RUN chmod: the final stage is the TARGET platform, so any RUN here
# would execute under qemu when cross-building. With no RUN the amd64 image needs no emulation at
# all (and the layer is one fewer).
COPY --chmod=0755 deploy/compose/control-plane-entrypoint.sh /usr/local/bin/entrypoint.sh
# The production posture guard (E14 T1). Baked into the SAME image; the local profile keeps
# the default ENTRYPOINT below, and production.yml overrides `entrypoint:` to run this first.
COPY --chmod=0755 deploy/compose/production-entrypoint.sh /usr/local/bin/production-entrypoint.sh
COPY --chmod=0755 --from=build /out/palai-control-plane /usr/local/bin/palai-control-plane
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
