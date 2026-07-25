#!/bin/sh
# scripts/release/release-verify.sh — E18 T4. THE unified release verifier: the WHOLE release index,
# checked OFFLINE, with every digest recomputed from the artifact's own bytes.
#
# It is the one command a promote runs (scripts/release/promote.sh) and the one an operator runs on a
# received release. It COMPOSES the two verifiers that already exist rather than re-deriving them —
# there is exactly one signature checker in this repo (E14 T5) and one SBOM/decision checker (E18 T2):
#
#   (1) PROVENANCE + SIGNATURE   scripts/release/provenance-verify.sh: the openssl P-256 signature
#       over the signed sha256sums root (which EXECS the E14 T5 verifier VERBATIM), the digest chain,
#       the refusal of any file the signed root does not list, the attestation's subject binding, and
#       the materials recomputed from the source tree.
#   (2) SBOM + VULNERABILITY DECISION   scripts/release/sbom-tool.py verify: every artifact has SPDX
#       *and* CycloneDX documents whose digests are their bytes, no unindexed SBOM rides along, and the
#       §51.3 policy decision is RE-DERIVED from the raw findings instead of read out of the manifest.
#   (3) RELEASE-LEVEL legs neither of them makes, below: the index's own digest recompute, image
#       identity, "a release must be SCANNED", the attestation must DECLARE the vulnerability decision,
#       and an out-of-band revocation list.
#   (4) SDK BUNDLE (optional)   scripts/release/sdk-verify.sh over PALAI_RELEASE_SDK_DIR: the SDK
#       packages are a release artifact family with their OWN signed root, so they are verified by
#       their own verifier, under this one command.
#
# RECOMPUTE-OVER-COPY (plan §2): no leg trusts a value the release states about itself. Digests come
# from bytes, the image id comes from the tar's config blob, the vulnerability verdict comes from the
# raw findings, the materials come from the source tree.
#
# TRUST MODEL (E14 T5, unchanged): the public key MUST come from OUT OF BAND — arg 2 or
# PALAI_RELEASE_PUBKEY — never from the release dir (provenance-verify.sh enforces that and refuses a
# key that resolves inside it unless PALAI_RELEASE_ALLOW_BUNDLED_PUBKEY=1 says it is a local proof).
#
# FAIL-CLOSED VERIFIER RESOLUTION (plan §2, the E16 T7 e4aeb6f pattern): the VERIFYING CODE comes from
# outside the thing it verifies. This script needs three siblings — provenance-verify.sh, sbom-tool.py
# and runner-verify.sh — and REFUSES if it is itself running from inside the release dir, or if a
# sibling is missing, unless PALAI_RELEASE_ALLOW_BUNDLED_VERIFIER=1 opts in for a same-session local
# proof. Nothing is ever resolved out of the release dir. (provenance.sh deliberately does NOT stage a
# copy of this script into a release: obtain scripts/release/ out of band with the key.)
#
# HONEST NAMING — what this proves and nothing more: an openssl ECDSA P-256 signature over a
# SLSA-SHAPED attestation, a set of digests that match bytes, and a policy decision that re-derives.
# It consults NO identity service and NO transparency log, it establishes no SLSA level, and the
# vulnerability result is only as fresh as the PINNED DB snapshot the report names. Out-of-band
# delivery of the trust root is operator ceremony (the E14 T5 model); the revocation list is a SHAPE
# defined here and in the index, with the real advisory flow living in T9's process doc and plan §6.
#
# Usage:
#   release-verify.sh <release-dir> <pubkey>
#   release-verify.sh --network-none <release-dir> <pubkey> [tool-image]
# Env:
#   PALAI_RELEASE_PUBKEY        the out-of-band key when arg 2 is absent
#   PALAI_RELEASE_SDK_DIR       an SDK bundle to verify too (its key: PALAI_SDK_PUBKEY)
#   PALAI_RELEASE_REVOCATIONS   an out-of-band revocation list (palai.release-revocations/v1)
#   PALAI_RELEASE_SOURCE_ROOT / PALAI_RELEASE_EXPECTED_BUILDER_ID / PALAI_RELEASE_ALLOW_* — honoured by
#   provenance-verify.sh, which this script execs; they are not re-interpreted here.
set -eu

