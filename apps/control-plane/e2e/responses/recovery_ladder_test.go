//go:build e2e

package responses

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	if err := orch.ExecuteAttempt(context.Background(), h.descriptor(runID, 2)); err != nil {
		t.Fatalf("second attempt (exact stand-down) error = %v", err)
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
