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
# Usage:
#   RELEASE=self-host-0.2.0 scripts/release/promote.sh            # gate an rc promote
#   scripts/release/promote.sh self-host-0.2.0 stable            # gate a stable promote (awaits operator legs)
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
exec go run ./tests/uat/cmd/promote --release "$release" --to "$to"
