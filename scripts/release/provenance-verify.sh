#!/bin/sh
# scripts/release/provenance-verify.sh — E18 T3. Verify an attested release directory with NO
# network at all. This is the primitive plan §T4's unified release verifier consumes.
#
# FOUR legs, in this order (each one can only run because the one before it passed):
#
#   (1) SIGNATURE   the openssl P-256 detached signature over the signed root `sha256sums`. This
#                   EXECS the E14 T5 verifier VERBATIM (scripts/package/runner/verify.sh) — there is
#                   exactly ONE signing tool in this repo and this script does not add a second.
#   (2) DIGEST CHAIN  `sha256sum -c sha256sums`: every file the signed root names — the index, the
#                   attestation, the SBOMs, every artifact — matches its signed digest, AND the
#                   release dir contains NOTHING ELSE. `sha256sum -c` alone only checks the files it
#                   is TOLD about; an unsigned drop-in (a second CLI binary, or an SBOM that T2's
#                   glob and T4 would go on to consume) is invisible to it, so the file set is
#                   compared both ways. Only the signature envelope itself is exempt, by construction.
#   (3) BINDING     every subject digest in the attestation is RECOMPUTED from the artifact's bytes,
#                   the subject set is exactly the release index's artifact set, and builder.id is the
#                   EXPECTED one. (1)+(2) already prove nobody edited the files; (3) proves the
#                   attestation DESCRIBES them and names a builder we accept — a builder that copied a
#                   digest out of a log instead of hashing the bytes, or that claims a CI workflow
#                   identity that never ran, produces a perfectly signed attestation that fails here.
#   (4) MATERIALS   the resolved dependencies are RECOMPUTED from the canonical source: the git
#                   commit (asked of the source tree itself, never read back out of the index), each
#                   release Dockerfile's own digest, every base image those Dockerfiles pin, and
#                   go.mod/go.sum. A dropped or invented input fails here even when the attestation
#                   was re-signed with the real key. With no `git` on PATH the commit cannot be
#                   recomputed; that degrades LOUDLY (a named GIT ABSENT warning), never silently.
#
# TRUST MODEL (E14 T5, unchanged): the public key MUST come from OUT OF BAND — arg 2 or
# PALAI_RELEASE_PUBKEY — never from the release dir. A channel attacker can swap the artifacts, the
# signature AND a sibling key at once; then the signature is just a second checksum. So a key that
# resolves INSIDE the release dir is refused unless PALAI_RELEASE_ALLOW_BUNDLED_PUBKEY=1 says out
# loud that this is a same-session local proof. Pin the key out of band with
# PALAI_RUNNER_PUBKEY_FINGERPRINT (the E14 T5 verifier honours it and refuses a key that misses).
#
# FAIL-CLOSED VERIFIER RESOLUTION (plan §2, the E16 T7 pattern): the VERIFYING CODE must also come
# from outside the thing it verifies. This script PREFERS the runner-verify.sh sitting beside it (in
# the repo workflow that is the git-tracked scripts/release/runner-verify.sh). If the only copy
# available is the release dir's own — or this script IS the release dir's copy — it REFUSES, unless
# PALAI_RELEASE_ALLOW_BUNDLED_VERIFIER=1 explicitly opts in for a same-session local proof.
#
# SOURCE ROOT for leg (4): defaults to this script's own repo root, overridable with
# PALAI_RELEASE_SOURCE_ROOT. A release dir shipped WITHOUT the source tree cannot recompute its
# materials; that degrades leg (4) to "whatever the signature covers", so it too is an explicit
# opt-in (PALAI_RELEASE_ALLOW_UNSOURCED_MATERIALS=1) rather than a silent skip.
#
# HONEST NAMING: this verifies an openssl signature and a SLSA-SHAPED attestation. It does not
# consult any identity service or transparency log, and it does not establish a SLSA level.
#
# Usage:
#   provenance-verify.sh <release-dir> <pubkey>
#   provenance-verify.sh --network-none <release-dir> <pubkey> [tool-image]
set -eu

