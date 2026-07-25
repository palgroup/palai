#!/usr/bin/env bash
# scripts/release/provenance.sh — E18 T3. Attest a release directory produced by
# scripts/release/build.sh: an in-toto Statement carrying a SLSA provenance v1 predicate, then ONE
# openssl detached signature over a sha256sums root that binds the index, the attestation and the
# SBOM slot together.
#
# WHAT IT WRITES into --dir
#   provenance.intoto.json        the attestation (subjects, materials, build definition)
#   sha256sums                    the SIGNED ROOT: sha256 of every other file in the dir
#   sha256sums.sha256             its digest manifest    }  the signature ENVELOPE, in the shape
#   sha256sums.sig                its detached signature }  the E14 T5 verifier already understands
#   palai-release-signing.pub     the public half (a TRUST ANCHOR — see the trust model below)
#   runner-verify.sh              BYTE COPY of scripts/package/runner/verify.sh (E14 T5, VERBATIM)
#   provenance-verify.sh          copy of scripts/release/provenance-verify.sh (offline verifier)
#   release-index.json            PATCHED: the `provenance` slots now name the attestation
#
# ORDER — build.sh → (T2: sbom.sh) → provenance.sh → (T4: release-verify.sh). This script hashes
# EVERY file in the dir into the signed root, so whatever T2 has already written is bound by the
# same signature with no coordination; any SBOM present is additionally recorded as a byproduct of
# the run. That is the SBOM slot: it needs no change here when T2 lands.
#
# SIGNING — the E14 T5 command set, VERBATIM (`openssl genpkey -algorithm EC -pkeyopt
# ec_paramgen_curve:P-256` / `openssl pkey -pubout` / `openssl dgst -sha256 -sign`). There is exactly
# ONE signing tool in this repo and this is it; a second tool or keyring is a review reject (plan §2).
# A release passes an operator-held key via --key / PALAI_RELEASE_SIGNING_KEY; with none set this
# mints an EPHEMERAL key so the local proof is self-contained. A signing key is NEVER written into
# the release dir, never echoed, and never recorded in the attestation.
#
# TRUST MODEL (E14 T5, unchanged) — palai-release-signing.pub is written beside the artifacts for
# convenience, but a verifier MUST take its key from OUT OF BAND: an attacker who can swap the
# artifacts can swap a sibling key too, which would make the signature nothing but a second checksum.
#
# RECOMPUTE-OVER-COPY (plan §2) — every digest in the attestation is hashed HERE from the artifact's
# bytes. The index's own digest column is not trusted: it is re-derived and a mismatch ABORTS. The
# materials are likewise parsed out of the Dockerfiles/go.sum in the source tree, never transcribed.
#
# HONEST CEILING (plan §2 honest naming, §T3): the predicate is written in the standard, widely
# consumable SLSA shape, but the signature is openssl and the builder is a local developer session
# (`builder.id: https://palai.dev/builders/local-macos-session` — a TypeURI because SLSA requires one
# there, saying the same true thing). No keyless-identity, no transparency-log entry, and no SLSA
# level is claimed — the attestation says so itself, in internalParameters.honest_ceiling. The
# recorded toolchain versions are a RECORD, not an attested input: nothing recomputes them.
#
# Usage: provenance.sh --dir <release-out-dir> [--key <p256-key.pem>] [--source-root <repo>]
set -euo pipefail

self_dir="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$self_dir/../.." && pwd)"

dir=""
key="${PALAI_RELEASE_SIGNING_KEY:-}"
source_root="$repo_root"

while [ $# -gt 0 ]; do
  case "$1" in
    --dir) dir="$2"; shift 2;;
    --key) key="$2"; shift 2;;
    --source-root) source_root="$(cd "$2" && pwd)"; shift 2;;
    *) echo "provenance.sh: unknown argument $1" >&2; exit 2;;
  esac
done

[ -n "$dir" ] || { echo "provenance.sh: --dir <release-out-dir> is required" >&2; exit 2; }
dir="$(cd "$dir" && pwd)"
[ -f "$dir/release-index.json" ] || {
  echo "provenance.sh: $dir has no release-index.json — run scripts/release/build.sh first" >&2; exit 2; }

command -v openssl >/dev/null 2>&1 || { echo "provenance.sh: openssl not found" >&2; exit 3; }
command -v python3 >/dev/null 2>&1 || { echo "provenance.sh: python3 not found" >&2; exit 3; }

