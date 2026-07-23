package uat

import (
	"encoding/json"
	"strings"
	"testing"
)

// baseManifest returns a fresh, fully-populated valid manifest as a mutable map so each
// case can drop or tamper one field.
func baseManifest() map[string]any {
	return map[string]any{
		"release":     "local-live-0.1.0",
		"git_sha":     "abc1234",
		"api_version": "v1",
		"migration":   "000002_retention",
		"captured_at": "2026-07-18T10:00:00Z",
		"cases": []any{
			map[string]any{
				"id":                  "LP-003",
				"status":              "PASS",
				"proof_class":         "live-provider",
				"run_id":              "run_abc",
				"image_digest":        "sha256:" + strings.Repeat("a", 64),
				"provider_request_id": "chatcmpl-xyz",
				"mtls_enroll":         "runner-local cn=controller",
				"terminal":            map[string]any{"type": "response.completed", "count": 1},
				"usage":               map[string]any{"input_tokens": 5, "output_tokens": 3, "total_tokens": 8},
				"db_assertions":       []any{"runs.state=completed"},
				"checksum":            "sha256:" + strings.Repeat("b", 64),
			},
		},
	}
}

func marshal(t *testing.T, m map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return raw
}

func caseOf(m map[string]any) map[string]any { return m["cases"].([]any)[0].(map[string]any) }

func hasKind(fs []Finding, kind string) bool {
	for _, f := range fs {
		if f.Kind == kind {
			return true
		}
	}
	return false
}

// completeRecoveryProof returns a fresh §26.12 RecoveryProof map with every field group populated —
// the shape the recovery.proof.v1 journal event serializes to.
func completeRecoveryProof() map[string]any {
	return map[string]any{
		"previous_attempt_id":    "att_prev",
		"new_attempt_id":         "att_new",
		"level":                  "compatible_checkpoint",
		"checkpoint_id":          "chk_1",
		"transcript_boundary_id": "bnd_1",
		"replayed_tool_calls":    []any{},
		"reused_tool_calls":      []any{"tcall_a"},
		"config_model_changes":   []any{},
		"semantic_loss_assessed": true,
		"duration_ms":            42,
	}
}

// TestRecoveryProofFieldsComplete pins REC-006 (spec §26.12): a case that claims recovery passes only
// with a COMPLETE proof; dropping any one field group makes it a Finding.
func TestRecoveryProofFieldsComplete(t *testing.T) {
	m := baseManifest()
	c := caseOf(m)
	c["recovery_claim"] = "continued"
	c["recovery_proof"] = completeRecoveryProof()
	if f := VerifyManifest(marshal(t, m), nil); len(f) != 0 {
		t.Fatalf("a complete §26.12 recovery proof should pass, got %v", f)
	}

	for _, field := range []string{
		"previous_attempt_id", "new_attempt_id", "level", "checkpoint_id", "transcript_boundary_id",
		"replayed_tool_calls", "reused_tool_calls", "config_model_changes", "semantic_loss_assessed", "duration_ms",
	} {
		m := baseManifest()
		c := caseOf(m)
		c["recovery_claim"] = "continued"
		proof := completeRecoveryProof()
		delete(proof, field)
		c["recovery_proof"] = proof
		if !hasKind(VerifyManifest(marshal(t, m), nil), "invalid") {
			t.Fatalf("a recovery proof missing %q must be a Finding", field)
		}
	}
}

// TestVerifierRejectsContinuedLogWithoutProof pins the REC-006 core: a "continued"/"resumed" marker
// is NEVER evidence on its own — a recovery claim with no §26.12 proof block is a Finding.
func TestVerifierRejectsContinuedLogWithoutProof(t *testing.T) {
	m := baseManifest()
	c := caseOf(m)
	c["recovery_claim"] = "resumed" // claims recovery but carries NO proof
	if !hasKind(VerifyManifest(marshal(t, m), nil), "missing") {
		t.Fatal("a recovery claim with no §26.12 RecoveryProof must be a Finding (continued/resumed alone is not proof)")
	}
}

// TestDedupeClaimRequiresOriginalLinkage pins AUT-001 (spec §20.x): a duplicated-event case does NOT pass
// on a "deduplicated" marker alone — it must carry a DedupeProof that links a DISTINCT original to the
// duplicate and shows exactly one canonical action. Missing proof is a Finding; a self-linked duplicate or
// a fan-out to two actions is invalid.
func TestDedupeClaimRequiresOriginalLinkage(t *testing.T) {
	m := baseManifest()
	c := caseOf(m)
	c["dedupe_claim"] = "deduplicated"
	c["dedupe_proof"] = map[string]any{
		"original_delivery_id": "del_orig", "duplicate_delivery_id": "del_dup", "canonical_action_count": 1,
	}
	if f := VerifyManifest(marshal(t, m), nil); len(f) != 0 {
		t.Fatalf("a complete dedupe proof should pass, got %v", f)
	}

	m = baseManifest()
	caseOf(m)["dedupe_claim"] = "deduplicated" // claims dedupe but carries NO proof
	if !hasKind(VerifyManifest(marshal(t, m), nil), "missing") {
		t.Fatal("a dedupe claim with no proof must be a Finding (a 'deduplicated' marker is not original-linkage proof)")
	}

	for _, bad := range []map[string]any{
		{"original_delivery_id": "del_same", "duplicate_delivery_id": "del_same", "canonical_action_count": 1}, // no distinct original
		{"original_delivery_id": "del_orig", "duplicate_delivery_id": "del_dup", "canonical_action_count": 2},  // fanned out to 2 actions
	} {
		m = baseManifest()
		c = caseOf(m)
		c["dedupe_claim"] = "deduplicated"
		c["dedupe_proof"] = bad
		if !hasKind(VerifyManifest(marshal(t, m), nil), "invalid") {
			t.Fatalf("an invalid dedupe proof %v must be a Finding", bad)
		}
	}
}

