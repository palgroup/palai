//go:build component

// Gateway-side component test for the CapabilityWorker contract (E17 Task 9): the server-side guarantee that a
// redeemed secret VALUE never reaches the durable journal, enforced at the outbound gateway over a REAL store.
package workers_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/workers"
)

// TestGatewayRefusesResultEchoingRedeemedSecret is the SHOULD-FIX 3 crown: the store journals the worker's
// receipt/output VERBATIM, so the GATEWAY enforces server-side that a redeemed secret VALUE never reaches the
// durable journal — a result echoing it (in the receipt OR the output artifact) is refused (403) before any
// persistence. This makes WRK-004 hold against a hostile worker, not only the honest fixture. A clean result
// is still accepted (the refusal is targeted).
func TestGatewayRefusesResultEchoingRedeemedSecret(t *testing.T) {
	const marker = "GW-SECRET-do-not-leak-3a9f"
	cs := openHarness(t)
	store := newStore(cs, fakeSecrets{vals: map[string]string{"build-cache-token": marker}}, nil)
	gw := workers.NewGateway(store, 5*time.Minute)
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()
	tenant := seedTenant(t, cs)

	enrollTok := gw.IssueEnrollmentToken(tenant, "swift-toolchain")
	workload := gwEnroll(t, srv.URL, enrollTok)

	jobID, err := store.DispatchJob(context.Background(), tenant, workers.JobSpec{
		Capability: "swift-toolchain", Operation: "swift.build-check",
		SecretHandleRefs: []string{"build-cache-token"}, Deadline: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("DispatchJob() error = %v", err)
	}

	// Claim + redeem over the gateway so the session records the redeemed value.
	claim := gwPostJSON(t, srv.URL+"/capability/claim", workload, nil)
	if claim["job_id"] != jobID {
		t.Fatalf("claim job = %v, want %s", claim["job_id"], jobID)
	}
	redeem := gwPostJSON(t, srv.URL+"/capability/redeem", workload, []byte(`{"job_id":"`+jobID+`","handle_name":"build-cache-token"}`))
	valB64, _ := redeem["value_b64"].(string)
	got, _ := base64.StdEncoding.DecodeString(valB64)
	if string(got) != marker {
		t.Fatalf("redeemed value = %q, want the marker", got)
	}

	// A hostile worker echoes the secret into its receipt: 403, and nothing lands in the journal.
	leakReceipt := `{"job_id":"` + jobID + `","class":"completed","operation":"swift.build-check","receipt":{"leak":"` + marker + `"}}`
	if status := gwRawPost(t, srv.URL+"/capability/result", workload, leakReceipt); status != http.StatusForbidden {
		t.Fatalf("result echoing the secret in the receipt = %d, want 403 (refused before the journal)", status)
	}
	// The secret in the OUTPUT artifact is scanned too.
	leakOutput := `{"job_id":"` + jobID + `","class":"completed","operation":"swift.build-check","output_artifact_b64":"` +
		base64.StdEncoding.EncodeToString([]byte("report: "+marker+" end")) + `"}`
	if status := gwRawPost(t, srv.URL+"/capability/result", workload, leakOutput); status != http.StatusForbidden {
		t.Fatalf("result with the secret in the output artifact = %d, want 403", status)
	}
	assertJournalKinds(t, cs, jobID, []string{"dispatched", "leased"})
	assertJournalHasNoSecret(t, cs, jobID, marker)

	// A clean result (no secret) is accepted — the refusal is targeted, not a blanket block.
	clean := `{"job_id":"` + jobID + `","class":"completed","operation":"swift.build-check","receipt":{"ok":true}}`
	if status := gwRawPost(t, srv.URL+"/capability/result", workload, clean); status != http.StatusOK {
		t.Fatalf("clean result = %d, want 200 accepted", status)
	}
	if kind, _ := latestJobEntry(t, cs, jobID); kind != "completed" {
		t.Fatalf("clean result did not complete the job: latest kind = %q", kind)
	}
}

// gwEnroll spends an enrollment token over the gateway and returns the workload token.
func gwEnroll(t *testing.T, base, enrollTok string) string {
	t.Helper()
	out := gwPostJSON(t, base+"/capability/enroll", enrollTok, []byte(`{"capability_version":"0.1.0","os":"darwin","arch":"arm64","capacity":1}`))
	tok, _ := out["workload_token"].(string)
	if tok == "" {
		t.Fatalf("enroll returned no workload token: %v", out)
	}
	return tok
}

// gwPostJSON POSTs body with a bearer token and decodes a JSON object response (200/204).
func gwPostJSON(t *testing.T, url, token string, body []byte) map[string]any {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return map[string]any{}
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s status = %d", url, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return out
}

// gwRawPost POSTs a raw body with a bearer token and returns the status code.
func gwRawPost(t *testing.T, url, token, body string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}
