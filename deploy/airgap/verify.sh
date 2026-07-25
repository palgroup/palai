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
# ...and so must the VERIFYING CODE (FAIL-CLOSED VERIFIER RESOLUTION, E16 T7 e4aeb6f — applied here
# by E18 T4). This script execs runner-verify.sh (the E14 T5 openssl check); if that were taken from
# the bundle, the same channel attacker could replace it with `exit 0`, tamper the payloads, and
# REGENERATE the whole digest chain over their own files — the out-of-band public key would never be
# consulted and verify would exit 0. So the signer is resolved from OUTSIDE the bundle, or not at all:
#
#   1. a runner-verify.sh sitting next to THIS script — what an operator gets by obtaining verify.sh
#      and runner-verify.sh out of band together with the key (both live in the repo, ~80 lines each);
#   2. failing that, the git-tracked scripts/package/runner/verify.sh two levels up — the ONE signer
#      itself, present when this script is run from a checkout. No third byte copy of it is created:
#      another copy is another thing that can drift from the one signing tool.
#   3. failing both — including when THIS script IS the bundle's own copy — REFUSE. The bundle's
#      runner-verify.sh is used only under PALAI_AIRGAP_ALLOW_BUNDLED_VERIFIER=1, which says out loud
#      that this is a same-session local proof with no channel attacker in it.
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
	#
	# Inside the container the only verifier is the bundle's copy (self_dir == /bundle); --network-none
	# is a same-session, no-egress local proof (it demonstrates offline-verifiability, NOT channel-swap
	# resistance), so it opts into the bundled verifier explicitly. For channel-attack safety run the
	# git-tracked host deploy/airgap/verify.sh, whose signer resolves outside the bundle.
	exec docker run --rm --network none \
		-e PALAI_AIRGAP_ALLOW_BUNDLED_VERIFIER=1 \
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
pub_dir="$(cd "$(dirname "$pub")" && pwd -P)"
pub="${pub_dir%/}/$(basename "$pub")"

self_dir="$(cd "$(dirname "$0")" && pwd -P)"

cd "$bundle"
bundle_abs="$(pwd -P)"

# inside <path> <dir> — TRUE when <path> IS <dir> or anything under it, decided by DEVICE+INODE
# identity rather than by spelling: one level down, a symlinked bundle, macOS's /tmp -> /private/tmp
# and an APFS case respelling are all the same location under another name.
[ / -ef / ] 2>/dev/null || { echo "verify: REFUSING — this shell's \`test\` has no -ef, so the location fence cannot be decided." >&2; exit 3; }
inside() {
	_p="$1"
	while :; do
		[ "$_p" -ef "$2" ] && return 0
		case "$_p" in /|//|.|"") return 1 ;; esac
		_p="$(dirname "$_p")"
	done
}

for f in sha256sums sha256sums.sig sha256sums.sha256 runner-verify.sh manifest.json; do
	[ -f "$f" ] || { echo "verify: bundle missing $f" >&2; exit 2; }
done

# FAIL-CLOSED VERIFIER RESOLUTION (see the header): the signer comes from outside the bundle, or the
# run refuses. When THIS script is the bundle's own copy, nothing beside it is out of band — not even
# "two levels up", which from inside a bundle is just wherever the attacker's tarball was unpacked.
verifier=""
if ! inside "$self_dir" "$bundle_abs"; then
	if [ -f "$self_dir/runner-verify.sh" ]; then
		verifier="$self_dir/runner-verify.sh"
	elif [ -f "$self_dir/../../scripts/package/runner/verify.sh" ]; then
		verifier="$self_dir/../../scripts/package/runner/verify.sh"
	fi
	# ...and whatever that resolved to must still not live inside the bundle (a symlink, a checkout
	# unpacked under the bundle dir): the rule is the location, not the spelling of the path.
	if [ -n "$verifier" ] && inside "$(cd "$(dirname "$verifier")" && pwd -P)" "$bundle_abs"; then
		verifier=""
	fi
fi
if [ -z "$verifier" ]; then
	if [ "${PALAI_AIRGAP_ALLOW_BUNDLED_VERIFIER:-}" = "1" ]; then
		verifier="$bundle_abs/runner-verify.sh"
		echo "verify: WARNING — using the BUNDLE's runner-verify.sh (PALAI_AIRGAP_ALLOW_BUNDLED_VERIFIER=1; same-session local proof only, NOT channel-attack safe: a channel that swapped this file for \`exit 0\` would never run the signature check)" >&2
	else
		echo "verify: REFUSING — no trusted out-of-band runner-verify.sh; the bundle's own copy is untrusted (a channel attacker can neuter it and regenerate the whole digest chain)." >&2
		echo "verify: run the git-tracked deploy/airgap/verify.sh from a checkout, or put runner-verify.sh (obtained out of band with the key) beside this script, or set PALAI_AIRGAP_ALLOW_BUNDLED_VERIFIER=1 for a same-session local proof." >&2
		exit 2
	fi
fi
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

echo "airgap-verify: OK — signature + digest chain verified for bundle $bundle"
