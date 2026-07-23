#!/bin/sh
# E14 T5 — verify a Palai runner host package BEFORE extracting it onto a runner host. It (1)
# recomputes the tarball sha256 and compares it to the manifest, and (2) checks the detached
# openssl ECDSA signature against the pinned public key. It FAILS (non-zero exit) on ANY
# mismatch: a single flipped byte in the tarball breaks both the digest and the signature
# (proven by package_test.go's tamper case). Pin/trust palai-runner-signing.pub out of band —
# the signature only proves the tarball was signed by whoever holds the matching private key.
#
# Usage: verify.sh <tarball> [pubkey] [sig] [sha256-manifest]
set -eu

tarball="${1:?usage: verify.sh <tarball> [pubkey] [sig] [sha256-manifest]}"
pubkey="${2:-$(dirname "$tarball")/palai-runner-signing.pub}"
sig="${3:-${tarball}.sig}"
manifest="${4:-${tarball}.sha256}"

for f in "$tarball" "$pubkey" "$sig" "$manifest"; do
	[ -f "$f" ] || { echo "verify: missing $f" >&2; exit 2; }
done

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		echo "verify: no sha256 tool (need sha256sum or shasum)" >&2; exit 3
	fi
}

want="$(awk '{print $1; exit}' "$manifest")"
got="$(sha256_of "$tarball")"
if [ "$want" != "$got" ]; then
	echo "verify: FAIL — sha256 mismatch (manifest $want != tarball $got)" >&2
	exit 1
fi

if ! openssl dgst -sha256 -verify "$pubkey" -signature "$sig" "$tarball" >/dev/null 2>&1; then
	echo "verify: FAIL — signature does not verify against $pubkey" >&2
	exit 1
fi

echo "verify: OK — sha256 and signature verified for $tarball"
