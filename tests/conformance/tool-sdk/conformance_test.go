// Package toolsdk_test is the Go leg of the shared tool-sdk conformance corpus
// (spec §28.23, TOL-018). It drives the Extension SDK's four server-side
// surfaces — define-tool schema emit, tool-http.v1 signed-invocation verify +
// callback sign, normalized {result|problem} bodies, and the tool_call_id
// idempotency store — against the SAME JSON fixtures the TS and Python legs run,
// so a polyglot drift fails a TEST. The signature vectors ALSO run against the
// reference webhook.Verify/Signer, so a T4<->SDK signing divergence fails here,
// not a review. No network or credential is involved (known-key vectors only).
package toolsdk_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	webhook "github.com/palgroup/palai/adapters/integrations/webhook"
	extsdk "github.com/palgroup/palai/packages/extension-sdk"
)

func load(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("corpus", name))
	if err != nil {
		t.Fatalf("read corpus %s: %v", name, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decode corpus %s: %v", name, err)
	}
}

// TestSchemaEmitCanonicalBytes proves the define-tool helper emits byte-identical
// canonical registration bytes (sorted-key compact) for every corpus vector.
func TestSchemaEmitCanonicalBytes(t *testing.T) {
	var corpus struct {
		Vectors []struct {
			Name       string                `json:"name"`
			Definition extsdk.ToolDefinition `json:"definition"`
			Canonical  string                `json:"canonical"`
		}
	}
	load(t, "schema-emit.json", &corpus)
	if len(corpus.Vectors) == 0 {
		t.Fatal("empty schema-emit corpus")
	}
	for _, v := range corpus.Vectors {
		got, err := v.Definition.Canonical()
		if err != nil {
			t.Fatalf("%s: emit: %v", v.Name, err)
		}
		if string(got) != v.Canonical {
			t.Errorf("%s: canonical bytes mismatch\n got: %s\nwant: %s", v.Name, got, v.Canonical)
		}
	}
}

// TestSignatureVerifyMatchesReference runs every signature vector through the SDK
// verifier AND the reference webhook verifier: both must reach the corpus's
// expected accept/reject, so drift between the two signing implementations is a
// test failure. Valid vectors additionally assert byte-identical signing.
func TestSignatureVerifyMatchesReference(t *testing.T) {
	var corpus struct {
		Vectors []struct {
			Name             string `json:"name"`
			Secret           string `json:"secret"`
			WebhookID        string `json:"webhook_id"`
			Timestamp        int64  `json:"timestamp"`
			Body             string `json:"body"`
			Signature        string `json:"signature"`
			Now              int64  `json:"now"`
			ToleranceSeconds int    `json:"tolerance_seconds"`
			Expect           bool   `json:"expect"`
			ExpectSignature  string `json:"expect_signature"`
		}
	}
	load(t, "signature-verify.json", &corpus)
	if len(corpus.Vectors) == 0 {
		t.Fatal("empty signature-verify corpus")
	}
	for _, v := range corpus.Vectors {
		secret := []byte(v.Secret)
		ts := time.Unix(v.Timestamp, 0)
		now := time.Unix(v.Now, 0)
		tol := time.Duration(v.ToleranceSeconds) * time.Second
		body := []byte(v.Body)

		if got := extsdk.Verify(secret, v.WebhookID, ts, body, v.Signature, now, tol); got != v.Expect {
			t.Errorf("%s: extsdk.Verify = %v, want %v", v.Name, got, v.Expect)
		}
		// Drift guard: the T4 reference verifier must agree on the SAME vector.
		if ref := webhook.Verify(secret, v.WebhookID, ts, body, v.Signature, now, tol); ref != v.Expect {
			t.Errorf("%s: webhook.Verify (reference) = %v, want %v", v.Name, ref, v.Expect)
		}
		if v.ExpectSignature != "" {
			if h := extsdk.Sign(secret, v.WebhookID, ts, body); h != v.ExpectSignature {
				t.Errorf("%s: extsdk.Sign = %s, want %s", v.Name, h, v.ExpectSignature)
			}
			ref := webhook.NewSigner(secret).Headers(v.WebhookID, ts, 1, body)[webhook.HeaderSignature]
			if ref != "v1="+v.ExpectSignature {
				t.Errorf("%s: webhook signer header = %s, want v1=%s", v.Name, ref, v.ExpectSignature)
			}
		}
	}
}

// TestResultNormalizeCanonicalBytes proves the sync {result|problem} body and the
// tool-http.v1 callback envelope emit byte-identical canonical bytes.
func TestResultNormalizeCanonicalBytes(t *testing.T) {
	var corpus struct {
		Vectors []struct {
			Name        string         `json:"name"`
			Kind        string         `json:"kind"`
			Outcome     string         `json:"outcome"`
			OperationID string         `json:"operation_id"`
			ToolCallID  string         `json:"tool_call_id"`
			Payload     map[string]any `json:"payload"`
			Canonical   string         `json:"canonical"`
		}
	}
	load(t, "result-normalize.json", &corpus)
	if len(corpus.Vectors) == 0 {
		t.Fatal("empty result-normalize corpus")
	}
	for _, v := range corpus.Vectors {
		var got []byte
		var err error
		switch {
		case v.Kind == "sync" && v.Outcome == "result":
			got, err = extsdk.SyncResult(v.Payload)
		case v.Kind == "sync" && v.Outcome == "problem":
			got, err = extsdk.SyncProblem(v.Payload)
		case v.Kind == "callback" && v.Outcome == "result":
			got, err = extsdk.Callback(v.OperationID, v.ToolCallID, v.Payload)
		case v.Kind == "callback" && v.Outcome == "problem":
			got, err = extsdk.CallbackProblem(v.OperationID, v.ToolCallID, v.Payload)
		default:
			t.Fatalf("%s: unknown kind/outcome %s/%s", v.Name, v.Kind, v.Outcome)
		}
		if err != nil {
			t.Fatalf("%s: build: %v", v.Name, err)
		}
		if string(got) != v.Canonical {
			t.Errorf("%s: canonical mismatch\n got: %s\nwant: %s", v.Name, got, v.Canonical)
		}
	}
}

// TestIdempotencyStoreReplayAndConflict proves the store mirrors the executor's
// rule: a same-hash duplicate replays the stored answer, a diverged hash is a
// conflict (the caller answers 409).
func TestIdempotencyStoreReplayAndConflict(t *testing.T) {
	s := extsdk.NewIdempotencyStore()
	id, hash := "tcall_x", "sha256:aaa"
	if out, _ := s.Classify(id, hash); out != extsdk.Fresh {
		t.Fatalf("first classify = %v, want Fresh", out)
	}
	stored := []byte(`{"result":{"ok":true}}`)
	s.Store(id, hash, stored)
	if out, resp := s.Classify(id, hash); out != extsdk.Replay || string(resp) != string(stored) {
		t.Fatalf("replay classify = %v / %s, want Replay / %s", out, resp, stored)
	}
	if out, _ := s.Classify(id, "sha256:different"); out != extsdk.Conflict {
		t.Fatalf("diverged classify = %v, want Conflict", out)
	}
}
