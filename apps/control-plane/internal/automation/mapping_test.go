package automation

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestMappingLanguageDeniesNetworkFsSecretEscape pins the load-bearing security property (spec
// §20.2.2): the mapping language is a CLOSED set of safe verbs (select/const/default/when/secret), so a
// network fetch, a filesystem read, or a process exec is UNEXPRESSIBLE — the compiler rejects any
// unrecognized verb structurally, not via a blacklist — and a SecretRef may only name a value in the
// trigger's allowlist. This is what keeps a tenant-authored mapping from becoming an SSRF/LFI/RCE gadget.
func TestMappingLanguageDeniesNetworkFsSecretEscape(t *testing.T) {
	allowed := []string{"partner_token"}
	escapes := map[string]string{
		"network fetch":           `{"fields":{"x":{"fetch":"http://169.254.169.254/latest/meta-data"}}}`,
		"filesystem read":         `{"fields":{"x":{"file":"/etc/passwd"}}}`,
		"process exec":            `{"fields":{"x":{"exec":"rm -rf /"}}}`,
		"unknown verb":            `{"fields":{"x":{"eval":"1+1"}}}`,
		"out-of-allowlist secret": `{"fields":{"x":{"secret":"root_credentials"}}}`,
	}
	for name, doc := range escapes {
		if _, err := CompileMapping([]byte(doc), allowed); err == nil {
			t.Errorf("%s: CompileMapping accepted an escape it must reject: %s", name, doc)
		}
	}
	// The safe verbs compile: a select with a default, a constant, a conditional, and an ALLOWED secret.
	safe := `{"fields":{
		"a":{"select":"order.id"},
		"b":{"const":"nightly"},
		"c":{"select":"order.status","default":"unknown"},
		"d":{"when":{"path":"order.priority","equals":"high","then":{"const":"urgent"},"else":{"const":"normal"}}},
		"e":{"secret":"partner_token"}
	}}`
	if _, err := CompileMapping([]byte(safe), allowed); err != nil {
		t.Fatalf("CompileMapping rejected the safe verb set: %v", err)
	}
}

// TestMappingFailureFailedDeliveryNoRun pins the pure half of AUT-003: when the mapped canonical input
// is schema-invalid (a required output field the source payload does not supply), Apply returns a TYPED
// schema error. The pipeline half — that such a failure records a `failed` delivery and NO runs row —
// rides the real-PG pipeline test (A6).
func TestMappingFailureFailedDeliveryNoRun(t *testing.T) {
	m, err := CompileMapping([]byte(`{"fields":{"id":{"select":"order.id"}},"required":["id"]}`), nil)
	if err != nil {
		t.Fatalf("CompileMapping error = %v", err)
	}
	// The source payload lacks order.id, so the required output field is absent → typed schema error.
	if _, err := m.Apply(map[string]any{"order": map[string]any{"status": "paid"}}); !errors.Is(err, ErrMappingSchema) {
		t.Fatalf("Apply on a schema-invalid mapping error = %v, want ErrMappingSchema", err)
	}
}

// TestMappingPreviewAgainstRedactedFixture proves a mapping produces the expected canonical input from a
// source payload, and that a SecretRef stays a REDACTED handle — the plaintext secret never enters the
// mapped input (a preview is safe to log/return). select/const/default/when all evaluate as documented.
func TestMappingPreviewAgainstRedactedFixture(t *testing.T) {
	m, err := CompileMapping([]byte(`{"fields":{
		"order_id":{"select":"order.id"},
		"kind":{"const":"fulfillment"},
		"status":{"select":"order.status","default":"unknown"},
		"tier":{"when":{"path":"order.priority","equals":"high","then":{"const":"urgent"},"else":{"const":"normal"}}},
		"token":{"secret":"partner_token"}
	},"required":["order_id"]}`), []string{"partner_token"})
	if err != nil {
		t.Fatalf("CompileMapping error = %v", err)
	}
	payload := map[string]any{"order": map[string]any{"id": "ord_42", "priority": "high"}}
	out, err := m.Apply(payload)
	if err != nil {
		t.Fatalf("Apply error = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal mapped input error = %v", err)
	}
	if got["order_id"] != "ord_42" {
		t.Errorf("order_id = %v, want ord_42", got["order_id"])
	}
	if got["kind"] != "fulfillment" {
		t.Errorf("kind = %v, want fulfillment", got["kind"])
	}
	if got["status"] != "unknown" { // order.status absent → default
		t.Errorf("status = %v, want unknown (default)", got["status"])
	}
	if got["tier"] != "urgent" { // order.priority == high → then
		t.Errorf("tier = %v, want urgent", got["tier"])
	}
	// The secret is a REDACTED handle, never plaintext: {"secret_ref":"partner_token"}.
	ref, ok := got["token"].(map[string]any)
	if !ok || ref["secret_ref"] != "partner_token" {
		t.Errorf("token = %v, want a {secret_ref: partner_token} handle", got["token"])
	}
	if strings.Contains(string(out), "root_credentials") {
		t.Error("mapped input leaked an out-of-band secret name")
	}
}

// TestKeyExprSameLanguage proves the dedupe-key and correlation-key expressions reuse the SAME mapping
// language (no second language): a single-rule expression evaluates to a stringified scalar.
func TestKeyExprSameLanguage(t *testing.T) {
	e, err := CompileExpr(`{"select":"order.id"}`, nil)
	if err != nil {
		t.Fatalf("CompileExpr error = %v", err)
	}
	got, err := e.EvalString(map[string]any{"order": map[string]any{"id": "ord_99"}})
	if err != nil {
		t.Fatalf("EvalString error = %v", err)
	}
	if got != "ord_99" {
		t.Fatalf("EvalString = %q, want ord_99", got)
	}
	// The same closed verb set: a network fetch is unexpressible in a key expr too.
	if _, err := CompileExpr(`{"fetch":"http://evil"}`, nil); err == nil {
		t.Fatal("CompileExpr accepted a fetch escape in a key expression")
	}
}
