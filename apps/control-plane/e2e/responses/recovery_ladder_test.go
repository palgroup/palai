//go:build e2e

package responses

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator/recovery"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// memCheckpointStore is an in-memory CheckpointObjectStore (Put + Get) for the recovery-ladder e2e:
// it holds the opaque checkpoint bytes keyed by object key, counts Get calls so a test can prove the
// exact rung never reads a checkpoint, and can tamper a stored object to fail the §26.4 integrity
// condition. Its checksum matches artifacts.Store's "sha256:<hex>" so a clean restore verifies.
type memCheckpointStore struct {
	mu   sync.Mutex
	objs map[string][]byte
	gets int
}

func newMemCheckpointStore() *memCheckpointStore {
	return &memCheckpointStore{objs: map[string][]byte{}}
}

func (s *memCheckpointStore) Put(_ context.Context, key string, body []byte) (string, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objs[key] = append([]byte(nil), body...)
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), int64(len(body)), nil
}

func (s *memCheckpointStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	b, ok := s.objs[key]
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), b...), true, nil
}

func (s *memCheckpointStore) getCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets
}

// tamperAll corrupts every stored object's bytes so the restore's sha256 no longer matches the
// recorded checksum — the §26.4 integrity condition fails and the checkpoint is rejected (ENG-010).
func (s *memCheckpointStore) tamperAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, b := range s.objs {
		if len(b) > 0 {
			b[0] ^= 0xff
			s.objs[k] = b
		}
	}
}

func (s *memCheckpointStore) objectCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.objs)
}

// objects returns a copy of every stored checkpoint object's bytes, so a test can scan the raw
// checkpoint surface for a leaked credential (§26.2 / SAN-005 exclusion invariant, extended to checkpoints).
func (s *memCheckpointStore) objects() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]byte, 0, len(s.objs))
	for _, b := range s.objs {
		out = append(out, append([]byte(nil), b...))
	}
	return out
}

// countingDialer wraps a dialer and counts Dial calls, so the exact rung can be proven to stand down
// WITHOUT dialing a fresh engine (ENG-008).
type countingDialer struct {
	inner execution.EngineDialer
	mu    sync.Mutex
	count int
}

func (d *countingDialer) Dial(ctx context.Context, a execution.AttemptDescriptor) (execution.EngineChannel, error) {
	d.mu.Lock()
	d.count++
	d.mu.Unlock()
	return d.inner.Dial(ctx, a)
}

func (d *countingDialer) dials() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.count
}

// checkpointSink builds a CheckpointSink over a caller store + the real recovery persistence layer,
// so a recovery e2e persists checkpoints to a swappable in-memory object store.
func (h *harness) checkpointSink(store execution.CheckpointObjectStore) *execution.CheckpointSink {
	return execution.NewCheckpointSink(store, recovery.New(h.spine.Pool()))
}

// recoveryEventLevels reads the level field of every attempt.recovering.v1 journaled for a session.
func (h *harness) recoveryEventLevels(sessionID string) []string {
	h.t.Helper()
	return h.eventLevels(sessionID, "attempt.recovering.v1")
}

func (h *harness) eventLevels(sessionID, typ string) []string {
	h.t.Helper()
	rows, err := h.spine.Pool().Query(context.Background(),
		`SELECT payload FROM events WHERE session_id=$1 AND organization_id=$2 AND project_id=$3 AND type=$4 ORDER BY seq`,
		sessionID, h.tenant.Organization, h.tenant.Project, typ)
	if err != nil {
		h.t.Fatalf("read %s events error = %v", typ, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			h.t.Fatalf("scan %s payload error = %v", typ, err)
		}
		var body struct {
			Level string `json:"level"`
		}
		_ = json.Unmarshal(payload, &body)
		out = append(out, body.Level)
	}
	return out
}