# The attestation must describe THIS tree: refuse if the source root has moved off the commit the
# index was built from, because the materials below are read out of the working tree.
index_commit="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["commit"])' "$dir/release-index.json")"
head_commit="$(git -C "$source_root" rev-parse HEAD 2>/dev/null || echo unknown)"
if [ "$index_commit" != "$head_commit" ]; then
  echo "provenance.sh: the release index was built from $index_commit but $source_root is at $head_commit —" >&2
  echo "  the materials would describe a different tree than the artifacts. Refusing." >&2
  exit 1
fi

# Stage the verifiers BEFORE hashing, so both are covered by the signed root.
cp "$repo_root/scripts/package/runner/verify.sh" "$dir/runner-verify.sh"
cp "$self_dir/provenance-verify.sh"              "$dir/provenance-verify.sh"
chmod 0755 "$dir/runner-verify.sh" "$dir/provenance-verify.sh"

tool_ver() { command -v "$1" >/dev/null 2>&1 && { "$@" 2>&1 | head -1; } || echo "absent"; }

REL_DIR="$dir" SRC_ROOT="$source_root" STATEMENT="provenance.intoto.json" \
BUILDER_ID="https://palai.dev/builders/local-macos-session" \
TC_GO="$(tool_ver go version)" \
TC_OPENSSL="$(tool_ver openssl version)" \
TC_DOCKER="$(tool_ver docker --version)" \
TC_HOST="$(uname -srm)" \
BUILD_SH_SHA="$(python3 -c 'import hashlib,sys; print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "$self_dir/build.sh")" \
PROVENANCE_SH_SHA="$(python3 -c 'import hashlib,sys; print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "$0")" \
python3 - <<'PY'
import hashlib, json, os, re, sys
from datetime import datetime, timezone

rel, src = os.environ["REL_DIR"], os.environ["SRC_ROOT"]
statement_file = os.environ["STATEMENT"]

def die(msg):
    print("provenance.sh: " + msg, file=sys.stderr)
    sys.exit(1)

def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()

index_path = os.path.join(rel, "release-index.json")
with open(index_path, encoding="utf-8") as fh:
    index = json.load(fh)

# --- subjects: RECOMPUTED from the artifact bytes, cross-checked against the index --------------
# The subject list is EXACTLY the release index's artifact set. The other files in the dir — the
# host CLI convenience copy `palai` (a byte copy of an indexed artifact), release-manifest.json, the
# staged verifiers — are release plumbing, not released artifacts; they are bound by the signed
# sha256sums root below rather than by a subject entry, and the verifier enforces that the two sets
# match exactly (an invented subject fails just as a missing one does).
subjects = []
for art in index["artifacts"]:
    path = os.path.join(rel, art["file"])
    if not os.path.isfile(path):
        die("release index names %s but it is not in %s" % (art["file"], rel))
    digest = sha256_file(path)
    claimed = (art.get("digest") or "").removeprefix("sha256:")
    if claimed != digest:
        die("%s: the index claims sha256:%s but the bytes are sha256:%s — refusing to attest a"
            " digest that was not recomputed" % (art["file"], claimed, digest))
    subjects.append({
        "name": art["file"],
        "digest": {"sha256": digest},
        "annotations": {"kind": art["kind"], "os": art["os"], "arch": art["arch"]},
    })
if not subjects:
    die("the release index lists no artifacts — an attestation with no subject binds nothing")

# --- materials ----------------------------------------------------------------------------------
# The Dockerfiles scripts/release/build.sh builds. Their base images are release inputs even for a
# --no-images run: they are what the build DEFINITION pins, and externalParameters.args below shows
# which legs this particular invocation ran.
DOCKERFILES = [
    "deploy/compose/control-plane.Dockerfile",
    "deploy/compose/runner.Dockerfile",
    "engines/reference/Dockerfile",
]
FROM_RE = re.compile(r"^FROM\s+(.*)$", re.IGNORECASE)

commit = index["commit"]
source_uri = "git+https://github.com/palgroup/palai@" + commit
materials = [{
    "uri": source_uri,
    "digest": {"gitCommit": commit},
    "name": "source",
}]

# In-repo files are addressed as a fragment of the SOURCE uri: `file://deploy/…` would parse as
# host=deploy per RFC 8089, and points nowhere a consumer can resolve.
def in_repo_uri(rel_path):
    return "%s#%s" % (source_uri, rel_path)

