//go:build component

package execution

import (
	"context"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"

	"github.com/palgroup/palai/storage"
)

// denyingFirer is a fake HookFirer that DENIES at one point and passes every other through — enough to drive
// the before_model / before_repository_publish seams against a real spine without the full registry.
type denyingFirer struct {
	point  string
	hookID string
	reason string
	fired  int
}

func (f *denyingFirer) Fire(_ context.Context, ev extensions.HookEvent) (extensions.HookOutcome, error) {
	if ev.Point == f.point {
		f.fired++
		return extensions.HookOutcome{Denied: true, Reason: f.reason, HookID: f.hookID, Payload: ev.Payload}, nil
	}
	return extensions.HookOutcome{Payload: ev.Payload}, nil
}

// scriptedFirer is a fake HookFirer that transforms before_tool arguments and/or denies/redacts after_tool —
// deterministic control for the tool_dispatch replay/identity proofs.
type scriptedFirer struct {
	beforeToolArgs  map[string]any // before_tool: transform-patch arguments to this (nil = passthrough)
	afterToolDeny   string         // after_tool: deny with this reason (empty = no deny)
	afterToolResult map[string]any // after_tool: transform-patch result to this (nil = passthrough)
}

func (f *scriptedFirer) Fire(_ context.Context, ev extensions.HookEvent) (extensions.HookOutcome, error) {
	out := extensions.HookOutcome{Payload: ev.Payload}
	switch ev.Point {
	case extensions.HookPointBeforeTool:
		if f.beforeToolArgs != nil {
			p := clonePayloadMap(ev.Payload)
			p["arguments"] = f.beforeToolArgs
			out.Payload = p
		}
	case extensions.HookPointAfterTool:
		if f.afterToolDeny != "" {
			return extensions.HookOutcome{Denied: true, Reason: f.afterToolDeny, HookID: "hook_after", Payload: ev.Payload}, nil
		}
		if f.afterToolResult != nil {
			p := clonePayloadMap(ev.Payload)
			p["result"] = f.afterToolResult
			out.Payload = p
		}
	}
	return out, nil
}

func clonePayloadMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// TestBeforeToolTransformKeepsOriginalLedgerIdentity is the MUST-FIX 1 pin proof: a before_tool TRANSFORM
// patches the args a PURE tool runs with, but the committed ledger IDENTITY (request_hash) must stay the
// model's ORIGINAL — otherwise a redelivery with the original args false-diverges and bricks the run. It
// asserts the stored request_hash == hash(original) and that re-dispatching the SAME call with the ORIGINAL
// args replays cleanly (no "diverged content"). The `arguments` column stays PATCHED (honest audit).
func TestBeforeToolTransformKeepsOriginalLedgerIdentity(t *testing.T) {
	ctx := context.Background()
	cs, tenant, sessionID, runID := openLedgerSpine(t)
	pool := cs.Pool()

	broker := toolbroker.New(toolbroker.Tool{
		Name: "note.pure", InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"}, ReplayClass: toolbroker.ClassPure,
		Invoke: func(map[string]any) (map[string]any, error) { return map[string]any{"ok": true}, nil },
	})
	orch := &Orchestrator{spine: cs, tools: broker, hooks: &scriptedFirer{beforeToolArgs: map[string]any{"patched": true}}}

	originalArgs := map[string]any{"original": float64(1)}
	callID := redeliveryID("tc")
	st := &attemptState{attempt: AttemptDescriptor{RunID: contracts.RunID(runID), AttemptID: contracts.AttemptID(redeliveryID("att")), Fence: 1}, tenant: tenant, sessionID: sessionID, ch: &recordingChannel{}}
	if err := orch.dispatchTool(ctx, st, toolRequestFrame(callID, "note.pure", originalArgs)); err != nil {
		t.Fatalf("dispatchTool(transform) error = %v", err)
	}

	// The committed IDENTITY is the model's ORIGINAL, never the patched args.
	var storedHash string
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT request_hash FROM tool_calls WHERE id=$1`, callID).Scan(&storedHash); err != nil {
		t.Fatalf("read stored request_hash: %v", err)
	}
	wantHash := toolbroker.RequestHash("note.pure", originalArgs)
	if storedHash != wantHash {
		t.Fatalf("stored request_hash = %q, want hash(original) %q — a transform poisoned the ledger identity", storedHash, wantHash)
	}

	// A redelivery with the ORIGINAL args (the engine never saw the patch) replays cleanly — no false divergence.
	st2 := &attemptState{attempt: AttemptDescriptor{RunID: contracts.RunID(runID), AttemptID: contracts.AttemptID(redeliveryID("att")), Fence: 1}, tenant: tenant, sessionID: sessionID, ch: &recordingChannel{}}
	if err := orch.dispatchTool(ctx, st2, toolRequestFrame(callID, "note.pure", originalArgs)); err != nil {
		t.Fatalf("redelivery with original args = %v, want clean replay (a transform must not brick the run)", err)
	}
}

// TestAfterToolReDeliveryReappliesDeny is the MUST-FIX 2 proof: after_tool is DELIVERY-scoped, so a committed
// replay must RE-FIRE it — a crash between an after_tool deny and the engine consuming the result must NOT
// convert the deny into an allow. Attempt 1 denies (the model gets a denial); the committed-replay re-drive
// delivers the SAME denial, never the raw result.
func TestAfterToolReDeliveryReappliesDeny(t *testing.T) {
	ctx := context.Background()
	cs, tenant, sessionID, runID := openLedgerSpine(t)

	broker := toolbroker.New(toolbroker.Tool{
		Name: "read.pure", InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"}, ReplayClass: toolbroker.ClassPure,
		Invoke: func(map[string]any) (map[string]any, error) { return map[string]any{"secret": "raw-value"}, nil },
	})
	orch := &Orchestrator{spine: cs, tools: broker, hooks: &scriptedFirer{afterToolDeny: "result withheld by policy"}}

	callID := redeliveryID("tc")
	// Attempt 1: executes + commits, then after_tool denies → the model gets a denial.
	st := &attemptState{attempt: AttemptDescriptor{RunID: contracts.RunID(runID), AttemptID: contracts.AttemptID(redeliveryID("att")), Fence: 1}, tenant: tenant, sessionID: sessionID, ch: &recordingChannel{}}
	if err := orch.dispatchTool(ctx, st, toolRequestFrame(callID, "read.pure", map[string]any{})); err != nil {
		t.Fatalf("dispatchTool attempt 1 = %v", err)
	}
	if !deliveredDeny(st.ch.(*recordingChannel), callID) {
		t.Fatalf("attempt 1 did not deliver the after_tool deny: %+v", st.ch.(*recordingChannel).sent)
	}

	// Attempt 2 (committed replay): after_tool re-fires on the delivery path → the SAME denial, never the raw
	// result. Without the re-fire, the crash would have un-denied the delivery.
	st2 := &attemptState{attempt: AttemptDescriptor{RunID: contracts.RunID(runID), AttemptID: contracts.AttemptID(redeliveryID("att")), Fence: 1}, tenant: tenant, sessionID: sessionID, ch: &recordingChannel{}}
	if err := orch.dispatchTool(ctx, st2, toolRequestFrame(callID, "read.pure", map[string]any{})); err != nil {
		t.Fatalf("dispatchTool attempt 2 (replay) = %v", err)
	}
	ch2 := st2.ch.(*recordingChannel)
	if !deliveredDeny(ch2, callID) {
		t.Fatalf("committed replay delivered the RAW result, not the after_tool deny (a crash un-denied it): %+v", ch2.sent)
	}
	for _, f := range ch2.sent {
		if content, _ := f.Data["content"].(string); f.Type == "tool.result" && strings.Contains(content, "raw-value") {
			t.Fatalf("committed replay leaked the raw result past the after_tool deny: %q", content)
		}
	}
}

// assertEventJournaled asserts the session's journal carries exactly `want` events of the given type (the
// reused policy.denied.v1 kind for a hook deny).
func assertEventJournaled(t *testing.T, cs *coordinator.Store, sessionID, eventType string, want int) {
	t.Helper()
	var got int
	if err := cs.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM events WHERE session_id = $1 AND type = $2`, sessionID, eventType).Scan(&got); err != nil {
		t.Fatalf("count %s events: %v", eventType, err)
	}
	if got != want {
		t.Fatalf("journaled %d %s events, want %d", got, eventType, want)
	}
}