// TestOccurrenceClaimRequiresSingleCanonical pins AUT-007 (spec §33): a scheduler-occurrence case must
// carry an OccurrenceProof with the occurrence id, both instants (planned/admitted, so lateness is
// visible), and exactly one canonical occurrence — competing replicas racing to two rows is not proof.
func TestOccurrenceClaimRequiresSingleCanonical(t *testing.T) {
	m := baseManifest()
	c := caseOf(m)
	c["occurrence_claim"] = "single_canonical"
	c["occurrence_proof"] = map[string]any{
		"occurrence_id": "occ_1", "planned_at": "2026-07-21T00:00:00Z", "admitted_at": "2026-07-21T00:00:01Z", "canonical_count": 1,
	}
	if f := VerifyManifest(marshal(t, m), nil); len(f) != 0 {
		t.Fatalf("a complete occurrence proof should pass, got %v", f)
	}

	m = baseManifest()
	caseOf(m)["occurrence_claim"] = "single_canonical"
	if !hasKind(VerifyManifest(marshal(t, m), nil), "missing") {
		t.Fatal("an occurrence claim with no proof must be a Finding")
	}

	for _, field := range []string{"occurrence_id", "planned_at", "admitted_at"} {
		m = baseManifest()
		c = caseOf(m)
		c["occurrence_claim"] = "single_canonical"
		proof := map[string]any{
			"occurrence_id": "occ_1", "planned_at": "2026-07-21T00:00:00Z", "admitted_at": "2026-07-21T00:00:01Z", "canonical_count": 1,
		}
		delete(proof, field)
		c["occurrence_proof"] = proof
		if !hasKind(VerifyManifest(marshal(t, m), nil), "invalid") {
			t.Fatalf("an occurrence proof missing %q must be a Finding", field)
		}
	}
	// Two canonical occurrences (a replica race that produced duplicates) is invalid.
	m = baseManifest()
	c = caseOf(m)
	c["occurrence_claim"] = "single_canonical"
	c["occurrence_proof"] = map[string]any{
		"occurrence_id": "occ_1", "planned_at": "2026-07-21T00:00:00Z", "admitted_at": "2026-07-21T00:00:01Z", "canonical_count": 2,
	}
	if !hasKind(VerifyManifest(marshal(t, m), nil), "invalid") {
		t.Fatal("an occurrence proof with canonical_count != 1 must be a Finding")
	}
}

// TestCallbackClaimRequiresSingleSemanticDelivery pins AUT-011/013 (spec §21.x): a callback case must
// carry a CallbackProof with both delivery ids, at least one attempt, exactly one semantic receipt at the
// receiver (a signed retry deduped to one), and a run terminal left intact — a callback counted twice or
// one that mutated the run terminal is not proof.
func TestCallbackClaimRequiresSingleSemanticDelivery(t *testing.T) {
	full := func() map[string]any {
		return map[string]any{
			"delivery_id": "del_cb", "webhook_delivery_id": "whd_1", "attempts": 2,
			"receiver_receipt_count": 1, "run_terminal_intact": true,
		}
	}
	m := baseManifest()
	c := caseOf(m)
	c["callback_claim"] = "delivered_once"
	c["callback_proof"] = full()
	if f := VerifyManifest(marshal(t, m), nil); len(f) != 0 {
		t.Fatalf("a complete callback proof should pass, got %v", f)
	}

	m = baseManifest()
	caseOf(m)["callback_claim"] = "delivered_once"
	if !hasKind(VerifyManifest(marshal(t, m), nil), "missing") {
		t.Fatal("a callback claim with no proof must be a Finding")
	}

	for _, mutate := range []func(map[string]any){
		func(p map[string]any) { delete(p, "delivery_id") },
		func(p map[string]any) { delete(p, "webhook_delivery_id") },
		func(p map[string]any) { p["attempts"] = 0 },                // no delivery attempt
		func(p map[string]any) { p["receiver_receipt_count"] = 2 },  // counted twice (dedup failed)
		func(p map[string]any) { p["run_terminal_intact"] = false }, // callback disturbed the run terminal
	} {
		m = baseManifest()
		c = caseOf(m)
		c["callback_claim"] = "delivered_once"
		proof := full()
		mutate(proof)
		c["callback_proof"] = proof
		if !hasKind(VerifyManifest(marshal(t, m), nil), "invalid") {
			t.Fatalf("an invalid callback proof %v must be a Finding", proof)
		}
	}
}

// TestAdvertisingClaimRequiresHonestProof pins EXT-001/002 (spec §28.5): an advertising case does NOT pass
// on a marker alone — it must carry an AdvertisingProof with a hashed advertised schema list, at least one
// tool name, and an HONEST selection mode. Missing proof is a Finding; an empty hash, no tool names, or an
// unnamed/other mode is invalid — a "forced" call is named "forced", never dressed as spontaneous.
func TestAdvertisingClaimRequiresHonestProof(t *testing.T) {
	full := func() map[string]any {
		return map[string]any{
			"advertised_schema_hash": "sha256:" + strings.Repeat("d", 64),
			"tool_names":             []any{"palai.workspace.file"}, "mode": "spontaneous",
		}
	}
	m := baseManifest()
	c := caseOf(m)
	c["advertising_claim"] = "advertised"
	c["advertising_proof"] = full()
	if f := VerifyManifest(marshal(t, m), nil); len(f) != 0 {
		t.Fatalf("a complete advertising proof should pass, got %v", f)
	}
	// A forced mode is a valid, HONEST proof — the point is that it is NAMED forced, not that it is rejected.
	m = baseManifest()
	c = caseOf(m)
	c["advertising_claim"] = "advertised"
	forced := full()
	forced["mode"] = "forced"
	c["advertising_proof"] = forced
	if f := VerifyManifest(marshal(t, m), nil); len(f) != 0 {
		t.Fatalf("a forced advertising proof (honestly named) should pass, got %v", f)
	}

	m = baseManifest()
	caseOf(m)["advertising_claim"] = "advertised" // no proof
	if !hasKind(VerifyManifest(marshal(t, m), nil), "missing") {
		t.Fatal("an advertising claim with no proof must be a Finding")
	}
	for _, mutate := range []func(map[string]any){
		func(p map[string]any) { delete(p, "advertised_schema_hash") },
		func(p map[string]any) { p["tool_names"] = []any{} }, // advertised nothing
		func(p map[string]any) { p["mode"] = "" },            // hides how the tool was selected
		func(p map[string]any) { p["mode"] = "auto-magic" },  // not an honest mode
	} {
		m = baseManifest()
		c = caseOf(m)
		c["advertising_claim"] = "advertised"
		proof := full()
		mutate(proof)
		c["advertising_proof"] = proof
		if !hasKind(VerifyManifest(marshal(t, m), nil), "invalid") {
			t.Fatalf("an invalid advertising proof %v must be a Finding", proof)
		}
	}
}

