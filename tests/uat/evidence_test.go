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
	for _, fake := range []string{"fake-local", "local", "receipt"} {
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
