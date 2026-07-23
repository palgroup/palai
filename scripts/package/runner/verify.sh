#!/bin/sh
# E14 T5 — verify a Palai runner host package BEFORE extracting it onto a runner host. It (1)
# recomputes the tarball sha256 and compares it to the manifest, and (2) checks the detached
# openssl ECDSA signature against a trusted public key. It FAILS (non-zero exit) on ANY mismatch.
#
# TRUST MODEL — the public key MUST come from OUT OF BAND, never from the package directory. A
# channel attacker can swap the tarball, its .sig, AND its .sha256 all at once and re-sign with
# their OWN key; if you also trusted the .pub sitting beside them, the signature would be nothing
# but a second checksum. So this script REQUIRES an explicit pubkey (arg 2 or PALAI_RUNNER_PUBKEY)
# and has NO sibling default — obtain the key (or pin its fingerprint) from a separate trusted
# channel (the project's release page / your config management), then pass it here. Optionally set
# PALAI_RUNNER_PUBKEY_FINGERPRINT to the key's sha256 and this refuses a key that does not match.
#
# Usage: verify.sh <tarball> <pubkey> [sig] [manifest]
set -eu

tarball="${1:?usage: verify.sh <tarball> <pubkey> [sig] [manifest]}"
pubkey="${2:-${PALAI_RUNNER_PUBKEY:-}}"
if [ -z "$pubkey" ]; then
	echo "verify: a trusted public key is REQUIRED (arg 2 or PALAI_RUNNER_PUBKEY)." >&2
	echo "verify: obtain it OUT OF BAND — never from the package dir — then re-run." >&2
	exit 2
fi
sig="${3:-${tarball}.sig}"
manifest="${4:-${tarball}.sha256}"

command -v openssl >/dev/null 2>&1 || { echo "verify: openssl not found — cannot check the signature" >&2; exit 3; }

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

# Optional fingerprint pin: refuse a public key whose sha256 does not match the out-of-band value.
if [ -n "${PALAI_RUNNER_PUBKEY_FINGERPRINT:-}" ]; then
	fp="$(sha256_of "$pubkey")"
	if [ "$fp" != "$PALAI_RUNNER_PUBKEY_FINGERPRINT" ]; then
		echo "verify: FAIL — public key fingerprint $fp does not match the pinned $PALAI_RUNNER_PUBKEY_FINGERPRINT" >&2
		exit 1
	fi
fi

want="$(awk '{print $1; exit}' "$manifest")"
got="$(sha256_of "$tarball")"
if [ "$want" != "$got" ]; then
	echo "verify: FAIL — sha256 mismatch (manifest $want != tarball $got)" >&2
	exit 1
fi

# Capture openssl's own message so a real error (e.g. an unreadable key) is surfaced, not swallowed
# into a generic "signature does not verify".
if ! msg="$(openssl dgst -sha256 -verify "$pubkey" -signature "$sig" "$tarball" 2>&1)"; then
	echo "verify: FAIL — signature check failed against $pubkey: $msg" >&2
	exit 1
fi

echo "verify: OK — sha256 and signature verified for $tarball against $pubkey"
