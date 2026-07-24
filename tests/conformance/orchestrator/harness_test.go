// Package orchestrator_test is the §35.1 external-orchestrator conformance suite (E17 T8). It pins the
// FIVE-STEP contract that ANY durable orchestrator (Temporal, Restate, a CI pipeline, or a plain script)
// follows to drive a Palai run — it is NOT tied to one vendor. The fixtures drive a scripted FAKE
// orchestrator against the REAL api.NewRouter (genuine HTTP round-trips through the real middleware +
// handlers), backed by an in-process fake control plane that plays the run engine + callback deliverer.
//
// The fake control plane's completion is EXPLICIT (complete(runID)), so the whole five-step sequence is
// deterministic and Docker-free: the durable invariants themselves (idempotent admission, the callback
// outbox) are proven against real Postgres in tests/component + apps/control-plane/internal/automation.
// A run against a real Temporal instance is plan §6 operator leg 6; no native adapter is written.
//
// It carries NO build tag, exactly like its tests/conformance siblings (api, engine, contracts): it is
// Docker-free and in-process, so `go test ./...` (make test-unit / verify) gates it with no extra tier.
package orchestrator_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/contracts"
)

const orchToken = "orch-conformance-token"

var orchScope = middleware.Scope{Organization: "org_orch", Project: "prj_orch", Principal: "prin_orch"}

// run is one canonical run the fake control plane owns. The identity (responseID/runID/sessionID) is
// minted once at admission and NEVER replaced; status advances queued → terminal on an explicit
// complete(). events is the session journal the SSE stream tails.
type run struct {
	mu          sync.Mutex
	responseID  string
	runID       string
	sessionID   string
	createdBody []byte // the queued projection returned at create and REPLAYED on a same-key retry
	status      string
	events      []contracts.Event
	webhookURL  string // registered callback for the webhook wait mode (out-of-band, see registerWebhook)
}

// fakeControlPlane implements middleware.Verifier + api.Admitter + api.EventReader + api.SessionManager,
// so the real router serves the whole five-step surface over it. Admission dedupes by Idempotency-Key —
// the single retry owner that collapses a retry storm to one run.
type fakeControlPlane struct {
	mu        sync.Mutex
	byKey     map[string]*run // idempotency key → run (dedupe / reconcile-by-key)
	byID      map[string]*run // responseID → run
	bySession map[string]*run // sessionID → run
	httpc     *http.Client
}

func newFakeControlPlane() *fakeControlPlane {
	return &fakeControlPlane{
		byKey:     map[string]*run{},
		byID:      map[string]*run{},
		bySession: map[string]*run{},
		httpc:     &http.Client{Timeout: 5 * time.Second},
	}
}

// VerifyAPIKey accepts the single conformance token, resolving it to the fixed scope. Handlers derive
// scope from this identity, never a request-body field.
func (f *fakeControlPlane) VerifyAPIKey(_ context.Context, token string) (middleware.Scope, error) {
	if token != orchToken {
		return middleware.Scope{}, middleware.ErrInvalidToken
	}
	return orchScope, nil
}

// AdmitResponse is STEP 1's server half: it dedupes by IdempotencyKey. A NEW key mints a run born
// `queued` (seeded with one non-terminal journal event); the SAME key REPLAYS the original run's
// queued handle verbatim (Replayed=true) — so a retry storm and reconcile-by-key both resolve to the
// one run, never a duplicate.
func (f *fakeControlPlane) AdmitResponse(_ context.Context, req api.AdmitRequest) (api.AdmitResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.byKey[req.IdempotencyKey]; r != nil {
		return api.AdmitResult{ResponseID: r.responseID, Body: r.createdBody, Replayed: true}, nil
	}
	r := &run{
		responseID:  req.ResponseID,
		runID:       req.RunID,
		sessionID:   req.SessionID,
		createdBody: req.Body,
		status:      "queued",
		events:      []contracts.Event{event(req.SessionID, req.RunID, 1, "run.accepted.v1", "queued")},
	}
	f.byKey[req.IdempotencyKey] = r
	f.byID[r.responseID] = r
	f.bySession[r.sessionID] = r
	return api.AdmitResult{ResponseID: r.responseID, Body: r.createdBody}, nil
}

