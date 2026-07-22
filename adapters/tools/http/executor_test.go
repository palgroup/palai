package remotehttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/webhook"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/egress"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// harness is a real local remote-tool server that VERIFIES the HMAC signature server-side (the customer
// SDK's job, spec §28.24) exactly as the T9 SDK verifier will, counts real executions, and dedupes on the
// Idempotency-Key. It is the external-effect witness the tests assert against.
type harness struct {
	secret    []byte
	tolerance time.Duration
	result    map[string]any
	respond   func(*harness, http.ResponseWriter, invokeSeen) // per-test status/body override

	mu    sync.Mutex
	execs int
	seen  map[string]bool // Idempotency-Key -> already executed
}

type invokeSeen struct {
	idempotencyKey string
	rawBody        []byte
}

func newHarness(t *testing.T) (*harness, *httptest.Server) {
	t.Helper()
	h := &harness{secret: []byte("remote-tool-secret"), tolerance: 5 * time.Minute,
		result: map[string]any{"ok": true}, seen: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(srv.Close)
	return h, srv
}

func (h *harness) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	id := r.Header.Get(webhook.HeaderID)
	sig := r.Header.Get(webhook.HeaderSignature)
	unix, err := strconv.ParseInt(r.Header.Get(webhook.HeaderTimestamp), 10, 64)
	if err != nil || !webhook.Verify(h.secret, id, time.Unix(unix, 0), raw, sig, time.Now(), h.tolerance) {
		w.WriteHeader(http.StatusUnauthorized) // verify-before-anything: an unsigned/tampered request never executes
		return
	}
	seen := invokeSeen{idempotencyKey: r.Header.Get("Idempotency-Key"), rawBody: raw}
	if h.respond != nil {
		h.respond(h, w, seen)
		return
	}
	h.mu.Lock()
	if !h.seen[seen.idempotencyKey] {
		h.execs++ // a NEW key is a real execution; a repeat is deduped (settles one execution)
		h.seen[seen.idempotencyKey] = true
	}
	h.mu.Unlock()
	writeResult(w, http.StatusOK, h.result)
}

func (h *harness) executions() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.execs
}

func writeResult(w http.ResponseWriter, status int, result map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"result": result})
}

// fakeLedger is an in-memory remote_tool_operations for the Docker-free unit tier (the pgx Operations is
// exercised in the component suite). It honours the one-pending-per-call invariant.
type fakeLedger struct {
	mu     sync.Mutex
	byOp   map[string]*fakeOp
	byCall map[string]*fakeOp
}

type fakeOp struct {
	callID string
	state  string
	result []byte
	deadln time.Time
}

func newFakeLedger() *fakeLedger {
	return &fakeLedger{byOp: map[string]*fakeOp{}, byCall: map[string]*fakeOp{}}
}

func (f *fakeLedger) Open(_ context.Context, in OpenOperation) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ex, ok := f.byCall[in.ToolCallID]; ok && ex.state == "pending" {
		return false, nil
	}
	op := &fakeOp{callID: in.ToolCallID, state: "pending", deadln: in.Deadline}
	f.byOp[in.OperationID] = op
	f.byCall[in.ToolCallID] = op
	return true, nil
}

func (f *fakeLedger) Poll(_ context.Context, operationID string) (string, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	op := f.byOp[operationID]
	if op == nil {
		return "", nil, errors.New("no such operation")
	}
	return op.state, op.result, nil
}

func (f *fakeLedger) CompleteSync(_ context.Context, operationID string, result []byte, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if op := f.byOp[operationID]; op != nil && op.state == "pending" {
		op.state, op.result = "completed", result
	}
	return nil
}

func (f *fakeLedger) Timeout(_ context.Context, operationID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if op := f.byOp[operationID]; op != nil && op.state == "pending" {
		op.state = "timed_out"
	}
	return nil
}

func (f *fakeLedger) ProberRead(_ context.Context, toolCallID string) (string, []byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	op := f.byCall[toolCallID]
	if op == nil || op.result == nil {
		return "", nil, false, nil
	}
	return op.state, op.result, true, nil
}

// completeByCall marks a tool_call's operation completed with a callback-delivered result (the async 202
// test drives it after a 202 so the executor's poll returns it — the executor's minted operation id is
// internal, but the row is shared by tool_call_id).
func (f *fakeLedger) completeByCall(toolCallID string, result map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if op := f.byCall[toolCallID]; op != nil {
		op.state, op.result = "completed", mustJSON(result)
	}
}

func baseInvocation(h *harness, url string) Invocation {
	return Invocation{
		URL: url, AllowPrivate: true, Secret: h.secret,
		ToolCallID: "tc_remote_1", ToolRevision: "pub.ns.tool@3", RunID: "run_1", AttemptID: "att_1",
		RequestHash: "sha256:abc", Arguments: map[string]any{"q": "x"},
		Org: "org_1", Project: "prj_1", SecretRef: "sig-ref", Fence: 5, TimeoutMS: 2000,
	}
}