if [ "${1:-}" = "--network-none" ]; then
	shift
	rel="${1:?usage: provenance-verify.sh --network-none <release-dir> <pubkey> [tool-image]}"
	pub="${2:?usage: provenance-verify.sh --network-none <release-dir> <pubkey> [tool-image]}"
	tool="${3:-${PALAI_PROVENANCE_TOOL_IMAGE:-}}"
	[ -n "$tool" ] || { echo "verify: a tool image is REQUIRED (arg 3 or PALAI_PROVENANCE_TOOL_IMAGE) — an already-loaded image with openssl + python3 (the project's own reference-engine image qualifies)" >&2; exit 2; }
	rel_abs="$(cd "$rel" && pwd -P)"
	pub_dir="$(cd "$(dirname "$pub")" && pwd -P)"
	pub_base="$(basename "$pub")"
	repo="$(cd "$(dirname "$0")/../.." && pwd -P)"
	# --network none: the container has NO network device. The repo is mounted read-only so BOTH the
	# verifying code and the canonical materials source come from OUTSIDE the release dir — the
	# fail-closed resolution below is satisfied naturally, with no opt-in. CEILING: a minimal tool
	# image has openssl+python3 but no git, so leg (4) cannot ask /repo what commit it is at and says
	# GIT ABSENT out loud; every other material is still recomputed from that read-only mount.
	exec docker run --rm --network none \
		-v "$repo:/repo:ro" \
		-v "$rel_abs:/release:ro" \
		-v "$pub_dir/$pub_base:/pub:ro" \
		--entrypoint /bin/sh "$tool" /repo/scripts/release/provenance-verify.sh /release /pub
fi

rel="${1:?usage: provenance-verify.sh <release-dir> <pubkey>}"
pub="${2:-${PALAI_RELEASE_PUBKEY:-}}"
if [ -z "$pub" ]; then
	echo "verify: a trusted public key is REQUIRED (arg 2 or PALAI_RELEASE_PUBKEY)." >&2
	echo "verify: obtain it OUT OF BAND — never from the release dir — then re-run." >&2
	exit 2
fi
pub_dir="$(cd "$(dirname "$pub")" && pwd -P)"
pub="${pub_dir%/}/$(basename "$pub")"

self_dir="$(cd "$(dirname "$0")" && pwd -P)"
cd "$rel"
rel_abs="$(pwd -P)"

# inside <path> <dir> — TRUE when <path> IS <dir> or anything under it, decided by DEVICE+INODE
# identity rather than by spelling. A string comparison is defeated by any second NAME for the same
# directory: one level down, a symlinked release, macOS's /tmp -> /private/tmp, an APFS case
# respelling. Both fences below are about the LOCATION a file comes from, not how the path is written.
[ / -ef / ] 2>/dev/null || { echo "verify: REFUSING — this shell's \`test\` has no -ef, so the location fence cannot be decided." >&2; exit 3; }
inside() {
	_p="$1"
	while :; do
		[ "$_p" -ef "$2" ] && return 0
		case "$_p" in /|//|.|"") return 1 ;; esac
		_p="$(dirname "$_p")"
	done
}

# The trust ANCHOR gets the same fail-closed treatment as the verifying code below: provenance.sh
# leaves palai-release-signing.pub out of the signed root on purpose, so a key taken from inside the
# release dir turns the signature into a second checksum an attacker controls end to end.
if inside "$pub_dir" "$rel_abs"; then
	if [ "${PALAI_RELEASE_ALLOW_BUNDLED_PUBKEY:-}" = "1" ]; then
		echo "verify: WARNING — the public key comes from INSIDE the release dir (PALAI_RELEASE_ALLOW_BUNDLED_PUBKEY=1): an attacker who swapped the artifacts could have swapped this key too, so the signature proves only self-consistency. Same-session local proof only." >&2
	else
		echo "verify: REFUSING — the public key $pub is INSIDE the release dir it would verify ($rel_abs); it is not a trust anchor (it is not even covered by the signed root)." >&2
		echo "verify: obtain the key OUT OF BAND and pass it, optionally pinning PALAI_RUNNER_PUBKEY_FINGERPRINT, or set PALAI_RELEASE_ALLOW_BUNDLED_PUBKEY=1 for a same-session local proof." >&2
		exit 2
	fi
