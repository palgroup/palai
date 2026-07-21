//go:build e2e

// Package responses holds the end-to-end proof for the response kernel: admission →
// durable run job → orchestrator → live subprocess engine → committed terminal
// response. It runs only under `make test-e2e TEST=responses`, which starts a
// throwaway PostgreSQL container and exports PALAI_E2E_POSTGRES_URL and
// PALAI_ENGINE_DIR. The real API router and the real coordinator run in-process
// against that database; the reference engine runs as a real subprocess through the
// injectable EngineChannel seam, driven by a deterministic fake provider (no network,
// no credentials). The OCI/mTLS runner path is the same seam's production
// implementation, proven separately in Tasks 11b/11c.
package responses

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	"github.com/palgroup/palai/packages/runner"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s is required; run make test-e2e TEST=responses", name)
	}
	return v
}

func newID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

// harness wires the real store, the real API router, and the coordinator against a
// shared PostgreSQL, plus a deterministic fake provider and the reference engine dir.
type harness struct {
	t         *testing.T
	repo      *store.Store
	spine     *coordinator.Store
	base      string
	token     string
	tenant    coordinator.Tenant
	provider  *scriptedProvider
	engineDir string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	repo, err := store.Open(ctx, requireEnv(t, "PALAI_E2E_POSTGRES_URL"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(repo.Close)
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	token := newID("e2e-tok")
	tenant := seedTenantWithKey(t, repo.Spine().Pool(), token)
	srv := httptest.NewServer(api.NewRouter(repo, repo, repo, repo, repo, repo, nil, nil, nil, api.SSEConfig{}, nil))
	t.Cleanup(srv.Close)

	return &harness{
		t: t, repo: repo, spine: repo.Spine(), base: srv.URL, token: token, tenant: tenant,
		provider: &scriptedProvider{}, engineDir: requireEnv(t, "PALAI_ENGINE_DIR"),
	}
}

// seedTenantWithKey creates org -> project -> principal -> api_key; the stored verifier
// is the hash of token, never token itself.
func seedTenantWithKey(t *testing.T, pool *pgxpool.Pool, token string) coordinator.Tenant {
	t.Helper()
	ctx := context.Background()
	tenant := coordinator.Tenant{Organization: newID("org"), Project: newID("prj")}
	principalID := newID("prin")
	exec := func(sql string, args ...any) {
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec %q error = %v", sql, err)
		}
	}
	exec(`INSERT INTO organizations (id) VALUES ($1)`, tenant.Organization)
	exec(`INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, tenant.Project, tenant.Organization)
	exec(`INSERT INTO principals (id, organization_id, project_id, kind) VALUES ($1, $2, $3, 'service')`,
		principalID, tenant.Organization, tenant.Project)
	exec(`INSERT INTO api_keys (id, organization_id, project_id, principal_id, key_hash) VALUES ($1, $2, $3, $4, $5)`,
		newID("key"), tenant.Organization, tenant.Project, principalID, coordinator.HashAPIKey(token))
	return tenant
}

// admit posts a response over the real API, minting a session + root run + the queued
// dispatch job. It returns the response, session, and run ids.
func (h *harness) admit() (responseID, sessionID, runID string) {
	h.t.Helper()
	return h.admitWith(`{"input":"do the work"}`, newID("idem"))
}

// admitWith is admit with a caller-chosen body and idempotency key, so a retention
// test can create a store:false response and later replay the exact same request.
func (h *harness) admitWith(body, idemKey string) (responseID, sessionID, runID string) {
	h.t.Helper()
	resp := h.postResponse(body, idemKey, h.token)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		h.t.Fatalf("POST status = %d, want 202", resp.StatusCode)
	}
	var r contracts.Response
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		h.t.Fatalf("decode response error = %v", err)
	}
	return string(r.ID), string(r.SessionID), string(r.RunID)
}

// postResponse issues POST /v1/responses and returns the raw response so a caller can
// assert on a non-2xx status (e.g. a 410 replay after purge).
func (h *harness) postResponse(body, idemKey, token string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.base+"/v1/responses", strings.NewReader(body))
	if err != nil {
		h.t.Fatalf("build POST error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", idemKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("POST /v1/responses error = %v", err)
	}
	return resp
}

// getResponse issues GET /v1/responses/{id} with the given bearer token and returns
// the raw response.
func (h *harness) getResponse(responseID, token string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.base+"/v1/responses/"+responseID, nil)
	if err != nil {
		h.t.Fatalf("build GET error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("GET /v1/responses error = %v", err)
	}
	return resp
}

// purgedAt reads a response's purge timestamp, or nil if it has not been purged.
func (h *harness) purgedAt(responseID string) *time.Time {
	h.t.Helper()
	var purged *time.Time
	if err := h.spine.Pool().QueryRow(context.Background(),
		`SELECT purged_at FROM responses WHERE id=$1 AND organization_id=$2 AND project_id=$3`,
		responseID, h.tenant.Organization, h.tenant.Project).Scan(&purged); err != nil {
		h.t.Fatalf("read purged_at error = %v", err)
	}
	return purged
}

// newOrchestrator builds the kernel over the given dialer with the deterministic fake
// provider and the conformance math tool.
func (h *harness) newOrchestrator(dialer execution.EngineDialer) *execution.Orchestrator {
	return h.newOrchestratorWithAdapter(dialer, h.provider)
}

// newOrchestratorWithAdapter builds the kernel over a caller-supplied model adapter — the
// reclaim fault proof swaps in an idempotent, crash-injecting provider.
func (h *harness) newOrchestratorWithAdapter(dialer execution.EngineDialer, adapter modelbroker.ModelAdapter) *execution.Orchestrator {
	return h.newOrchestratorWithTools(dialer, adapter)
}

// newOrchestratorWithTools is newOrchestratorWithAdapter with extra tools registered beyond the
// conformance math add — a stack opts into the model-facing task/todo/file tools this way.
func (h *harness) newOrchestratorWithTools(dialer execution.EngineDialer, adapter modelbroker.ModelAdapter, extra ...toolbroker.Tool) *execution.Orchestrator {
	models := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"fake": adapter},
		Secrets:  modelbroker.StaticResolver{modelbroker.SecretRef("model"): "unused"},
	})
	tools := toolbroker.New(append([]toolbroker.Tool{toolbroker.ConformanceMathAdd()}, extra...)...)
	return execution.NewOrchestrator(h.repo, dialer, models, tools)
}

// runWorker starts a coordinator worker whose handler executes the claimed run job
// through the orchestrator. It returns a stop func the test defers.
func (h *harness) runWorker(orch *execution.Orchestrator) func() {
	return h.runWorkerWithRetry(orch, coordinator.RetryPolicy{MaxAttempts: 1, BaseBackoff: 10 * time.Millisecond, MaxBackoff: 100 * time.Millisecond})
}

// runWorkerWithRetry is runWorker with a caller-chosen retry policy — the dead-letter
// bridge proof drives every attempt to failure until the ceiling dead-letters the job.
func (h *harness) runWorkerWithRetry(orch *execution.Orchestrator, retry coordinator.RetryPolicy) func() {
	h.t.Helper()
	handler := func(ctx context.Context, claim coordinator.Claim, payload []byte) (string, error) {
		var body struct {
			RunID string `json:"run_id"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			return "", err
		}
		// Carry the claimed job id, so the recovery ladder's exact rung excludes this attempt's OWN
		// live lease (mirrors execute_run.go). Without it a worker attempt would see its own job as a
		// live sibling and wrongly stand down.
		desc := h.descriptor(body.RunID, claim.Fence)
		desc.JobID = claim.JobID
		if err := orch.ExecuteAttempt(ctx, desc); err != nil {
			return "", err
		}
		return "run:" + body.RunID + ":executed", nil
	}
	worker := coordinator.NewWorker(h.spine, coordinator.WorkerConfig{
		Owner: newID("e2e-worker"), Lease: 30 * time.Second, Heartbeat: 5 * time.Second, PollInterval: 25 * time.Millisecond,
		Retry: retry,
	}, handler)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = worker.Run(ctx) }()
	return cancel
}

