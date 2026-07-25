#!/usr/bin/env bash
# E16 T7 — build the three Palai SDK packages LOCALLY with provenance: per-package artifacts +
# a sha256 manifest + an openssl P-256 DETACHED signature over the signed root `sha256sums` +
# a build-input record (git ref + toolchain versions). This BUILDS + CHECKSUMS + SIGNS only —
# it does NOT PUBLISH (npm/PyPI/Go proxy is E18 §T5/§6 work). The manifest SAYS so.
#
# E18 T2 retired the "sbom/provenance are intentionally null" placeholders: `sbom` now carries REAL
# SPDX + CycloneDX documents (scripts/release/sbom.sh, digest-pinned generator) and `provenance`
# names the build-input record + the openssl-signed root that this script has always produced. The
# one field still null is `provenance.attestation` — the in-toto/SLSA statement belongs to
# scripts/release/provenance.sh over a release DIRECTORY, and the manifest says exactly that.
#
#   packages/palai-sdk-<v>.tgz              @palai/sdk  (npm pack — no install, no network)
#   packages/palai-<v>-py3-none-any.whl     python wheel  }  uv build (hatchling backend)
#   packages/palai-<v>.tar.gz               python sdist  }
#   packages/palai-go-sdk-<v>-src.tar.gz    Go module SOURCE snapshot — deterministic tar. Go
#                                           packages are consumed AS SOURCE by tag (no wheel/tgz
#                                           build artifact); this tarball is the source PROVENANCE,
#                                           the published unit is the git tag (E18). Honest note.
#   build-input.json   git ref/commit + toolchain versions (node/npm, python/uv, go) + built_at.
#   sbom/              SPDX + CycloneDX per package, the license inventory, and the scanner report
#                      when the pinned DB snapshot is hydrated (scripts/release/sbom.sh).
#   manifest.json      schema, version, per-package {file, sha256, status, sbom}, sbom + provenance.
#   sha256sums         the SIGNED ROOT: sha256 of every file above.
#   sha256sums.sha256 / sha256sums.sig / palai-sdk-signing.pub   the signature ENVELOPE.
#   sdk-verify.sh      the offline verifier (copy of scripts/release/sdk-verify.sh).
#   runner-verify.sh   BYTE COPY of scripts/package/runner/verify.sh — the E14 T5 verifier VERBATIM.
#                      There is exactly ONE signing tool across the repo; no second signer.
#
# SIGNING — the SAME openssl `genpkey EC P-256` / `pkey -pubout` / `dgst -sha256 -sign` commands as
# E14 T5 (scripts/package/runner/build.sh) and E15 T4 (airgap-build.sh). A release passes an
# operator-held PALAI_SDK_SIGNING_KEY; with none set this mints an EPHEMERAL key so the local proof
# is self-contained.
#
# Env: VERSION (0.1.0), OUT (dist/sdk-bundle), PALAI_SDK_SIGNING_KEY (optional),
#      PALAI_SDK_PACKAGES ("typescript python go" default; subset for a scoped/hermetic build —
#      the component test sets "go", which needs only go+tar, no npm/uv/network).
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
VERSION="${VERSION:-0.1.0}"
OUT="${OUT:-$root/dist/sdk-bundle}"
PACKAGES="${PALAI_SDK_PACKAGES:-typescript python go}"
# Fixed mtime for the deterministic Go source tar (touch -t format), E14 T5 idiom.
MTIME='202601010000.00'

step() { echo "sdk-package: $*" >&2; }

rm -rf "$OUT"
mkdir -p "$OUT/packages"

# pkgs_json accumulates one manifest entry per package (built OR honestly-skipped).
pkgs_json=""
add_pkg() { # name file status
	local sha="" entry
	if [ "$3" = "built" ] && [ -f "$OUT/packages/$2" ]; then
		sha="$(cd "$OUT" && sha256sum "packages/$2" | awk '{print $1}')"
	fi
	entry="$(printf '    {"name": "%s", "file": %s, "sha256": %s, "status": "%s"}' \
		"$1" \
		"$( [ -n "$2" ] && printf '"packages/%s"' "$2" || echo null )" \
		"$( [ -n "$sha" ] && printf '"%s"' "$sha" || echo null )" \
		"$3")"
	if [ -z "$pkgs_json" ]; then pkgs_json="$entry"; else pkgs_json="$pkgs_json,
$entry"; fi
}

want() { case " $PACKAGES " in *" $1 "*) return 0;; *) return 1;; esac; }

# --- TypeScript: npm pack (@palai/sdk) ------------------------------------------------------
if want typescript; then
	if command -v npm >/dev/null 2>&1; then
		step "npm pack @palai/sdk"
		# npm pack tars the package per files/.npmignore with a normalized mtime; no install/network.
		( cd "$root/sdks/typescript" && npm pack --silent --pack-destination "$OUT/packages" >/dev/null )
		tgz="$(cd "$OUT/packages" && ls -1 palai-sdk-*.tgz | head -1)"
		add_pkg "@palai/sdk" "$tgz" built
	else
		step "npm absent — recording @palai/sdk as skipped (honest)"
		add_pkg "@palai/sdk" "" "skipped:npm-absent"
	fi
