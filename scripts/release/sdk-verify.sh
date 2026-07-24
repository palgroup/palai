#!/bin/sh
# E16 T7 — OFFLINE Palai SDK bundle verifier (mirrors deploy/airgap/verify.sh, E15 T4). Two checks,
# BOTH doable with NO network:
#
#   (1) SIGNATURE — the openssl P-256 detached signature over the signed root `sha256sums`.
#       This EXECS the E14 T5 verifier VERBATIM (shipped in the bundle as runner-verify.sh, a byte
#       copy of scripts/package/runner/verify.sh); there is exactly ONE signing tool.
#   (2) DIGEST CHAIN — `sha256sum -c sha256sums`: every bundle file (each SDK package, manifest.json,
#       build-input.json) matches its signed digest. (1)+(2) bind the signature to every byte.
#
# TRUST MODEL (inherited from E14 T5): the trusted public key MUST come OUT OF BAND, never from the
# bundle dir — a channel attacker can swap the artifacts, the signature, AND a sibling key at once.
# Pass the key as arg 2 or PALAI_SDK_PUBKEY. The VERIFYING CODE must also come out of band: this
# script PREFERS a runner-verify.sh sitting next to it over the bundle's copy (a channel could swap
# the bundle's for `exit 0`); it falls back to the bundle's only for the same-session local proof.
#
# To PROVE no network is needed: `sdk-verify.sh --network-none <bundle> <pubkey> <tool-image>`
# re-execs this script inside `docker run --network none <tool-image>` (an openssl-capable image).
#
# Usage:
#   sdk-verify.sh <bundle-dir> <pubkey>                          # verify (host or inside a container)
#   sdk-verify.sh --network-none <bundle-dir> <pubkey> [image]   # verify inside a --network none box
set -eu

if [ "${1:-}" = "--network-none" ]; then
	shift
	bundle="${1:?usage: sdk-verify.sh --network-none <bundle-dir> <pubkey> [tool-image]}"
	pub="${2:?usage: sdk-verify.sh --network-none <bundle-dir> <pubkey> [tool-image]}"
	tool="${3:-${PALAI_SDK_TOOL_IMAGE:-}}"
	[ -n "$tool" ] || { echo "verify: a tool image is REQUIRED (arg 3 or PALAI_SDK_TOOL_IMAGE) — an openssl-capable, already-loaded image" >&2; exit 2; }
	bundle_abs="$(cd "$bundle" && pwd)"
	pub_dir="$(cd "$(dirname "$pub")" && pwd)"
	pub_base="$(basename "$pub")"
	exec docker run --rm --network none \
		-v "$bundle_abs:/bundle:ro" \
		-v "$pub_dir/$pub_base:/pub:ro" \
		--entrypoint /bin/sh "$tool" /bundle/sdk-verify.sh /bundle /pub
fi

bundle="${1:?usage: sdk-verify.sh <bundle-dir> <pubkey>}"
pub="${2:-${PALAI_SDK_PUBKEY:-}}"
if [ -z "$pub" ]; then
	echo "verify: a trusted public key is REQUIRED (arg 2 or PALAI_SDK_PUBKEY)." >&2
	echo "verify: obtain it OUT OF BAND — never from the bundle dir — then re-run." >&2
	exit 2
fi
case "$pub" in
	/*) : ;;
	*) pub="$(cd "$(dirname "$pub")" && pwd)/$(basename "$pub")" ;;
esac

self_dir="$(cd "$(dirname "$0")" && pwd)"
verifier="$self_dir/runner-verify.sh"

cd "$bundle"

for f in sha256sums sha256sums.sig sha256sums.sha256 runner-verify.sh manifest.json; do
	[ -f "$f" ] || { echo "verify: bundle missing $f" >&2; exit 2; }
done
[ -f "$verifier" ] || verifier="$(pwd)/runner-verify.sh"
echo "verify: using verifier $verifier" >&2

# (1) SIGNATURE over the signed root — E14 T5 verifier VERBATIM. `sha256sums` plays the role of
# "the tarball"; sha256sums.sha256 is its digest manifest; sha256sums.sig is the detached sig.
echo "verify: (1) signature over sha256sums (E14 T5 openssl verifier) ..." >&2
sh "$verifier" sha256sums "$pub" sha256sums.sig sha256sums.sha256

# (2) DIGEST CHAIN — every file listed in the (now signature-proven) sha256sums matches.
echo "verify: (2) digest chain (sha256sum -c sha256sums) ..." >&2
if command -v sha256sum >/dev/null 2>&1; then
	sha256sum -c sha256sums
elif command -v shasum >/dev/null 2>&1; then
	shasum -a 256 -c sha256sums
else
	echo "verify: no sha256 tool (need sha256sum or shasum)" >&2
	exit 3
fi

echo "sdk-verify: OK — signature + digest chain verified for bundle $bundle"