// TestSkillClaimRequiresDigestAndScan pins TOL-011 (spec §28.15-28.16): a skill case must carry a SkillProof
// with an exact pinned digest AND a recorded scan result — a "loaded" marker, a skill with no digest (so the
// run could drift to "latest"), or one enabled without a scan is not proof.
func TestSkillClaimRequiresDigestAndScan(t *testing.T) {
	m := baseManifest()
	c := caseOf(m)
	c["skill_claim"] = "enabled"
	c["skill_proof"] = map[string]any{"digest": "sha256:" + strings.Repeat("e", 64), "scan_result": "clean"}
	if f := VerifyManifest(marshal(t, m), nil); len(f) != 0 {
		t.Fatalf("a complete skill proof should pass, got %v", f)
	}

	m = baseManifest()
	caseOf(m)["skill_claim"] = "enabled" // no proof
	if !hasKind(VerifyManifest(marshal(t, m), nil), "missing") {
		t.Fatal("a skill claim with no proof must be a Finding")
	}
	for _, field := range []string{"digest", "scan_result"} {
		m = baseManifest()
		c = caseOf(m)
		c["skill_claim"] = "enabled"
		proof := map[string]any{"digest": "sha256:" + strings.Repeat("e", 64), "scan_result": "clean"}
		delete(proof, field)
		c["skill_proof"] = proof
		if !hasKind(VerifyManifest(marshal(t, m), nil), "invalid") {
			t.Fatalf("a skill proof missing %q must be a Finding", field)
		}
	}
}

// TestCrashIsolationClaimCannotBeMarkerPassed pins EXT-005 (spec §28.21, the E12 EXIT gate): a crash-
// isolation case must carry a CrashIsolationProof where ALL FOUR facts hold — the breaker tripped, the run
// saw tool_unavailable, the control-plane stayed stable, and a separate run flowed. A marker alone, or ANY
// one fact false, is not isolation, so the EXT-005 release gate can never be marker-passed.
func TestCrashIsolationClaimCannotBeMarkerPassed(t *testing.T) {
	full := func() map[string]any {
		return map[string]any{
			"breaker_tripped": true, "tool_unavailable_visible": true,
			"control_plane_stable": true, "other_run_flowed": true,
		}
	}
	m := baseManifest()
	c := caseOf(m)
	c["crash_isolation_claim"] = "isolated"
	c["crash_isolation_proof"] = full()
	if f := VerifyManifest(marshal(t, m), nil); len(f) != 0 {
		t.Fatalf("a complete crash-isolation proof should pass, got %v", f)
	}

	m = baseManifest()
	caseOf(m)["crash_isolation_claim"] = "isolated" // marker, no proof
	if !hasKind(VerifyManifest(marshal(t, m), nil), "missing") {
		t.Fatal("a crash-isolation claim with no proof must be a Finding (EXT-005 cannot be marker-passed)")
	}
	// ANY one isolation fact false makes it invalid — a crash that took the core down, or that the run never
	// saw, is the opposite of isolation.
	for _, field := range []string{"breaker_tripped", "tool_unavailable_visible", "control_plane_stable", "other_run_flowed"} {
		m = baseManifest()
		c = caseOf(m)
		c["crash_isolation_claim"] = "isolated"
		proof := full()
		proof[field] = false
		c["crash_isolation_proof"] = proof
		if !hasKind(VerifyManifest(marshal(t, m), nil), "invalid") {
			t.Fatalf("a crash-isolation proof with %q false must be a Finding", field)
		}
	}
}

// completeProvisioningProof returns a fresh MCI-001 ProvisioningProof map: the created tenant's ids, an
// applied config_policy, the canonical restart-less spine, its re-derivable digest, and zero restarts.
func completeProvisioningProof() map[string]any {
	steps := make([]any, len(ManagedCloudStepIDs))
	for i, s := range ManagedCloudStepIDs {
		steps[i] = s
	}
	return map[string]any{
		"org_id": "org_b", "project_id": "proj_b", "api_key_id": "key_b",
		"config_policy_applied": true, "step_ids": steps,
		"journey_digest": hashParts(ManagedCloudStepIDs...),
		"restart_count":  0,
	}
}