// recoveryProof reads the single §26.12 RecoveryProof journaled for a session (recovery.proof.v1).
func (h *harness) recoveryProof(sessionID string) (recovery.RecoveryProof, bool) {
	h.t.Helper()
	var payload []byte
	err := h.spine.Pool().QueryRow(context.Background(),
		`SELECT payload FROM events WHERE session_id=$1 AND organization_id=$2 AND project_id=$3 AND type='recovery.proof.v1' ORDER BY seq DESC LIMIT 1`,
		sessionID, h.tenant.Organization, h.tenant.Project).Scan(&payload)
	if err != nil {
		return recovery.RecoveryProof{}, false
	}
	var proof recovery.RecoveryProof
	if err := json.Unmarshal(payload, &proof); err != nil {
		h.t.Fatalf("decode RecoveryProof error = %v", err)
	}
	return proof, true
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// countingTool is a side-effect-free tool whose Invoke increments a counter, so a test can prove a
// completed tool is NOT re-executed after a restore (the ENG-009 no-replay guarantee).
type countingTool struct {
	mu    sync.Mutex
	count int
}

func (c *countingTool) tool() toolbroker.Tool {
	return toolbroker.Tool{
		Name:         "recovery.count",
		InputSchema:  map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": true},
		OutputSchema: map[string]any{"type": "object", "properties": map[string]any{"ran": map[string]any{"type": "integer"}}, "required": []any{"ran"}, "additionalProperties": false},
		Invoke: func(map[string]any) (map[string]any, error) {
			c.mu.Lock()
			c.count++
			n := c.count
			c.mu.Unlock()
			return map[string]any{"ran": n}, nil
		},
	}
}

func (c *countingTool) runs() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

// crashThenFinishProvider drives one tool step (recovery.count) then a final answer, but the FIRST
// time it reaches the second (post-tool) step it returns an error — a mid-run crash AFTER the tool
// boundary's checkpoint was persisted. The next attempt, resuming past the tool, reaches the second
// step live and finishes. So attempt-1 crashes at step 2; attempt-2 completes it.
type crashThenFinishProvider struct {
	mu        sync.Mutex
	toolSteps int
}

func (p *crashThenFinishProvider) Execute(_ context.Context, req modelbroker.Request, _ string, _ func(modelbroker.Delta)) (modelbroker.Result, error) {
	sawTool := false
	for _, m := range req.Messages {
		if m.Role == "tool" {
			sawTool = true
		}
	}
	res := modelbroker.Result{ModelRequestID: req.ModelRequestID, Model: "fake", Usage: contracts.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8}, Attempts: 1}
	if !sawTool {
		res.ProviderRequestID = "prov_tool"
		res.ToolCalls = []modelbroker.ToolCall{{ID: "call_c", Name: "recovery.count", Arguments: "{}"}}
		res.FinishReason = "tool_calls"
		return res, nil
	}
	p.mu.Lock()
	p.toolSteps++
	n := p.toolSteps
	p.mu.Unlock()
	if n == 1 {
		return modelbroker.Result{}, errRecoveryCrash
	}
	res.ProviderRequestID = "prov_final"
	res.Output = "12"
	res.FinishReason = "stop"
	return res, nil
}

var errRecoveryCrash = errorString("injected crash after the tool boundary")

type errorString string

func (e errorString) Error() string { return string(e) }

// runToToolBoundaryCrash drives attempt-1 to the tool boundary (persisting a checkpoint) and then a
// crash at the next model step. It returns the orchestrator + counting tool + store so the caller
// drives attempt-2 through the ladder. The crash leaves a durable checkpoint at the tool boundary.
func (h *harness) runToToolBoundaryCrash(store *memCheckpointStore, forbidReconstruction bool) (orch *execution.Orchestrator, tool *countingTool, respID, sessionID, runID string) {
	h.t.Helper()
	tool = &countingTool{}
	provider := &crashThenFinishProvider{}
	orch = h.newOrchestratorWithTools(subprocessDialer{engineDir: h.engineDir}, provider, tool.tool())
	orch.SetCheckpointSink(h.checkpointSink(store))
	if forbidReconstruction {
		orch.SetReconstructionForbidden(true)
	}
	respID, sessionID, runID = h.admit()
	if err := orch.ExecuteAttempt(context.Background(), h.descriptor(runID, 1)); err == nil {
		h.t.Fatal("attempt-1 should crash at the second model step")
	}
	if store.objectCount() == 0 {
		h.t.Fatal("attempt-1 persisted no checkpoint at the tool boundary")
	}
	if tool.runs() != 1 {
		h.t.Fatalf("tool ran %d times on attempt-1, want 1", tool.runs())
	}
	return orch, tool, respID, sessionID, runID
}

