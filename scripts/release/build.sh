#!/usr/bin/env bash
# scripts/release/build.sh — E15 T2 release builder, extended by E18 T1 into the release MATRIX.
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
# E18 T1 adds:
#   * the MATRIX — images for linux/amd64 + linux/arm64 (buildx, cross-compiled in-Dockerfile so no
#     qemu is needed), the CLI for darwin/linux × amd64/arm64, and the runner host package per arch;
#   * release-index.json — every artifact with its RECOMPUTED sha256 digest, arch and kind, plus
#     sbom/provenance fields that are DEFINED but EMPTY (T2 and T3 fill them);
#   * BINARY-level reproducibility — from -trimpath -buildvcs=false -ldflags "-s -w -buildid=" (a Go
#     binary embeds no build timestamp) plus the E14 T5 packager's FIXED tar mtime, so two runs from the
#     same commit yield BIT-IDENTICAL binaries and host packages. What SOURCE_DATE_EPOCH (default: the
#     commit timestamp, so it is a function of the commit, not of the clock) buys is a stable built_at,
#     which is what keeps the INDEX byte-stable across those runs — not the binaries.
#     (scripts/release/repro_test.go asserts both by re-running this script twice.)
#
# HONEST CEILINGS (design invariant §2 "honest naming"):
#   * Reproducibility is BINARY-level. Image-LAYER reproducibility is NOT claimed: `docker save`
#     tars carry layer/config timestamps, so two image builds are equivalent, not byte-identical.
#     That is why the index's `reproducibility` block names both halves explicitly.
#   * Hermeticity is "offline compile against a warmed, go.sum-pinned cache": `go mod download` is
#     the ONLY step allowed to reach the module proxy, and EVERY compile runs GOPROXY=off — the two
#     in-Dockerfile stages AND the host legs (CLI matrix + runner host packages), so the claim holds
#     for all 12 artifacts and not just the images (scripts/release/hermetic_test.go proves the host
#     legs REFUSE to dial, by running this script with a COLD module cache). A fully network-free
#     build from a cold cache is the operator/CI leg (plan §6 leg 1).
#   * Every digest in the index is RECOMPUTED from the artifact's bytes (§2 recompute-over-copy) —
#     nothing is scraped from a build log. For images that is the sha256 of the shipped tar, and the
#     image id is read out of the tar's own manifest.json, not from `docker image inspect`.
#   * A qemu boot-smoke of the amd64 image is NOT a substitute for a full UAT run on amd64 — that
#     run is plan §6 leg 3.
#
# Usage:
#   scripts/release/build.sh --tag <suffix> --out <dir> [--version <v>] [--no-images]
#                            [--platforms <list>] [--cli-targets <list>] [--runner-archs <list>]
#     --tag           image tag suffix; produces palai/{control-plane,runner,reference-engine}:<suffix>
#                     for the host platform and :<suffix>-<arch> for the others
#     --out           directory the manifest, index, binaries, packages and image tars are written to
#     --version       override the VERSION file (else PALAI_VERSION env, else ./VERSION)
#     --no-images     build only binaries + host packages (fast, Docker-free)
#     --platforms     image platforms (default linux/amd64,linux/arm64)
#     --cli-targets   CLI <os>/<arch> pairs (default darwin/amd64,darwin/arm64,linux/amd64,linux/arm64)
#     --runner-archs  runner host package arches (default amd64,arm64)
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$repo_root"

tag=""
out=""
version_override="${PALAI_VERSION:-}"
build_images=1
platforms="linux/amd64,linux/arm64"
cli_targets="darwin/amd64,darwin/arm64,linux/amd64,linux/arm64"
runner_archs="amd64,arm64"

# invocation_args records THIS invocation for the release index, which is what E18 T3's provenance
# attestation names as the build command's parameters. --out is deliberately NOT recorded: it is a
# destination, not a build input, and recording it would make the index differ between two runs that
# built the identical release into different directories.
invocation_args=""
record_arg() { # json-escape and append
  local esc
  esc="$(printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g')"
  invocation_args="${invocation_args:+$invocation_args, }\"$esc\""
}