fi

# --- Python: uv build (sdist + wheel) -------------------------------------------------------
if want python; then
	if command -v uv >/dev/null 2>&1; then
		step "uv build palai (sdist + wheel)"
		( cd "$root/sdks/python" && uv build --out-dir "$OUT/packages" >&2 )
		rm -f "$OUT/packages/.gitignore"  # uv drops a `.gitignore` in the dist dir — not a release artifact
		whl="$(cd "$OUT/packages" && ls -1 palai-*-py3-none-any.whl | head -1)"
		sdist="$(cd "$OUT/packages" && ls -1 palai-*.tar.gz | grep -v go-sdk | head -1)"
		add_pkg "palai (wheel)" "$whl" built
		add_pkg "palai (sdist)" "$sdist" built
	else
		step "uv absent — recording palai (python) as skipped (honest)"
		add_pkg "palai (python)" "" "skipped:uv-absent"
	fi
fi

# --- Go: deterministic SOURCE snapshot (package = source; no build artifact) -----------------
if want go; then
	step "deterministic Go module source snapshot (sdks/go)"
	gostage="$(mktemp -d)"
	# Tracked files only (git ls-files) — a stray untracked file is NOT snapshotted/signed.
	( cd "$root/sdks/go" && git ls-files ) > "$gostage/members"
	if [ ! -s "$gostage/members" ]; then step "sdks/go has no tracked files — abort"; exit 1; fi
	# Stage a copy, normalize mtime/owner, then tar deterministically (E14 T5 idiom).
	godir="$gostage/palai-go-sdk-$VERSION"
	mkdir -p "$godir"
	( cd "$root/sdks/go" && tar -cf - -T "$gostage/members" ) | ( cd "$godir" && tar -xf - )
	find "$godir" -exec touch -t "$MTIME" {} +
	if tar --version 2>/dev/null | grep -qiE 'bsdtar|libarchive'; then
		tar_flags="--uid 0 --gid 0 --numeric-owner --no-mac-metadata --no-xattrs"
	else
		tar_flags="--owner=0 --group=0 --numeric-owner"
	fi
	members="$(cd "$gostage" && find "palai-go-sdk-$VERSION" -type f | LC_ALL=C sort)"
	go_tgz="palai-go-sdk-$VERSION-src.tar.gz"
	# shellcheck disable=SC2086  # our own space-safe lists, intended to word-split
	( cd "$gostage" && tar $tar_flags -cf - $members ) | gzip -n -9 > "$OUT/packages/$go_tgz"
	rm -rf "$gostage"
	add_pkg "github.com/palgroup/palai/sdks/go (source)" "$go_tgz" built
fi

# --- build-input record ---------------------------------------------------------------------
step "write build-input.json (git ref + toolchain versions)"
tool_ver() { command -v "$1" >/dev/null 2>&1 && { $1 $2 2>&1 | head -1; } || echo "absent"; }
cat > "$OUT/build-input.json" <<JSON
{
  "schema": "palai-sdk-build-input/v1",
  "version": "$VERSION",
  "git_commit": "$(git -C "$root" rev-parse HEAD 2>/dev/null || echo unknown)",
  "git_describe": "$(git -C "$root" describe --tags --always --dirty 2>/dev/null || echo unknown)",
  "built_at_utc": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "packages_requested": "$PACKAGES",
  "toolchains": {
    "node": "$(tool_ver node --version)",
    "npm":  "$(tool_ver npm --version)",
    "python": "$(tool_ver python3 --version)",
    "uv":   "$(tool_ver uv --version)",
    "go":   "$(tool_ver go version)"
  }
}
JSON

# --- manifest.json ---------------------------------------------------------------------------
# E18 T2 retires the "sbom/provenance are intentionally null" note: `provenance` now names the
# record this script has always actually produced, and `sbom` is filled in below by
# scripts/release/sbom.sh with REAL SPDX + CycloneDX documents (it rewrites both `sbom` and
# `sbom_note`). The placeholder written here is what survives only when the generator is absent.
step "write manifest.json"
cat > "$OUT/manifest.json" <<JSON
{
  "schema": "palai-sdk-manifest/v1",
  "version": "$VERSION",
  "api_version": "2026-07-16",
  "created_utc": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "maturity": "rc",
  "signing": "openssl ECDSA P-256 detached signature over sha256sums (E14 T5 tool VERBATIM; verify with sdk-verify.sh)",
  "note": "LOCAL build + checksum + provenance only. PUBLISH to npm/PyPI/Go proxy is E18 (supply-chain).",
  "packages": [
$pkgs_json
  ],
  "sbom": null,
  "sbom_note": "not yet produced — scripts/release/sbom.sh runs below and replaces this",
  "provenance": {
    "build_input": "build-input.json",
    "signed_root": "sha256sums",
    "signature": ["sha256sums.sha256", "sha256sums.sig"],
    "signer": "openssl ECDSA P-256 detached (E14 T5 tool, VERBATIM — the one signing tool in this repo)",
    "verify": "sdk-verify.sh <bundle> <out-of-band-pubkey>",
    "attestation": null
  },
  "provenance_note": "REAL and offline-verifiable: build-input.json records the git ref and the toolchain versions this bundle was built from, and sha256sums is signed with the repo's one signing tool. \`attestation\` stays null because the in-toto/SLSA v1 statement is written by scripts/release/provenance.sh (E18 T3) over a release DIRECTORY, and an SDK bundle is not one — carrying the SDK packages into the release index is release-orchestration work (plan §T5). No Sigstore, Fulcio or transparency-log claim is made here or anywhere."
}
JSON