if [ "${1:-}" = "--network-none" ]; then
	shift
	rel="${1:?usage: release-verify.sh --network-none <release-dir> <pubkey> [tool-image]}"
	pub="${2:?usage: release-verify.sh --network-none <release-dir> <pubkey> [tool-image]}"
	tool="${3:-${PALAI_RELEASE_TOOL_IMAGE:-}}"
	[ -n "$tool" ] || { echo "verify: a tool image is REQUIRED (arg 3 or PALAI_RELEASE_TOOL_IMAGE) — an already-loaded image with openssl + python3 (the project's own reference-engine image qualifies)" >&2; exit 2; }
	rel_abs="$(cd "$rel" && pwd)"
	pub_dir="$(cd "$(dirname "$pub")" && pwd)"
	pub_base="$(basename "$pub")"
	repo="$(cd "$(dirname "$0")/../.." && pwd)"
	# --network none: the container has NO network device. The repo is mounted read-only, so the
	# verifying code AND the canonical materials source both come from OUTSIDE the release dir — the
	# fail-closed resolution is satisfied structurally, with no opt-in. CEILING: a minimal tool image
	# has openssl+python3 but no git, so provenance-verify's commit recomputation says GIT ABSENT out
	# loud; every other input is still recomputed from that read-only mount.
	exec docker run --rm --network none \
		-v "$repo:/repo:ro" \
		-v "$rel_abs:/release:ro" \
		-v "$pub_dir/$pub_base:/pub:ro" \
		--entrypoint /bin/sh "$tool" /repo/scripts/release/release-verify.sh /release /pub
fi

rel="${1:?usage: release-verify.sh <release-dir> <pubkey>}"
pub="${2:-${PALAI_RELEASE_PUBKEY:-}}"
if [ -z "$pub" ]; then
	echo "verify: a trusted public key is REQUIRED (arg 2 or PALAI_RELEASE_PUBKEY)." >&2
	echo "verify: obtain it OUT OF BAND — never from the release dir — then re-run." >&2
	exit 2