for rel_df in DOCKERFILES:
    path = os.path.join(src, rel_df)
    if not os.path.isfile(path):
        die("release Dockerfile %s is missing from %s" % (rel_df, src))
    materials.append({
        "uri": in_repo_uri(rel_df),
        "digest": {"sha256": sha256_file(path)},
        "name": "dockerfile",
    })
    stages, pinned = set(), 0
    with open(path, encoding="utf-8") as fh:
        for raw in fh:
            m = FROM_RE.match(raw.strip())
            if not m:
                continue
            fields = [f for f in m.group(1).split() if not f.startswith("--")]
            if not fields:
                die("%s: a FROM line names no image" % rel_df)
            ref = fields[0]
            if len(fields) > 2 and fields[1].upper() == "AS":
                stages.add(fields[2])
            if ref == "scratch" or ref in stages:
                continue
            if "@sha256:" not in ref:
                die("%s: base image %s is not digest-pinned — the materials would have a hole" % (rel_df, ref))
            tag, digest = ref.split("@sha256:", 1)
            if not re.fullmatch(r"[0-9a-f]{64}", digest):
                die("%s: base image %s has a malformed digest" % (rel_df, ref))
            pinned += 1
            # Canonical purl for a docker image: the version component is the digest, the tag is a
            # qualifier (`pkg:docker/golang@sha256:…?tag=1.26.4`).
            name_part, sep, tag_part = tag.rpartition(":")
            if not sep or "/" in tag_part:
                name_part, tag_part = tag, ""
            materials.append({
                "uri": "pkg:docker/%s@sha256:%s%s" % (name_part, digest,
                                                      "?tag=" + tag_part if tag_part else ""),
                "digest": {"sha256": digest},
                "name": "base-image",
                "annotations": {"dockerfile": rel_df, "ref": ref},
            })
    if pinned == 0:
        die("%s pins no base image — refusing to attest an empty materials set for it" % rel_df)

for rel_mod in ("go.mod", "go.sum"):
    path = os.path.join(src, rel_mod)
    if not os.path.isfile(path):
        die("%s is missing from %s" % (rel_mod, src))
    materials.append({
        "uri": in_repo_uri(rel_mod),
        "digest": {"sha256": sha256_file(path)},
        "name": "module-pin",
    })

# --- SBOM slot (T2) -------------------------------------------------------------------------------
# Anything T2 wrote into the release dir is bound by the signed root below; here it is also named as
# a byproduct of the run, with its digest recomputed like everything else.
byproducts = []
for root, _dirs, files in os.walk(rel):
    for name in sorted(files):
        low = name.lower()
        if low.startswith("sbom") or low.endswith((".spdx.json", ".cdx.json")):
            path = os.path.join(root, name)
            byproducts.append({
                "uri": "file://" + os.path.relpath(path, rel),
                "digest": {"sha256": sha256_file(path)},
                "name": "sbom",
            })

invocation = index.get("invocation")
if not isinstance(invocation, dict) or "args" not in invocation:
    die("release-index.json carries no invocation record — rebuild with the current"
        " scripts/release/build.sh so the attestation can name the build command")

repro = index.get("reproducibility", {})
now = datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")

honest_ceiling = (
    "The signature over this release is an openssl ECDSA P-256 detached signature on the sha256sums"
    " root (the E14 T5 tool, verbatim) — it is NOT a Sigstore/Fulcio keyless signature and no Rekor"
    " transparency log entry exists for it. builder.id is"
    " https://palai.dev/builders/local-macos-session: a URI because SLSA provenance v1 requires one"
    " there, but it names a local developer session, not a hosted isolated builder, so this is an"
    " L2-shaped mechanism only. An actual SLSA L2 claim requires the real CI run (plan §6 leg 1),"
    " which is also what fills internalParameters.ci_workflow_identity."
)