// GetResponse is the poll wait's read + STEP 4's structured result: it returns the run's CURRENT
// projection (queued before complete, the terminal result + artifact ref after).
func (f *fakeControlPlane) GetResponse(_ context.Context, _ middleware.Scope, id string) (api.RetrieveResult, error) {
	f.mu.Lock()
	r := f.byID[id]
	f.mu.Unlock()
	if r == nil {
		return api.RetrieveResult{}, nil
	}
	return api.RetrieveResult{Body: r.projection(), Found: true}, nil
}

// CancelResponse is STEP 3's cancel: it advances a non-terminal run to canceled and returns its
// projection. Naturally idempotent — a canceled terminal is monotonic.
func (f *fakeControlPlane) CancelResponse(_ context.Context, _ middleware.Scope, id string) (api.RetrieveResult, error) {
	f.mu.Lock()
	r := f.byID[id]
	f.mu.Unlock()
	if r == nil {
		return api.RetrieveResult{}, nil
	}
	r.mu.Lock()
	if r.status == "queued" {
		r.status = "canceled"
		r.events = append(r.events, event(r.sessionID, r.runID, len(r.events)+1, "run.canceled.v1", "canceled"))
	}
	r.mu.Unlock()
	return api.RetrieveResult{Body: r.projection(), Found: true}, nil
}

// ListResponses pages the admitted runs newest-first. The suite never lists, so a minimal keyset is
// enough to satisfy the interface.
func (f *fakeControlPlane) ListResponses(_ context.Context, _ middleware.Scope, q api.ListQuery) ([]api.ListRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows := make([]api.ListRow, 0, len(f.byID))
	for id, r := range f.byID {
		rows = append(rows, api.ListRow{ID: id, CreatedAt: time.Unix(0, int64(len(r.events))), Body: r.projection()})
		if len(rows) >= q.Limit {
			break
		}
	}
	return rows, nil
}

// EventReader — the SSE wait's server half. SessionExists gates the stream; After tails the run's journal.
func (f *fakeControlPlane) SessionExists(_ context.Context, _, _, sessionID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.bySession[sessionID]
	return ok, nil
}

func (f *fakeControlPlane) ResolveCursor(_ context.Context, _, _, _, _ string) (int64, bool, error) {
	return 0, false, nil
}