# --- SBOM + license inventory (+ vulnerability scan when the pinned DB is hydrated) ------------
# BEFORE the sha256sums root below, so the signature covers every SBOM file. The generator runs in
# a digest-pinned container; when it cannot run, the manifest says SO rather than looking scanned.
sbom_skip=""
if [ "${PALAI_SDK_SBOM:-auto}" = "off" ]; then
	sbom_skip="disabled by PALAI_SDK_SBOM=off"
elif ! command -v docker >/dev/null 2>&1; then
	sbom_skip="docker absent — the SBOM generator is a digest-pinned container"
fi
if [ -z "$sbom_skip" ]; then
	db_dir="${PALAI_VULN_DB_DIR:-$root/dist/vulndb}"
	db_schema="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["db_schema"])' \
		"$root/scripts/release/vulndb.lock.json")"
	scan_args=""
	if [ ! -f "$db_dir/$db_schema/import.json" ]; then
		scan_args="--no-scan"
		step "the pinned vulnerability DB is not hydrated at $db_dir — SBOMs only, recorded as unscanned"
	fi
	# shellcheck disable=SC2086  # intentional word-split of a 0-or-1 flag
	SBOM_NO_SCAN_REASON="the pinned vulnerability DB snapshot is not hydrated on this host" \
		"$root/scripts/release/sbom.sh" --dir "$OUT" --sdk $scan_args
else
	step "SBOM skipped ($sbom_skip) — recording it honestly in manifest.json"
	SKIP="$sbom_skip" python3 - "$OUT/manifest.json" <<'PY'
import json, os, sys
p = sys.argv[1]
doc = json.load(open(p, encoding="utf-8"))
doc["sbom"] = None
doc["sbom_note"] = ("NO SBOM in this bundle: %s. A null sbom is not a clean result — it is the "
                    "absence of one, and a release bundle never ships this way." % os.environ["SKIP"])
with open(p, "w", encoding="utf-8") as fh:
    json.dump(doc, fh, indent=2, ensure_ascii=False)
    fh.write("\n")
PY
fi

# --- ship the verifier (E14 T5 verbatim + the SDK offline wrapper) --------------------------
step "stage verifiers (runner-verify.sh = E14 T5 VERBATIM; sdk-verify.sh = offline wrapper)"
cp "$root/scripts/package/runner/verify.sh" "$OUT/runner-verify.sh"
cp "$root/scripts/release/sdk-verify.sh"     "$OUT/sdk-verify.sh"
chmod 0755 "$OUT/runner-verify.sh" "$OUT/sdk-verify.sh"

# --- sha256sums (the signed root) + detached signature (E14 T5 openssl, VERBATIM) -----------
step "compute sha256sums (the signed root) over every bundle file"
# Every file EXCEPT the signature envelope; stable order (airgap-build.sh emsali).
( cd "$OUT" && find . -type f \
	! -name 'sha256sums' ! -name 'sha256sums.sha256' ! -name 'sha256sums.sig' \
	! -name 'palai-sdk-signing.pub' \
	| LC_ALL=C sort | while IFS= read -r f; do sha256sum "$f"; done > sha256sums )

step "sign sha256sums (openssl ECDSA P-256 detached — E14 T5 VERBATIM)"
stage="$(mktemp -d)"; trap 'rm -rf "$stage"' EXIT
if [ -n "${PALAI_SDK_SIGNING_KEY:-}" ]; then
	signing_key="$PALAI_SDK_SIGNING_KEY"
	step "signing with operator key $signing_key"
else
	signing_key="$stage/signing.key"
	openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$signing_key" 2>/dev/null
	step "no PALAI_SDK_SIGNING_KEY set — generated an EPHEMERAL signing key (local proof only)"
fi
openssl pkey -in "$signing_key" -pubout -out "$OUT/palai-sdk-signing.pub"
( cd "$OUT" && sha256sum sha256sums > sha256sums.sha256 )
openssl dgst -sha256 -sign "$signing_key" -out "$OUT/sha256sums.sig" "$OUT/sha256sums"

step "wrote bundle to $OUT"
( cd "$OUT" && ls -1 ) >&2
# Emit the bundle dir on stdout so callers (sdk_package_test.go) can locate it.
echo "$OUT"