// TestCompatibleCheckpointRestoresBoundaryNoToolReplay proves the compatible rung (ENG-009, spec
// §26.3 rung 2): a fresh attempt RESTORES the tool boundary from the checkpoint and resumes PAST the
// completed tool — the tool is not re-executed — and the run finishes, with a complete §26.12 proof.
func TestCompatibleCheckpointRestoresBoundaryNoToolReplay(t *testing.T) {
	h := newHarness(t)
	store := newMemCheckpointStore()
	orch, tool, respID, sessionID, runID := h.runToToolBoundaryCrash(store, false)

	if err := orch.ExecuteAttempt(context.Background(), h.descriptor(runID, 2)); err != nil {
		t.Fatalf("attempt-2 (compatible restore) error = %v", err)
	}
	if tool.runs() != 1 {
		t.Fatalf("tool ran %d times total, want 1 (the completed tool must NOT replay after restore)", tool.runs())
	}
	if !contains(h.recoveryEventLevels(sessionID), string(recovery.LevelCompatibleCheckpoint)) {
		t.Fatalf("no compatible_checkpoint rung; levels = %v", h.recoveryEventLevels(sessionID))
	}
	proof, ok := h.recoveryProof(sessionID)
	if !ok || !proof.Complete() || proof.Level != recovery.LevelCompatibleCheckpoint {
		t.Fatalf("recovery proof missing/incomplete: %+v (ok=%v)", proof, ok)
	}
	if got, _ := h.response(respID); got != "completed" {
		t.Fatalf("run state after restore = %q, want completed", got)
	}
}

// TestIncompatibleCheckpointFallsToTranscriptWithRejectedEvent proves the transcript rung (ENG-010,
// spec §26.3 rung 3): a checkpoint whose bytes are corrupt (sha256 != recorded) is REJECTED with a
// reason, and the run reconstructs from the transcript instead — never labelled an exact/compatible
// resume. The completed tool still does not double-execute (the same-broker ledger caches it).
func TestIncompatibleCheckpointFallsToTranscriptWithRejectedEvent(t *testing.T) {
	h := newHarness(t)
	store := newMemCheckpointStore()
	orch, tool, respID, sessionID, runID := h.runToToolBoundaryCrash(store, false)

	store.tamperAll() // corrupt the stored bytes: the §26.4 checksum condition now fails

	if err := orch.ExecuteAttempt(context.Background(), h.descriptor(runID, 2)); err != nil {
		t.Fatalf("attempt-2 (transcript reconstruction) error = %v", err)
	}
	if n := len(h.eventLevels(sessionID, "checkpoint.rejected.v1")); n == 0 {
		t.Fatal("no checkpoint.rejected.v1 for the corrupt checkpoint")
	}
	if !contains(h.recoveryEventLevels(sessionID), string(recovery.LevelTranscriptReconstruction)) {
		t.Fatalf("no transcript_reconstruction rung; levels = %v", h.recoveryEventLevels(sessionID))
	}
	if contains(h.recoveryEventLevels(sessionID), string(recovery.LevelCompatibleCheckpoint)) {
		t.Fatal("a rejected checkpoint must NOT be labelled a compatible resume")
	}
	if tool.runs() != 1 {
		t.Fatalf("tool ran %d times total, want 1 (the cached tool result is reused, not re-run)", tool.runs())
	}
	proof, ok := h.recoveryProof(sessionID)
	if !ok || !proof.Complete() || proof.Level != recovery.LevelTranscriptReconstruction {
		t.Fatalf("recovery proof missing/incomplete: %+v (ok=%v)", proof, ok)
	}
	if got, _ := h.response(respID); got != "completed" {
		t.Fatalf("run state after transcript reconstruction = %q, want completed", got)
	}
}