func (f *fakeControlPlane) After(_ context.Context, _, _, sessionID string, cursor int64, limit int) ([]contracts.Event, error) {
	f.mu.Lock()
	r := f.bySession[sessionID]
	f.mu.Unlock()
	if r == nil {
		return nil, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]contracts.Event, 0, len(r.events))
	for _, e := range r.events {
		if int64(e.Sequence) > cursor {
			out = append(out, e)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeControlPlane) RecordAttachDenied(_ context.Context, _, _, _, _ string) error { return nil }

// SessionManager — STEP 3's message command. AcceptCommand accepts a steer for a known session; an
// unknown session is a 404 with no existence disclosure.
func (f *fakeControlPlane) CreateSession(_ context.Context, _ middleware.Scope) (api.SessionResult, error) {
	return api.SessionResult{}, nil
}

func (f *fakeControlPlane) GetSession(_ context.Context, _ middleware.Scope, id string) (api.SessionResult, error) {
	f.mu.Lock()
	r := f.bySession[id]
	f.mu.Unlock()
	if r == nil {
		return api.SessionResult{Found: false}, nil
	}
	body, _ := json.Marshal(map[string]any{"id": id, "object": "session", "status": "active"})
	return api.SessionResult{Body: body, Found: true}, nil
}

func (f *fakeControlPlane) ListSessions(_ context.Context, _ middleware.Scope, _ api.ListQuery) ([]api.ListRow, error) {
	return nil, nil
}

func (f *fakeControlPlane) AcceptCommand(_ context.Context, _ middleware.Scope, sessionID string, req contracts.CommandCreateRequest) (api.CommandResult, error) {
	f.mu.Lock()
	_, ok := f.bySession[sessionID]
	f.mu.Unlock()
	if !ok {
		return api.CommandResult{SessionNotFound: true}, nil
	}
	body, _ := json.Marshal(map[string]any{
		"id": "cmd_" + string(req.CommandID), "object": "command", "kind": req.Kind, "delivery": req.Delivery, "status": "queued",
	})
	return api.CommandResult{Body: body}, nil
}

// --- run engine controls (the fake plays Palai's run engine + callback deliverer) -----------------

// complete advances a run to its terminal state: it appends the terminal journal event and, if a webhook
// was registered, POSTs the terminal projection to the callback URL (Palai's async callback delivery). The
// webhook body is a NOTIFICATION the orchestrator does not trust — it reconciles by reading the canonical
// result back (the receiver GETs the response). complete() is explicit so the five-step sequence is race-free.
func (f *fakeControlPlane) complete(t *testing.T, runID string) {
	t.Helper()
	f.mu.Lock()
	var r *run
	for _, cand := range f.byID {
		if cand.runID == runID {
			r = cand
			break
		}
	}
	f.mu.Unlock()
	if r == nil {
		t.Fatalf("complete: no run %q", runID)
	}
	r.mu.Lock()
	if r.status == "queued" {
		r.status = "completed"
		r.events = append(r.events, event(r.sessionID, r.runID, len(r.events)+1, "run.completed.v1", "completed"))
	}
	url, body := r.webhookURL, r.projection()
	r.mu.Unlock()

	if url != "" {
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			t.Fatalf("build webhook request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := f.httpc.Do(req)
		if err != nil {
			t.Fatalf("deliver webhook: %v", err)
		}
		_ = resp.Body.Close()
	}
}

// registerWebhook records the orchestrator's callback URL for a run. In production the callback rides the
// create request's `callback` field and the outbox delivers it (proven in
// apps/control-plane/internal/automation/callback_component_test.go); the conformance tier registers it
// out of band so the webhook wait mode's orchestrator-facing contract is exercised Docker-free.
func (f *fakeControlPlane) registerWebhook(runID, url string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.byID {
		if r.runID == runID {
			r.mu.Lock()
			r.webhookURL = url
			r.mu.Unlock()
			return
		}
	}
}

// projection renders the run's current wire projection: the queued handle before complete, the terminal
// result (STEP 4: structured output + an artifact ref in metadata) after.
func (r *run) projection() []byte {
	resp := contracts.Response{
		ID:        contracts.ResponseID(r.responseID),
		Object:    "response",
		Status:    r.status,
		Model:     "",
		Output:    []contracts.ContentItem{},
		Usage:     contracts.Usage{},
		RunID:     contracts.RunID(r.runID),
		SessionID: contracts.SessionID(r.sessionID),
		CreatedAt: "2026-07-24T00:00:00Z",
	}
	if r.status == "completed" {
		resp.Output = []contracts.ContentItem{{"type": "output_text", "text": "orchestrated result"}}
		resp.Metadata = map[string]any{"artifacts": []any{map[string]any{"id": "artifact_" + r.runID, "kind": "result"}}}
	}
	body, _ := json.Marshal(resp)
	return body
}

// event builds one canonical journal event for the SSE stream.
func event(sessionID, runID string, seq int, typ, status string) contracts.Event {
	return contracts.Event{
		ID:          contracts.EventID("evt_" + runID + "_" + strconv.Itoa(seq)),
		Type:        typ,
		Sequence:    seq,
		SessionID:   contracts.SessionID(sessionID),
		RunID:       contracts.RunID(runID),
		Source:      "palai/conformance",
		Specversion: "1.0",
		Time:        "2026-07-24T00:00:00Z",
		Data:        map[string]any{"status": status},
	}
}

// newTestServer mounts the REAL router + middleware over a fresh fake control plane. Only the response +
// session + events seams are wired (the five-step contract's surface); the rest stay nil.
func newTestServer(t *testing.T) (*httptest.Server, *fakeControlPlane) {
	t.Helper()
	f := newFakeControlPlane()
	sse := api.SSEConfig{PollInterval: 2 * time.Millisecond, Heartbeat: time.Hour, WriteTimeout: 5 * time.Second, BatchLimit: 256}
	srv := httptest.NewServer(api.NewRouter(f, f, f, f, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, sse, nil, nil))
	t.Cleanup(srv.Close)
	return srv, f
}

func (f *fakeControlPlane) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.byID)
}

// ── the orchestrator side: a plain Go script driving the REAL HTTP surface ───────────────────────
//
// These helpers ARE the vendor-neutral orchestrator (a script — one of §35.1's four orchestrator
// classes). They speak only the public contract (Authorization + Idempotency-Key headers, the JSON
// bodies), so any Temporal/Restate/CI implementation that speaks the same contract is equally pinned.

