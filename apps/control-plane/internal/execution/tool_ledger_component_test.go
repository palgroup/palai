//go:build component

package execution

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"

	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// The E10 T7 tool-ledger reconciliation proof (spec §26.6-26.7): drive dispatchTool against a REAL
// spine and assert the durable ledger — not just the in-memory broker — dedups a redelivered call across
// a simulated process kill (a FRESH broker per attempt), and that an irreversible kill-mid-execute
// enters `uncertain` and STOPS rather than re-firing the effect.

// openLedgerSpine opens a migrated spine + a seeded active run, returning the store and scope.
func openLedgerSpine(t *testing.T) (*coordinator.Store, coordinator.Tenant, string, string) {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
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
	tenant := coordinator.Tenant{Organization: redeliveryID("org"), Project: redeliveryID("prj")}
	sessionID, runID := redeliveryID("ses"), redeliveryID("run")
	pool := cs.Pool()
	execSQL(t, pool, `INSERT INTO organizations (id) VALUES ($1)`, tenant.Organization)
	execSQL(t, pool, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, tenant.Project, tenant.Organization)
	execSQL(t, pool, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`, sessionID, tenant.Organization, tenant.Project)
	execSQL(t, pool, `INSERT INTO runs (id, organization_id, project_id, session_id, state) VALUES ($1, $2, $3, $4, 'running')`, runID, tenant.Organization, tenant.Project, sessionID)
	return cs, tenant, sessionID, runID
}

// ledgerAttempt builds an orchestrator (with the given broker — a fresh one simulates a new process) and
// a fresh attemptState with a recording channel, at the given fence.
func ledgerAttempt(cs *coordinator.Store, broker *toolbroker.Broker, tenant coordinator.Tenant, sessionID, runID string, fence uint64) (*Orchestrator, *attemptState, *recordingChannel) {
	ch := &recordingChannel{}
	orch := &Orchestrator{spine: cs, tools: broker}
	st := &attemptState{
		attempt:   AttemptDescriptor{RunID: contracts.RunID(runID), AttemptID: contracts.AttemptID(redeliveryID("att")), Fence: fence},
		tenant:    tenant,
		sessionID: sessionID,
	}
	st.ch = ch
	return orch, st, ch
}

func toolRequestFrame(callID, name string, args map[string]any) contracts.EngineFrame {
	return contracts.EngineFrame{Type: "tool.request", Data: map[string]any{"tool_call_id": callID, "name": name, "arguments": args}}
}

// TestPureToolReplayLabeledNoDuplication proves the DURABLE cross-kill dedup (TOL-001, spec §26.7): a
// tool committed once is replayed from the ledger — LABELED replayed — by a FRESH-process attempt
// (a new broker whose in-memory cache is empty), so the external effect fires exactly once semantically.
func TestPureToolReplayLabeledNoDuplication(t *testing.T) {
	ctx := context.Background()
	cs, tenant, sessionID, runID := openLedgerSpine(t)
	var runs int32
	mkBroker := func() *toolbroker.Broker {
		return toolbroker.New(toolbroker.Tool{
			Name: "count.pure", InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
			Invoke: func(map[string]any) (map[string]any, error) { atomic.AddInt32(&runs, 1); return map[string]any{"ok": true}, nil },
		})
	}
	callID := redeliveryID("tc")

	// First attempt (process 1): executes + commits the durable row + delivers a non-replayed result.
	orch1, st1, ch1 := ledgerAttempt(cs, mkBroker(), tenant, sessionID, runID, 1)
	if err := orch1.dispatchTool(ctx, st1, toolRequestFrame(callID, "count.pure", map[string]any{})); err != nil {
		t.Fatalf("first dispatchTool error = %v", err)
	}
	if got := toolResults(ch1); len(got) != 1 || got[0].replayed {
		t.Fatalf("first dispatch results = %+v, want one non-replayed", got)
	}

	// Fresh process (a NEW broker — empty in-memory cache) reclaims and re-dispatches the same call_id.
	// Only the DURABLE ledger row can dedup it now; it replays LABELED without re-executing.
	orch2, st2, ch2 := ledgerAttempt(cs, mkBroker(), tenant, sessionID, runID, 2)
	if err := orch2.dispatchTool(ctx, st2, toolRequestFrame(callID, "count.pure", map[string]any{})); err != nil {
		t.Fatalf("reclaim dispatchTool error = %v", err)
	}
	got := toolResults(ch2)
	if len(got) != 1 || !got[0].replayed {
		t.Fatalf("reclaim dispatch results = %+v, want one LABELED replayed (durable ledger dedup)", got)
	}
	if n := atomic.LoadInt32(&runs); n != 1 {
		t.Fatalf("tool executed %d times across a process kill, want 1 (durable dedup, TOL-001)", n)
	}
}

// TestIrreversibleUncertainNeverAutoReplays proves TOL-003 (spec §26.7): an irreversible tool killed
// AFTER execute but BEFORE commit leaves an 'executing' row; a reclaiming attempt finds it, marks it
// `uncertain`, and STOPS — it does NOT re-fire the effect and sends NO tool.result (the run cannot
// continue on an uncertain result). The manual_resolution exit is reachable from uncertain.
func TestIrreversibleUncertainNeverAutoReplays(t *testing.T) {
	ctx := context.Background()
	cs, tenant, sessionID, runID := openLedgerSpine(t)
	var runs int32
	broker := toolbroker.New(toolbroker.Tool{
		Name: "effect.irr", InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		ReplayClass: toolbroker.ClassIrreversible,
		Invoke:      func(map[string]any) (map[string]any, error) { atomic.AddInt32(&runs, 1); return map[string]any{"charged": true}, nil },
	})
	callID := redeliveryID("tc")

	// Simulate the kill window: the effect ran and the durable 'executing' marker is written, but the
	// commit never landed (the crash). BeginToolCall is the pre-execute marker dispatchTool writes.
	if err := cs.BeginToolCall(ctx, tenant, sessionID, "", runID, 1, callID, "effect.irr", []byte(`{}`), "irreversible", "sha256:x", "", ""); err != nil {
		t.Fatalf("BeginToolCall error = %v", err)
	}
	atomic.AddInt32(&runs, 1) // the external effect fired before the crash

	// Reclaim: dispatchTool finds the 'executing' irreversible row → uncertain-STOP.
	orch, st, ch := ledgerAttempt(cs, broker, tenant, sessionID, runID, 2)
	err := orch.dispatchTool(ctx, st, toolRequestFrame(callID, "effect.irr", map[string]any{}))
	if !errors.Is(err, errToolUncertainWait) {
		t.Fatalf("reclaim dispatchTool error = %v, want errToolUncertainWait (uncertain-STOP)", err)
	}
	if got := toolResults(ch); len(got) != 0 {
		t.Fatalf("uncertain-STOP delivered %+v tool.results, want none (§26.7: blocks continuation)", got)
	}
	if n := atomic.LoadInt32(&runs); n != 1 {
		t.Fatalf("irreversible tool ran %d times, want 1 (never auto-replays, TOL-003)", n)
	}
	var state, recon string
	if err := cs.Pool().QueryRow(ctx, `SELECT state, reconciliation_state FROM tool_calls WHERE id=$1`, callID).Scan(&state, &recon); err != nil {
		t.Fatalf("read uncertain row error = %v", err)
	}
	if state != "uncertain" || recon != "reconciling" {
		t.Fatalf("row after STOP = {state:%q recon:%q}, want {uncertain reconciling}", state, recon)
	}
	// The manual_resolution exit is reachable (a human must resolve an irreversible uncertain effect).
	if err := cs.ReconcileToolCall(ctx, tenant, sessionID, "", runID, callID, "manual_resolution", nil); err != nil {
		t.Fatalf("escalate to manual_resolution error = %v", err)
	}
	if err := cs.Pool().QueryRow(ctx, `SELECT state FROM tool_calls WHERE id=$1`, callID).Scan(&state); err != nil {
		t.Fatalf("re-read row error = %v", err)
	}
	if state != "manual_resolution" {
		t.Fatalf("row after escalate = %q, want manual_resolution", state)
	}
}

// TestLateCallbackAfterFenceAdvanceDenied proves TOL-017's fence half (spec §26.7): a tool result
// committed under a fence the ledger has advanced past (a reclaiming attempt re-leased at a higher fence)
// is rejected as stale rather than overwriting the newer row — the CommitToolResult fence guard.
func TestLateCallbackAfterFenceAdvanceDenied(t *testing.T) {
	ctx := context.Background()
	cs, tenant, sessionID, runID := openLedgerSpine(t)
	callID := redeliveryID("tc")

	// A reclaiming attempt re-leased the call at fence 5 (the ledger's current fence).
	if err := cs.BeginToolCall(ctx, tenant, sessionID, "", runID, 5, callID, "effect.irr", []byte(`{}`), "irreversible", "sha256:x", "", ""); err != nil {
		t.Fatalf("BeginToolCall(fence 5) error = %v", err)
	}
	// A LATE callback from the superseded attempt (fence 2) tries to commit — the fence advanced past it.
	_, err := cs.CommitToolResult(ctx, tenant, sessionID, "", runID, 2, callID, "effect.irr",
		[]byte(`{}`), []byte(`{"charged":true}`), "irreversible", "sha256:x", "tool_call.completed.v1", []byte(`{}`))
	if !errors.Is(err, coordinator.ErrStaleToolCommit) {
		t.Fatalf("stale-fence commit error = %v, want ErrStaleToolCommit (TOL-017)", err)
	}
	// The row is untouched — still executing, not falsely completed by the stale writer.
	var state string
	if err := cs.Pool().QueryRow(ctx, `SELECT state FROM tool_calls WHERE id=$1`, callID).Scan(&state); err != nil {
		t.Fatalf("read row error = %v", err)
	}
	if state != "executing" {
		t.Fatalf("row after stale commit = %q, want executing (stale writer rejected)", state)
	}
	// The reclaiming attempt at the current fence commits fine.
	if _, err := cs.CommitToolResult(ctx, tenant, sessionID, "", runID, 5, callID, "effect.irr",
		[]byte(`{}`), []byte(`{"charged":true}`), "irreversible", "sha256:x", "tool_call.completed.v1", []byte(`{}`)); err != nil {
		t.Fatalf("current-fence commit error = %v, want success", err)
	}
}

// TestOutageCommandsDeliverCanonicalOrderAfterRecovery proves ENG-012's outage ordering (spec §26.9, E10
// T7 fork 2): queue + steer + interrupt messages accepted DURING an outage are delivered, on the
// recovering attempt, in CANONICAL order (creation / applied_sequence — interrupt accepted first, then
// steer, then queue), at the boundary pump — never spliced into a reconstructed step. The order is
// STABLE across a second reclaim (a flipped order would rebuild a different request for a committed
// step). The pre-first-step cancellation hook (a pending pause) is proven separately by the pause path;
// here the run continues and the three outage messages fold in canonical order.
func TestOutageCommandsDeliverCanonicalOrderAfterRecovery(t *testing.T) {
	ctx := context.Background()
	h := newRedeliveryHarness(t, "interrupt") // the interrupt-delivery message, accepted FIRST in the outage
	interruptID := h.commandID
	steerID := h.enqueue(t, "steer", "then steer Y")
	queueID := h.enqueue(t, "queue", "and queue Z")

	// The recovering attempt drains the outage messages at its first input boundary, in canonical order.
	st1, ch1 := h.attemptAt()
	if err := h.orch.pumpCommands(ctx, st1, "mr_step1"); err != nil {
		t.Fatalf("recovery pumpCommands() error = %v", err)
	}
	want := []string{interruptID, steerID, queueID}
	if got := ch1.deliverOrder(); !slicesEqual(got, want) {
		t.Fatalf("recovery delivery order = %v, want canonical [interrupt, steer, queue] %v", got, want)
	}

	// A SECOND reclaim redelivers them at the same boundary in the IDENTICAL order (stable across reclaims).
	st2, ch2 := h.attemptAt()
	if err := h.orch.pumpCommands(ctx, st2, "mr_step1"); err != nil {
		t.Fatalf("second reclaim pumpCommands() error = %v", err)
	}
	if got := ch2.deliverOrder(); !slicesEqual(got, want) {
		t.Fatalf("second reclaim order = %v, want the SAME canonical order %v (no divergence)", got, want)
	}

	// No injection into a reconstructed step: a DIFFERENT boundary delivers none of them.
	st3, ch3 := h.attemptAt()
	if err := h.orch.pumpCommands(ctx, st3, "mr_step9"); err != nil {
		t.Fatalf("wrong-boundary pumpCommands() error = %v", err)
	}
	if got := ch3.deliverOrder(); len(got) != 0 {
		t.Fatalf("wrong boundary delivered %v, want none (never spliced into another step)", got)
	}
}

// toolResult captures a delivered tool.result frame's content + replayed label.
type toolResult struct {
	content  string
	replayed bool
}

func toolResults(ch *recordingChannel) []toolResult {
	var out []toolResult
	for _, f := range ch.sent {
		if f.Type != "tool.result" {
			continue
		}
		r := toolResult{content: asString(f.Data["content"])}
		r.replayed, _ = f.Data["replayed"].(bool)
		out = append(out, r)
	}
	return out
}
