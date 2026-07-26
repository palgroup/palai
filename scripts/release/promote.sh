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
# That is ALL it adds. Whether a release family must ALWAYS carry a verified artifact set — including the
# SH-3 stable flip — is E18 T10's SupplyChainProof, NOT this wrapper's rule: a fence here that refused an
# unnamed dir would run BEFORE the evidence gate and shadow the E15 T6 operator-leg refusal that
# scripts/uat/sh2 and scripts/uat/sdk-parity both grep for (pinned by TestPromoteReachesTheEvidenceGate).
#
# E18 T5 adds the TWO-PERSON half, and it is OPT-IN for the same reason: when PALAI_RELEASE_APPROVAL names a
# palai.release-approval/v1 record, the promote is ALSO judged against release-policy.md's two-person rule
# (builder != approver, both authorized, protected environment, no admin bypass — uat.ApprovalGate, unit-pinned
# by tests/uat/approval_test.go). scripts/release/publish.sh ALWAYS sets it, so a publication cannot skip it; a
# bare `promote.sh rc` stays the evidence gate it has always been and says so in its PASS line.
#
# Usage:
#   RELEASE=self-host-0.2.0 scripts/release/promote.sh            # gate an rc promote
#   scripts/release/promote.sh self-host-0.2.0 stable            # gate a stable promote (awaits operator legs)
# Env:
#   PALAI_RELEASE_DIR      a built release directory to verify offline before promoting (when named)
#   PALAI_RELEASE_PUBKEY   the OUT-OF-BAND trust root for it (never taken from inside the release dir)
#   PALAI_RELEASE_APPROVAL the protected environment's two-person approval record (when named)
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
if [ -n "${PALAI_RELEASE_DIR:-}" ]; then
  echo "promote: verifying the release artifacts offline before the evidence gate ..." >&2
  if ! scripts/release/release-verify.sh "$PALAI_RELEASE_DIR" "${PALAI_RELEASE_PUBKEY:-}"; then
    echo "promote REFUSED: $PALAI_RELEASE_DIR did not verify — a release whose artifacts do not verify cannot be tagged" >&2
    exit 1
  fi
fi
if [ -n "${PALAI_RELEASE_APPROVAL:-}" ]; then
  exec go run ./tests/uat/cmd/promote --release "$release" --to "$to" --approval "$PALAI_RELEASE_APPROVAL"
fi
exec go run ./tests/uat/cmd/promote --release "$release" --to "$to"