fi

for f in sha256sums sha256sums.sig sha256sums.sha256 runner-verify.sh release-index.json provenance.intoto.json; do
	[ -f "$f" ] || { echo "verify: release dir is missing $f" >&2; exit 2; }
done

command -v openssl >/dev/null 2>&1 || { echo "verify: openssl not found" >&2; exit 3; }
command -v python3 >/dev/null 2>&1 || { echo "verify: python3 not found — legs (3) and (4) parse JSON" >&2; exit 3; }

verifier="$self_dir/runner-verify.sh"
if [ ! -f "$verifier" ] || inside "$self_dir" "$rel_abs"; then
	if [ "${PALAI_RELEASE_ALLOW_BUNDLED_VERIFIER:-}" = "1" ]; then
		verifier="$rel_abs/runner-verify.sh"
		echo "verify: WARNING — using the RELEASE DIR's runner-verify.sh (PALAI_RELEASE_ALLOW_BUNDLED_VERIFIER=1; same-session local proof only, NOT channel-attack safe)" >&2
	else
		echo "verify: REFUSING — no trusted out-of-band runner-verify.sh beside this script ($self_dir); the release dir's own copy is untrusted (a channel attacker can neuter it). A verifier resolved from INSIDE the release dir ($rel_abs) is refused however the path is spelled." >&2
		echo "verify: run the git-tracked scripts/release/provenance-verify.sh, or set PALAI_RELEASE_ALLOW_BUNDLED_VERIFIER=1 for a same-session local proof." >&2
		exit 2
	fi
fi
echo "verify: using verifier $verifier" >&2

# The source of truth for leg (4). Default: this script's repo root, when it looks like one.
src="${PALAI_RELEASE_SOURCE_ROOT:-}"
if [ -z "$src" ]; then
	candidate="$(cd "$self_dir/../.." && pwd)"
	[ -f "$candidate/deploy/compose/control-plane.Dockerfile" ] && src="$candidate"
fi

echo "verify: (1) signature over sha256sums (E14 T5 openssl verifier) ..." >&2
sh "$verifier" sha256sums "$pub" sha256sums.sig sha256sums.sha256

echo "verify: (2) digest chain (every release file matches its signed digest) ..." >&2
if command -v sha256sum >/dev/null 2>&1; then
	sha256sum -c sha256sums
elif command -v shasum >/dev/null 2>&1; then
	shasum -a 256 -c sha256sums
else
	echo "verify: no sha256 tool (need sha256sum or shasum)" >&2
	exit 3
fi

# ... and NOTHING ELSE is here. The check above is one-directional: it verifies the files the signed
# root names and is blind to files it does not. The four excluded names are the signature envelope,
# which cannot sign itself (provenance.sh excludes exactly these when it builds the root).
# ponytail: O(n*m) grep over a release dir's few dozen paths; sort+comm if a release ever gets big.
listed="$(sed 's/^[0-9a-f]*[ *]*//' sha256sums)"
# SYMLINKS COUNT. `-type f` does not list them, so `rider.link -> /etc/hosts` rode along under a
# "nothing unlisted" line: unsigned by construction (the signed root lists no symlink either) and the
# classic traversal vector for whatever extracts or consumes this tree next.
unlisted="$(find . \( -type f -o -type l \) \
	! -name 'sha256sums' ! -name 'sha256sums.sha256' ! -name 'sha256sums.sig' \
	! -name 'palai-release-signing.pub' \
	| LC_ALL=C sort | while IFS= read -r f; do
		printf '%s\n' "$listed" | grep -qxF -- "$f" || printf '%s\n' "$f"
	done)"
if [ -n "$unlisted" ]; then
	echo "verify: FAIL — the release dir carries file(s) the signed sha256sums root does NOT list, so nothing vouches for their bytes:" >&2
	printf '%s\n' "$unlisted" | sed 's/^/  /' >&2
	exit 1
fi

