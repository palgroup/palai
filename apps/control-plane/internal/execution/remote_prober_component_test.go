//go:build component

package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/webhook"
	remotehttp "github.com/palgroup/palai/adapters/tools/http"
	"github.com/palgroup/palai/apps/control-plane/api"
)

// TestLateCallbackAfterDeadlineEntersReconciliationNotSilentCommit proves the signed half of TOL-017: a
// remote-tool callback that arrives AFTER the executor's deadline is verified + one-use-consumed, but it
// writes late_result to the operation ledger and NEVER commits to the tool ledger (no silent commit). The
// uncertain tool_call the timeout parked is then resolved through the RemoteToolProber to
// reconciled_completed — the late result re-enters reasoning via the reconcile machine, not a stale
// commit. It exercises the REAL callback endpoint (real HMAC verify) + the REAL reconcile loop.
func TestLateCallbackAfterDeadlineEntersReconciliationNotSilentCommit(t *testing.T) {
	ctx := context.Background()
	cs, tenant, sessionID, runID := openLedgerSpine(t)
	pool := cs.Pool()
	ops := remotehttp.NewOperations(pool)
	secret := []byte("remote-tool-late-secret")

	resolver := func(org, ref string) ([]byte, error) {
		if org == tenant.Organization && ref == "sig-ref" {
			return secret, nil
		}
		return nil, nil
	}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/tool-callbacks/{operation_id}", api.NewToolCallbackHandler(ops, resolver))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// The timeout outcome: a reversible remote tool_call parked `uncertain` (dispatchTool's uncertain-STOP
	// after the executor's deadline error), and a remote operation whose deadline has already passed. The
	// reconcile derives session/response from the run row (session_id is set; response_id coalesces to '').
	callID := redeliveryID("tc")
	execSQL(t, pool,
		`INSERT INTO tool_calls (id, organization_id, project_id, run_id, fence, state, name, arguments, replay_class, reconciliation_state)
		 VALUES ($1,$2,$3,$4,5,'uncertain','remote.lookup','{}','reversible','reconciling')`,
		callID, tenant.Organization, tenant.Project, runID)
	_ = sessionID
	operationID := redeliveryID("rop")
	token := redeliveryID("tok")
	opened, err := ops.Open(ctx, remotehttp.OpenOperation{
		OperationID: operationID, Org: tenant.Organization, Project: tenant.Project, ToolCallID: callID,
		SecretRef: "sig-ref", TokenHash: remotehttp.HashToken(token),
		Deadline: time.Now().Add(-time.Minute), Fence: 5, // deadline already passed -> a callback is LATE
	})
	if err != nil || !opened {
		t.Fatalf("open past-deadline operation = opened:%v err:%v", opened, err)
	}

	// The LATE signed callback: verified + one-use-consumed, recorded late_result — NOT a ledger commit.
	if status := postSignedCallback(t, srv, operationID, token, secret, map[string]any{"answer": 42}); status != http.StatusOK {
		t.Fatalf("late callback status = %d, want 200 (accepted as late)", status)
	}
	var opState string
	if err := pool.QueryRow(ctx, `SELECT state FROM remote_tool_operations WHERE id=$1`, operationID).Scan(&opState); err != nil {
		t.Fatalf("read operation error = %v", err)
	}
	if opState != "late_result" {
		t.Fatalf("operation state after late callback = %q, want late_result (past the deadline)", opState)
	}
	// The tool ledger was NOT touched by the callback — still uncertain, never silently committed.
	var callState string
	if err := pool.QueryRow(ctx, `SELECT state FROM tool_calls WHERE id=$1`, callID).Scan(&callState); err != nil {
		t.Fatalf("read tool_call error = %v", err)
	}
	if callState != "uncertain" {
		t.Fatalf("tool_call state after late callback = %q, want STILL uncertain (no silent commit)", callState)
	}

	// The RemoteToolProber reads late_result as the destination's applied result, so the reconcile loop
	// drives the uncertain call to reconciled_completed (the late result re-enters reasoning).
	reconciler := NewUncertainReconciler(cs, NewRemoteToolProber(ops), time.Hour, 100)
	if _, err := reconciler.Sweep(ctx); err != nil {
		t.Fatalf("reconcile sweep error = %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM tool_calls WHERE id=$1`, callID).Scan(&callState); err != nil {
		t.Fatalf("re-read tool_call error = %v", err)
	}
	if callState != "reconciled_completed" {
		t.Fatalf("tool_call state after reconcile = %q, want reconciled_completed (late result reconciled, not silently committed)", callState)
	}
}

// postSignedCallback signs a result callback (id = operationID, like the CP verifies) and POSTs it,
// returning the status.
func postSignedCallback(t *testing.T, srv *httptest.Server, operationID, token string, secret []byte, result map[string]any) int {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"protocol": "tool-http.v1", "tool_call_id": "tc_ignored", "operation_id": operationID, "result": result})
	headers := webhook.NewSigner(secret).Headers(operationID, time.Now(), 1, raw)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/tool-callbacks/"+operationID, bytes.NewReader(raw))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set(remotehttp.HeaderCallbackToken, token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("post callback error = %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