while [ $# -gt 0 ]; do
  case "$1" in
    --tag) tag="$2"; record_arg "$1"; record_arg "$2"; shift 2;;
    --out) out="$2"; shift 2;;
    --version) version_override="$2"; record_arg "$1"; record_arg "$2"; shift 2;;
    --no-images) build_images=0; record_arg "$1"; shift;;
    --platforms) platforms="$2"; record_arg "$1"; record_arg "$2"; shift 2;;
    --cli-targets) cli_targets="$2"; record_arg "$1"; record_arg "$2"; shift 2;;
    --runner-archs) runner_archs="$2"; record_arg "$1"; record_arg "$2"; shift 2;;
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

# SOURCE_DATE_EPOCH defaults to the COMMIT time, never `now`: the build's timestamps are then a
# function of the source, which is what makes two runs of this script comparable. An operator can
# override it, and the index records the value that was used.
SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct 2>/dev/null || echo 0)}"
export SOURCE_DATE_EPOCH
# BSD date wants -r, GNU date wants -d @; try both (this script runs on macOS and on CI linux).
built_at="$(date -u -r "$SOURCE_DATE_EPOCH" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
  || date -u -d "@$SOURCE_DATE_EPOCH" +%Y-%m-%dT%H:%M:%SZ)"

host_os="$(go env GOOS)"
host_arch="$(go env GOARCH)"

# A cross-ONLY image matrix cannot produce release-manifest.json: that file (the E15 T2 upgrade
# contract) names HOST-platform refs + daemon image ids, and a foreign-arch-only build has none.
# Refuse HERE rather than after paying for three image builds and then dying at `docker image
# inspect ""` with neither the manifest nor the index written.
if [ "$build_images" -eq 1 ] && ! echo ",$platforms," | grep -q ",linux/${host_arch},"; then
  echo "build.sh: --platforms '$platforms' omits the host platform linux/${host_arch}, but" >&2
  echo "  release-manifest.json pins host-platform image refs — add linux/${host_arch} or pass --no-images" >&2
  exit 2
fi

mkdir -p "$out" "$out/cli"

# The ONE reproducible Go flag set for every release binary (the same set scripts/package/runner
# uses): -trimpath drops the build path, -buildvcs=false keeps git state out of the bytes,
# -buildid= zeroes the toolchain build id, -s -w strip symbols/DWARF.
release_ldflags="-s -w -buildid= -X github.com/palgroup/palai/packages/version.Stamp=${stamp}"

# Hermeticity for the HOST legs, so the index's `hermetic` string is true for every artifact and not
# only for the two in-Dockerfile compiles: the CLI matrix compiles offline against the warmed,
# go.sum-pinned module cache, exactly like the Dockerfile stages do. A cold cache FAILS the build
# ("module lookup disabled by GOPROXY=off") instead of quietly resolving through the ambient proxy —
# warm it with `make bootstrap` (go mod download). Exported, so nothing downstream can lose it; the
# runner packager pins the same pair on its own compile for callers that invoke it directly.
export GOPROXY=off GOFLAGS=-mod=readonly

# artifacts_json accumulates one release-index entry per built artifact, in emission order (which is
# deterministic: the matrix lists are walked in the order given).
artifacts_json=""
add_artifact() { # kind file os arch [extra-json]
  local digest entry
  # recompute-over-copy: the digest is the artifact's BYTES, hashed here, never scraped from a log.
  digest="$(cd "$out" && sha256sum "$2" | cut -d' ' -f1)"
  entry="$(printf '    {"kind": "%s", "file": "%s", "os": "%s", "arch": "%s", "digest": "sha256:%s"%s, "sbom": null, "provenance": null}' \
    "$1" "$2" "$3" "$4" "$digest" "${5:-}")"
  if [ -z "$artifacts_json" ]; then artifacts_json="$entry"; else artifacts_json="$artifacts_json,
$entry"; fi
}