// TestPolicyForbidsReconstructionExplicitFailure proves the explicit-failure rung (spec §26.3 rung
// 4): with reconstruction forbidden by policy, an incompatible checkpoint fails the run with a typed
// reason rather than silently reconstructing or looping.
func TestPolicyForbidsReconstructionExplicitFailure(t *testing.T) {
	h := newHarness(t)
	store := newMemCheckpointStore()
	orch, _, respID, sessionID, runID := h.runToToolBoundaryCrash(store, true)

	store.tamperAll() // incompatible checkpoint + reconstruction forbidden -> explicit failure

	if err := orch.ExecuteAttempt(context.Background(), h.descriptor(runID, 2)); err != nil {
		t.Fatalf("attempt-2 (explicit failure) error = %v", err)
	}
	if !contains(h.recoveryEventLevels(sessionID), string(recovery.LevelExplicitFailure)) {
		t.Fatalf("no explicit_failure rung; levels = %v", h.recoveryEventLevels(sessionID))
	}
	if got, _ := h.response(respID); got != "failed" {
		t.Fatalf("run state = %q, want failed (explicit recovery failure, not a silent drop or retry)", got)
	}
}

// engineModelRequestID replicates the reference engine's deterministic model_request_id (protocol.py
// _stable_id("mreq","model",run,step)), so a test can name the exact input boundary a message folded
// at and prove the drain gate chose the LAST replayed boundary, not an earlier one.
func engineModelRequestID(runID string, step int) string {
	sum := sha256.Sum256([]byte("model\x1f" + runID + "\x1f" + strconv.Itoa(step)))
	return "mreq_" + hex.EncodeToString(sum[:])[:24]
}

func (h *harness) deliveredBoundary(runID, commandID string) string {
	h.t.Helper()
	var b string
	if err := h.spine.Pool().QueryRow(context.Background(),
		`SELECT boundary_request_id FROM delivered_messages WHERE run_id=$1 AND command_id=$2 AND organization_id=$3 AND project_id=$4`,
		runID, commandID, h.tenant.Organization, h.tenant.Project).Scan(&b); err != nil {
		h.t.Fatalf("read delivered boundary for %s: %v", commandID, err)
	}
	return b
}

// threeStepThenCrashProvider drives two tool steps then a final answer, crashing the FIRST time it
// reaches the final (third) step — so attempt-1 commits steps 1 and 2 then dies, and attempt-2
// reconstructs by replaying steps 1 and 2 before a live step 3. It records whether the live step saw
// the fresh message, which proves the message folded at the first live boundary, not a replayed one.
type threeStepThenCrashProvider struct {
	mu            sync.Mutex
	finalCalls    int
	finalSawFresh bool
}

func (p *threeStepThenCrashProvider) Execute(_ context.Context, req modelbroker.Request, _ string, _ func(modelbroker.Delta)) (modelbroker.Result, error) {
	toolResults := 0
	for _, m := range req.Messages {
		if m.Role == "tool" {
			toolResults++
		}
	}
	res := modelbroker.Result{ModelRequestID: req.ModelRequestID, Model: "fake", Usage: contracts.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8}, Attempts: 1}
	switch {
	case toolResults == 0:
		res.ProviderRequestID = "prov_tool1"
		res.ToolCalls = []modelbroker.ToolCall{{ID: "c1", Name: "recovery.count", Arguments: "{}"}}
		res.FinishReason = "tool_calls"
		return res, nil
	case toolResults == 1:
		res.ProviderRequestID = "prov_tool2"
		res.ToolCalls = []modelbroker.ToolCall{{ID: "c2", Name: "recovery.count", Arguments: "{}"}}
		res.FinishReason = "tool_calls"
		return res, nil
	default:
		p.mu.Lock()
		p.finalCalls++
		n := p.finalCalls
		if n >= 2 {
			for _, m := range req.Messages {
				if strings.Contains(m.Content, "LIVE-FOLD") {
					p.finalSawFresh = true
				}
			}
		}
		p.mu.Unlock()
		if n == 1 {
			return modelbroker.Result{}, errRecoveryCrash // attempt-1 crashes at the live step
		}
		res.ProviderRequestID = "prov_final"
		res.Output = "done"
		res.FinishReason = "stop"
		return res, nil
	}
}