// TestProvisioningClaimRequiresRestartlessSpine pins MCI-001 (plan §T11 T2 + the restart-less spine): a
// provisioning case must carry the created tenant's ids, an applied config_policy, the ordered spine + a
// well-formed digest, and zero restarts. A marker alone is missing; a dropped id, an unapplied policy, a
// malformed digest, or any restart is invalid.
func TestProvisioningClaimRequiresRestartlessSpine(t *testing.T) {
	m := baseManifest()
	c := caseOf(m)
	c["provisioning_claim"] = "provisioned"
	c["provisioning_proof"] = completeProvisioningProof()
	if f := VerifyManifest(marshal(t, m), nil); len(f) != 0 {
		t.Fatalf("a complete provisioning proof should pass, got %v", f)
	}

	m = baseManifest()
	caseOf(m)["provisioning_claim"] = "provisioned" // marker, no proof
	if !hasKind(VerifyManifest(marshal(t, m), nil), "missing") {
		t.Fatal("a provisioning claim with no proof must be a Finding (a 'provisioned' marker is not restart-less-spine proof)")
	}

	for _, mutate := range []func(map[string]any){
		func(p map[string]any) { delete(p, "org_id") },
		func(p map[string]any) { delete(p, "project_id") },
		func(p map[string]any) { delete(p, "api_key_id") },
		func(p map[string]any) { p["config_policy_applied"] = false },          // policy never took
		func(p map[string]any) { p["journey_digest"] = "not-a-sha256" },        // malformed digest
		func(p map[string]any) { p["restart_count"] = 1 },                      // the process restarted
		func(p map[string]any) { p["step_ids"] = []any{"MCI-001", "MCI-002"} }, // short spine
	} {
		m = baseManifest()
		c = caseOf(m)
		c["provisioning_claim"] = "provisioned"
		proof := completeProvisioningProof()
		mutate(proof)
		c["provisioning_proof"] = proof
		if !hasKind(VerifyManifest(marshal(t, m), nil), "invalid") {
			t.Fatalf("an invalid provisioning proof %v must be a Finding", proof)
		}
	}
}

// TestSecretResolveClaimRequiresNoRestartNoLeak pins MCI-002 (plan §T11 T3): a secret-resolve case must
// carry the ref+version resolved by a run with no restart and the value never surfaced. A marker is missing;
// a dropped field, a restart, or a surfaced value is invalid.
func TestSecretResolveClaimRequiresNoRestartNoLeak(t *testing.T) {
	full := func() map[string]any {
		return map[string]any{"ref": "mcp.token", "version": "2", "resolved_in_run": "run_j", "restart_count": 0, "value_surfaced": false}
	}
	m := baseManifest()
	c := caseOf(m)
	c["secret_resolve_claim"] = "resolved"
	c["secret_resolve_proof"] = full()
	if f := VerifyManifest(marshal(t, m), nil); len(f) != 0 {
		t.Fatalf("a complete secret-resolve proof should pass, got %v", f)
	}

	m = baseManifest()
	caseOf(m)["secret_resolve_claim"] = "resolved"
	if !hasKind(VerifyManifest(marshal(t, m), nil), "missing") {
		t.Fatal("a secret-resolve claim with no proof must be a Finding")
	}
	for _, mutate := range []func(map[string]any){
		func(p map[string]any) { delete(p, "ref") },
		func(p map[string]any) { delete(p, "version") },
		func(p map[string]any) { delete(p, "resolved_in_run") },
		func(p map[string]any) { p["restart_count"] = 1 },     // needed a restart
		func(p map[string]any) { p["value_surfaced"] = true }, // the plaintext leaked
	} {
		m = baseManifest()
		c = caseOf(m)
		c["secret_resolve_claim"] = "resolved"
		proof := full()
		mutate(proof)
		c["secret_resolve_proof"] = proof
		if !hasKind(VerifyManifest(marshal(t, m), nil), "invalid") {
			t.Fatalf("an invalid secret-resolve proof %v must be a Finding", proof)
		}
	}
}

// TestIsolationClaimRequiresRealDeny pins MCI-003/004 + TEN-001/002 (the brief's load-bearing cross-tenant
// invariant): an isolation case must carry two DISTINCT tenants, a real 404/403 deny, and zero leaked rows —
// a "isolated" log line is not proof. A marker is missing; a self-isolation, an allow status, or a leaked
// row is invalid.
func TestIsolationClaimRequiresRealDeny(t *testing.T) {
	full := func() map[string]any {
		return map[string]any{"owner_tenant": "org_a", "requester_tenant": "org_b", "resource": "run", "observed_status": 404, "leaked_rows": 0}
	}
	m := baseManifest()
	c := caseOf(m)
	c["isolation_claim"] = "isolated"
	c["isolation_proof"] = full()
	if f := VerifyManifest(marshal(t, m), nil); len(f) != 0 {
		t.Fatalf("a complete isolation proof should pass, got %v", f)
	}
	// A 403 RLS-deny is equally valid.
	m = baseManifest()
	c = caseOf(m)
	c["isolation_claim"] = "isolated"
	rls := full()
	rls["observed_status"] = 403
	c["isolation_proof"] = rls
	if f := VerifyManifest(marshal(t, m), nil); len(f) != 0 {
		t.Fatalf("a 403 RLS-deny isolation proof should pass, got %v", f)
	}

	m = baseManifest()
	caseOf(m)["isolation_claim"] = "isolated"
	if !hasKind(VerifyManifest(marshal(t, m), nil), "missing") {
		t.Fatal("an isolation claim with no proof must be a Finding (a log line is not a cross-tenant deny)")
	}
	for _, mutate := range []func(map[string]any){
		func(p map[string]any) { p["requester_tenant"] = "org_a" }, // same tenant on both sides
		func(p map[string]any) { delete(p, "resource") },
		func(p map[string]any) { p["observed_status"] = 200 }, // tenant B was ALLOWED in
		func(p map[string]any) { p["leaked_rows"] = 1 },       // a tenant-A row leaked
	} {
		m = baseManifest()
		c = caseOf(m)
		c["isolation_claim"] = "isolated"
		proof := full()
		mutate(proof)
		c["isolation_proof"] = proof
		if !hasKind(VerifyManifest(marshal(t, m), nil), "invalid") {
			t.Fatalf("an invalid isolation proof %v must be a Finding", proof)
		}
	}
}

