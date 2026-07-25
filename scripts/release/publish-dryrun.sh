#!/usr/bin/env bash
# scripts/release/publish-dryrun.sh — E18 T5. The publish mechanics, REHEARSED: everything a real
# publication would do to npm, PyPI and the Go module proxy — with nothing published and no registry
# contacted. There are NO registry credentials in this project (plan §5, §6 leg 2) and there is no step
# here that could use one.
#
# THE PROOF THAT NOTHING DIALLED OUT is not the word "dry-run" in the transcript; it is that every leg
# runs with its registry BLACKHOLED and still has to succeed:
#   * npm     `npm_config_registry=http://127.0.0.1:9/` (the discard port) + `--offline`;
#   * python  no network at all — the metadata is read out of the built wheel/sdist with the stdlib;
#   * go      `GOPROXY=off GOSUMDB=off GOFLAGS=-mod=mod`.
# A leg that reached for a registry would fail the run instead of quietly succeeding.
#
# THREE LEGS
#   (1) npm            `npm publish --dry-run <tarball>` over the built @palai/sdk tarball.
#   (2) wheel + sdist  a `twine check`-CLASS validation. twine is not vendored here (adding a PyPI
#                      dependency to check a PyPI upload is circular), so this performs the same core
#                      inspection offline with the stdlib: the wheel's `*.dist-info/METADATA` and the
#                      sdist's `PKG-INFO` must parse, carry Metadata-Version / Name / Version, declare a
#                      renderable long-description content type, and AGREE with each other and with the
#                      bundle manifest's version. What it does NOT do is twine's HTML render of the
#                      description — that is named, not silently dropped.
#   (3) go module tag  a Go module has no build artifact: the published unit IS the git tag (E16 T7's
#                      honest note; E16 §6 leg 3 inherited here). So the rehearsal is the TAG: the name
#                      is DERIVED from the module path (a module in a subdirectory is tagged
#                      `<subdir>/vX.Y.Z`), the major-version suffix rule is checked, the release tag and
#                      the module tag must NOT already exist (release-policy.md: "A released tag or
#                      artifact is never overwritten"), a SIGNED tag needs a signing identity, and the
#                      module must build with the proxy OFF.
#
# NO TAG IS EVER CREATED HERE — not even when a signing identity is present. This script prints the exact
# `git tag -s` commands a real publication would run and stops. `git tag -a` (unsigned) and `git push`
# appear nowhere in this repository: a release is anchored to a SIGNED tag or it is not a release
# (release-policy.md, "Required release identity"), and the signing identity is plan §6 leg 1.
#
# Usage: publish-dryrun.sh <sdk-bundle-dir> [version]     (version defaults to the bundle manifest's)
set -euo pipefail

bundle="${1:?usage: publish-dryrun.sh <sdk-bundle-dir> [version]}"
bundle="$(cd "$bundle" && pwd -P)"
root="$(cd "$(dirname "$0")/../.." && pwd -P)"

step() { echo "publish-dryrun: $*" >&2; }
fail() { echo "publish-dryrun: REFUSED: $*" >&2; exit 1; }

[ -f "$bundle/manifest.json" ] || fail "$bundle has no manifest.json — that is not an SDK bundle"
version="${2:-$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["version"])' "$bundle/manifest.json")}"
[ -n "$version" ] || fail "the bundle manifest names no version"

# The BUILT package whose file ends with <suffix>, or empty. A package the bundle recorded as skipped is
# not silently treated as absent: its leg says which one was skipped and why the bundle says so.
pkg_file() {
	python3 - "$bundle/manifest.json" "$1" <<'PY'
import json, sys
doc = json.load(open(sys.argv[1], encoding="utf-8"))
for p in doc.get("packages", []):
    f = p.get("file") or ""
    if p.get("status") == "built" and f.endswith(sys.argv[2]):
        print(f)
        break
PY
}

echo "publish-dryrun: rehearsing the publication of version $version from $bundle" >&2
echo "publish-dryrun: there are NO registry credentials in this project; every leg runs against a" >&2
echo "publish-dryrun: blackholed registry so a leg that reached out would FAIL (plan §6 leg 2)" >&2

