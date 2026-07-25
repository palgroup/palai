#!/usr/bin/env bash
# scripts/release/sbom.sh — E18 T2. SBOM (SPDX **and** CycloneDX), license inventory and
# vulnerability scan for a release directory built by scripts/release/build.sh, or for an SDK
# bundle built by scripts/release/sdk-package.sh.
#
# ORDER — build.sh → **sbom.sh** → provenance.sh (T3) → release-verify.sh (T4). This script MUST
# run before the attestation: provenance.sh hashes every file in the release dir into the signed
# sha256sums root and provenance-verify.sh REFUSES any file that is not in it, so an SBOM written
# after the signature is an unsigned rider and fails verification — correctly. Nothing may be added
# to a release dir after it is attested.
#
# WHAT IT WRITES into <dir>/sbom/
#   <artifact>.spdx.json     SPDX 2.3 JSON   }  one pair per indexed artifact; the `.spdx.json` /
#   <artifact>.cdx.json      CycloneDX JSON  }  `.cdx.json` suffixes are what provenance.sh globs
#                                              into runDetails.byproducts.
#   <artifact>.grype.json    the raw scanner report for that artifact
#   license-inventory.json   every package the SBOMs saw, with its license, plus a by-license roll-up
#   vuln-report.json         the aggregate finding list + the policy decision
#   vuln-policy.json         a COPY of the policy the gate ran, so the decision is re-checkable offline
#
# and then PATCHES release-index.json (or the SDK bundle's manifest.json): the per-artifact `sbom`
# slot names both formats with a digest RECOMPUTED from the SBOM's bytes (§2 recompute-over-copy —
# nothing is transcribed from a tool's log), and the top-level `sbom` block records the generator,
# the scanner, the pinned DB snapshot and the gate result.
#
# PINS (design invariant §2 — digest everywhere, mutable tag nowhere)
#   * the generator and the scanner are containers addressed by `@sha256`, never by a tag;
#   * the vulnerability DB is a PINNED OFFLINE SNAPSHOT recorded in scripts/release/vulndb.lock.json
#     (archive checksum + snapshot date + grype's own import record). Every scan runs
#     `--network none` with auto-update OFF: the ONLY step allowed to reach the network is
#     `sbom.sh --hydrate-db`, which is exactly the shape build.sh uses for `go mod download`.
#
# POLICY GATE (§51.3) — a finding at a blocking severity (default: Critical) BLOCKS the release:
# this script exits non-zero, so every caller in the chain (release.yml, promote) stops. An
# exception is only ever TIME-BOUND and OWNER-ATTRIBUTED (§62.2 P2 discipline); the shape lives in
# scripts/release/vuln-policy.json and a malformed exception REFUSES the whole policy file rather
# than being silently ignored.
#
# HONEST CEILINGS (§2 honest naming)
#   * The vulnerability DB is a PINNED SNAPSHOT taken at the date the manifest records. It is NOT a
#     live CVE feed: a CVE published after that date is invisible here, and continuous rescanning of
#     already-shipped artifacts is an operator leg (plan §6), not something this script does.
#   * The scan's coverage is EXACTLY what the SBOM sees — for a static Go binary that is the Go
#     module list the linker embedded (no C libraries, no OS packages); for an image it is the
#     package DB inside that image. Anything vendored by copy, statically linked from C, or executed
#     from a base layer the cataloger cannot read is outside the result, and a clean scan does not
#     speak for it.
#   * SBOM BYTES ARE NOT REPRODUCIBLE: syft stamps each document with its own id and a timestamp, so
#     two runs over identical artifacts produce different SBOM digests. The index records the digest
#     of THIS run's bytes. T1's binary-level reproducibility claim is unaffected — that is a property
#     of the artifacts, which are hashed independently.
#
# Usage:
#   sbom.sh --dir <release-dir> [--sdk] [--no-scan] [--db <dir>] [--policy <f>] [--lock <f>]
#   sbom.sh --dir <release-dir> --verify            re-verify an already-populated dir, offline
#   sbom.sh --gate <grype-report.json> [--policy <f>]   run the policy gate over one report
#   sbom.sh --hydrate-db [--db <dir>] [--relock]    THE ONLY NETWORKED STEP: fetch the DB snapshot
set -euo pipefail

self_dir="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$self_dir/../.." && pwd)"