// descriptor builds a single-attempt descriptor for a run. The subprocess dialer
// ignores ImageDigest; the frame bound feeds the stdout scanner.
func (h *harness) descriptor(runID string, fence int64) execution.AttemptDescriptor {
	return execution.AttemptDescriptor{
		RunID:       contracts.RunID(runID),
		AttemptID:   contracts.AttemptID(newID("att")),
		Fence:       uint64(fence),
		ImageDigest: "sha256:" + strings.Repeat("a", 64),
		Limits:      runner.Limits{WallTimeMS: 60000, MaxStdoutBytes: 1 << 20, MaxStderrBytes: 1 << 16, MaxFrameBytes: 1 << 20, MaxMemoryBytes: 1 << 28, MaxProcessCount: 64},
	}
}

// scriptedProvider is the deterministic fake model provider: the first call (no tool
// result in the conversation yet) asks for the add tool; the next call, seeing the
// tool result, returns the final output. calls counts adapter invocations so a test
// can prove a deduped frame never double-dispatches.
type scriptedProvider struct{ calls int32 }

func (p *scriptedProvider) Execute(_ context.Context, req modelbroker.Request, _ string, _ func(modelbroker.Delta)) (modelbroker.Result, error) {
	atomic.AddInt32(&p.calls, 1)
	sawTool := false
	for _, m := range req.Messages {
		if m.Role == "tool" {
			sawTool = true
		}
	}
	res := modelbroker.Result{
		ModelRequestID: req.ModelRequestID,
		Model:          "fake",
		Usage:          contracts.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8},
		Attempts:       1,
	}
	if sawTool {
		res.ProviderRequestID = "prov_final"
		res.Output = "12"
		res.FinishReason = "stop"
		return res, nil
	}
	res.ProviderRequestID = "prov_tool"
	res.ToolCalls = []modelbroker.ToolCall{{ID: "call_add", Name: "palai.conformance.math.add", Arguments: `{"a":7,"b":5}`}}
	res.FinishReason = "tool_calls"
	return res, nil
}