# --- CLI matrix -------------------------------------------------------------------------------
for target in $(echo "$cli_targets" | tr ',' ' '); do
  goos="${target%%/*}"; goarch="${target##*/}"
  bin="cli/palai-${goos}-${goarch}"
  echo "build.sh: CLI ${goos}/${goarch} at $stamp" >&2
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -buildvcs=false -ldflags "$release_ldflags" -o "$out/$bin" ./cmd/cli
  add_artifact cli "$bin" "$goos" "$goarch"
done

# $out/palai is the host CLI at its historical path (upgrade-drill and the docs invoke it directly).
# It is a COPY of the matrix artifact when the host target is in the matrix, so its digest is one of
# the indexed ones rather than a seventh, separately-built binary.
if [ -f "$out/cli/palai-${host_os}-${host_arch}" ]; then
  cp "$out/cli/palai-${host_os}-${host_arch}" "$out/palai"
else
  CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "$release_ldflags" -o "$out/palai" ./cmd/cli
fi

# --- runner host package per arch -------------------------------------------------------------
# The E14 T5 packager already takes ARCH/OUT and already builds deterministically (fixed mtime,
# uid/gid 0, gzip -n). Per-arch OUT dirs keep the two packages' sibling files apart. It also emits a
# .sig/.pub with an EPHEMERAL key when no operator key is set — the ECDSA signature is not
# reproducible (random k) and is NOT indexed here: binding signatures to the index is T3's job.
for arch in $(echo "$runner_archs" | tr ',' ' '); do
  echo "build.sh: runner host package linux/${arch}" >&2
  pkg="$(VERSION="$version" ARCH="$arch" OUT="$out/runner-package/$arch" scripts/package/runner/build.sh)"
  add_artifact runner-host-package "runner-package/$arch/$pkg" linux "$arch"
done

# --- images -----------------------------------------------------------------------------------
cp_image=""
runner_image=""
engine_image=""

if [ "$build_images" -eq 1 ]; then
  mkdir -p "$out/images"
  # image_id_from_tar reads the image config digest out of the SHIPPED tar (docker's manifest.json
  # names the config blob), so the id — like the tar digest — comes from artifact bytes.
  # `|| true`: with `set -o pipefail` an unreadable manifest (or a grep that matches nothing) would
  # abort the whole script from the ASSIGNMENT below, with no diagnostic at all. Return empty instead
  # and let the caller's guard say what went wrong.
  image_id_from_tar() {
    tar -xOf "$1" manifest.json 2>/dev/null \
      | grep -oE '"[Cc]onfig":[^,]*' | grep -oE '[0-9a-f]{64}' | head -1 | sed 's/^/sha256:/' || true
  }

  # build_image publishes the host-platform ref in $host_ref (a global, NOT stdout: add_artifact
  # appends to $artifacts_json, and a command substitution would run it in a subshell and drop it).
  host_ref=""
  build_image() { # name dockerfile context platform extra-build-args...
    local name="$1" dockerfile="$2" context="$3" platform="$4"; shift 4
    local goarch="${platform##*/}" goos="${platform%%/*}" ref tarball image_id
    host_ref=""
    ref="palai/${name}:${tag}-${goarch}"
    tarball="images/${name}-${goos}-${goarch}.tar"
    echo "build.sh: image $ref ($platform)" >&2
    # --load then `docker save`: the tar is the shippable artifact form (the same one the air-gap
    # bundle ships). `--output type=docker,dest=…` would do it in one step, but the `docker` driver
    # that ships with Docker Desktop rejects the docker exporter to a file ("Docker exporter feature
    # is currently not supported for docker driver") — and load+save works on BOTH drivers without
    # asking the operator to create a builder. The Dockerfiles cross-compile from $BUILDPLATFORM, so
    # a foreign-arch image builds without emulation.
    docker buildx build --platform "$platform" --load -t "$ref" \
      "$@" -f "$dockerfile" "$context" >&2
    docker save -o "$out/$tarball" "$ref" >&2
    # An unreadable manifest.json would make image_id_from_tar print NOTHING and the index would then
    # carry "image_id": "" — an EMPTY identity that T3 would bake into provenance. Fail instead.
    image_id="$(image_id_from_tar "$out/$tarball")"
    case "$image_id" in
      sha256:????????????????????????????????????????????????????????????????) ;;
      *) echo "build.sh: no image id in $tarball's manifest.json (got '$image_id') — refusing to index an empty digest" >&2
         exit 1;;
    esac
    add_artifact image "$tarball" "$goos" "$goarch" \
      "$(printf ', "ref": "%s", "image_id": "%s"' "$ref" "$image_id")"
    # The daemon-native image (Docker Desktop's VM is linux/<host arch>) ALSO carries the plain
    # palai/<name>:<tag> ref, which the compose stack, the upgrade drill and the boot smoke resolve.
    if [ "$platform" = "linux/${host_arch}" ]; then
      docker tag "$ref" "palai/${name}:${tag}" >&2
      host_ref="palai/${name}:${tag}"
    fi
  }

  for platform in $(echo "$platforms" | tr ',' ' '); do
    build_image control-plane deploy/compose/control-plane.Dockerfile . "$platform" \
      --build-arg "PALAI_VERSION_STAMP=${stamp}"
    [ -z "$host_ref" ] || cp_image="$host_ref"
    build_image runner deploy/compose/runner.Dockerfile . "$platform" \
      --build-arg "PALAI_VERSION_STAMP=${stamp}"
    [ -z "$host_ref" ] || runner_image="$host_ref"
    build_image reference-engine engines/reference/Dockerfile engines/reference "$platform"
    [ -z "$host_ref" ] || engine_image="$host_ref"
  done

  # release-manifest.json (below) is the E15 T2 upgrade contract: it names the HOST-platform images
  # by their local ref + daemon image id, which is what `palai upgrade` pins and compares.
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