# --- PINS -----------------------------------------------------------------------------------
# Resolved from the multi-arch OCI index digest (`docker pull` + RepoDigests), so the pin covers
# every platform rather than one architecture's manifest.
SYFT_IMAGE="anchore/syft@sha256:13b53ebabe3d215268c90cf8fb9b875f0183908245f376fd4b3a2cb69d21d484"    # v1.49.0
GRYPE_IMAGE="anchore/grype@sha256:fd4ab4d1042b522c896e73bdf09ab8bf384fa417df99d6dd0d6e1008c7e7c821"  # v0.116.0

# syft's default DIRECTORY cataloger set reads INSTALLED state and skips the two "declared"
# catalogers. Our SDK packages are declaration-only trees (a go.mod, a package.json — no vendor dir,
# no node_modules), so without these the SBOM for them would be honestly empty but useless.
SYFT_CATALOGERS="+go-module-file-cataloger,+javascript-package-cataloger,+javascript-lock-cataloger"

dir=""
mode="generate"
sdk=0
scan=1
gate_report=""
relock=0
db_dir="${PALAI_VULN_DB_DIR:-$repo_root/dist/vulndb}"
policy="$self_dir/vuln-policy.json"
lock="$self_dir/vulndb.lock.json"

while [ $# -gt 0 ]; do
  case "$1" in
    --dir) dir="$2"; shift 2;;
    --sdk) sdk=1; shift;;
    --verify) mode="verify"; shift;;
    --gate) mode="gate"; gate_report="$2"; shift 2;;
    --hydrate-db) mode="hydrate"; shift;;
    --relock) relock=1; shift;;
    --no-scan) scan=0; shift;;
    --db) db_dir="$2"; shift 2;;
    --policy) policy="$2"; shift 2;;
    --lock) lock="$2"; shift 2;;
    *) echo "sbom.sh: unknown argument $1" >&2; exit 2;;
  esac
done

command -v python3 >/dev/null 2>&1 || { echo "sbom.sh: python3 not found" >&2; exit 3; }

# --- hydrate: the ONE networked step ----------------------------------------------------------
if [ "$mode" = "hydrate" ]; then
  command -v docker >/dev/null 2>&1 || { echo "sbom.sh: docker not found" >&2; exit 3; }
  mkdir -p "$db_dir"
  echo "sbom.sh: fetching the vulnerability DB snapshot into $db_dir (the only networked step)" >&2
  docker run --rm -e GRYPE_DB_CACHE_DIR=/db -v "$db_dir:/db" "$GRYPE_IMAGE" db update >&2
  LOCK="$lock" DB_DIR="$db_dir" GRYPE_IMAGE="$GRYPE_IMAGE" RELOCK="$relock" python3 - <<'PY'
import json, os, sys, urllib.parse

db, lock_path = os.environ["DB_DIR"], os.environ["LOCK"]
imports = [os.path.join(r, "import.json") for r, _d, f in os.walk(db) if "import.json" in f]
if len(imports) != 1:
    sys.exit("sbom.sh: expected exactly one import.json under %s, found %d" % (db, len(imports)))
rec = json.load(open(imports[0], encoding="utf-8"))
# grype records the archive it installed as a URL whose query carries the archive's sha256 and
# whose filename carries the snapshot instant. Both are the PIN; parse them, never retype them.
url = urllib.parse.urlparse(rec["source"])
checksum = urllib.parse.parse_qs(url.query).get("checksum", [""])[0]
if not checksum.startswith("sha256:"):
    sys.exit("sbom.sh: the DB source URL carries no sha256 checksum — refusing to lock an unpinnable snapshot")
name = url.path.rsplit("/", 1)[-1]                       # vulnerability-db_v6.1.9_2026-07-25T00:39:09Z_….tar.zst
parts = name.split("_")
if len(parts) < 3:
    sys.exit("sbom.sh: cannot read a snapshot date out of %r" % name)