# --- (1) npm ---------------------------------------------------------------------------------------
npm_blackhole="http://127.0.0.1:9/"
tgz="$(pkg_file .tgz)"
if [ -z "$tgz" ]; then
	step "(1) npm: this bundle carries no built @palai/sdk tarball (PALAI_SDK_PACKAGES excluded it) — leg SKIPPED, not passed"
elif ! command -v npm >/dev/null 2>&1; then
	step "(1) npm: npm is absent on this host — leg SKIPPED, not passed"
else
	step "(1) npm publish --dry-run over $tgz, registry blackholed at $npm_blackhole"
	npm_config_registry="$npm_blackhole" npm publish --dry-run --offline "$bundle/$tgz" >&2
	step "(1) npm OK — the tarball is publishable and NOTHING was uploaded"
fi

# --- (2) wheel + sdist (twine check-class) ---------------------------------------------------------
whl="$(pkg_file .whl)"
sdist="$(python3 - "$bundle/manifest.json" <<'PY'
import json, sys
doc = json.load(open(sys.argv[1], encoding="utf-8"))
for p in doc.get("packages", []):
    f = p.get("file") or ""
    if p.get("status") == "built" and f.endswith(".tar.gz") and "go-sdk" not in f:
        print(f)
        break
PY
)"
if [ -z "$whl" ] && [ -z "$sdist" ]; then
	step "(2) python: this bundle carries no built wheel/sdist — leg SKIPPED, not passed"
else
	step "(2) twine-check-class metadata validation of the wheel + sdist (offline, stdlib)"
	BUNDLE="$bundle" WHEEL="$whl" SDIST="$sdist" VERSION="$version" python3 - <<'PY'
import email.parser, io, os, sys, tarfile, zipfile

bundle, version = os.environ["BUNDLE"], os.environ["VERSION"]
problems, seen = [], {}


def metadata(name, text):
    msg = email.parser.Parser().parsestr(text)
    for field in ("Metadata-Version", "Name", "Version"):
        if not msg.get(field):
            problems.append("%s: core metadata field %s is missing" % (name, field))
    ctype = msg.get("Description-Content-Type", "")
    body = (msg.get_payload() or "").strip() or msg.get("Description", "")
    if body and ctype not in ("", "text/markdown", "text/x-rst", "text/plain"):
        problems.append("%s: Description-Content-Type %r is not renderable" % (name, ctype))
    seen[name] = (msg.get("Name"), msg.get("Version"))
    print("  %-9s Metadata-Version=%s Name=%s Version=%s Description-Content-Type=%s"
          % (name, msg.get("Metadata-Version"), msg.get("Name"), msg.get("Version"), ctype or "(none)"))


wheel = os.environ["WHEEL"]
if wheel:
    with zipfile.ZipFile(os.path.join(bundle, wheel)) as z:
        names = [n for n in z.namelist() if n.endswith(".dist-info/METADATA")]
        if len(names) != 1:
            problems.append("wheel: %d .dist-info/METADATA members (want exactly 1)" % len(names))
        else:
            metadata("wheel", z.read(names[0]).decode("utf-8"))

sdist = os.environ["SDIST"]
if sdist:
    with tarfile.open(os.path.join(bundle, sdist)) as t:
        names = [n for n in t.getnames() if n.count("/") == 1 and n.endswith("/PKG-INFO")]
        if len(names) != 1:
            problems.append("sdist: %d top-level PKG-INFO members (want exactly 1)" % len(names))
        else:
            fh = t.extractfile(names[0])
            metadata("sdist", io.TextIOWrapper(fh, encoding="utf-8").read())

# twine uploads the pair together: a wheel and an sdist that disagree publish two different releases
# under one version.
if len(seen) == 2 and len(set(seen.values())) != 1:
    problems.append("wheel and sdist disagree about (Name, Version): %r" % (seen,))
for name, (_, ver) in seen.items():
    if ver != version:
        problems.append("%s declares version %r but this publication is %r" % (name, ver, version))