// TestArtifactClaimRequiresMatchedDigest pins MCI-004 (plan §T11 T5): an artifact case must carry the
// artifact id, a well-formed sha256 content digest, a non-empty body, and a digest that matched the bytes.
// A marker is missing; a malformed digest, an empty body, or an unmatched digest is invalid.
func TestArtifactClaimRequiresMatchedDigest(t *testing.T) {
	full := func() map[string]any {
		return map[string]any{"artifact_id": "art_1", "content_digest": "sha256:" + strings.Repeat("f", 64), "byte_len": 12, "digest_matches": true}
	}
	m := baseManifest()
	c := caseOf(m)
	c["artifact_claim"] = "downloaded"
	c["artifact_proof"] = full()
	if f := VerifyManifest(marshal(t, m), nil); len(f) != 0 {
		t.Fatalf("a complete artifact proof should pass, got %v", f)
	}

	m = baseManifest()
	caseOf(m)["artifact_claim"] = "downloaded"
	if !hasKind(VerifyManifest(marshal(t, m), nil), "missing") {
		t.Fatal("an artifact claim with no proof must be a Finding")
	}
	for _, mutate := range []func(map[string]any){
		func(p map[string]any) { delete(p, "artifact_id") },
		func(p map[string]any) { p["content_digest"] = "not-a-sha256" }, // malformed digest
		func(p map[string]any) { p["byte_len"] = 0 },                    // empty body
		func(p map[string]any) { p["digest_matches"] = false },          // digest did not match the bytes
	} {
		m = baseManifest()
		c = caseOf(m)
		c["artifact_claim"] = "downloaded"
		proof := full()
		mutate(proof)
		c["artifact_proof"] = proof
		if !hasKind(VerifyManifest(marshal(t, m), nil), "invalid") {
			t.Fatalf("an invalid artifact proof %v must be a Finding", proof)
		}
	}
}

// TestRefusalClaimRequiresDenyBeforeCompute pins MCI-005 (plan §T11 T6/T7): a refusal case must carry a
// known limit kind, a status matching that kind (429 rate, 402 budget), and no billable compute. A marker is
// missing; an unknown kind, a mismatched status, or compute that started is invalid.
func TestRefusalClaimRequiresDenyBeforeCompute(t *testing.T) {
	for _, ok := range []map[string]any{
		{"limit_kind": "rate", "observed_status": 429, "billable_compute_started": false},
		{"limit_kind": "budget", "observed_status": 402, "billable_compute_started": false},
	} {
		m := baseManifest()
		c := caseOf(m)
		c["refusal_claim"] = "refused"
		c["refusal_proof"] = ok
		if f := VerifyManifest(marshal(t, m), nil); len(f) != 0 {
			t.Fatalf("a complete refusal proof %v should pass, got %v", ok, f)
		}
	}

	m := baseManifest()
	caseOf(m)["refusal_claim"] = "refused"
	if !hasKind(VerifyManifest(marshal(t, m), nil), "missing") {
		t.Fatal("a refusal claim with no proof must be a Finding")
	}
	for _, bad := range []map[string]any{
		{"limit_kind": "gremlin", "observed_status": 429, "billable_compute_started": false}, // unknown kind
		{"limit_kind": "rate", "observed_status": 402, "billable_compute_started": false},    // rate must be 429
		{"limit_kind": "budget", "observed_status": 429, "billable_compute_started": false},  // budget must be 402
		{"limit_kind": "rate", "observed_status": 429, "billable_compute_started": true},     // burned compute
	} {
		m = baseManifest()
		c := caseOf(m)
		c["refusal_claim"] = "refused"
		c["refusal_proof"] = bad
		if !hasKind(VerifyManifest(marshal(t, m), nil), "invalid") {
			t.Fatalf("an invalid refusal proof %v must be a Finding", bad)
		}
	}
}

// TestRouteClaimRequiresDistinctPerProject pins MCI-006 (plan §T11 T8): a route case must carry two projects'
// DISTINCT resolved model ids and distinct connections. A marker is missing; equal models or a shared
// connection is invalid — per-project routing was not proven.
func TestRouteClaimRequiresDistinctPerProject(t *testing.T) {
	full := func() map[string]any {
		return map[string]any{"project_a_model": "gpt-4o-mini", "project_b_model": "gpt-4o", "distinct_connections": true}
	}
	m := baseManifest()
	c := caseOf(m)
	c["route_claim"] = "routed"
	c["route_proof"] = full()
	if f := VerifyManifest(marshal(t, m), nil); len(f) != 0 {
		t.Fatalf("a complete route proof should pass, got %v", f)
	}

	m = baseManifest()
	caseOf(m)["route_claim"] = "routed"
	if !hasKind(VerifyManifest(marshal(t, m), nil), "missing") {
		t.Fatal("a route claim with no proof must be a Finding")
	}
	for _, mutate := range []func(map[string]any){
		func(p map[string]any) { delete(p, "project_a_model") },
		func(p map[string]any) { p["project_b_model"] = "gpt-4o-mini" }, // identical models
		func(p map[string]any) { p["distinct_connections"] = false },    // shared connection
	} {
		m = baseManifest()
		c = caseOf(m)
		c["route_claim"] = "routed"
		proof := full()
		mutate(proof)
		c["route_proof"] = proof
		if !hasKind(VerifyManifest(marshal(t, m), nil), "invalid") {
			t.Fatalf("an invalid route proof %v must be a Finding", proof)
		}
	}
}

