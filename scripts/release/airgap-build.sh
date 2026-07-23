#!/usr/bin/env bash
# E15 T4 — build the §45.9 SIGNED OFFLINE air-gap bundle. It stages, under $OUT:
#
#   images/*.tar          OCI images by digest (`docker save`): postgres, object-store,
#                         control-plane, runner, reference-engine, registry (the mirror).
#   runner/               the E14 T5 signed runner HOST package (its own tarball + sig + pub + verify).
#   bin/palai-linux-<a>   the CLI binary (cross-compiled).
#   compose/  helm/       a copy of deploy/compose + deploy/helm (the T3 chart).
#   migrations/           a copy of storage/migrations.
#   install.sh verify.sh airgap.yml runner-verify.sh   the offline install/verify tooling.
#   manifest.json         metadata: version, per-image digests, component list, and the
#                         SBOM/provenance fields (DEFINED but empty — their production is E18;
#                         the manifest SAYS so — honest naming).
#   sha256sums            the signed root: sha256 of every file above.
#   sha256sums.sha256 / sha256sums.sig / palai-airgap-signing.pub
#                         the signature ENVELOPE (NOT self-listed): the openssl ECDSA P-256
#                         detached signature over sha256sums.
#
# SIGNING — ONE tool, reused VERBATIM from E14 T5 (scripts/package/runner/build.sh): the same
# `openssl genpkey EC P-256` / `openssl pkey -pubout` / `openssl dgst -sha256 -sign` commands.
# No second signer is introduced. A release passes an operator-held PALAI_AIRGAP_SIGNING_KEY;
# with none set this mints an EPHEMERAL key so the local proof is self-contained.
#
# HONEST CEILING (plan §T4/§6): the bundle carries HOST-ARCH images (a real release is multi-arch);
# the real air-gapped facility, the operator trust-root/mirror ceremony, and a real private model
# server are the operator leg; SBOM/provenance production is E18.
#
# Env: VERSION (0.1.0), ARCH (host), OUT (dist/airgap-bundle), PALAI_AIRGAP_SIGNING_KEY (optional),
#      PALAI_AIRGAP_IMAGES=build|skip (default build; `skip` stages no image tars — used by the
#      component test, which exercises the sign/verify/digest-chain machinery without a daemon).
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
VERSION="${VERSION:-0.1.0}"
ARCH="${ARCH:-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')}"
OUT="${OUT:-$root/dist/airgap-bundle}"
IMAGES="${PALAI_AIRGAP_IMAGES:-build}"

step() { echo "airgap-build: $*" >&2; }

rm -rf "$OUT"
mkdir -p "$OUT/images" "$OUT/runner" "$OUT/bin"

# --- images ---------------------------------------------------------------------------------
# images_json accumulates one manifest entry per saved image.
images_json=""
add_image() { # ref file id
	local entry
	entry="$(printf '    {"name": "%s", "ref": "%s", "id": "%s", "file": "images/%s"}' \
		"$(basename "$3" | sed 's/\.tar$//')" "$1" "$2" "$3")"
	# shellcheck disable=SC2034
	if [ -z "$images_json" ]; then images_json="$entry"; else images_json="$images_json,
$entry"; fi
}

if [ "$IMAGES" = "build" ]; then
	# Pinned upstream bases — reuse the SAME digests the base compose pins (single source of truth).
	pg_ref="$(grep -oE 'postgres@sha256:[0-9a-f]+' "$root/deploy/compose/compose.yaml" | head -1)"
	s3_ref="$(grep -oE 'chrislusf/seaweedfs@sha256:[0-9a-f]+' "$root/deploy/compose/compose.yaml" | head -1)"
	s3_ref="docker.io/$s3_ref"
	reg_ref="registry:2"

	# Build the three repo images. PALAI_AIRGAP_REUSE_LOCAL=1 reuses an already-built
	# `palai/<name>:local` (same real artifact) instead of invoking BuildKit — the drill sets it
	# on constrained hosts where a concurrent build OOMs BuildKit; a release/CI build leaves it
	# unset and builds fresh from source.
	build_or_reuse() { # name context [dockerfile]
		local name="$1" ctx="$2" df="${3:-}"
		if [ "${PALAI_AIRGAP_REUSE_LOCAL:-}" = "1" ] && docker image inspect "palai/$name:local" >/dev/null 2>&1; then
			step "reuse palai/$name:local (PALAI_AIRGAP_REUSE_LOCAL=1)"
			docker tag "palai/$name:local" "palai/$name:$VERSION"
		elif [ -n "$df" ]; then
			docker build -q -t "palai/$name:$VERSION" -f "$df" "$ctx" >&2
		else
			docker build -q -t "palai/$name:$VERSION" "$ctx" >&2
		fi
	}
	step "build (or reuse) control-plane + runner + reference-engine images"
	build_or_reuse control-plane    "$root" "$root/deploy/compose/control-plane.Dockerfile"
	build_or_reuse runner           "$root" "$root/deploy/compose/runner.Dockerfile"
	build_or_reuse reference-engine "$root/engines/reference"

	step "pull pinned bases (postgres, seaweedfs, registry:2)"
	docker pull -q "$pg_ref"  >&2
	docker pull -q "$s3_ref"  >&2
	docker pull -q "$reg_ref" >&2

	# Save each image and record its config digest (image ID). save_one <local-ref> <file> <manifest-ref>
	save_one() {
		local ref="$1" file="$2" mref="$3" id
		id="$(docker image inspect "$ref" --format '{{.Id}}')"
		step "docker save $ref -> images/$file (id $id)"
		docker save "$ref" -o "$OUT/images/$file"
		add_image "$mref" "$id" "$file"
	}
	save_one "palai/control-plane:$VERSION"    "control-plane.tar"   "palai/control-plane:$VERSION"
	save_one "palai/runner:$VERSION"           "runner.tar"          "palai/runner:$VERSION"
	save_one "palai/reference-engine:$VERSION" "reference-engine.tar" "palai/reference-engine:$VERSION"
	save_one "$pg_ref"                         "postgres.tar"        "$pg_ref"
	save_one "$s3_ref"                         "object-store.tar"    "$s3_ref"
	save_one "$reg_ref"                        "registry.tar"        "$reg_ref"