echo "verify: (3) subject binding + (4) materials ..." >&2
REL_DIR="$rel_abs" SRC_ROOT="$src" python3 - <<'PY'
import hashlib, json, os, re, shutil, subprocess, sys

rel = os.environ["REL_DIR"]
src = os.environ["SRC_ROOT"]
allow_unsourced = os.environ.get("PALAI_RELEASE_ALLOW_UNSOURCED_MATERIALS") == "1"
statement_file = "provenance.intoto.json"
# The builder identity this verifier accepts. A verifier that takes ANY builder.id blesses a
# fabricated CI identity — the one belief the honest ceiling exists to prevent. A release built
# somewhere else is verified by naming that builder explicitly, never by not looking.
expected_builder = (os.environ.get("PALAI_RELEASE_EXPECTED_BUILDER_ID")
                    or "https://palai.dev/builders/local-macos-session")
failures = []

def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()

with open(os.path.join(rel, "release-index.json"), encoding="utf-8") as fh:
    index = json.load(fh)
with open(os.path.join(rel, statement_file), encoding="utf-8") as fh:
    st = json.load(fh)

# --- (3) subject binding --------------------------------------------------------------------------
if st.get("_type") != "https://in-toto.io/Statement/v1":
    failures.append("statement _type is %r, not the in-toto Statement v1 URI" % st.get("_type"))
if st.get("predicateType") != "https://slsa.dev/provenance/v1":
    failures.append("predicateType is %r, not the SLSA provenance v1 URI" % st.get("predicateType"))

predicate = st.get("predicate") or {}
build_def = predicate.get("buildDefinition") or {}
run_details = predicate.get("runDetails") or {}
builder_id = ((run_details.get("builder") or {}).get("id") or "")
if builder_id != expected_builder:
    failures.append("runDetails.builder.id is %r, not the expected %r — this verifier does not bless an"
                    " unrecognised builder identity (set PALAI_RELEASE_EXPECTED_BUILDER_ID to verify a"
                    " release from a different builder you trust)" % (builder_id, expected_builder))

if index.get("provenance") != statement_file:
    failures.append("release-index.provenance is %r, not %r" % (index.get("provenance"), statement_file))

subjects = {}
for s in st.get("subject") or []:
    name = s.get("name")
    if name in subjects:
        failures.append("subject %r is listed twice" % name)
    subjects[name] = ((s.get("digest") or {}).get("sha256") or "")

artifacts = index.get("artifacts") or []
if not artifacts:
    failures.append("the release index lists no artifacts — the binding would be vacuous")

for art in artifacts:
    name = art["file"]
    path = os.path.join(rel, name)
    if not os.path.isfile(path):
        failures.append("indexed artifact %s is not in the release dir" % name)
        continue
    actual = sha256_file(path)
    if name not in subjects:
        failures.append("artifact %s is NOT a subject of the attestation" % name)
    else:
        claimed = subjects.pop(name)
        if claimed != actual:
            failures.append("subject %s: the attestation says sha256:%s but the bytes are sha256:%s"
                            % (name, claimed, actual))
    if (art.get("digest") or "").removeprefix("sha256:") != actual:
        failures.append("release index digest for %s is not the file's bytes" % name)
    if art.get("provenance") != statement_file:
        failures.append("release index artifact %s does not point at %s" % (name, statement_file))
for extra in subjects:
    failures.append("the attestation claims subject %r, which the release index does not list" % extra)

# --- (4) materials, RECOMPUTED from the canonical source -------------------------------------------
DOCKERFILES = [
    "deploy/compose/control-plane.Dockerfile",
    "deploy/compose/runner.Dockerfile",
    "engines/reference/Dockerfile",
]
FROM_RE = re.compile(r"^FROM\s+(.*)$", re.IGNORECASE)

# Compared as a SET of (role, algorithm, digest). Two Dockerfiles pinning the SAME base image
# therefore collapse to one expectation: what is checked is that every input the source pins is
# declared and nothing else is, not how many times an input is annotated.
declared = set()
for d in build_def.get("resolvedDependencies") or []:
    for alg, value in (d.get("digest") or {}).items():
        declared.add((d.get("name"), alg, value))