// subprocessDialer runs the reference engine as a bare subprocess over stdio. The
// optional hooks let a test intercept sends (commit-before-deliver) or re-deliver a
// received frame (duplicate dedup); zero hooks is the plain live channel.
type subprocessDialer struct {
	engineDir string
	onSend    func(contracts.EngineFrame)
	dupType   string
	mutateDup bool
}

func (d subprocessDialer) Dial(_ context.Context, attempt execution.AttemptDescriptor) (execution.EngineChannel, error) {
	// Plain exec.Command, not exec.CommandContext(dialCtx): the engine PROCESS must outlive
	// the orchestrator's 20s dial deadline. A mid-run steer that keeps the provider busy past
	// 20s would otherwise see the process killed at the dial ctx expiry ("signal: killed").
	// Teardown is explicit — Close kills the process — and Receive honors ctx so a canceled
	// worker still unwinds (see subprocessChannel.Receive).
	cmd := exec.Command("uv", "run", "--locked", "--project", d.engineDir, "python", "-m", "palai_engine")
	cmd.Env = []string{
		"PALAI_RUN_ID=" + string(attempt.RunID),
		"PALAI_ATTEMPT_ID=" + string(attempt.AttemptID),
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		// The reference engine is a src-layout, uv package=false project: pytest adds
		// src to the path, so a bare `-m palai_engine` needs it on PYTHONPATH.
		"PYTHONPATH=" + filepath.Join(d.engineDir, "src"),
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	ch := &subprocessChannel{cmd: cmd, stdin: stdin, stderr: &stderr, onSend: d.onSend, dupType: d.dupType, mutateDup: d.mutateDup}
	ch.scanner = bufio.NewScanner(stdout)
	ch.scanner.Buffer(make([]byte, 0, 64*1024), int(attempt.Limits.MaxFrameBytes))
	if err := ch.write(helloFrame(attempt)); err != nil {
		return nil, err
	}
	return ch, nil
}

type subprocessChannel struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	scanner   *bufio.Scanner
	stderr    *bytes.Buffer
	onSend    func(contracts.EngineFrame)
	dupType   string
	mutateDup bool
	duped     bool
	pending   *contracts.EngineFrame
}

