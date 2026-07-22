//go:build component

// Package remotetools holds the real-PostgreSQL + real-local-HTTP component tests for the E12 T4 remote
// HTTP tool: the signed callback endpoint, the one-use audience-bound token, and the late-callback
// reconciliation timeline. They run only under `make test-component TEST=remote-tools`, which starts a
// throwaway Postgres and exports PALAI_COMPONENT_POSTGRES_URL. The build tag keeps them out of the
// credential-free unit tier.
package remotetools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/adapters/integrations/webhook"
	remotehttp "github.com/palgroup/palai/adapters/tools/http"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/packages/coordinator"
)

const testSecretRef = "sig-ref"

var testSecret = []byte("remote-tool-callback-secret")

func newID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

// harness is a migrated spine + the operation ledger + a live callback endpoint (the real handler over
// the real Operations store, verifying real HMAC + consuming the real one-use token).
type harness struct {
	ops        *remotehttp.Operations
	pool       *pgxpool.Pool
	server     *httptest.Server
	org        string
	project    string
	runID      string
	toleranceN time.Duration
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=remote-tools")
	}
	ctx := context.Background()
	cs, err := coordinator.Open(ctx, url)
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	pool := cs.Pool()
	org, project, session, runID := newID("org"), newID("prj"), newID("ses"), newID("run")
	exec(t, pool, `INSERT INTO organizations (id) VALUES ($1)`, org)
	exec(t, pool, `INSERT INTO projects (id, organization_id) VALUES ($1,$2)`, project, org)
	exec(t, pool, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1,$2,$3)`, session, org, project)
	exec(t, pool, `INSERT INTO runs (id, organization_id, project_id, session_id) VALUES ($1,$2,$3,$4)`, runID, org, project, session)

	ops := remotehttp.NewOperations(pool)
	resolver := func(o, ref string) ([]byte, error) {
		if o == org && ref == testSecretRef {
			return testSecret, nil
		}
		return nil, nil // an unresolvable ref -> generic 404
	}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/tool-callbacks/{operation_id}", api.NewToolCallbackHandler(ops, resolver))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return &harness{ops: ops, pool: pool, server: server, org: org, project: project, runID: runID}
}

// openOperation seeds a tool_call (the FK target) + a pending operation and returns its id + raw token.
func (h *harness) openOperation(t *testing.T, deadline time.Time, fence uint64) (operationID, token string) {
	t.Helper()
	callID := newID("tcall")
	exec(t, h.pool,
		`INSERT INTO tool_calls (id, organization_id, project_id, run_id, fence, state, name, arguments)
		 VALUES ($1,$2,$3,$4,$5,'executing','remote.lookup','{}')`,
		callID, h.org, h.project, h.runID, int64(fence))
	operationID = newID("rop")
	token = newID("tok")
	opened, err := h.ops.Open(context.Background(), remotehttp.OpenOperation{
		OperationID: operationID, Org: h.org, Project: h.project, ToolCallID: callID,
		SecretRef: testSecretRef, TokenHash: remotehttp.HashToken(token), Deadline: deadline, Fence: fence,
	})
	if err != nil || !opened {
		t.Fatalf("open operation = opened:%v err:%v, want a fresh pending row", opened, err)
	}
	return operationID, token
}

// postCallback signs and POSTs a result callback. The signature id is the operationID (the path segment),
// matching the CP's verify id. secret/token override the operation's real ones for the reject-path tests.
func (h *harness) postCallback(t *testing.T, operationID, token string, secret []byte, result map[string]any) *http.Response {
	t.Helper()
	envelope := map[string]any{
		"protocol": "tool-http.v1", "tool_call_id": "tc_ignored_by_verify", "operation_id": operationID, "result": result,
	}
	raw, _ := json.Marshal(envelope)
	headers := webhook.NewSigner(secret).Headers(operationID, time.Now(), 1, raw)
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/v1/tool-callbacks/"+operationID, bytes.NewReader(raw))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set(remotehttp.HeaderCallbackToken, token)
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("post callback error = %v", err)
	}
	return resp
}

// TestAsync202CallbackAcceptedOnceUnderFence proves the signed-callback half of TOL-016/017: a signed
// callback to a pending operation is accepted exactly ONCE (the row completes with the result + hash,
// carrying the fence the invoke ran under), a DUPLICATE callback with the SAME result is idempotent, and
// a duplicate with a DIVERGED result is a 409 — the result-hash sameness gate.
func TestAsync202CallbackAcceptedOnceUnderFence(t *testing.T) {
	h := newHarness(t)
	operationID, token := h.openOperation(t, time.Now().Add(time.Minute), 7)

	// First signed callback: accepted, row completes under the fence.
	resp := h.postCallback(t, operationID, token, testSecret, map[string]any{"answer": 42})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first callback status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
	var state, resultHash string
	var fence int64
	var result []byte
	if err := h.pool.QueryRow(context.Background(),
		`SELECT state, result, result_hash, fence FROM remote_tool_operations WHERE id=$1`, operationID).
		Scan(&state, &result, &resultHash, &fence); err != nil {
		t.Fatalf("read operation error = %v", err)
	}
	if state != "completed" || fence != 7 || string(result) == "" {
		t.Fatalf("after callback: state=%q fence=%d result=%s, want completed/7/<result>", state, fence, result)
	}
	if resultHash != remotehttp.ResultHash(map[string]any{"answer": 42}) {
		t.Fatalf("stored result_hash = %q, want the canonical result hash", resultHash)
	}

	// A duplicate callback with the SAME result is idempotent (200, no 409).
	dup := h.postCallback(t, operationID, token, testSecret, map[string]any{"answer": 42})
	if dup.StatusCode != http.StatusOK {
		t.Fatalf("idempotent duplicate status = %d, want 200", dup.StatusCode)
	}
	dup.Body.Close()

	// A duplicate callback with a DIVERGED result is a 409 (the result-hash sameness gate).
	diverged := h.postCallback(t, operationID, token, testSecret, map[string]any{"answer": 99})
	if diverged.StatusCode != http.StatusConflict {
		t.Fatalf("diverged duplicate status = %d, want 409", diverged.StatusCode)
	}
	diverged.Body.Close()
}

// TestCallbackTokenOneUseAudienceBound proves the token is one-use AND audience-bound: a callback to
// operation A bearing operation B's token is a generic 404 (the token binds to A's operation, not B's), a
// bad signature is a 401, and only the correct token+signature consumes A exactly once.
func TestCallbackTokenOneUseAudienceBound(t *testing.T) {
	h := newHarness(t)
	opA, tokenA := h.openOperation(t, time.Now().Add(time.Minute), 3)
	_, tokenB := h.openOperation(t, time.Now().Add(time.Minute), 4)

	// A's callback bearing B's token: the token hash does not match A's stored hash -> generic 404
	// (audience mismatch, no oracle). The signature is valid (A's secret), so this isolates the token gate.
	crossed := h.postCallback(t, opA, tokenB, testSecret, map[string]any{"answer": 1})
	if crossed.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-operation token status = %d, want 404 (audience-bound)", crossed.StatusCode)
	}
	crossed.Body.Close()

	// A bad signature (wrong secret) with the RIGHT token is a 401 — verify-before-persist.
	unsigned := h.postCallback(t, opA, tokenA, []byte("not-the-secret"), map[string]any{"answer": 1})
	if unsigned.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad-signature status = %d, want 401", unsigned.StatusCode)
	}
	unsigned.Body.Close()

	// The correct token + signature consumes A once.
	ok := h.postCallback(t, opA, tokenA, testSecret, map[string]any{"answer": 1})
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("valid callback status = %d, want 200", ok.StatusCode)
	}
	ok.Body.Close()
	var state string
	if err := h.pool.QueryRow(context.Background(), `SELECT state FROM remote_tool_operations WHERE id=$1`, opA).Scan(&state); err != nil {
		t.Fatalf("read A error = %v", err)
	}
	if state != "completed" {
		t.Fatalf("A state after valid callback = %q, want completed", state)
	}
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q error = %v", sql, err)
	}
}