// TestUnsignedOrTamperedRemoteRequestRejected proves the signed-transport half of TOL-016: a correctly
// signed invoke executes once against the real server; an invoke signed with the WRONG secret is refused
// (ErrRemoteRejected) and never executes; and a byte-tampered request (a valid signature over a different
// body) is rejected server-side — the MAC binds the exact body. The execution counter is the witness.
func TestUnsignedOrTamperedRemoteRequestRejected(t *testing.T) {
	h, srv := newHarness(t)
	exec := NewExecutor(newFakeLedger())
	ctx := context.Background()

	// A correctly signed invoke executes once.
	if _, err := exec.Invoke(ctx, baseInvocation(h, srv.URL)); err != nil {
		t.Fatalf("signed invoke error = %v, want a resolved result", err)
	}
	if got := h.executions(); got != 1 {
		t.Fatalf("executions after a signed invoke = %d, want 1", got)
	}

	// An invoke signed with the wrong secret is refused and never executes.
	bad := baseInvocation(h, srv.URL)
	bad.ToolCallID = "tc_remote_wrongsecret"
	bad.Secret = []byte("not-the-servers-secret")
	if _, err := exec.Invoke(ctx, bad); !errors.Is(err, ErrRemoteRejected) {
		t.Fatalf("wrong-secret invoke err = %v, want ErrRemoteRejected", err)
	}
	if got := h.executions(); got != 1 {
		t.Fatalf("executions after a wrong-secret invoke = %d, want still 1 (never executed)", got)
	}

	// A byte-tampered request: a valid signature over body A, but body B on the wire. The server recomputes
	// the MAC over B and rejects — the signature binds the EXACT body, not just the headers.
	bodyA := []byte(`{"protocol":"tool-http.v1","tool_call_id":"tc_x","arguments":{}}`)
	bodyB := []byte(`{"protocol":"tool-http.v1","tool_call_id":"tc_x","arguments":{"evil":true}}`)
	sig := webhook.NewSigner(h.secret).Headers("tc_x", time.Now(), 1, bodyA)
	req, _ := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(bodyB))
	for k, v := range sig {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("tampered request error = %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("tampered request status = %d, want 401", resp.StatusCode)
	}
	if got := h.executions(); got != 1 {
		t.Fatalf("executions after a tampered request = %d, want still 1", got)
	}
}

// TestRemoteDuplicateRetrySameToolCallIdSingleExecution proves the Idempotency-Key dedupe half of TOL-016
// over the signed transport (E10 T7's counter test, now signed): two invokes carrying the SAME
// tool_call_id — a retry — key the same Idempotency-Key, so the real server settles ONE execution.
func TestRemoteDuplicateRetrySameToolCallIdSingleExecution(t *testing.T) {
	h, srv := newHarness(t)
	exec := NewExecutor(newFakeLedger())
	ctx := context.Background()

	in := baseInvocation(h, srv.URL)
	if _, err := exec.Invoke(ctx, in); err != nil {
		t.Fatalf("first invoke error = %v", err)
	}
	// A retry (same tool_call_id) — the fake ledger's completed row would normally short-circuit in the
	// broker, but here we drive the EXECUTOR directly to prove the SERVER dedupes on the Idempotency-Key.
	retry := baseInvocation(h, srv.URL)
	retry.ToolCallID = in.ToolCallID
	if _, err := exec.Invoke(ctx, retry); err != nil {
		t.Fatalf("retry invoke error = %v", err)
	}
	if got := h.executions(); got != 1 {
		t.Fatalf("executions after a same-id retry = %d, want 1 (Idempotency-Key dedupe)", got)
	}
}

// TestRemoteEgressVetted proves the SSRF gate on the outbound invoke: a remote tool endpoint pointed at a
// private/loopback address WITHOUT the self-host flag is a terminal egress deny — the invoke never leaves
// the process, so the server never executes.
func TestRemoteEgressVetted(t *testing.T) {
	h, srv := newHarness(t)
	exec := NewExecutor(newFakeLedger())

	in := baseInvocation(h, srv.URL)
	in.AllowPrivate = false // a PUBLIC remote tool must not resolve to a loopback/private address
	_, err := exec.Invoke(context.Background(), in)
	if !errors.Is(err, egress.ErrDenied) {
		t.Fatalf("loopback invoke without allow-private err = %v, want egress.ErrDenied", err)
	}
	if got := h.executions(); got != 0 {
		t.Fatalf("executions after an egress-denied invoke = %d, want 0", got)
	}
}

