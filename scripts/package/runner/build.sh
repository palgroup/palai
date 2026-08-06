#!/usr/bin/env bash
# E14 T5 — build the signed Palai runner host package: a DETERMINISTIC tarball (runner binary +
# systemd unit + host launcher + env template + runbook), a sha256 manifest, a detached openssl
# signature, and the public key to verify it. The operator downloads these onto a runner VM,
# runs verify.sh, extracts the tarball, and enables palai-runner.service. See runner-host.md.
#
# Determinism: the linux binary is built with -trimpath (path-independent) and every staged file
# is stamped to a fixed mtime with uid/gid 0, so two builds of the same source on the SAME tar
# toolchain yield a byte-identical tarball (asserted by package_test.go). bsdtar and GNU tar spell
# owner-zeroing differently and emit different archive formats, so cross-toolchain byte-identity is
# NOT claimed — a release rebuild reproduces on the machine/toolchain it was cut on. The ECDSA
# signature is NOT reproducible
# (random k) — that is fine, the SIGNED artifact (the tarball) is what must be deterministic.
#
# Signing tool: `openssl dgst -sha256 -sign/-verify` over an ECDSA P-256 key. Rationale (per the
# task ladder — the tool already in the toolchain wins): openssl is already a build dependency
# (T1 mints the edge/CA certs with it), so no new tool is added; ECDSA P-256 keeps the key and
# signature compact with no key-size ceremony, and `openssl dgst` is exactly the detached-sign
# form the brief named. A release uses an operator-held signing key (PALAI_RUNNER_SIGNING_KEY);
# with none set this generates an EPHEMERAL keypair so the local proof is self-contained.
set -euo pipefail

VERSION="${VERSION:-0.1.0}"
ARCH="${ARCH:-amd64}"
# ‼️ THE OS IS A PARAMETER AND THE ARTIFACT IS NAMED FOR THE DEVICE CONTRACT. `install.sh` fetches
# palai-<version>-<os>-<arch>.tar.gz and expects one binary called `palai` inside it (device plan T7b),
# so this producer answers that name rather than the host-package one it used to. A Mac fleet cannot be
# served by a builder that only knows linux.
OS="${OS:-linux}"
# Fixed mtime for every archive member (touch -t format) → reproducible tar.
MTIME='202601010000.00'

root="$(git rev-parse --show-toplevel)"
out="${OUT:-$root/dist/runner-package}"
pkg="palai-${VERSION}-${OS}-${ARCH}"
tarball="${pkg}.tar.gz"

mkdir -p "$out"
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT

# GOPROXY=off + -mod=readonly (E18 T1): the compile is offline against the warmed, go.sum-pinned
# module cache, which is what lets the release index claim hermeticity for the host package too. A
# cold cache fails here ("module lookup disabled by GOPROXY=off") rather than fetching.
echo "build: cross-compiling cmd/runner (${OS}/${ARCH}) as palai" >&2
( cd "$root" && CGO_ENABLED=0 GOOS="$OS" GOARCH="$ARCH" GOPROXY=off GOFLAGS=-mod=readonly \
	go build -trimpath -buildvcs=false -ldflags='-s -w' -o "$stage/palai" ./cmd/runner )

# Stage the package members FLAT (no subdirs, so tar member order is fully controlled).
here="$root/scripts/package/runner"
cp "$root/docs/operations/runner-host.md" "$stage/runner-host.md"
# ‼️ THE SYSTEMD BRIDGE IS LINUX-ONLY AND IS NOT WHAT A DEVICE USES. `palai enroll` writes and loads the
# service itself on both platforms; these three ship for the deployments that already run from an
# operator-edited /etc/palai/runner.env, which is the compatibility window §3.7 grants them. A darwin
# archive carries none of it — a LaunchAgent is written by enrol, not copied from a tarball.
if [ "$OS" = "linux" ]; then
	cp "$root/deploy/systemd/palai-runner.service" "$stage/palai-runner.service"
	cp "$here/palai-runner.sh"                     "$stage/palai-runner.sh"
	cp "$here/runner.env.example"                  "$stage/runner.env.example"
fi

# Normalize perms then mtime (perms first — touch is last so nothing re-stamps it).
chmod 0755 "$stage/palai"
chmod 0644 "$stage/runner-host.md"
if [ "$OS" = "linux" ]; then
	chmod 0755 "$stage/palai-runner.sh"
	chmod 0644 "$stage/palai-runner.service" "$stage/runner.env.example"
fi
find "$stage" -exec touch -t "$MTIME" {} +

# Deterministic tar: sorted members, uid/gid 0, numeric owner; gzip -n drops the name+timestamp
# from the gzip header. bsdtar and GNU tar zero the owner with DIFFERENT flags (and --no-mac-metadata
# is bsdtar-only — GNU tar errors on it, which pipefail would turn into a failed release build), so
# pick the flag set by tar flavor.
if tar --version 2>/dev/null | grep -qiE 'bsdtar|libarchive'; then
	tar_flags="--uid 0 --gid 0 --numeric-owner --no-mac-metadata --no-xattrs"
else
	tar_flags="--owner=0 --group=0 --numeric-owner"
fi
members="$(cd "$stage" && find . -type f | sed 's|^\./||' | LC_ALL=C sort)"
# shellcheck disable=SC2086  # $tar_flags and $members are our own space-safe lists, intended to split
( cd "$stage" && tar $tar_flags -cf - $members ) | gzip -n -9 > "$out/$tarball"

# sha256 manifest + detached signature.
( cd "$out" && sha256sum "$tarball" > "${tarball}.sha256" )

# ‼️ AND THE MANIFEST install.sh READS, WHICH IS A DIFFERENT DOCUMENT FROM THE LINE ABOVE. `.sha256` is
# per-artifact and travels beside it under a detached signature; `checksums.txt` covers every artifact of
# one VERSION and is fetched as its own request, so a digest and the file it protects are never served by
# one credential. Appending rather than overwriting is what lets a multi-arch build accumulate: each
# OS/ARCH invocation adds its own line and re-running one replaces only that line.
( cd "$out" \
	&& touch checksums.txt \
	&& grep -v " ${tarball}\$" checksums.txt > checksums.txt.next || true \
	&& sha256sum "$tarball" >> checksums.txt.next \
	&& LC_ALL=C sort -k2 checksums.txt.next > checksums.txt \
	&& rm -f checksums.txt.next )

keydir="$stage"
if [ -n "${PALAI_RUNNER_SIGNING_KEY:-}" ]; then
	signing_key="$PALAI_RUNNER_SIGNING_KEY"
	echo "build: signing with operator key $signing_key" >&2
else
	signing_key="$keydir/signing.key"
	openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$signing_key" 2>/dev/null
	echo "build: no PALAI_RUNNER_SIGNING_KEY set — generated an EPHEMERAL signing key (local proof only)" >&2
fi
openssl pkey -in "$signing_key" -pubout -out "$out/palai-runner-signing.pub"
openssl dgst -sha256 -sign "$signing_key" -out "$out/${tarball}.sig" "$out/$tarball"

# Ship the verify script beside the artifacts.
cp "$here/verify.sh" "$out/verify.sh"
chmod 0755 "$out/verify.sh"

echo "build: wrote to $out:" >&2
( cd "$out" && ls -1 "$tarball" "${tarball}.sha256" "${tarball}.sig" palai-runner-signing.pub verify.sh >&2 )
# Emit the tarball name on stdout so callers (package_test.go) can locate it.
echo "$tarball"