# release-index.json — the skeleton of release-policy.md's "Required release artifacts" list. Every
# artifact is identified ONLY by its digest (§2: mutable tags are not release inputs); sbom and
# provenance are DEFINED and EMPTY here and are filled by T2/T3.
#
# An image entry's "ref" is INFORMATIONAL ONLY — the local tag this build loaded, kept so a human can
# see which image a tar came from. §2 is categorical that a manifest names artifacts by digest, so T4's
# verifier MUST resolve an image by its "digest" (the tar's bytes) or "image_id" and must NEVER resolve
# "ref": a tag move would silently repoint it.
index="$out/release-index.json"
cat > "$index" <<EOF
{
  "schema": "palai.release-index/v1",
  "version": "${version}",
  "stamp": "${stamp}",
  "commit": "${commit}",
  "source_date_epoch": "${SOURCE_DATE_EPOCH}",
  "built_at": "${built_at}",
  "invocation": { "command": "scripts/release/build.sh", "args": [${invocation_args}] },
  "reproducibility": {
    "claimed": "binary-level: two builds of this commit produce bit-identical binaries and host packages",
    "not_claimed": "image-layer-level: docker image tars carry layer timestamps and are not byte-identical",
    "go_flags": "-trimpath -buildvcs=false -ldflags \"${release_ldflags}\"",
    "hermetic": "go mod download is the only proxy access (go.sum-pinned); EVERY compile — the in-image stages AND the host legs (cli, runner host package) — runs GOPROXY=off -mod=readonly against that warmed cache"
  },
  "artifacts": [
${artifacts_json}
  ],
  "sbom": null,
  "provenance": null,
  "notes": "sbom is produced by E18 T2 and provenance/signature by E18 T3; a qemu boot-smoke of a foreign-arch image is not a full UAT run on that arch (plan §6 leg 3)."
}
EOF

# Both files are consumed as JSON (T2/T3/T4 and `palai upgrade`), and both are written by heredoc from
# operator-supplied strings — so validate the BYTES before claiming success. Without this a
# `--version 'x"}'` exits 0 having written an index that no consumer can parse.
for f in "$manifest" "$index"; do
  python3 -m json.tool "$f" >/dev/null \
    || { echo "build.sh: $f is not valid JSON — check --version/--tag for shell/JSON metacharacters" >&2; exit 1; }
done

echo "build.sh: wrote $manifest (version $version, stamp $stamp)" >&2
echo "build.sh: wrote $index (SOURCE_DATE_EPOCH=$SOURCE_DATE_EPOCH)" >&2