// TestBeforeModelPolicyDenyFailsStep proves a before_model POLICY deny fails the model step VISIBLY (spec
// §28.17): dispatchModel journals policy.denied.v1 and returns an error BEFORE the provider is ever routed —
// the model call never happens. It drives dispatchModel against a real spine with a nil model broker, so a
// reached Route would panic (proving the deny short-circuits before it).
func TestBeforeModelPolicyDenyFailsStep(t *testing.T) {
	ctx := context.Background()
	cs, tenant, sessionID, runID := openLedgerSpine(t)
	firer := &denyingFirer{point: extensions.HookPointBeforeModel, hookID: "hook_m", reason: "model calls are frozen for this project"}

	orch := &Orchestrator{spine: cs, hooks: firer, route: defaultModelRoute}
	st := &attemptState{
		attempt:   AttemptDescriptor{RunID: contracts.RunID(runID), AttemptID: contracts.AttemptID(redeliveryID("att")), Fence: 1},
		tenant:    tenant,
		sessionID: sessionID,
		ch:        &recordingChannel{},
	}
	frame := contracts.EngineFrame{Type: "model.request", Data: map[string]any{"model_request_id": redeliveryID("mreq"), "messages": []any{}}}

	continues, err := orch.dispatchModel(ctx, st, frame)
	if err == nil {
		t.Fatal("before_model deny did not fail the step")
	}
	if continues {
		t.Fatal("a denied model step reported it continues")
	}
	if firer.fired != 1 {
		t.Fatalf("before_model hook fired %d times, want 1", firer.fired)
	}
	assertEventJournaled(t, cs, sessionID, "policy.denied.v1", 1)
}

// TestBeforeRepositoryPublishDenyRejects proves a before_repository_publish POLICY deny REJECTS the
// publication (spec §28.17): RequestPublication journals policy.denied.v1 and returns a denied result the
// model sees — no pending approval is recorded. The hook sees the RESOLVED destination (operation/branch/
// remote), not just the tool name.
func TestBeforeRepositoryPublishDenyRejects(t *testing.T) {
	ctx := context.Background()
	cs, tenant, sessionID, runID := openLedgerSpine(t)
	pool := cs.Pool()

	// Seed a binding + preparation receipt so RunPublicationTarget resolves a destination for the run.
	bindingID := redeliveryID("repo")
	execSQL(t, pool, `INSERT INTO repository_bindings (id, organization_id, project_id, provider, repository_identity, clone_url, default_branch)
		VALUES ($1,$2,$3,'github','o/r','git@h:o/r.git','main')`, bindingID, tenant.Organization, tenant.Project)
	execSQL(t, pool, `INSERT INTO preparation_receipts (id, repository_binding_id, organization_id, project_id, base_commit, tree_hash, branch, run_id)
		VALUES ($1,$2,$3,$4,'basecommit','treehash','agent/work',$5)`,
		redeliveryID("prcpt"), bindingID, tenant.Organization, tenant.Project, runID)

	firer := &denyingFirer{point: extensions.HookPointBeforeRepositoryPublish, hookID: "hook_pub", reason: "publishing is disabled in this project"}
	reg := &publicationRegistry{store: cs, hooks: firer}
	scope := toolbroker.TaskScope{Org: tenant.Organization, Project: tenant.Project, SessionID: sessionID, RunID: runID}

	result, err := reg.RequestPublication(ctx, scope, map[string]any{"operation": "push_branch", "head_sha": "deadbeef"})
	if err != nil {
		t.Fatalf("RequestPublication() error = %v", err)
	}
	if result["status"] != "denied" || result["reason"] != firer.reason {
		t.Fatalf("publication not rejected by the hook: %+v", result)
	}
	if firer.fired != 1 {
		t.Fatalf("before_repository_publish hook fired %d times, want 1", firer.fired)
	}
	assertEventJournaled(t, cs, sessionID, "policy.denied.v1", 1)
}
