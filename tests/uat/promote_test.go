package uat

import (
	"os"
	"path/filepath"
	"testing"
)

// completeUpgradeCase returns a manifest whose single case carries a COMPLETE UpgradeProof (both rollbacks +
// drained) and a restore proof (a RestoreVerifyProof) — the shape PromoteGate must accept for rc.
func promotableManifest() map[string]any {
	m := baseManifest()
	c := caseOf(m)
	c["upgrade_claim"] = "upgraded"
	c["upgrade_proof"] = map[string]any{
		"from_version": "v0.1.0-0-gaaaaaaa", "to_version": "v0.2.0-rc1-gbbbbbbb",
		"surviving_run_id": "run_s", "surviving_run_completed": true,
		"continuity_event_ids":    []any{"response.created", "response.completed"},
		"event_continuity_digest": hashParts("response.created", "response.completed"),
		"app_rollback":            true, "engine_alias_rollback": true, "rollback_drained": true,
		"step_ids": UpgradeStepIDs, "journey_digest": hashParts(UpgradeStepIDs...),
	}
	c["restore_verify_claim"] = "verified"
	c["restore_verify_proof"] = map[string]any{
		"archive_checksum": true, "migration_version": true, "tenant_ids": true,
		"run_retrieval": true, "rls_isolation": true, "secret_decrypt": true,
	}
	return m
}

// TestPromoteGateRefusesWithoutRollbackAndRestore pins the SH-2 exit-gate sentence (plan §7): promote.sh (which
// wraps PromoteGate) REFUSES a bundle that lacks a rollback proof or a restore/DR proof, and a beyond-rc
// promote also refuses without the operator-leg attestation.
func TestPromoteGateRefusesWithoutRollbackAndRestore(t *testing.T) {
	// A complete bundle passes rc.
	if r := PromoteGate(marshal(t, promotableManifest()), "rc"); len(r) != 0 {
		t.Fatalf("a bundle with rollback + restore proof must promote to rc, got refusals: %v", r)
	}

	// No upgrade proof at all -> refused.
	m := baseManifest()
	c := caseOf(m)
	c["restore_verify_claim"] = "verified"
	c["restore_verify_proof"] = map[string]any{"archive_checksum": true, "migration_version": true, "tenant_ids": true, "run_retrieval": true, "rls_isolation": true, "secret_decrypt": true}
	if r := PromoteGate(marshal(t, m), "rc"); len(r) == 0 {
		t.Fatal("a bundle with NO UpgradeProof must be refused (no rollback proof)")
	}

	// Upgrade proof present but the rollback did NOT drain the active run (MF-3) -> refused.
	m = promotableManifest()
	caseOf(m)["upgrade_proof"].(map[string]any)["rollback_drained"] = false
	if r := PromoteGate(marshal(t, m), "rc"); len(r) == 0 {
		t.Fatal("an UpgradeProof whose rollback did not drain the active run must be refused (T2 MF-3)")
	}

	// Rollback proof present but NO restore/DR proof -> refused.
	m = promotableManifest()
	delete(caseOf(m), "restore_verify_claim")
	delete(caseOf(m), "restore_verify_proof")
	if r := PromoteGate(marshal(t, m), "rc"); len(r) == 0 {
		t.Fatal("a bundle with NO restore/DR proof must be refused")
	}

	// A complete rc bundle still cannot promote to STABLE without the operator-leg attestation.
	if r := PromoteGate(marshal(t, promotableManifest()), "stable"); len(r) == 0 {
		t.Fatal("promote to stable must be refused without an operator_attestation (E14 §6 legs 1-2)")
	}
	// With the attestation present, the stable-only refusal clears (rc invariants still hold).
	m = promotableManifest()
	m["operator_attestation"] = map[string]any{"leg1_cloud_vm_install": "2026-07-20 run by ops", "leg2_separate_host_restore": "2026-07-21 run by ops"}
	if r := PromoteGate(marshal(t, m), "stable"); len(r) != 0 {
		t.Fatalf("a complete bundle WITH the operator attestation must promote to stable, got: %v", r)
	}
}

// TestSelfHost02PromotesToRCNotStable proves the SHIPPED self-host-0.2.0 bundle promotes to rc (it carries the
// rollback + restore proofs) but is HONESTLY refused for stable — the operator legs have not run, and the gate
// never auto-claims they did.
func TestSelfHost02PromotesToRCNotStable(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(moduleDir(t), "evidence", "releases", "self-host-0.2.0", "manifest.json"))
	if err != nil {
		t.Fatalf("read self-host-0.2.0 manifest: %v", err)
	}
	if r := PromoteGate(raw, "rc"); len(r) != 0 {
		t.Fatalf("self-host-0.2.0 must promote to rc, got refusals: %v", r)
	}
	if r := PromoteGate(raw, "stable"); len(r) == 0 {
		t.Fatal("self-host-0.2.0 must be refused for stable — the E14 §6 operator legs have not run (no operator_attestation)")
	}
}

// moduleDir walks up to the module root (the dir holding go.mod). Named to avoid colliding with the
// uat-tagged repoRoot in self_host_journey_test.go.
func moduleDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
