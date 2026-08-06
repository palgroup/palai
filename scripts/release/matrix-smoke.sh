#!/usr/bin/env bash
# scripts/release/matrix-smoke.sh — E18 T1: the IMAGE half of the release matrix, under a runnable
# check. repro_test.go covers the binary half (it runs --no-images), so until this existed NOTHING
# mechanical exercised --platforms, `docker buildx build`/`docker save`, the tar-derived image id, or
# boot-smoke.sh — they reproduced only when someone ran them by hand.
#
# It builds BOTH image platforms (host arch included, so release-manifest.json is produced), then:
#   * asserts every image entry in release-index.json carries a real image_id and a digest that EQUALS
#     the tar's bytes (recompute-over-copy), and that the tar's config blob really names that arch;
#   * BOOTS the amd64 tar through boot-smoke.sh — on an arm64 host that is a real emulated boot.
# The binary legs are reduced to the host target on purpose: repro_test.go already builds those twice.
#
# Docker is SHARED infrastructure: every image this run loaded or tagged, and the whole output dir,
# are removed on exit including on failure. The build CACHE is left alone (pruning it would slow down
# every other stack on this daemon).
#
# HONEST CEILING (plan §6 leg 3): a boot-smoke is not a full UAT run on that architecture.
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
tag="matrixsmoke"
out="$(mktemp -d)"

cleanup() {
  # Remove exactly the refs this run created — the index names them — never a blanket prune.
  if [ -f "$out/release-index.json" ]; then
    python3 -c 'import json,sys
for a in json.load(open(sys.argv[1]))["artifacts"]:
    if a["kind"] == "image": print(a["ref"])' "$out/release-index.json" 2>/dev/null \
      | while read -r ref; do docker image rm -f "$ref" >/dev/null 2>&1 || true; done
  fi
  for name in control-plane runner reference-engine; do
    docker image rm -f "palai/$name:$tag" >/dev/null 2>&1 || true
  done
  rm -rf "$out"
}
trap cleanup EXIT

step() { echo "matrix-smoke: $*" >&2; }

step "building the full image matrix (linux/amd64 + linux/arm64) into $out"
# A local smoke builds from the developer's tree, which is normally dirty; build.sh refuses that for a
# RELEASE (a "-dirty" stamp names no unique bytes) and this declares itself a scratch build.
PALAI_RELEASE_ALLOW_DIRTY=1 "$root/scripts/release/build.sh" --tag "$tag" --out "$out" \
  --platforms linux/amd64,linux/arm64 \
  --cli-targets "$(go env GOOS)/$(go env GOARCH)" \
  --agent-targets "$(go env GOOS)/$(go env GOARCH)"

# The index's image half, checked against the artifacts' own bytes.
python3 - "$out" <<'PY'
import hashlib, json, os, sys, tarfile

out = sys.argv[1]
idx = json.load(open(os.path.join(out, "release-index.json")))
images = [a for a in idx["artifacts"] if a["kind"] == "image"]
if len(images) != 6:
    sys.exit(f"matrix-smoke: expected 6 image artifacts (3 images x 2 platforms), got {len(images)}")

for a in images:
    path = os.path.join(out, a["file"])
    if not (a.get("image_id", "") or "").startswith("sha256:") or len(a["image_id"]) != 71:
        sys.exit(f"matrix-smoke: {a['file']} carries no usable image_id ({a.get('image_id')!r}) — an"
                 " empty identity would reach T3's provenance")
    with open(path, "rb") as fh:
        got = "sha256:" + hashlib.file_digest(fh, "sha256").hexdigest()
    if got != a["digest"]:
        sys.exit(f"matrix-smoke: {a['file']} index digest {a['digest']} != its bytes {got}")
    # The arch is read out of the tar's own config blob, not from the file name or a docker query.
    with tarfile.open(path) as tf:
        cfg = json.load(tf.extractfile(json.load(tf.extractfile("manifest.json"))[0]["Config"]))
    if cfg["architecture"] != a["arch"]:
        sys.exit(f"matrix-smoke: {a['file']} config blob says architecture={cfg['architecture']},"
                 f" index says {a['arch']}")
    print(f"matrix-smoke: {a['file']} {a['digest']} image_id={a['image_id']} arch={cfg['architecture']} OK")
PY

step "booting the amd64 tar (emulated on an arm64 host)"
bash "$root/scripts/release/boot-smoke.sh" "$out/images/control-plane-linux-amd64.tar" linux/amd64

step "PASS: 6 image tars indexed with recomputed digests + non-empty image ids; the amd64 image booted"
step "CEILING: a boot-smoke is not a full UAT run on amd64 (plan §6 leg 3)"
