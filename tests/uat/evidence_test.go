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
}