else
	step "PALAI_AIRGAP_IMAGES=skip — staging no image tars (sign/verify machinery only)"
fi

# --- runner host package (E14 T5, VERBATIM) -------------------------------------------------
step "build the signed E14 runner host package"
OUT="$OUT/runner" ARCH="$ARCH" bash "$root/scripts/package/runner/build.sh" >/dev/null

# --- CLI binary -----------------------------------------------------------------------------
step "cross-compile the CLI (linux/$ARCH)"
( cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" \
	go build -trimpath -buildvcs=false -ldflags='-s -w' -o "$OUT/bin/palai-linux-$ARCH" ./cmd/cli )

# --- compose + helm chart + migrations + air-gap tooling ------------------------------------
step "copy compose, helm chart, migrations, and the offline install/verify tooling"
cp -R "$root/deploy/compose" "$OUT/compose"
cp -R "$root/deploy/helm"    "$OUT/helm"
cp -R "$root/storage/migrations" "$OUT/migrations"
cp "$root/deploy/airgap/install.sh" "$OUT/install.sh"
cp "$root/deploy/airgap/verify.sh"  "$OUT/verify.sh"
cp "$root/deploy/airgap/airgap.yml" "$OUT/airgap.yml"
# The E14 T5 verifier, byte copy — verify.sh execs it for the top-level signature (ONE signer).
cp "$root/scripts/package/runner/verify.sh" "$OUT/runner-verify.sh"
chmod 0755 "$OUT/install.sh" "$OUT/verify.sh" "$OUT/runner-verify.sh"

# --- manifest.json --------------------------------------------------------------------------
step "write manifest.json (SBOM/provenance fields DEFINED but empty — E18)"
cat > "$OUT/manifest.json" <<JSON
{
  "schema": "palai-airgap-manifest/v1",
  "version": "$VERSION",
  "arch": "$ARCH",
  "created_utc": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "maturity": "rc",
  "signing": "openssl ECDSA P-256 detached signature over sha256sums (E14 T5 tool VERBATIM; verify with runner-verify.sh)",
  "images": [
$images_json
  ],
  "components": {
    "runner_host_package": "runner/",
    "cli_binary": "bin/palai-linux-$ARCH",
    "compose": "compose/",
    "helm_chart": "helm/palai/",
    "migrations": "migrations/"
  },
  "sbom": null,
  "sbom_note": "SBOM production is E18; this field is intentionally empty in the RC bundle.",
  "provenance": null,
  "provenance_note": "Provenance/SLSA production is E18; this field is intentionally empty in the RC bundle."
}
JSON

# --- sha256sums (the signed root) + detached signature (E14 T5 openssl, VERBATIM) -----------
step "compute sha256sums (the signed root) over every bundle file"
# List every file EXCEPT the signature envelope (sha256sums*, the pubkey); stable order.
( cd "$OUT" && find . -type f \
	! -name 'sha256sums' ! -name 'sha256sums.sha256' ! -name 'sha256sums.sig' \
	! -name 'palai-airgap-signing.pub' \
	| LC_ALL=C sort | while IFS= read -r f; do sha256sum "$f"; done > sha256sums )

step "sign sha256sums (openssl ECDSA P-256 detached — E14 T5 VERBATIM)"
stage="$(mktemp -d)"; trap 'rm -rf "$stage"' EXIT
if [ -n "${PALAI_AIRGAP_SIGNING_KEY:-}" ]; then
	signing_key="$PALAI_AIRGAP_SIGNING_KEY"
	step "signing with operator key $signing_key"
else
	signing_key="$stage/signing.key"
	openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$signing_key" 2>/dev/null
	step "no PALAI_AIRGAP_SIGNING_KEY set — generated an EPHEMERAL signing key (local proof only)"
fi
openssl pkey -in "$signing_key" -pubout -out "$OUT/palai-airgap-signing.pub"
( cd "$OUT" && sha256sum sha256sums > sha256sums.sha256 )
openssl dgst -sha256 -sign "$signing_key" -out "$OUT/sha256sums.sig" "$OUT/sha256sums"

step "wrote bundle to $OUT"
( cd "$OUT" && ls -1 ) >&2
# Emit the bundle dir on stdout so callers (airgap_test.go, drill.sh) can locate it.
echo "$OUT"