// TestFreshCommandsNotDrainedAtReplayedBoundary proves the replayed-boundary drain gate (spec §26.9,
// E10 T4 fork 4): a message queued during an outage folds at the FIRST LIVE boundary (before the
// first live step), NEVER at a replayed boundary — where it would rewrite a step LookupModelResult
// replays by id (silent divergence). No checkpoint sink here, so recovery is transcript
// reconstruction (run.start replay), which is the only path with replayed boundaries.
func TestFreshCommandsNotDrainedAtReplayedBoundary(t *testing.T) {
	h := newHarness(t)
	tool := &countingTool{}
	provider := &threeStepThenCrashProvider{}
	rec := &deliverRecorder{}
	dialer := subprocessDialer{engineDir: h.engineDir, onSend: rec.onSend}
	orch := h.newOrchestratorWithTools(dialer, provider, tool.tool())

	respID, sessionID, runID := h.admit()

	// attempt-1: commits steps 1 and 2 (two model steps + two tool boundaries), crashes at step 3.
	if err := orch.ExecuteAttempt(context.Background(), h.descriptor(runID, 1)); err == nil {
		t.Fatal("attempt-1 should crash at the third (live) step")
	}
	if got := h.committedModelSteps(runID); got != 2 {
		t.Fatalf("committed model steps after attempt-1 = %d, want 2", got)
	}

	// A FRESH message queued during the outage.
	msgID := newID("cmd")
	if cmd := h.submitCommand(sessionID, `{"command_id":"`+msgID+`","kind":"send_message","delivery":"queue","message":"LIVE-FOLD"}`); cmd.Status != "queued" {
		t.Fatalf("send_message status = %q, want queued", cmd.Status)
	}
	_ = sessionID

	// attempt-2: reconstruct (replay steps 1+2, live step 3). The fresh message folds ONCE, at the
	// last replayed boundary (step 2's), into the live step 3.
	if err := orch.ExecuteAttempt(context.Background(), h.descriptor(runID, 2)); err != nil {
		t.Fatalf("attempt-2 (reconstruction) error = %v", err)
	}
	if st, _ := h.response(respID); st != "completed" {
		t.Fatalf("run state = %q, want completed", st)
	}
	if !provider.finalSawFresh {
		t.Fatal("the fresh message did not fold into the first LIVE step (it was lost at a replayed boundary)")
	}
	if boundary, want := h.deliveredBoundary(runID, msgID), engineModelRequestID(runID, 2); boundary != want {
		t.Fatalf("message delivered at boundary %q, want the last-replayed (step 2) boundary %q", boundary, want)
	}
	if count, last := rec.snapshot(); count != 1 || last != "LIVE-FOLD" {
		t.Fatalf("message.deliver frames = %d (last %q), want exactly 1 of LIVE-FOLD", count, last)
	}
}

func (h *harness) committedModelSteps(runID string) int {
	return h.count(`SELECT count(*) FROM model_requests WHERE run_id=$1 AND organization_id=$2 AND project_id=$3 AND state='completed'`,
		runID, h.tenant.Organization, h.tenant.Project)
}

