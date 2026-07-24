package sdkparity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

// promotableManifest returns a manifest whose cases carry a COMPLETE ThreeLanguageEqualityProof AND a COMPLETE
// GatewayOffProof — the shape SDKParityPromoteGate must accept for rc.
func promotableManifest() map[string]any {
	agreed := json.RawMessage(`{"id":"r","output_text":"ok","status":"response.completed"}`)
	canon, _ := canonical(agreed)
	equalityDigest := hashParts(canon)

	equalityCase := map[string]any{
		"id": "API-012", "status": "PASS", "proof_class": "e2e-deterministic", "run_id": "run_p",
		"image_digest": "sha256:" + repeat("a", 64), "provider_request_id": "prov_final",
		"mtls_enroll":   "runner-local cn=controller",
		"terminal":      map[string]any{"type": "response.completed", "count": 1},
		"db_assertions": []any{"parity"}, "checksum": hashParts("API-012", "run_p", equalityDigest),
		"three_language_equality_claim": "equal",
		"three_language_equality_proof": map[string]any{
			"run_id": "run_p",
			"client_outputs": map[string]any{
				"typescript": agreed, "python": agreed, "go": agreed, "cli": agreed,
			},
			"equality_digest": equalityDigest,
		},
	}
	gatewayCase := map[string]any{
		"id": "MOD-003", "status": "PASS", "proof_class": "e2e-deterministic", "run_id": "run_p",
		"image_digest": "sha256:" + repeat("a", 64), "provider_request_id": "prov_final",
		"mtls_enroll":   "runner-local cn=controller",
		"terminal":      map[string]any{"type": "response.completed", "count": 1},
		"db_assertions": []any{"gateway-off"}, "checksum": hashParts("MOD-003", "run_p", equalityDigest),
		"gateway_off_claim": "gateway-off",
		"gateway_off_proof": map[string]any{
			"config_digest": hashParts(uat.GatewayOffRouteConfig...), "gateway_route": "gw@openai-compatible",
			"proxy_killed": true, "gateway_run_failed": true,
			"direct_run_id": "run_direct", "direct_provider_request_id": "chatcmpl-abc", "direct_completed": true,
		},
	}
	return map[string]any{
		"release": "sdk-provider-parity-0.1.0", "git_sha": "abc1234", "api_version": "v1",
		"migration": "000034", "captured_at": "2026-07-24T06:00:00Z",
		"cases": []any{equalityCase, gatewayCase},
	}
}

func repeat(s string, n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = s[0]
	}
	return string(out)
}

func marshalJSON(t *testing.T, m map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return raw
}

// TestSDKParityPromoteGateRefusesWithoutEqualityAndGatewayOff pins the E16 exit-gate sentence (plan §7): the
// promote gate REFUSES a bundle that lacks a COMPLETE ThreeLanguageEqualityProof or a COMPLETE GatewayOffProof,
// and a beyond-rc promote also refuses without the operator-leg attestation.
func TestSDKParityPromoteGateRefusesWithoutEqualityAndGatewayOff(t *testing.T) {
	// A complete bundle passes rc.
	if r := uat.SDKParityPromoteGate(marshalJSON(t, promotableManifest()), "rc"); len(r) != 0 {
		t.Fatalf("a bundle with equality + gateway-off proof must promote to rc, got refusals: %v", r)
	}

	// No gateway-off proof -> refused.
	m := promotableManifest()
	cases := m["cases"].([]any)
	m["cases"] = []any{cases[0]} // only the equality case
	if r := uat.SDKParityPromoteGate(marshalJSON(t, m), "rc"); len(r) == 0 {
		t.Fatal("a bundle with NO GatewayOffProof must be refused")
	}

	// No equality proof -> refused.
	m = promotableManifest()
	cases = m["cases"].([]any)
	m["cases"] = []any{cases[1]} // only the gateway-off case
	if r := uat.SDKParityPromoteGate(marshalJSON(t, m), "rc"); len(r) == 0 {
		t.Fatal("a bundle with NO ThreeLanguageEqualityProof must be refused")
	}

	// Equality proof present but the four clients diverge (fabricated equality) -> Complete() false -> refused.
	m = promotableManifest()
	eq := m["cases"].([]any)[0].(map[string]any)["three_language_equality_proof"].(map[string]any)
	eq["client_outputs"].(map[string]any)["cli"] = json.RawMessage(`{"id":"r","output_text":"DIFFERENT"}`)
	if r := uat.SDKParityPromoteGate(marshalJSON(t, m), "rc"); len(r) == 0 {
		t.Fatal("a fabricated-equality proof (a diverging client) must be refused (the crown anti-fabrication)")
	}

	// A complete rc bundle still cannot promote to STABLE without the operator-leg attestation.
	if r := uat.SDKParityPromoteGate(marshalJSON(t, promotableManifest()), "stable"); len(r) == 0 {
		t.Fatal("promote to stable must be refused without an operator_attestation (§6 operator legs)")
	}
	// With the attestation present, the stable-only refusal clears.
	m = promotableManifest()
	m["operator_attestation"] = map[string]any{"leg_registry_publish": "E18", "leg_real_litellm": "run by ops"}
	if r := uat.SDKParityPromoteGate(marshalJSON(t, m), "stable"); len(r) != 0 {
		t.Fatalf("a complete bundle WITH the operator attestation must promote to stable, got: %v", r)
	}
}

// TestSDKParityShippedBundlePromotesToRCNotStable proves the SHIPPED sdk-provider-parity-0.1.0 bundle promotes
// to rc (it carries the equality + gateway-off proofs) but is HONESTLY refused for stable — the §6 operator legs
// (published-registry release + real LiteLLM/private-server gateway) have not run.
func TestSDKParityShippedBundlePromotesToRCNotStable(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "evidence", "releases", "sdk-provider-parity-0.1.0", "manifest.json"))
	if err != nil {
		t.Fatalf("read sdk-provider-parity-0.1.0 manifest: %v", err)
	}
	// The family dispatcher must route this bundle to the SDK-parity gate and pass rc.
	if r := uat.PromoteGateFor(raw, "rc"); len(r) != 0 {
		t.Fatalf("sdk-provider-parity-0.1.0 must promote to rc, got refusals: %v", r)
	}
	if r := uat.PromoteGateFor(raw, "stable"); len(r) == 0 {
		t.Fatal("sdk-provider-parity-0.1.0 must be refused for stable — the §6 operator legs have not run (no operator_attestation)")
	}
}