const orchTaskInput = "orchestrated task"

// handle is Palai's CANONICAL run identity returned at create. It is NEVER the external workflow id —
// the whole contract keeps the two separate (§38.6).
type handle struct {
	responseID string
	runID      string
	sessionID  string
	status     string
}

// derivedKey mirrors the TS kit's workflowIdempotencyKey (sdks/typescript/src/orchestrator.ts): the
// idempotency key is a PURE function of the external workflow id — sha256, hex, first 32 chars, "wf_"
// prefixed — so a Go orchestrator and the TS kit bridge an external workflow identity to Palai's single
// retry owner IDENTICALLY. Same workflow id ⇒ same key (replays reconcile); different id ⇒ different key.
func derivedKey(workflowID string) string {
	sum := sha256.Sum256([]byte(workflowID))
	return "wf_" + hex.EncodeToString(sum[:])[:32]
}

// createBody is STEP 1's request: the run input plus the external workflow id as UNTRUSTED correlation
// metadata (§38.6 — metadata.workflow_id never overrides identity, it is just a label the run carries).
func createBody(workflowID string) string {
	return `{"input":"` + orchTaskInput + `","metadata":{"workflow_id":"` + workflowID + `"}}`
}

func do(t *testing.T, method, url, idemKey, body string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	req.Header.Set("Authorization", "Bearer "+orchToken)
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, url, err)
	}
	return resp
}

func decode(t *testing.T, resp *http.Response) contracts.Response {
	t.Helper()
	defer resp.Body.Close()
	var out contracts.Response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}

// create runs STEP 1 against the real router with the derived key and returns Palai's canonical handle.
// Create is 202 for BOTH a fresh admission and a same-key replay (§20.9) — the replay carries the
// ORIGINAL identity, which is what reconcile-by-key relies on.
func create(t *testing.T, srv *httptest.Server, workflowID, key string) handle {
	t.Helper()
	resp := do(t, http.MethodPost, srv.URL+"/v1/responses", key, createBody(workflowID))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: status = %d, want 202", resp.StatusCode)
	}
	r := decode(t, resp)
	return handle{responseID: string(r.ID), runID: string(r.RunID), sessionID: string(r.SessionID), status: r.Status}
}

// command runs STEP 3: it posts a durable command to the run's session. Acceptance is 202 (durably
// queued, not applied). command_id carries the idempotency, so no Idempotency-Key header.
func command(t *testing.T, srv *httptest.Server, sessionID, body string) {
	t.Helper()
	resp := do(t, http.MethodPost, srv.URL+"/v1/sessions/"+sessionID+"/commands", "", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("command: status = %d, want 202 (body %s)", resp.StatusCode, body)
	}
}

// getResult reads the canonical terminal Response by identity (STEP 4 + the webhook mode's read-back).
func getResult(t *testing.T, srv *httptest.Server, responseID string) contracts.Response {
	t.Helper()
	resp := do(t, http.MethodGet, srv.URL+"/v1/responses/"+responseID, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get result: status = %d, want 200", resp.StatusCode)
	}
	return decode(t, resp)
}

func isTerminal(status string) bool {
	switch status {
	case "completed", "failed", "canceled", "timed_out", "budget_exceeded", "expired":
		return true
	}
	return false
}

// pollWait is STEP 2 (poll): it polls the run to a terminal projection. Bounded so a stuck run fails
// the test rather than hangs.
func pollWait(t *testing.T, srv *httptest.Server, responseID string) contracts.Response {
	t.Helper()
	for i := 0; i < 200; i++ {
		r := getResult(t, srv, responseID)
		if isTerminal(r.Status) {
			return r
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("poll: response %s never reached a terminal status", responseID)
	return contracts.Response{}
}

// streamWait is STEP 2 (SSE): it drains the session event stream to the terminal frame (the server
// closes the connection after it). It asserts the stream carried the NON-terminal run.accepted.v1 as
// well as the terminal run.completed.v1 — proving it tails the journal, not just reports an end state.
func streamWait(t *testing.T, srv *httptest.Server, sessionID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/sessions/"+sessionID+"/events", nil)
	if err != nil {
		t.Fatalf("build stream request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+orchToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream: status = %d, want 200", resp.StatusCode)
	}
	var sawAccepted, sawTerminal bool
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: run.accepted.v1"):
			sawAccepted = true
		case strings.HasPrefix(line, "event: run.completed.v1"):
			sawTerminal = true
		}
	}
	if !sawAccepted {
		t.Fatal("stream: never saw the non-terminal run.accepted.v1 (not tailing the journal)")
	}
	if !sawTerminal {
		t.Fatal("stream: never saw the terminal run.completed.v1 (stream did not close on terminal)")
	}
}