// TestLadderPrefersExactWhenLeaseAlive proves the exact rung (ENG-008, spec §26.3 rung 1): while the
// ORIGINAL attempt still holds a live response.run lease, a second attempt on the same run stands
// down WITHOUT dialing a fresh engine or reading the checkpoint — the original run finishes
// untouched, and the exact rung is journaled and visible.
func TestLadderPrefersExactWhenLeaseAlive(t *testing.T) {
	h := newHarness(t)
	gp := newGatedProvider()
	store := newMemCheckpointStore()
	dialer := &countingDialer{inner: subprocessDialer{engineDir: h.engineDir}}
	orch := h.newOrchestratorWithAdapter(dialer, gp)
	orch.SetCheckpointSink(h.checkpointSink(store))
	stop := h.runWorker(orch)
	defer stop()

	respID, sessionID, runID := h.admit()

	// attempt-1 parks mid-run: its response.run job is claimed with a live lease.
	select {
	case <-gp.started:
	case <-time.After(30 * time.Second):
		t.Fatal("first model step never started")
	}
	if d := dialer.dials(); d != 1 {
		t.Fatalf("dials after attempt-1 start = %d, want 1", d)
	}

	// A SECOND attempt on the SAME run, direct-driven with no claimed job of its own: the original
	// lease is alive, so it takes the exact rung and stands down. No dial, no checkpoint read.
	if err := orch.ExecuteAttempt(context.Background(), h.descriptor(runID, 2)); !errors.Is(err, execution.ErrExactStandDown) {
		t.Fatalf("second attempt should stand down retryably (ErrExactStandDown), got %v", err)
	}
	if d := dialer.dials(); d != 1 {
		t.Fatalf("dials after exact stand-down = %d, want 1 (the exact rung must NOT dial)", d)
	}
	if g := store.getCount(); g != 0 {
		t.Fatalf("checkpoint Get calls during exact stand-down = %d, want 0 (the checkpoint is untouched)", g)
	}
	if !contains(h.recoveryEventLevels(sessionID), string(recovery.LevelExact)) {
		t.Fatalf("no attempt.recovering.v1 at level=exact; levels = %v", h.recoveryEventLevels(sessionID))
	}

	// Release attempt-1: the original run finishes untouched.
	close(gp.release)
	h.awaitResponseState(respID, "completed", 60*time.Second)
}

// TestMutualExactStandDownRequeuesInsteadOfHanging proves MUST-FIX #2 (spec §26.3 rung 1): two
// concurrent response.run jobs for ONE run each see the OTHER's live lease, so each takes the exact
// rung. If stand-down COMPLETED the job, both would complete and the run would never be driven — a
// permanent hang no reconciler recovers. Instead stand-down returns a RETRYABLE error so the worker
// requeues (with jitter), and a retry drives the run once the tie breaks or finds it terminal.
func TestMutualExactStandDownRequeuesInsteadOfHanging(t *testing.T) {
	h := newHarness(t)
	dialer := &countingDialer{inner: subprocessDialer{engineDir: h.engineDir}}
	orch := h.newOrchestrator(dialer)
	_, _, runID := h.admit()

	// Two response.run jobs for the SAME run, both claimed with LIVE leases (running, lease in the
	// future) — the mutual-standoff window (a paused attempt's job reclaimable while resume minted a
	// second job).
	job1, job2 := newID("job"), newID("job")
	for _, j := range []string{job1, job2} {
		if _, err := h.spine.Pool().Exec(context.Background(),
			`INSERT INTO durable_jobs (id, organization_id, project_id, kind, status, lease_owner, lease_expires_at, fence, attempt_count, payload)
			 VALUES ($1, $2, $3, 'response.run', 'running', 'owner', clock_timestamp() + interval '1 minute', 1, 1, $4)`,
			j, h.tenant.Organization, h.tenant.Project, []byte(`{"run_id":"`+runID+`"}`)); err != nil {
			t.Fatalf("seed live job %s: %v", j, err)
		}
	}

	d1 := h.descriptor(runID, 1)
	d1.JobID = job1
	d2 := h.descriptor(runID, 2)
	d2.JobID = job2
	err1 := orch.ExecuteAttempt(context.Background(), d1)
	err2 := orch.ExecuteAttempt(context.Background(), d2)

	if !errors.Is(err1, execution.ErrExactStandDown) || !errors.Is(err2, execution.ErrExactStandDown) {
		t.Fatalf("mutual exact stand-down must return a retryable error (not complete the job), got %v / %v", err1, err2)
	}
	if d := dialer.dials(); d != 0 {
		t.Fatalf("exact stand-downs must not dial, got %d", d)
	}
	if st, _ := h.response(runID2resp(t, h, runID)); st == "completed" || st == "failed" {
		t.Fatalf("run must stay non-terminal + recoverable, got %q", st)
	}
}

func runID2resp(t *testing.T, h *harness, runID string) string {
	t.Helper()
	var respID string
	if err := h.spine.Pool().QueryRow(context.Background(),
		`SELECT response_id FROM runs WHERE id=$1 AND organization_id=$2 AND project_id=$3`,
		runID, h.tenant.Organization, h.tenant.Project).Scan(&respID); err != nil {
		t.Fatalf("resolve response for run: %v", err)
	}
	return respID
}