if not src:
    msg = ("materials cannot be recomputed: no source tree available (set PALAI_RELEASE_SOURCE_ROOT"
           " to the checkout at the attested commit)")
    if allow_unsourced:
        print("verify: WARNING — " + msg + "; leg (4) degraded to signature-only"
              " (PALAI_RELEASE_ALLOW_UNSOURCED_MATERIALS=1)", file=sys.stderr)
    else:
        failures.append("REFUSING — " + msg + ", and PALAI_RELEASE_ALLOW_UNSOURCED_MATERIALS is not set")
else:
    # The commit is RECOMPUTED like every other material: ask the source tree what it is at. Reading
    # it back out of the same index the attestation was generated from would be copy-over-recompute —
    # and the commit is precisely the material a lying builder forges.
    commit = index["commit"]
    git = shutil.which("git")
    if git:
        head = subprocess.run([git, "-C", src, "rev-parse", "HEAD"],
                              capture_output=True, text=True)
        if head.returncode != 0 or not head.stdout.strip():
            failures.append("materials: %s is not a git checkout, so the attested commit %s cannot be"
                            " recomputed" % (src, commit))
        else:
            commit = head.stdout.strip()
            if commit != index["commit"]:
                failures.append("materials: the release index says the build is at commit %s but the"
                                " source tree at %s is at %s" % (index["commit"], src, commit))
    else:
        print("verify: WARNING — GIT ABSENT: no git on PATH, so leg (4) cannot ask %s what commit it is"
              " at; the attested commit %s is taken on the ATTESTATION'S OWN WORD (every other material"
              " is still recomputed from the source tree)" % (src, commit), file=sys.stderr)
    expected = {("source", "gitCommit", commit)}
    for rel_df in DOCKERFILES:
        path = os.path.join(src, rel_df)
        if not os.path.isfile(path):
            failures.append("materials: release Dockerfile %s is missing from the source tree" % rel_df)
            continue
        expected.add(("dockerfile", "sha256", sha256_file(path)))
        stages = set()
        with open(path, encoding="utf-8") as fh:
            for raw in fh:
                m = FROM_RE.match(raw.strip())
                if not m:
                    continue
                fields = [f for f in m.group(1).split() if not f.startswith("--")]
                if not fields:
                    continue
                ref = fields[0]
                if len(fields) > 2 and fields[1].upper() == "AS":
                    stages.add(fields[2])
                if ref == "scratch" or ref in stages:
                    continue
                if "@sha256:" not in ref:
                    failures.append("materials: %s pins no digest for base image %s" % (rel_df, ref))
                    continue
                expected.add(("base-image", "sha256", ref.split("@sha256:", 1)[1]))
    for rel_mod in ("go.mod", "go.sum"):
        path = os.path.join(src, rel_mod)
        if os.path.isfile(path):
            expected.add(("module-pin", "sha256", sha256_file(path)))
        else:
            failures.append("materials: %s is missing from the source tree" % rel_mod)

    for name, alg, value in sorted(expected - declared):
        failures.append("materials: the attestation does not declare the %s %s:%s that the source"
                        " tree actually pins" % (name, alg, value))
    for name, alg, value in sorted(declared - expected):
        failures.append("materials: the attestation declares a %s %s:%s that the source tree does"
                        " not pin" % (name, alg, value))

if failures:
    for f in failures:
        print("verify: FAIL — " + f, file=sys.stderr)
    sys.exit(1)

print("verify: (3) %d subjects bound to recomputed bytes; (4) %d materials recomputed from source"
      % (len(artifacts), len(build_def.get("resolvedDependencies") or [])), file=sys.stderr)
PY

builder="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["predicate"]["runDetails"]["builder"]["id"])' provenance.intoto.json)"
echo "provenance-verify: OK — signature, digest chain (nothing unlisted), subject binding and materials verified for $rel_abs"
echo "provenance-verify: BUILT BY $builder — CEILING: an openssl ECDSA P-256 signature over a SLSA-SHAPED attestation. No identity service was consulted, no transparency log entry exists, and no SLSA level is established."