if problems:
    print("publish-dryrun: REFUSED: twine-check-class validation failed:", file=sys.stderr)
    for p in problems:
        print("  - " + p, file=sys.stderr)
    sys.exit(1)
print("  (twine itself is not vendored: this is the same core-metadata inspection, offline. The HTML")
print("   render of the long description that `twine check` also performs is NOT done here.)")
PY
	step "(2) python OK — the wheel/sdist metadata is publishable and NOTHING was uploaded"
fi

# --- (3) go module tag -----------------------------------------------------------------------------
step "(3) go module tag rehearsal (the published unit of a Go module is its TAG)"
module="$(awk '/^module /{print $2; exit}' "$root/sdks/go/go.mod")"
[ -n "$module" ] || fail "sdks/go/go.mod declares no module path"

# The tag name is DERIVED, never spelled out: a module in a subdirectory is published as <subdir>/vX.Y.Z.
subdir="${module#github.com/palgroup/palai/}"
[ "$subdir" != "$module" ] || fail "module path $module is not under this repository — the tag cannot be derived"
release_tag="v$version"
module_tag="$subdir/v$version"

# The major-version suffix rule: a v2+ module must carry /vN in its path, or `go get` resolves the wrong
# module. Rehearsing the tag is the last moment this is cheap to catch.
major="${version%%.*}"
case "$module" in
	*/v[0-9]*) want_major="${module##*/v}";;
	*) want_major="";;
esac
if [ -n "$want_major" ] && [ "$major" != "$want_major" ]; then
	fail "module path $module says major v$want_major but the tag would be $module_tag"
fi
if [ -z "$want_major" ] && [ "$major" != "0" ] && [ "$major" != "1" ]; then
	fail "$module has no /vN suffix, so it can only publish v0 or v1 — $module_tag needs the module path to become $module/v$major"
fi

# IMMUTABILITY (release-policy.md, "Revocation and rebuilds"): a released tag is never overwritten, so an
# existing tag stops the publication here rather than being moved. --force is never passed to git tag,
# anywhere in this repository.
for tag in "$release_tag" "$module_tag"; do
	if git -C "$root" rev-parse -q --verify "refs/tags/$tag" >/dev/null 2>&1; then
		fail "refs/tags/$tag already exists — a released tag or artifact is NEVER overwritten; rebuild as a new version (release-policy.md)"
	fi
done
step "(3) neither refs/tags/$release_tag nor refs/tags/$module_tag exists — the version is unpublished"

# What the proxy would serve must build with the proxy OFF: the Go SDK is stdlib-only by construction
# (E16 T4), so this both rehearses the consumer's build and proves this leg needs no network.
step "(3) building $module with GOPROXY=off (what the module proxy would serve, offline)"
( cd "$root/sdks/go" && GOPROXY=off GOSUMDB=off GOFLAGS=-mod=mod go build ./... >&2 )

# SIGNED, or not at all. A release is anchored to an annotated, cryptographically signed tag; without a
# signing identity the rehearsal REFUSES rather than falling back to an unsigned tag.
signer="${PALAI_RELEASE_TAG_SIGNER:-$(git -C "$root" config --get user.signingkey || true)}"
echo "publish-dryrun: the publication would run, and only these:" >&2
echo "    git tag -s $release_tag -m 'Palai $version'" >&2
echo "    git tag -s $module_tag -m 'Palai Go SDK $version'" >&2
if [ -z "$signer" ]; then
	fail "no tag signing identity (git config user.signingkey / PALAI_RELEASE_TAG_SIGNER) — a release is anchored to a SIGNED tag or it is not a release (release-policy.md); the real signing identity is plan §6 leg 1"
fi
step "(3) go module tag OK — signing identity $signer present; the tags were REHEARSED, not created"

echo "publish-dryrun: DONE — all legs rehearsed against blackholed registries. NOTHING was published," >&2
echo "publish-dryrun: no tag was created, and no credential was used or required (plan §6 leg 2)." >&2