// webhookReceiver is the orchestrator's callback endpoint for the WEBHOOK wait mode. It records the
// delivered notification; the orchestrator does NOT trust it — it reconciles by reading the canonical
// result back (§35.2).
type webhookReceiver struct {
	srv  *httptest.Server
	mu   sync.Mutex
	body []byte
}

func newWebhookReceiver(t *testing.T) *webhookReceiver {
	t.Helper()
	rc := &webhookReceiver{}
	rc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		rc.body = b
		rc.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(rc.srv.Close)
	return rc
}

func (rc *webhookReceiver) delivered() []byte {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.body
}

// assertResultAndArtifacts is STEP 4: the terminal Response carries a structured result (output) AND an
// artifact reference in metadata.
func assertResultAndArtifacts(t *testing.T, r contracts.Response) {
	t.Helper()
	if r.Status != "completed" {
		t.Fatalf("result: status = %q, want completed", r.Status)
	}
	if len(r.Output) == 0 {
		t.Fatal("result: no structured output on the terminal response")
	}
	arts, ok := r.Metadata["artifacts"].([]any)
	if !ok || len(arts) == 0 {
		t.Fatalf("result: no artifacts in metadata, got %v", r.Metadata)
	}
}

// TestOrchestratorFiveStepContract pins the §35.1 five-step contract end to end across ALL THREE wait
// modes. Each mode runs the SAME five steps against a fresh real router; only STEP 2 (how the
// orchestrator observes the run settle) differs. The fake control plane plays Palai's run engine, so
// complete() is the explicit terminal signal and the whole sequence is deterministic, Docker-free.
func TestOrchestratorFiveStepContract(t *testing.T) {
	for _, mode := range []string{"poll", "sse", "webhook"} {
		t.Run(mode, func(t *testing.T) {
			srv, f := newTestServer(t)
			wf := "wf-order-" + mode // the EXTERNAL orchestrator's workflow id
			key := derivedKey(wf)

			// STEP 1 — create-with-workflow-ID-metadata + idempotency-key.
			run := create(t, srv, wf, key)
			if run.status != "queued" {
				t.Fatalf("create: status = %q, want queued", run.status)
			}
			// Identity separation (§38.6): the canonical identity is server-minted and never the
			// external workflow id or the derived key.
			if run.responseID == "" || run.runID == "" || run.sessionID == "" {
				t.Fatalf("create did not mint a full canonical identity: %+v", run)
			}
			if run.responseID == wf || run.runID == wf || run.sessionID == wf ||
				run.responseID == key || run.runID == key || run.sessionID == key {
				t.Fatalf("canonical identity leaked the external workflow id/key: %+v (wf=%s key=%s)", run, wf, key)
			}

			// STEP 3 — message + approval commands, while the run is still non-terminal. (The cancel
			// command category settles a run, so it has its own test.)
			command(t, srv, run.sessionID, `{"command_id":"cmd-msg-1","kind":"send_message","delivery":"steer","message":"focus on the failing test"}`)
			command(t, srv, run.sessionID, `{"command_id":"cmd-appr-1","kind":"approve"}`)

			// STEP 2 — wait. complete() is the run engine settling the run; the mode is how the
			// orchestrator observes it.
			var result contracts.Response
			switch mode {
			case "poll":
				// A first poll observes the still-queued run, proving the loop's non-terminal path.
				if got := getResult(t, srv, run.responseID); got.Status != "queued" {
					t.Fatalf("poll before complete: status = %q, want queued", got.Status)
				}
				f.complete(t, run.runID)
				result = pollWait(t, srv, run.responseID)
			case "sse":
				f.complete(t, run.runID)
				streamWait(t, srv, run.sessionID)
				result = getResult(t, srv, run.responseID)
			case "webhook":
				rcv := newWebhookReceiver(t)
				f.registerWebhook(run.runID, rcv.srv.URL)
				f.complete(t, run.runID) // POSTs the terminal notification to the receiver
				if len(rcv.delivered()) == 0 {
					t.Fatal("webhook: no callback notification was delivered")
				}
				// The orchestrator does NOT trust the callback body — it reads the canonical result back.
				result = getResult(t, srv, run.responseID)
			}

			// STEP 4 — structured result + artifacts.
			assertResultAndArtifacts(t, result)
			if string(result.ID) != run.responseID {
				t.Fatalf("result identity drifted: got %s, want %s", result.ID, run.responseID)
			}

			// STEP 5 — reconcile-by-key: the SAME derived key replays the ONE run, never a duplicate.
			reconciled := create(t, srv, wf, key)
			if reconciled.responseID != run.responseID {
				t.Fatalf("reconcile minted a duplicate: got %s, want the original %s", reconciled.responseID, run.responseID)
			}
			if n := f.runCount(); n != 1 {
				t.Fatalf("expected exactly one run across the five steps, got %d", n)
			}
		})
	}
}