fi
case "$pub" in
	/*) : ;;
	*) pub="$(cd "$(dirname "$pub")" && pwd)/$(basename "$pub")" ;;
esac

self_dir="$(cd "$(dirname "$0")" && pwd)"
rel_abs="$(cd "$rel" && pwd)"

# --- fail-closed verifier resolution -------------------------------------------------------------
missing=""
for sibling in provenance-verify.sh sbom-tool.py runner-verify.sh; do
	[ -f "$self_dir/$sibling" ] || missing="$missing $sibling"
done
if [ -n "$missing" ] || [ "$self_dir" = "$rel_abs" ]; then
	if [ "${PALAI_RELEASE_ALLOW_BUNDLED_VERIFIER:-}" = "1" ]; then
		echo "verify: WARNING — the verifying code is incomplete or comes from the release dir itself (PALAI_RELEASE_ALLOW_BUNDLED_VERIFIER=1;${missing:+ missing:$missing;} same-session local proof only, NOT channel-attack safe)" >&2
	else
		[ "$self_dir" = "$rel_abs" ] && echo "verify: REFUSING — this script is running from INSIDE the release dir it would verify; a channel attacker can neuter it." >&2
		[ -n "$missing" ] && echo "verify: REFUSING — the verifying code is incomplete: no$missing beside this script." >&2
		echo "verify: run the git-tracked scripts/release/release-verify.sh (or obtain scripts/release/ out of band with the key), or set PALAI_RELEASE_ALLOW_BUNDLED_VERIFIER=1 for a same-session local proof." >&2
		exit 2
	fi
fi

[ -f "$rel_abs/release-index.json" ] || { echo "verify: $rel_abs has no release-index.json — this is not a release directory built by scripts/release/build.sh" >&2; exit 2; }

command -v python3 >/dev/null 2>&1 || { echo "verify: python3 not found" >&2; exit 3; }

# --- (1) provenance + signature ------------------------------------------------------------------
# FIRST, so that nothing after this point reads bytes the signature has not vouched for.
echo "release-verify: (1) signature + digest chain + subject binding + materials ..." >&2
sh "$self_dir/provenance-verify.sh" "$rel_abs" "$pub"

# --- (2) SBOM + the vulnerability decision, re-derived -------------------------------------------
echo "release-verify: (2) SBOM presence/digests + the §51.3 decision, re-derived ..." >&2
DIR="$rel_abs" MANIFEST="release-index.json" SDK=0 python3 "$self_dir/sbom-tool.py" verify

# --- (3) the release-level legs ------------------------------------------------------------------
echo "release-verify: (3) index digests, image identity, scan coverage, declared decision, revocations ..." >&2
REL_DIR="$rel_abs" python3 - <<'PY'
import hashlib, json, os, sys, tarfile

rel = os.environ["REL_DIR"]
failures = []


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


with open(os.path.join(rel, "release-index.json"), encoding="utf-8") as fh:
    index = json.load(fh)

artifacts = index.get("artifacts") or []
if not artifacts:
    failures.append("the release index lists no artifacts — verifying an empty set proves nothing")

# (a) every artifact digest RECOMPUTED from its bytes. provenance-verify.sh binds the same bytes to
# the attestation; this is the index's OWN column, checked against the tree it describes, because the
# index is what every downstream consumer (installer, promote, config) resolves an artifact by.
for art in artifacts:
    name = art.get("file")
    path = os.path.join(rel, name or "")
    if not name or not os.path.isfile(path):
        failures.append("indexed artifact %r is not in the release dir" % name)
        continue
    claimed = (art.get("digest") or "")
    if not claimed.startswith("sha256:"):
        # DIGEST EVERYWHERE, MUTABLE TAG NOWHERE (plan §2): an artifact with no digest is named only
        # by something that can move.
        failures.append("%s carries no sha256 digest in the index (%r) — a release artifact is named by"
                        " its digest, never by a tag" % (name, claimed))
        continue
    actual = sha256_file(path)
    if claimed.removeprefix("sha256:") != actual:
        failures.append("%s: the index says %s but the bytes are sha256:%s" % (name, claimed, actual))

    # (b) an image's IDENTITY, recomputed from the tar's own config blob — never from `ref` (which
    # build.sh records as informational: a tag move would silently repoint it) and never from a
    # `docker image inspect`, which this offline verifier cannot and must not need.
    if art.get("kind") != "image":
        continue
    image_id = art.get("image_id") or ""
    if not image_id.startswith("sha256:") or len(image_id) != 71:
        failures.append("image %s carries no usable image_id (%r) — an image resolved only by `ref`"
                        " is resolved by a mutable tag" % (name, art.get("image_id")))
        continue
    try:
        with tarfile.open(path) as tf:
            manifest = json.load(tf.extractfile("manifest.json"))
            config_member = manifest[0]["Config"]
            config_bytes = tf.extractfile(config_member).read()
            config = json.loads(config_bytes)
    except Exception as err:  # a corrupt tar is a failed verification, not a crashed verifier
        failures.append("image %s: cannot read its manifest.json/config blob (%s) — the image identity"
                        " cannot be recomputed" % (name, err))
        continue
    got = "sha256:" + hashlib.sha256(config_bytes).hexdigest()
    if got != image_id:
        failures.append("image %s: the config blob hashes to %s but the index pins image_id %s — the"
                        " tar does not carry the image the release names" % (name, got, image_id))
    if config.get("architecture") != art.get("arch"):
        failures.append("image %s: the config blob says architecture=%r, the index says %r"
                        % (name, config.get("architecture"), art.get("arch")))

# (c) a RELEASE must be scanned. sbom-tool.py verify accepts an unscanned dir on purpose (it says so
# out loud instead of claiming a clean result); the rule that a release cannot ship unscanned is this
# verifier's, and `scanned: false` is the flag it reads.
sbom = index.get("sbom")
if not isinstance(sbom, dict):
    failures.append("the release index carries no sbom block — a release without an SBOM cannot be"
                    " verified (run scripts/release/sbom.sh --dir <dir> before provenance.sh)")
    scan = {}
else:
    scan = sbom.get("vulnerability_scan") or {}
if not scan:
    failures.append("the sbom block records no vulnerability_scan")
elif not scan.get("scanned"):
    failures.append("REFUSING an UNSCANNED release: vulnerability_scan.scanned is false (%r). A"
                    " release run must never pass --no-scan; the absence of findings is not a clean"
                    " result." % scan.get("reason"))
elif scan.get("result") == "blocked":
    failures.append("the §51.3 policy BLOCKED this release (%s blocking finding(s)) — it cannot be"
                    " verified for promotion" % scan.get("blocking"))

# (d) the attestation must DECLARE the vulnerability decision, and every byproduct it declares must be
# the bytes it says. Everything under sbom/ is signed either way, but an attestation that does not name
# the decision is not a supply-chain record a consumer can read: it would have to go looking for a file
# it was never told about. provenance-verify.sh binds the SUBJECTS; nothing binds the byproducts, so
# the recompute lives here.
statement_file = index.get("provenance")
byproducts = []
if not statement_file:
    failures.append("the release index names no provenance attestation")
else:
    with open(os.path.join(rel, statement_file), encoding="utf-8") as fh:
        statement = json.load(fh)
    byproducts = ((statement.get("predicate") or {}).get("runDetails") or {}).get("byproducts") or []
    if not any(b.get("name") == "vuln-decision" for b in byproducts):
        failures.append("%s declares no `vuln-decision` byproduct — the attestation does not name the"
                        " §51.3 policy decision this release was gated on (declared byproducts: %s)"
                        % (statement_file, sorted({b.get("name") for b in byproducts}) or "none"))
    for b in byproducts:
        path = os.path.join(rel, b["uri"].removeprefix("file://"))
        if not os.path.isfile(path):
            failures.append("the declared %s byproduct %s is not in the release dir"
                            % (b.get("name"), b["uri"]))
            continue
        got = sha256_file(path)
        if (b.get("digest") or {}).get("sha256") != got:
            failures.append("the declared %s byproduct %s is sha256:%s, the attestation says sha256:%s"
                            % (b.get("name"), b["uri"], got, (b.get("digest") or {}).get("sha256")))

# (e) REVOCATION — an OUT-OF-BAND list only. The index defines the shape (see build.sh) and
# deliberately carries no pointer to a list: an attacker who can swap these artifacts could drop a
# pointer just as easily, so a revocation check that resolves through the release is no check at all.
# What this proves is the MATCH; the report/triage/advisory process is docs/security (plan §T9) and a
# real advisory feed is an operator leg (plan §6).
revocations = os.environ.get("PALAI_RELEASE_REVOCATIONS", "")
revoked_names = 0
if revocations:
    if not os.path.isfile(revocations):
        failures.append("PALAI_RELEASE_REVOCATIONS=%s is not a file — a revocation list that cannot be"
                        " read is not an absence of revocations" % revocations)
    else:
        with open(revocations, encoding="utf-8") as fh:
            doc = json.load(fh)
        if doc.get("schema") != "palai.release-revocations/v1":
            failures.append("the revocation list schema is %r, not palai.release-revocations/v1"
                            % doc.get("schema"))
        identities = {index.get("stamp"), index.get("version"), index.get("commit")}
        for art in artifacts:
            identities.add(art.get("digest"))
            identities.add(art.get("image_id"))
        identities.discard(None)
        for entry in doc.get("revoked") or []:
            if entry.get("id") in identities:
                failures.append("REVOKED: %s is on the out-of-band revocation list (%s)"
                                % (entry.get("id"), entry.get("reason", "no reason given")))
            revoked_names += 1

if failures:
    for f in failures:
        print("release-verify: FAIL — " + f, file=sys.stderr)
    sys.exit(1)

images = [a for a in artifacts if a.get("kind") == "image"]
print("release-verify: (3) %d artifact digest(s) recomputed from bytes, %d image identity(ies)"
      " recomputed from their config blobs, %d declared byproduct(s) recomputed, scan result %r,"
      " decision declared in %s%s"
      % (len(artifacts), len(images), len(byproducts), scan.get("result"), statement_file,
         ", %d revocation entry(ies) checked" % revoked_names if revocations else
         ", NO revocation list supplied (PALAI_RELEASE_REVOCATIONS)"), file=sys.stderr)
PY

# --- (4) the SDK bundle, when one is named -------------------------------------------------------
if [ -n "${PALAI_RELEASE_SDK_DIR:-}" ]; then
	echo "release-verify: (4) SDK bundle $PALAI_RELEASE_SDK_DIR (its own signed root) ..." >&2
	sh "$self_dir/sdk-verify.sh" "$PALAI_RELEASE_SDK_DIR" "${PALAI_SDK_PUBKEY:?verify: PALAI_RELEASE_SDK_DIR is set, so an OUT-OF-BAND PALAI_SDK_PUBKEY is required}"
else
	echo "release-verify: (4) no SDK bundle named (PALAI_RELEASE_SDK_DIR unset) — the SDK packages of this release are UNVERIFIED by this run" >&2
fi

echo "release-verify: OK — $rel_abs verified OFFLINE: signature, digest chain, attestation binding and materials; SBOM presence/digests and a re-derived §51.3 decision; index digests and image identities recomputed from bytes."
echo "release-verify: CEILING — an openssl ECDSA P-256 signature over a SLSA-SHAPED attestation. No identity service was consulted, no transparency log entry exists, no SLSA level is established, and the vulnerability result is only as fresh as the PINNED DB snapshot the report names."
