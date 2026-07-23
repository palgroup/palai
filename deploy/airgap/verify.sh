#!/bin/sh
# E15 T4 — OFFLINE air-gap bundle verifier. Two checks, BOTH doable with NO network:
#
#   (1) SIGNATURE — the openssl P-256 detached signature over the signed root `sha256sums`.
#       This EXECS the E14 T5 verifier VERBATIM (shipped in the bundle as runner-verify.sh,
#       a byte copy of scripts/package/runner/verify.sh); there is exactly ONE signing tool.
#   (2) DIGEST CHAIN — `sha256sum -c sha256sums`: every bundle file matches its signed digest.
#       `sha256sums` is the signed root and lists manifest.json + every image tar + the runner
#       host package + the CLI binary + compose/helm/migrations, so (1)+(2) bind the signature
#       to every byte of the bundle.
#
# TRUST MODEL (inherited from E14 T5): the trusted public key MUST come OUT OF BAND, never from
# the bundle directory — a channel attacker can swap the artifacts, their signature, AND a
# sibling key all at once. Pass the key as arg 2 or PALAI_AIRGAP_PUBKEY.
#
# To PROVE no network is needed, run `verify.sh --network-none <bundle> <pubkey> <tool-image>`:
# it re-execs this same script inside `docker run --network none <tool-image>` (an openssl-capable
# image, e.g. the bundle's postgres). If verification passes with the container's network fully
# removed, the check demonstrably phones nothing home.
#
# Usage:
#   verify.sh <bundle-dir> <pubkey>                          # verify (host or inside a container)
#   verify.sh --network-none <bundle-dir> <pubkey> [image]   # verify inside a --network none box
set -eu

if [ "${1:-}" = "--network-none" ]; then
	shift
	bundle="${1:?usage: verify.sh --network-none <bundle-dir> <pubkey> [tool-image]}"
	pub="${2:?usage: verify.sh --network-none <bundle-dir> <pubkey> [tool-image]}"
	tool="${3:-${PALAI_AIRGAP_TOOL_IMAGE:-}}"
	[ -n "$tool" ] || { echo "verify: a tool image is REQUIRED (arg 3 or PALAI_AIRGAP_TOOL_IMAGE) — an openssl-capable, already-loaded image (e.g. the bundle's postgres)" >&2; exit 2; }
	bundle_abs="$(cd "$bundle" && pwd)"
	pub_dir="$(cd "$(dirname "$pub")" && pwd)"
	pub_base="$(basename "$pub")"
	# --network none: the container has NO network device at all. Read-only mounts. If verify
	# passes here, the whole check ran with egress physically impossible.
	exec docker run --rm --network none \
		-v "$bundle_abs:/bundle:ro" \
		-v "$pub_dir/$pub_base:/pub:ro" \
		--entrypoint /bin/sh "$tool" /bundle/verify.sh /bundle /pub
fi

bundle="${1:?usage: verify.sh <bundle-dir> <pubkey>}"
pub="${2:-${PALAI_AIRGAP_PUBKEY:-}}"
if [ -z "$pub" ]; then
	echo "verify: a trusted public key is REQUIRED (arg 2 or PALAI_AIRGAP_PUBKEY)." >&2
	echo "verify: obtain it OUT OF BAND — never from the bundle dir — then re-run." >&2
	exit 2
fi
# Resolve the pubkey to an absolute path BEFORE we cd into the bundle.
case "$pub" in
	/*) : ;;
	*) pub="$(cd "$(dirname "$pub")" && pwd)/$(basename "$pub")" ;;
esac

cd "$bundle"

for f in sha256sums sha256sums.sig sha256sums.sha256 runner-verify.sh manifest.json; do
	[ -f "$f" ] || { echo "verify: bundle missing $f" >&2; exit 2; }
done

# (1) SIGNATURE over the signed root — E14 T5 verifier VERBATIM. `sha256sums` plays the role of
# "the tarball"; sha256sums.sha256 is its digest manifest; sha256sums.sig is the detached sig.
echo "verify: (1) signature over sha256sums (E14 T5 openssl verifier) ..." >&2
sh ./runner-verify.sh sha256sums "$pub" sha256sums.sig sha256sums.sha256

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

echo "airgap-verify: OK — signature + digest chain verified for bundle $bundle"
