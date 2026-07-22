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