// TestBindingClaimRequiresResolvedRef pins MCI-007 (plan §T11 T9): a binding case must carry the binding id,
// a non-empty connection_ref, and a resolution that took the ref path (not the global App fallback). A marker
// is missing; a dropped field or a global-App fallback is invalid.
func TestBindingClaimRequiresResolvedRef(t *testing.T) {
	full := func() map[string]any {
		return map[string]any{"binding_id": "bnd_1", "connection_ref": "github.acme", "resolved_via_ref": true}
	}
	m := baseManifest()
	c := caseOf(m)
	c["binding_claim"] = "bound"
	c["binding_proof"] = full()
	if f := VerifyManifest(marshal(t, m), nil); len(f) != 0 {
		t.Fatalf("a complete binding proof should pass, got %v", f)
	}

	m = baseManifest()
	caseOf(m)["binding_claim"] = "bound"
	if !hasKind(VerifyManifest(marshal(t, m), nil), "missing") {
		t.Fatal("a binding claim with no proof must be a Finding")
	}
	for _, mutate := range []func(map[string]any){
		func(p map[string]any) { delete(p, "binding_id") },
		func(p map[string]any) { delete(p, "connection_ref") }, // fell through to the global App
		func(p map[string]any) { p["resolved_via_ref"] = false },
	} {
		m = baseManifest()
		c = caseOf(m)
		c["binding_claim"] = "bound"
		proof := full()
		mutate(proof)
		c["binding_proof"] = proof
		if !hasKind(VerifyManifest(marshal(t, m), nil), "invalid") {
			t.Fatalf("an invalid binding proof %v must be a Finding", proof)
		}
	}
}

// TestSteerClaimRequiresAppliedCommand pins MCI-008 (plan §T11 T10): a steer case must carry the session, the
// durable command id, its kind, and that it was applied. A marker is missing; a dropped field or an unapplied
// command is invalid.
func TestSteerClaimRequiresAppliedCommand(t *testing.T) {
	full := func() map[string]any {
		return map[string]any{"session_id": "sess_1", "command_id": "cmd_1", "command_kind": "send_message", "applied": true}
	}
	m := baseManifest()
	c := caseOf(m)
	c["steer_claim"] = "steered"
	c["steer_proof"] = full()
	if f := VerifyManifest(marshal(t, m), nil); len(f) != 0 {
		t.Fatalf("a complete steer proof should pass, got %v", f)
	}

	m = baseManifest()
	caseOf(m)["steer_claim"] = "steered"
	if !hasKind(VerifyManifest(marshal(t, m), nil), "missing") {
		t.Fatal("a steer claim with no proof must be a Finding")
	}
	for _, mutate := range []func(map[string]any){
		func(p map[string]any) { delete(p, "session_id") },
		func(p map[string]any) { delete(p, "command_id") },
		func(p map[string]any) { delete(p, "command_kind") },
		func(p map[string]any) { p["applied"] = false }, // the command was rejected
	} {
		m = baseManifest()
		c = caseOf(m)
		c["steer_claim"] = "steered"
		proof := full()
		mutate(proof)
		c["steer_proof"] = proof
		if !hasKind(VerifyManifest(marshal(t, m), nil), "invalid") {
			t.Fatalf("an invalid steer proof %v must be a Finding", proof)
		}
	}
}

// completeInstallProof returns a fresh OPS-002 InstallProof map: the hardened posture, a CA-verified edge, a
// green config-validate + doctor, the canonical restart-less install spine, its re-derivable digest, and zero
// restarts.
func completeInstallProof() map[string]any {
	steps := make([]any, len(SelfHostStepIDs))
	for i, s := range SelfHostStepIDs {
		steps[i] = s
	}
	return map[string]any{
		"master_key_non_dev": true, "registration_closed": true, "edge_verified": true,
		"config_valid": true, "doctor_green": true, "step_ids": steps,
		"journey_digest": hashParts(SelfHostStepIDs...), "restart_count": 0,
	}
}

// TestInstallClaimRequiresHardenedRestartlessSpine pins OPS-002 (plan §T7 + the restart-less install spine):
// an install case must carry the hardened posture (non-dev master key, closed registration, CA-verified edge),
// a green config-validate + doctor, the ordered spine + a well-formed digest, and zero restarts. A marker
// alone is missing; an unhardened posture, an unverified edge, a red doctor, a malformed digest, a short spine,
// or any restart is invalid.
func TestInstallClaimRequiresHardenedRestartlessSpine(t *testing.T) {
	m := baseManifest()
	c := caseOf(m)
	c["install_claim"] = "installed"
	c["install_proof"] = completeInstallProof()
	if f := VerifyManifest(marshal(t, m), nil); len(f) != 0 {
		t.Fatalf("a complete install proof should pass, got %v", f)
	}

	m = baseManifest()
	caseOf(m)["install_claim"] = "installed" // marker, no proof
	if !hasKind(VerifyManifest(marshal(t, m), nil), "missing") {
		t.Fatal("an install claim with no proof must be a Finding (an 'installed' marker is not restart-less-install proof)")
	}

	for _, mutate := range []func(map[string]any){
		func(p map[string]any) { p["master_key_non_dev"] = false },  // booted on a dev-default key
		func(p map[string]any) { p["registration_closed"] = false }, // a public signup surface
		func(p map[string]any) { p["edge_verified"] = false },       // did not go through the CA-verified edge
		func(p map[string]any) { p["config_valid"] = false },
		func(p map[string]any) { p["doctor_green"] = false },
		func(p map[string]any) { p["journey_digest"] = "not-a-sha256" },   // malformed digest
		func(p map[string]any) { p["restart_count"] = 1 },                 // the control-plane restarted
		func(p map[string]any) { p["step_ids"] = []any{"clean-install"} }, // short spine
	} {
		m = baseManifest()
		c = caseOf(m)
		c["install_claim"] = "installed"
		proof := completeInstallProof()
		mutate(proof)
		c["install_proof"] = proof
		if !hasKind(VerifyManifest(marshal(t, m), nil), "invalid") {
			t.Fatalf("an invalid install proof %v must be a Finding", proof)
		}
	}
}