statement = {
    "_type": "https://in-toto.io/Statement/v1",
    "subject": subjects,
    "predicateType": "https://slsa.dev/provenance/v1",
    "predicate": {
        "buildDefinition": {
            "buildType": "https://palai.dev/buildtypes/release-matrix/v1",
            "externalParameters": {
                "command": invocation.get("command", "scripts/release/build.sh"),
                "args": invocation["args"],
                "version": index.get("version"),
                "stamp": index.get("stamp"),
                "source_date_epoch": index.get("source_date_epoch"),
                "go_flags": repro.get("go_flags"),
            },
            "internalParameters": {
                "hermetic": repro.get("hermetic"),
                "reproducibility_claimed": repro.get("claimed"),
                "reproducibility_not_claimed": repro.get("not_claimed"),
                "toolchains": {
                    "go": os.environ["TC_GO"],
                    "openssl": os.environ["TC_OPENSSL"],
                    "docker": os.environ["TC_DOCKER"],
                    "host": os.environ["TC_HOST"],
                },
                "toolchains_note": (
                    "RECORDED, NOT ATTESTED. These are what the build host reported; they are not in"
                    " resolvedDependencies and no verifier recomputes them, so they must never be"
                    " described as verified inputs. The inputs that ARE recomputed on verify are the"
                    " ones in buildDefinition.resolvedDependencies."
                ),
                "ci_workflow_identity": None,
                "ci_workflow_identity_note": (
                    "Defined and empty. A hosted workflow run (plan §T5) fills this with its own"
                    " identity; a local session has none and does not invent one."
                ),
                "sbom_slot": (
                    "SBOMs are produced by plan §T2 into this same release dir BEFORE this script"
                    " runs; every one found is listed in runDetails.byproducts and covered by the"
                    " signed sha256sums root. This run found %d." % len(byproducts)
                ),
                "honest_ceiling": honest_ceiling,
            },
            "resolvedDependencies": materials,
        },
        "runDetails": {
            "builder": {
                "id": os.environ["BUILDER_ID"],
                "version": {
                    "scripts/release/build.sh": "sha256:" + os.environ["BUILD_SH_SHA"],
                    "scripts/release/provenance.sh": "sha256:" + os.environ["PROVENANCE_SH_SHA"],
                },
            },
            "metadata": {
                "invocationId": "local-%s-%s" % (commit[:12], index.get("source_date_epoch")),
                "startedOn": now,
                "finishedOn": now,
            },
            "byproducts": byproducts,
        },
    },
}

with open(os.path.join(rel, statement_file), "w", encoding="utf-8") as fh:
    json.dump(statement, fh, indent=2, ensure_ascii=False)
    fh.write("\n")

# --- patch the index's provenance slots -----------------------------------------------------------
# The slot names the attestation FILE only; its digest lives in the signed sha256sums root, so there
# is exactly one copy of that digest and nothing to drift. T2 fills the sibling `sbom` slots.
index["provenance"] = statement_file
for art in index["artifacts"]:
    art["provenance"] = statement_file
index["signing"] = {
    "tool": "openssl ECDSA P-256 detached signature over sha256sums (E14 T5 tool, VERBATIM)",
    "root": "sha256sums",
    "envelope": ["sha256sums.sha256", "sha256sums.sig"],
    "public_key": "palai-release-signing.pub (convenience copy — a verifier MUST use an out-of-band key)",
    "verify": "scripts/release/provenance-verify.sh <release-dir> <out-of-band-pubkey>",
    "honest_ceiling": honest_ceiling,
}
with open(index_path, "w", encoding="utf-8") as fh:
    json.dump(index, fh, indent=2, ensure_ascii=False)
    fh.write("\n")

print("provenance.sh: wrote %s (%d subjects, %d materials, %d sbom byproducts)"
      % (statement_file, len(subjects), len(materials), len(byproducts)), file=sys.stderr)
PY

# --- sha256sums (the signed root) + detached signature (E14 T5 openssl, VERBATIM) ---------------
# Every file in the dir EXCEPT the signature envelope itself. That is what binds the index, the
# attestation, the SBOMs and the artifacts into ONE signature.
( cd "$dir" && find . -type f \
    ! -name 'sha256sums' ! -name 'sha256sums.sha256' ! -name 'sha256sums.sig' \
    ! -name 'palai-release-signing.pub' \
    | LC_ALL=C sort | while IFS= read -r f; do sha256sum "$f"; done > sha256sums )

stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT
if [ -n "$key" ]; then
  echo "provenance.sh: signing with the operator key at \$PALAI_RELEASE_SIGNING_KEY/--key" >&2
else
  key="$stage/signing.key"
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$key" 2>/dev/null
  echo "provenance.sh: no signing key given — generated an EPHEMERAL one (local proof only)" >&2
fi
openssl pkey -in "$key" -pubout -out "$dir/palai-release-signing.pub"
( cd "$dir" && sha256sum sha256sums > sha256sums.sha256 )
openssl dgst -sha256 -sign "$key" -out "$dir/sha256sums.sig" "$dir/sha256sums"

python3 -m json.tool "$dir/provenance.intoto.json" >/dev/null
python3 -m json.tool "$dir/release-index.json" >/dev/null
echo "provenance.sh: signed root $dir/sha256sums (verify: scripts/release/provenance-verify.sh $dir <pubkey>)" >&2