// TestExecutorAsync202PollsUntilCallback proves the async path: a server that answers 202 makes the
// executor poll the durable operation until the (out-of-band) signed callback resolves it, then return
// the callback-delivered result. Here the fake ledger stands in for the callback landing.
func TestExecutorAsync202PollsUntilCallback(t *testing.T) {
	h, srv := newHarness(t)
	h.respond = func(_ *harness, w http.ResponseWriter, _ invokeSeen) {
		w.WriteHeader(http.StatusAccepted) // 202: result comes later via the callback
	}
	ledger := newFakeLedger()
	exec := NewExecutor(ledger, WithPollInterval(5*time.Millisecond))

	in := baseInvocation(h, srv.URL)
	in.ToolCallID = "tc_async_1"
	in.TimeoutMS = 2000
	go func() {
		time.Sleep(25 * time.Millisecond)
		ledger.completeByCall("tc_async_1", map[string]any{"answer": 7})
	}()
	out, err := exec.Invoke(context.Background(), in)
	if err != nil {
		t.Fatalf("async invoke error = %v", err)
	}
	if out["answer"] != float64(7) {
		t.Fatalf("async result = %v, want the callback-delivered {answer:7}", out)
	}
}

// TestExecutorAsyncTimesOutWhenNoCallback proves the deadline path: a 202 with no callback before the
// deadline flips the operation timed_out and returns ErrRemoteTimeout — the durable executing marker then
// carries the tool_call to uncertain (the reconcile machine), never a silent hang.
func TestExecutorAsyncTimesOutWhenNoCallback(t *testing.T) {
	h, srv := newHarness(t)
	h.respond = func(_ *harness, w http.ResponseWriter, _ invokeSeen) { w.WriteHeader(http.StatusAccepted) }
	ledger := newFakeLedger()
	exec := NewExecutor(ledger, WithPollInterval(5*time.Millisecond))

	in := baseInvocation(h, srv.URL)
	in.ToolCallID = "tc_async_timeout"
	in.TimeoutMS = 40 // no callback arrives before this
	if _, err := exec.Invoke(context.Background(), in); !errors.Is(err, ErrRemoteTimeout) {
		t.Fatalf("no-callback async invoke err = %v, want ErrRemoteTimeout", err)
	}
}

// TestRemoteResultSchemaValidatedUntrusted proves the trust-boundary posture (spec §28.24): a remote
// tool result is UNTRUSTED content the broker schema-validates like any tool output. A result missing a
// required field is rejected (ErrInvalidArguments); a result carrying EXTRA fields (including a
// capability-shaped one) rides through as inert DATA — the tool-http.v1 envelope has no capability field,
// so a customer server can never smuggle an authority grant through its result.
func TestRemoteResultSchemaValidatedUntrusted(t *testing.T) {
	h, srv := newHarness(t)
	exec := NewExecutor(newFakeLedger())
	ctx := context.Background()

	outSchema := map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"ok": map[string]any{"type": "string"}},
		"required":             []any{"ok"},
		"additionalProperties": true,
	}
	tool := toolbroker.Tool{
		Name: "remote.echo", InputSchema: map[string]any{"type": "object"}, OutputSchema: outSchema,
		ReplayClass: toolbroker.ClassIdempotent,
		Exec: func(ctx context.Context, env toolbroker.ExecEnv, args map[string]any) (map[string]any, error) {
			in := baseInvocation(h, srv.URL)
			in.ToolCallID = string(env.CallID)
			in.Arguments = args
			return exec.Invoke(ctx, in)
		},
	}
	broker := toolbroker.New(tool)

	// A conforming result carrying an extra capability-shaped field passes — the extra is inert data.
	h.result = map[string]any{"ok": "yes", "capability": "admin", "granted_scopes": []any{"*"}}
	out, err := broker.Execute(ctx, contracts.ToolCallID("tc_ok"), "remote.echo", map[string]any{}, 1, toolbroker.ExecEnv{})
	if err != nil {
		t.Fatalf("conforming result execute error = %v", err)
	}
	if out.Result["ok"] != "yes" {
		t.Fatalf("result = %v, want ok:yes", out.Result)
	}
	if out.Result["capability"] != "admin" {
		t.Fatalf("extra field must ride through as inert data, got %v", out.Result["capability"])
	}

	// A schema-violating result (missing the required ok) is rejected as untrusted.
	h.result = map[string]any{"nope": 1}
	if _, err := broker.Execute(ctx, contracts.ToolCallID("tc_bad"), "remote.echo", map[string]any{}, 1, toolbroker.ExecEnv{}); !errors.Is(err, toolbroker.ErrInvalidArguments) {
		t.Fatalf("schema-violating result err = %v, want ErrInvalidArguments", err)
	}
}