func (c *subprocessChannel) Send(_ context.Context, frame contracts.EngineFrame) error {
	if c.onSend != nil {
		c.onSend(frame)
	}
	return c.write(frame)
}

func (c *subprocessChannel) write(frame contracts.EngineFrame) error {
	line, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if _, err := c.stdin.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write engine stdin: %w", err)
	}
	return nil
}

func (c *subprocessChannel) Receive(ctx context.Context) (contracts.EngineFrame, error) {
	if c.pending != nil {
		frame := *c.pending
		c.pending = nil
		if c.mutateDup {
			frame.Data = map[string]any{"tampered": true} // same id, different hash -> protocol violation
		}
		return frame, nil
	}
	// Honor ctx: a canceled worker (teardown) or a run whose engine outlives the dial
	// deadline must unwind here rather than block on stdout. The blocking scan runs in a
	// goroutine over a buffered channel, so a ctx-cancel return never leaks it — the parked
	// scan completes when Close kills the process and its stdout EOFs.
	type scanned struct {
		frame contracts.EngineFrame
		err   error
	}
	done := make(chan scanned, 1)
	go func() {
		frame, err := c.scan()
		done <- scanned{frame, err}
	}()
	select {
	case <-ctx.Done():
		return contracts.EngineFrame{}, ctx.Err()
	case r := <-done:
		return r.frame, r.err
	}
}

// scan reads one frame off the engine's stdout, applying the optional duplicate hook. It
// blocks until a line, EOF, or error; Receive wraps it in a ctx-aware select.
func (c *subprocessChannel) scan() (contracts.EngineFrame, error) {
	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return contracts.EngineFrame{}, err
		}
		// io.EOF stays the sentinel (the orchestrator detects a clean close by it); the
		// engine's redacted stderr rides along so an abnormal exit is not opaque.
		if c.stderr.Len() > 0 {
			return contracts.EngineFrame{}, fmt.Errorf("%w: engine stderr: %s", io.EOF, strings.TrimSpace(c.stderr.String()))
		}
		return contracts.EngineFrame{}, io.EOF
	}
	var frame contracts.EngineFrame
	if err := json.Unmarshal(c.scanner.Bytes(), &frame); err != nil {
		return contracts.EngineFrame{}, fmt.Errorf("decode engine frame: %w", err)
	}
	if !c.duped && c.dupType != "" && frame.Type == c.dupType {
		c.duped = true
		clone := frame
		c.pending = &clone
	}
	return frame, nil
}

func (c *subprocessChannel) Close() error {
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
	return nil
}

// scriptedChannel replays a fixed frame sequence and discards sends. It drives the
// orchestrator without a live engine, so a test can dispatch the same model_request_id
// twice (a real engine would reject a second model.result) to prove the cross-attempt,
// DB-side committed-result replay.
type scriptedChannel struct {
	frames []contracts.EngineFrame
	i      int
}

func (c *scriptedChannel) Send(context.Context, contracts.EngineFrame) error { return nil }
func (c *scriptedChannel) Close() error                                      { return nil }
func (c *scriptedChannel) Receive(context.Context) (contracts.EngineFrame, error) {
	if c.i >= len(c.frames) {
		return contracts.EngineFrame{}, io.EOF
	}
	f := c.frames[c.i]
	c.i++
	return f, nil
}