fresh = {
    "schema": "palai.vulndb-lock/v1",
    "scanner_image": os.environ["GRYPE_IMAGE"],
    "db_schema": os.path.basename(os.path.dirname(imports[0])),
    "db_version": rec["client_version"],
    "snapshot_date": parts[2],
    "archive_sha256": checksum.split(":", 1)[1],
    "archive_url": urllib.parse.urlunparse(url._replace(query="")),
    "local_digest": rec["digest"],
    "note": ("A PINNED OFFLINE SNAPSHOT of the grype vulnerability DB, not a live CVE feed. Every "
             "scan runs --network none against this snapshot; a CVE published after snapshot_date "
             "is invisible to it, and rescanning shipped artifacts against a newer snapshot is an "
             "operator leg (plan §6). The DB itself is ~2GB and is NOT committed: hydrate it with "
             "`scripts/release/sbom.sh --hydrate-db` into $PALAI_VULN_DB_DIR (default dist/vulndb)."),
}
old = json.load(open(lock_path, encoding="utf-8")) if os.path.exists(lock_path) else None
if old and old.get("archive_sha256") != fresh["archive_sha256"] and os.environ["RELOCK"] != "1":
    print(json.dumps(fresh, indent=2), file=sys.stderr)
    sys.exit("sbom.sh: the fetched snapshot (%s) is NOT the locked one (%s). The lock is the pin: "
             "re-run with --relock to move it deliberately." % (fresh["snapshot_date"], old.get("snapshot_date")))
with open(lock_path, "w", encoding="utf-8") as fh:
    json.dump(fresh, fh, indent=2, ensure_ascii=False)
    fh.write("\n")
print("sbom.sh: locked DB snapshot %s (%s)" % (fresh["snapshot_date"], fresh["archive_sha256"][:16]), file=sys.stderr)
PY
  exit 0
fi

# --- gate-only: run the policy over one scanner report -------------------------------------------
if [ "$mode" = "gate" ]; then
  [ -f "$gate_report" ] || { echo "sbom.sh: --gate needs a grype report, $gate_report is not a file" >&2; exit 2; }
  exec python3 "$self_dir/sbom-tool.py" gate "$policy" "$gate_report"
fi

[ -n "$dir" ] || { echo "sbom.sh: --dir <dir> is required" >&2; exit 2; }
dir="$(cd "$dir" && pwd)"
manifest_name="release-index.json"
[ "$sdk" -eq 1 ] && manifest_name="manifest.json"
[ -f "$dir/$manifest_name" ] || {
  echo "sbom.sh: $dir has no $manifest_name — run build.sh (or sdk-package.sh --sdk) first" >&2; exit 2; }

sbom_dir="$dir/sbom"

# --- verify: offline re-check of an already-populated dir ---------------------------------------
if [ "$mode" = "verify" ]; then
  exec env DIR="$dir" MANIFEST="$manifest_name" SDK="$sdk" \
    python3 "$self_dir/sbom-tool.py" verify
fi

command -v docker >/dev/null 2>&1 || { echo "sbom.sh: docker not found" >&2; exit 3; }

# --- the pinned DB must BE the pinned DB --------------------------------------------------------
if [ "$scan" -eq 1 ]; then
  LOCK="$lock" DB_DIR="$db_dir" python3 - <<'PY' || exit 1
import json, os, sys, urllib.parse
db, lock_path = os.environ["DB_DIR"], os.environ["LOCK"]
if not os.path.exists(lock_path):
    sys.exit("sbom.sh: no DB lock at %s — nothing is pinned. Run `sbom.sh --hydrate-db`." % lock_path)
lock = json.load(open(lock_path, encoding="utf-8"))
rec_path = os.path.join(db, lock["db_schema"], "import.json")
if not os.path.exists(rec_path):
    sys.exit("sbom.sh: the pinned vulnerability DB snapshot %s is not at %s.\n"
             "  Hydrate it once: scripts/release/sbom.sh --hydrate-db   (the only networked step;"
             " ~2GB, gitignored)" % (lock["snapshot_date"], db))
rec = json.load(open(rec_path, encoding="utf-8"))
checksum = urllib.parse.parse_qs(urllib.parse.urlparse(rec["source"]).query).get("checksum", [""])[0]
for what, got, want in (("archive sha256", checksum, "sha256:" + lock["archive_sha256"]),
                        ("local digest", rec["digest"], lock["local_digest"]),
                        ("db version", rec["client_version"], lock["db_version"])):
    if got != want:
        sys.exit("sbom.sh: the local vulnerability DB is NOT the pinned snapshot — %s is %r, the lock"
                 " says %r. Re-hydrate, or move the pin deliberately with --relock." % (what, got, want))
PY
fi

mkdir -p "$sbom_dir"
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT
validate_hash=true   # flipped off after the first scan; see the comment at the grype invocation