// TestBackupClaimRequiresSeparateStackRestore pins DR-002 (plan §T7 T4): a backup case must carry two DISTINCT
// stacks, a re-derivable manifest digest, an empty restore target, and a completed restore. A marker is
// missing; a same-stack restore, a malformed digest, or a non-empty target is invalid.
func TestBackupClaimRequiresSeparateStackRestore(t *testing.T) {
	full := func() map[string]any {
		return map[string]any{
			"source_project": "palai-src", "target_project": "palai-dst",
			"manifest_digest": "sha256:" + strings.Repeat("a", 64), "migration_version": 32,
			"target_was_empty": true, "restored": true,
		}
	}
	m := baseManifest()
	c := caseOf(m)
	c["backup_claim"] = "restored"
	c["backup_proof"] = full()
	if f := VerifyManifest(marshal(t, m), nil); len(f) != 0 {
		t.Fatalf("a complete backup proof should pass, got %v", f)
	}

	m = baseManifest()
	caseOf(m)["backup_claim"] = "restored"
	if !hasKind(VerifyManifest(marshal(t, m), nil), "missing") {
		t.Fatal("a backup claim with no proof must be a Finding")
	}
	for _, mutate := range []func(map[string]any){
		func(p map[string]any) { p["target_project"] = "palai-src" }, // restored into the SAME stack
		func(p map[string]any) { p["manifest_digest"] = "not-a-sha256" },
		func(p map[string]any) { p["migration_version"] = 0 },
		func(p map[string]any) { p["target_was_empty"] = false }, // clobbered a non-empty target
		func(p map[string]any) { p["restored"] = false },
	} {
		m = baseManifest()
		c = caseOf(m)
		c["backup_claim"] = "restored"
		proof := full()
		mutate(proof)
		c["backup_proof"] = proof
		if !hasKind(VerifyManifest(marshal(t, m), nil), "invalid") {
			t.Fatalf("an invalid backup proof %v must be a Finding", proof)
		}
	}
}

// TestRestoreVerifyClaimRequiresAllSixChecks pins DR-004..006 (plan §T7 T4): a restore-verify case must carry
// ALL SIX checks green — a marker, or ANY one check false (a checksum mismatch, RLS disabled on the restored
// data, a secret that no longer decrypts), is not a verified restore.
func TestRestoreVerifyClaimRequiresAllSixChecks(t *testing.T) {
	full := func() map[string]any {
		return map[string]any{
			"archive_checksum": true, "migration_version": true, "tenant_ids": true,
			"run_retrieval": true, "rls_isolation": true, "secret_decrypt": true,
		}
	}
	m := baseManifest()
	c := caseOf(m)
	c["restore_verify_claim"] = "verified"
	c["restore_verify_proof"] = full()
	if f := VerifyManifest(marshal(t, m), nil); len(f) != 0 {
		t.Fatalf("a complete restore-verify proof should pass, got %v", f)
	}

	m = baseManifest()
	caseOf(m)["restore_verify_claim"] = "verified"
	if !hasKind(VerifyManifest(marshal(t, m), nil), "missing") {
		t.Fatal("a restore-verify claim with no proof must be a Finding")
	}
	for _, field := range []string{"archive_checksum", "migration_version", "tenant_ids", "run_retrieval", "rls_isolation", "secret_decrypt"} {
		m = baseManifest()
		c = caseOf(m)
		c["restore_verify_claim"] = "verified"
		proof := full()
		proof[field] = false
		c["restore_verify_proof"] = proof
		if !hasKind(VerifyManifest(marshal(t, m), nil), "invalid") {
			t.Fatalf("a restore-verify proof with %q false must be a Finding", field)
		}
	}
}

// TestRemoteSigningSecretRedacted pins the E12 T10 credential-marker extension: a leaked whsec_ webhook/
// remote-tool signing secret (the E11 callback + E12 remote-tool/hook signed transports share the same
// webhook signer) is caught by the redaction pattern scan, so a plaintext signing secret fails the bundle
// by construction — the same whsec_ discipline scripts/verify/e01.sh applies, now in the evidence tier.
func TestRemoteSigningSecretRedacted(t *testing.T) {
	m := baseManifest()
	caseOf(m)["mtls_enroll"] = "signed with whsec_SENTINELdontleak0123456789"
	if !hasKind(VerifyManifest(marshal(t, m), nil), "secret") {
		t.Error("a leaked whsec_ signing secret was not caught by the redaction scan")
	}
}