// TestOrchestratorCancelCommand covers STEP 3's cancel category: cancel PROPAGATION via the real
// response-scoped endpoint (§35.2). A retried cancel settles once (a canceled terminal is monotonic),
// and the canceled run is itself a terminal STEP-4 result.
func TestOrchestratorCancelCommand(t *testing.T) {
	srv, _ := newTestServer(t)
	wf := "wf-cancel-5501"
	run := create(t, srv, wf, derivedKey(wf))

	for i := 0; i < 2; i++ { // idempotent: the second cancel is a monotonic no-op
		resp := do(t, http.MethodPost, srv.URL+"/v1/responses/"+run.responseID+"/cancel", "", "")
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("cancel #%d: status = %d, want 202", i, resp.StatusCode)
		}
		if r := decode(t, resp); r.Status != "canceled" {
			t.Fatalf("cancel #%d: status = %q, want canceled", i, r.Status)
		}
	}
	if r := getResult(t, srv, run.responseID); r.Status != "canceled" {
		t.Fatalf("result after cancel: status = %q, want canceled", r.Status)
	}
}

// TestOrchestratorKillAndReconcile is the kill-and-reconcile leg: an orchestrator creates a run, then
// CRASHES before persisting Palai's run id (it kept only its workflow id + the original request). On
// recovery it re-derives the key from the workflow id and replays the identical create — which must
// resolve to the SAME run, never a duplicate. A DIFFERENT workflow id is the control: it mints a
// distinct run, proving the dedupe is key-scoped, not a global collapse.
func TestOrchestratorKillAndReconcile(t *testing.T) {
	srv, f := newTestServer(t)
	wf := "wf-crash-9931"

	// The orchestrator starts the run. create() rebuilds the identical request from the workflow id,
	// so the reconcile below replays the EXACT same body — the recovery contract.
	first := create(t, srv, wf, derivedKey(wf))

	// KILL mid-flight: the orchestrator discards everything but {workflowId, request} — simulated by
	// not carrying `first` forward into the recovery path below.

	// RECONCILE: re-derive the key from the workflow id alone and replay the identical request.
	reconciled := create(t, srv, wf, derivedKey(wf))
	if reconciled.responseID != first.responseID {
		t.Fatalf("kill+reconcile minted a duplicate: got %s, want the original %s", reconciled.responseID, first.responseID)
	}
	if n := f.runCount(); n != 1 {
		t.Fatalf("kill+reconcile: expected exactly one run, got %d", n)
	}

	// The reconciled handle resolves the real outcome: the run engine settles it and the canonical
	// result reads back terminal — recovery lost nothing.
	f.complete(t, first.runID)
	if r := getResult(t, srv, reconciled.responseID); r.Status != "completed" {
		t.Fatalf("reconciled run did not settle: status = %q, want completed", r.Status)
	}

	// CONTROL: a DIFFERENT workflow id is a DIFFERENT run — dedupe is scoped to the key, not global.
	other := create(t, srv, "wf-crash-other", derivedKey("wf-crash-other"))
	if other.responseID == first.responseID {
		t.Fatalf("a different workflow id collapsed onto the original run %s", first.responseID)
	}
	if n := f.runCount(); n != 2 {
		t.Fatalf("expected two distinct runs after the control, got %d", n)
	}
}