# --- the artifact list, straight out of the manifest --------------------------------------------
# TSV: kind <tab> relative-file <tab> sbom base name. Emitting it from python keeps the two manifest
# shapes (release-index artifacts[] / SDK manifest packages[]) in ONE place.
DIR="$dir" MANIFEST="$manifest_name" SDK="$sdk" python3 - > "$stage/artifacts.tsv" <<'PY'
import json, os, sys
doc = json.load(open(os.path.join(os.environ["DIR"], os.environ["MANIFEST"]), encoding="utf-8"))
if os.environ["SDK"] == "1":
    rows = [("sdk-package", p["file"]) for p in doc["packages"] if p.get("file")]
else:
    rows = [(a["kind"], a["file"]) for a in doc["artifacts"]]
if not rows:
    sys.exit("sbom.sh: the manifest lists no artifacts — an SBOM run over nothing proves nothing")
for kind, f in rows:
    print("%s\t%s\t%s" % (kind, f, f.replace("/", "_")))
PY

# --- generate ------------------------------------------------------------------------------------
while IFS=$'\t' read -r kind file base; do
  [ -f "$dir/$file" ] || { echo "sbom.sh: the manifest names $file but it is not in $dir" >&2; exit 1; }
  mount="$dir"
  case "$kind:$file" in
    image:*)
      # An image tar is read as an image, so the SBOM is that image's package DB.
      src="docker-archive:/work/$file" ;;
    *.tar.gz|*.tgz|*.tar|*.whl|*.zip)
      # DECOMPRESS, THEN SCAN (the E14 T7 vacuous-scan lesson, in its SBOM form): a cataloger sees
      # nothing through deflate, so the members are unpacked first and the tree is scanned.
      mount="$stage/x"; rm -rf "$mount"; mkdir -p "$mount"
      case "$file" in
        *.whl|*.zip) ( cd "$mount" && python3 -m zipfile -e "$dir/$file" . ) ;;
        *.tar) tar -xf "$dir/$file" -C "$mount" ;;
        *) tar -xzf "$dir/$file" -C "$mount" ;;
      esac
      src="dir:/work" ;;
    *)
      src="file:/work/$file" ;;
  esac
  echo "sbom.sh: SBOM $file ($src)" >&2
  # --source-name pins the artifact's REAL path into the document: a `dir:` scan would otherwise
  # name the throwaway mount point.
  docker run --rm --network none -v "$mount:/work:ro" -v "$sbom_dir:/out" "$SYFT_IMAGE" \
    scan "$src" --select-catalogers "$SYFT_CATALOGERS" --source-name "$file" -q \
    -o "spdx-json=/out/$base.spdx.json" -o "cyclonedx-json=/out/$base.cdx.json" >&2

  if [ "$scan" -eq 1 ]; then
    [ "$validate_hash" = true ] && note=" (re-hashing the ~2GB DB once for this run)" || note=""
    echo "sbom.sh: scan $file against the pinned snapshot$note" >&2
    # The scan input is the SBOM, not the artifact: that is what makes the honest ceiling above
    # literally true — the scanner's coverage IS the SBOM's coverage, with nothing added.
    #
    # grype re-hashes the whole ~2GB DB on start. That check is worth paying ONCE per run (it is
    # what catches a locally corrupted or swapped DB file, which the lock's recorded digest cannot);
    # paying it per artifact would quadruple a twelve-artifact release for no extra assurance, since
    # every scan in the run reads the same file. So: on for the first scan, off for the rest.
    docker run --rm --network none \
      -e GRYPE_DB_CACHE_DIR=/db -e GRYPE_DB_AUTO_UPDATE=false -e GRYPE_DB_VALIDATE_AGE=false \
      -e "GRYPE_DB_VALIDATE_BY_HASH_ON_START=$validate_hash" \
      -v "$db_dir:/db" -v "$sbom_dir:/out" "$GRYPE_IMAGE" \
      "sbom:/out/$base.spdx.json" -o json --file "/out/$base.grype.json" -q >&2
    validate_hash=false
  fi
done < "$stage/artifacts.tsv"

cp "$policy" "$sbom_dir/vuln-policy.json"

# --- roll up, gate, and patch the manifest --------------------------------------------------------
DIR="$dir" MANIFEST="$manifest_name" SDK="$sdk" SCANNED="$scan" \
TSV="$stage/artifacts.tsv" LOCK="$lock" POLICY="$sbom_dir/vuln-policy.json" \
SYFT_IMAGE="$SYFT_IMAGE" GRYPE_IMAGE="$GRYPE_IMAGE" \
  python3 "$self_dir/sbom-tool.py" rollup