func TestEvidenceVerifier(t *testing.T) {
	// A valid, redacted bundle passes with no findings.
	if f := VerifyManifest(marshal(t, baseManifest()), nil); len(f) != 0 {
		t.Fatalf("valid manifest produced findings: %v", f)
	}

	// Each dropped release-level required field (git SHA, API version, migration) fails.
	for _, field := range []string{"git_sha", "api_version", "migration", "release"} {
		m := baseManifest()
		delete(m, field)
		if !hasKind(VerifyManifest(marshal(t, m), nil), "missing") {
			t.Errorf("dropping release field %q did not fail verification", field)
		}
	}

	// Each dropped case-level required field fails — including the provider request id, the
	// image digest, the checksum, and the DB assertion bundle.
	for _, field := range []string{"run_id", "image_digest", "provider_request_id", "checksum", "db_assertions", "mtls_enroll"} {
		m := baseManifest()
		delete(caseOf(m), field)
		if !hasKind(VerifyManifest(marshal(t, m), nil), "missing") {
			t.Errorf("dropping case field %q did not fail verification", field)
		}
	}

	// A malformed checksum and a non-singular terminal are invalid.
	m := baseManifest()
	caseOf(m)["checksum"] = "not-a-sha256"
	if !hasKind(VerifyManifest(marshal(t, m), nil), "invalid") {
		t.Error("a malformed checksum did not fail verification")
	}
	m = baseManifest()
	caseOf(m)["terminal"] = map[string]any{"type": "response.completed", "count": 2}
	if !hasKind(VerifyManifest(marshal(t, m), nil), "invalid") {
		t.Error("a terminal count != 1 did not fail verification")
	}

	// A live-provider case with a non-provider-shaped request id is invalid...
	m = baseManifest()
	caseOf(m)["provider_request_id"] = "fake-local"
	if !hasKind(VerifyManifest(marshal(t, m), nil), "invalid") {
		t.Error("a non-provider-shaped id on a live-provider case did not fail verification")
	}
	// ...but the same id on a deterministic case passes — the rule is scoped to live-provider.
	m = baseManifest()
	caseOf(m)["provider_request_id"] = "fake-local"
	caseOf(m)["proof_class"] = "e2e-deterministic"
	if f := VerifyManifest(marshal(t, m), nil); len(f) != 0 {
		t.Errorf("a fake-local id on a deterministic case must pass, got: %v", f)
	}

	// A plaintext credential is caught by the redaction pattern scan (sk- shaped)...
	m = baseManifest()
	caseOf(m)["provider_request_id"] = "sk-live-SENTINELDONTLEAK0123456789abcd"
	if !hasKind(VerifyManifest(marshal(t, m), nil), "secret") {
		t.Error("a credential-shaped token was not caught by the redaction scan")
	}
	// ...and by an explicit needle even when the value is not sk- shaped.
	needle := "TOPSECRETVALUE-abc123def456"
	m = baseManifest()
	caseOf(m)["mtls_enroll"] = "enrolled with " + needle
	if !hasKind(VerifyManifest(marshal(t, m), []string{needle}), "secret") {
		t.Error("a supplied credential needle was not caught")
	}

	// A leaked Git credential — a classic PAT (ghp_), a fine-grained PAT (github_pat_), a GitHub App
	// installation token (ghs_), and an App private-key PEM header — is caught by construction, so the
	// coding release (which mints Git credentials) fails on any of them just as it does on sk-.
	for _, gitToken := range []string{
		"ghp_SENTINELdontleak0123456789ABCDwxyz",
		"github_pat_11ABCDEFG0SENTINELdontleak0123456789",
		"ghs_SENTINELinstallationTokenDontLeak0123",
		"-----BEGIN RSA PRIVATE KEY-----",
	} {
		m = baseManifest()
		caseOf(m)["mtls_enroll"] = "leaked " + gitToken
		if !hasKind(VerifyManifest(marshal(t, m), nil), "secret") {
			t.Errorf("a leaked Git credential %q was not caught by the redaction scan", gitToken)
		}
	}
}

// TestExternalReceiptProofClass covers the external-receipt verifier rule (spec §10.2, E09 REP-006/008):
// a case in the external-receipt class must carry a REAL remote-ref/PR receipt — a git commit sha, a
// provider PR id, or a PR URL — the same discipline the live-provider class enforces on ^chatcmpl-. A
// fake/absent receipt fails; the model-run fields (provider request id, image digest, mTLS enroll,
// single terminal) are NOT required of a push/PR (it is not a model run), but run_id, db_assertions,
// and the checksum still are.
func TestExternalReceiptProofClass(t *testing.T) {
	// A well-formed external-receipt case: a real remote ref sha, no model-run fields, passes clean.
	base := func() map[string]any {
		return map[string]any{
			"release": "coding-0.1.0", "git_sha": "abc1234", "api_version": "v1",
			"migration": "000013_approvals_publications", "captured_at": "2026-07-20T10:00:00Z",
			"cases": []any{map[string]any{
				"id": "REP-006", "status": "PASS", "proof_class": "external-receipt",
				"run_id":           "run_push",
				"external_receipt": "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678",
				"db_assertions":    []any{"remote ref == approved head", "scoped token destroyed"},
				"checksum":         "sha256:" + strings.Repeat("c", 64),
			}},
		}
	}
	if f := VerifyManifest(marshal(t, base()), nil); len(f) != 0 {
		t.Fatalf("valid external-receipt manifest produced findings: %v", f)
	}

	// A PR URL and a numeric provider PR id are both accepted receipt shapes.
	for _, receipt := range []string{"https://github.com/acme/widgets/pull/42", "PR_kwDOABCDEF"} {
		m := base()
		caseOf(m)["external_receipt"] = receipt
		if f := VerifyManifest(marshal(t, m), nil); len(f) != 0 {
			t.Errorf("external receipt %q must pass, got: %v", receipt, f)
		}
	}

	// A missing receipt fails (the load-bearing proof is absent).
	m := base()
	delete(caseOf(m), "external_receipt")
	if !hasKind(VerifyManifest(marshal(t, m), nil), "missing") {
		t.Error("an external-receipt case with no receipt did not fail verification")
	}

	// A fake/non-receipt-shaped value fails — an external-receipt case must not pass with a fake remote.
	// A bare digit run is not a receipt either (both writers emit a node id or URL, never a bare number).
	for _, fake := range []string{"fake-local", "local", "receipt", "12345", "9"} {
		m = base()
		caseOf(m)["external_receipt"] = fake
		if !hasKind(VerifyManifest(marshal(t, m), nil), "invalid") {
			t.Errorf("a fake external receipt %q did not fail verification", fake)
		}
	}

	// run_id, db_assertions, and checksum are still required of an external-receipt case.
	for _, field := range []string{"run_id", "db_assertions", "checksum"} {
		m = base()
		delete(caseOf(m), field)
		if !hasKind(VerifyManifest(marshal(t, m), nil), "missing") {
			t.Errorf("dropping %q on an external-receipt case did not fail verification", field)
		}
	}
}
