#!/usr/bin/env bash
# E15 T6 — the mechanical SH-2 promote gate (plan §7): "a release without rollback/restore proof cannot be
# promoted." It REFUSES to tag/promote a release unless its evidence bundle (1) verifies clean through the
# shared verifier AND (2) carries a COMPLETE UpgradeProof (app + engine-alias rollback, drained) + a restore/DR
# proof (BackupProof / RestoreVerifyProof / DrillProof). A promote to `stable` ALSO awaits the E14 §6 operator
# legs 1-2 (real cloud-VM install + separate-host restore) via an operator_attestation note in the manifest —
# NEVER auto-claimed here; absent, the beyond-rc promote is refused. The refusal logic is Docker-free Go
# (uat.PromoteGate), unit-pinned by TestPromoteGateRefusesWithoutRollbackAndRestore, so this script is a thin,
# testable wrapper. Exits non-zero on any refusal.
#
#
# E18 T4 adds the SUPPLY-CHAIN half (SEC-101's promotion arm): when a release ARTIFACT directory is named,
# scripts/release/release-verify.sh must verify it OFFLINE and clean before any tag is blessed — a tampered
# byte anywhere in the index, an artifact, an SBOM, the attestation or the signature stops the promote here.
# Naming the dir is REQUIRED for a promote beyond rc: the SH-3 stable flip may not bless artifacts nobody
# verified. An rc may be gated on evidence alone (the E15 T6 contract, unchanged) — a release family that
# must ALWAYS carry a verified artifact set is E18 T10's SupplyChainProof, not this wrapper's rule.
#
# Usage:
#   RELEASE=self-host-0.2.0 scripts/release/promote.sh            # gate an rc promote
#   scripts/release/promote.sh self-host-0.2.0 stable            # gate a stable promote (awaits operator legs)
# Env:
#   PALAI_RELEASE_DIR      the built release directory to verify before promoting (REQUIRED for `stable`)
#   PALAI_RELEASE_PUBKEY   the OUT-OF-BAND trust root for it (never taken from inside the release dir)
set -euo pipefail
root="$(git rev-parse --show-toplevel)"
cd "$root"
# Two calling forms: `RELEASE=<name> promote.sh [target]` (target is $1) or `promote.sh <name> [target]`.
if [ -n "${RELEASE:-}" ]; then
  release="$RELEASE"
  to="${1:-rc}"
else
  release="${1:-}"
  to="${2:-rc}"
fi
if [ -z "$release" ]; then
  echo "usage: RELEASE=<name> scripts/release/promote.sh [rc|stable]  (or: promote.sh <name> [rc|stable])" >&2
  exit 2
fi
if [ -z "${PALAI_RELEASE_DIR:-}" ] && [ "$to" != "rc" ]; then
  echo "promote REFUSED: a promote to '$to' must name the release artifact directory it blesses" >&2
  echo "promote: set PALAI_RELEASE_DIR=<built release dir> and PALAI_RELEASE_PUBKEY=<out-of-band key>" >&2
  exit 1
fi
if [ -n "${PALAI_RELEASE_DIR:-}" ]; then
  echo "promote: verifying the release artifacts offline before the evidence gate ..." >&2
  if ! scripts/release/release-verify.sh "$PALAI_RELEASE_DIR" "${PALAI_RELEASE_PUBKEY:-}"; then
    echo "promote REFUSED: $PALAI_RELEASE_DIR did not verify — a release whose artifacts do not verify cannot be tagged" >&2
    exit 1
  fi
fi
exec go run ./tests/uat/cmd/promote --release "$release" --to "$to"
