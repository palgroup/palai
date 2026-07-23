#!/usr/bin/env bash
# scripts/release/build.sh — E15 T2 release builder.
#
# Stamps a single git-describe version into the control-plane, runner, and CLI via
# -ldflags -X packages/version.Stamp, and emits a release manifest (image digests + versions) the
# `palai upgrade` compat check reads. "Same binaries, different version stamp" (plan §2): the stamp is
# build metadata only — it never forks behaviour.
#
# The stamp is <VERSION>+g<git-describe>: VERSION (repo root, or --version / PALAI_VERSION) gives the
# semantic major.minor.patch the §48.2 support window compares; the git-describe suffix makes each build
# a distinct id (so N and N+1 from two different trees carry distinct stamps within the same minor).
#
# Usage:
#   scripts/release/build.sh --tag <suffix> --out <dir> [--version <v>] [--no-images]
#     --tag      image tag suffix; produces palai/{control-plane,runner,reference-engine}:<suffix>
#     --out      directory the manifest + stamped CLI binary are written to
#     --version  override the VERSION file (else PALAI_VERSION env, else ./VERSION)
#     --no-images  build only the CLI binary + a binary-only manifest (fast, Docker-free)
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$repo_root"

tag=""
out=""
version_override="${PALAI_VERSION:-}"
build_images=1

while [ $# -gt 0 ]; do
  case "$1" in
    --tag) tag="$2"; shift 2;;
    --out) out="$2"; shift 2;;
    --version) version_override="$2"; shift 2;;
    --no-images) build_images=0; shift;;
    *) echo "build.sh: unknown argument $1" >&2; exit 2;;
  esac
done

if [ -z "$out" ]; then
  echo "build.sh: --out <dir> is required" >&2
  exit 2
fi
if [ "$build_images" -eq 1 ] && [ -z "$tag" ]; then
  echo "build.sh: --tag <suffix> is required unless --no-images" >&2
  exit 2
fi

version="$version_override"
if [ -z "$version" ]; then
  if [ ! -f VERSION ]; then
    echo "build.sh: no VERSION file and no --version/PALAI_VERSION" >&2
    exit 2
  fi
  version="$(tr -d '[:space:]' < VERSION)"
fi

describe="$(git describe --tags --always --dirty 2>/dev/null || echo unknown)"
commit="$(git rev-parse HEAD 2>/dev/null || echo unknown)"
stamp="${version}+g${describe}"
built_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

mkdir -p "$out"

# The CLI carries the stamp too (the operator's `palai` binary reports its version).
echo "build.sh: stamping CLI at $stamp" >&2
CGO_ENABLED=0 go build -ldflags "-X github.com/palgroup/palai/packages/version.Stamp=${stamp}" \
  -o "$out/palai" ./cmd/cli

cp_image=""
runner_image=""
engine_image=""

if [ "$build_images" -eq 1 ]; then
  cp_image="palai/control-plane:${tag}"
  runner_image="palai/runner:${tag}"
  engine_image="palai/reference-engine:${tag}"

  echo "build.sh: building $cp_image" >&2
  docker build --build-arg "PALAI_VERSION_STAMP=${stamp}" \
    -f deploy/compose/control-plane.Dockerfile -t "$cp_image" .
  echo "build.sh: building $runner_image" >&2
  docker build --build-arg "PALAI_VERSION_STAMP=${stamp}" \
    -f deploy/compose/runner.Dockerfile -t "$runner_image" .
  echo "build.sh: building $engine_image" >&2
  docker build -t "$engine_image" engines/reference

  cp_digest="$(docker image inspect "$cp_image" --format '{{.Id}}')"
  runner_digest="$(docker image inspect "$runner_image" --format '{{.Id}}')"
  engine_digest="$(docker image inspect "$engine_image" --format '{{.Id}}')"
else
  cp_digest=""
  runner_digest=""
  engine_digest=""
fi

manifest="$out/release-manifest.json"
cat > "$manifest" <<EOF
{
  "version": "${version}",
  "stamp": "${stamp}",
  "commit": "${commit}",
  "built_at": "${built_at}",
  "images": {
    "control_plane": { "ref": "${cp_image}", "digest": "${cp_digest}" },
    "runner":        { "ref": "${runner_image}", "digest": "${runner_digest}" },
    "engine":        { "ref": "${engine_image}", "digest": "${engine_digest}" }
  }
}
EOF

echo "build.sh: wrote $manifest (version $version, stamp $stamp)" >&2