type scriptedDialer struct{ ch *scriptedChannel }

func (d scriptedDialer) Dial(context.Context, execution.AttemptDescriptor) (execution.EngineChannel, error) {
	return d.ch, nil
}

// scriptFrame builds a valid engine frame for the scripted channel. AttemptID is left
// empty so the orchestrator's identity check skips it; each frame gets a fresh id and an
// explicit sequence so a scripted stream can be contiguous (or deliberately gapped, to
// exercise the intake monotonicity gate).
func scriptFrame(typ, runID string, seq int, data map[string]any) contracts.EngineFrame {
	return contracts.EngineFrame{
		Protocol: "engine.v1",
		ID:       contracts.FrameID(newID("frm")),
		Type:     typ,
		Sequence: seq,
		Time:     time.Now().UTC().Format(time.RFC3339),
		RunID:    contracts.RunID(runID),
		Data:     data,
	}
}

func helloFrame(attempt execution.AttemptDescriptor) contracts.EngineFrame {
	return contracts.EngineFrame{
		Protocol:  "engine.v1",
		ID:        contracts.FrameID(newID("frm")),
		Type:      "supervisor.hello",
		Sequence:  1,
		Time:      time.Now().UTC().Format(time.RFC3339),
		RunID:     attempt.RunID,
		AttemptID: attempt.AttemptID,
		Data:      map[string]any{},
	}
}

// event is one journaled event, read straight from the durable log.
type event struct {
	seq int
	typ string
}

func (h *harness) events(sessionID string) []event {
	h.t.Helper()
	rows, err := h.spine.Pool().Query(context.Background(),
		`SELECT seq, type FROM events WHERE session_id=$1 AND organization_id=$2 AND project_id=$3 ORDER BY seq`,
		sessionID, h.tenant.Organization, h.tenant.Project)
	if err != nil {
		h.t.Fatalf("read events error = %v", err)
	}
	defer rows.Close()
	var out []event
	for rows.Next() {
		var e event
		if err := rows.Scan(&e.seq, &e.typ); err != nil {
			h.t.Fatalf("scan event error = %v", err)
		}
		out = append(out, e)
	}
	return out
}

// responseProjection is the terminal Response body finalize wrote.
type responseProjection struct {
	Output []map[string]any `json:"output"`
	Usage  contracts.Usage  `json:"usage"`
}

func (h *harness) response(responseID string) (state string, projection responseProjection) {
	h.t.Helper()
	var output []byte
	if err := h.spine.Pool().QueryRow(context.Background(),
		`SELECT state, output FROM responses WHERE id=$1 AND organization_id=$2 AND project_id=$3`,
		responseID, h.tenant.Organization, h.tenant.Project).Scan(&state, &output); err != nil {
		h.t.Fatalf("read response error = %v", err)
	}
	if len(output) > 0 {
		if err := json.Unmarshal(output, &projection); err != nil {
			h.t.Fatalf("decode projection %s error = %v", output, err)
		}
	}
	return state, projection
}

func (h *harness) count(query string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.spine.Pool().QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		h.t.Fatalf("count %q error = %v", query, err)
	}
	return n
}

// awaitResponseState polls the durable response state until it reaches want or the
// deadline elapses. It surfaces the engine stderr on timeout for diagnosis.
func (h *harness) awaitResponseState(responseID, want string, within time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(within)
	var last string
	for time.Now().Before(deadline) {
		if last, _ = h.response(responseID); last == want {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	h.t.Fatalf("response %s state = %q after %s, want %q", responseID, last, within, want)
}

// assertContiguous checks the events are exactly seq 1..len with no gaps.
func assertContiguous(t *testing.T, events []event) {
	t.Helper()
	for i, e := range events {
		if e.seq != i+1 {
			t.Fatalf("event %d sequence = %d, want %d (not contiguous): %+v", i, e.seq, i+1, events)
		}
	}
}
