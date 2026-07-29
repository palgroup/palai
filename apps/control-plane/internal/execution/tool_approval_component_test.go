//go:build component

package execution

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"

	"github.com/palgroup/palai/storage"
)

// The E23 T1 approval gate, against a REAL spine. Four claims, and the tree's own measurements are why
// each one is written as a test rather than trusted as a design:
//
//   1. A gated tool NEVER reaches Execute without a human. Measured on a counter; the answer is zero.
//   2. The run PARKS rather than answering. Today a run whose model called a publication tool keeps
//      going and reaches `completed`, so the click that arrives afterwards hits guardRunActive and the
//      human sees a 503 (E22's TestPublicationFromSlackCeiling... recorded exactly that). This is not a
//      new behaviour breaking a green one — it is a shipped bug being caught.
//   3. Approving runs the call ONCE and wakes the parked run.
//   4. An argument set that changed after the approval does not run: the hash no longer matches, so
//      there is no approval, so Execute is not called.
//
// Plus the half of expiry that did not exist: an unanswered question must not park a run forever.

// approvalHarness is a real spine + a seeded running run + a counting fake executor. The counter is the
// whole measurement: "did anything run" is not a matter of interpretation.
type approvalHarness struct {
	spine     *coordinator.Store
	tenant    coordinator.Tenant
	sessionID string
	runID     string
	callID    string
	executed  *int32
	broker    func() *toolbroker.Broker
}

func newApprovalHarness(t *testing.T, tool toolbroker.Tool) *approvalHarness {
	t.Helper()
	cs, tenant, sessionID, runID := openLedgerSpine(t)
	var executed int32
	tool.InputSchema = map[string]any{"type": "object"}
	tool.OutputSchema = map[string]any{"type": "object"}
	tool.Exec = func(context.Context, toolbroker.ExecEnv, map[string]any) (map[string]any, error) {
		atomic.AddInt32(&executed, 1)
		return map[string]any{"ok": true}, nil
	}
	h := &approvalHarness{
		spine: cs, tenant: tenant, sessionID: sessionID, runID: runID,
		callID: redeliveryID("tc"), executed: &executed,
	}
	h.broker = func() *toolbroker.Broker { return toolbroker.New(tool) }
	return h
}

// dispatch drives ONE attempt at the given fence — a fresh broker each time, because a fresh attempt is a
// fresh process and only the DURABLE ledger may carry state between them.
func (h *approvalHarness) dispatch(t *testing.T, fence uint64, args map[string]any) (*recordingChannel, error) {
	t.Helper()
	orch, st, ch := ledgerAttempt(h.spine, h.broker(), h.tenant, h.sessionID, h.runID, fence)
	return ch, orch.dispatchTool(context.Background(), st, toolRequestFrame(h.callID, "jira.transitionIssue", args))
}

func (h *approvalHarness) runState(t *testing.T) string {
	t.Helper()
	var state string
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM runs WHERE id = $1`, h.runID).Scan(&state); err != nil {
		t.Fatalf("read run state: %v", err)
	}
	return state
}

func (h *approvalHarness) callState(t *testing.T) string {
	t.Helper()
	var state string
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM tool_calls WHERE id = $1`, h.callID).Scan(&state); err != nil {
		t.Fatalf("read tool_call state: %v", err)
	}
	return state
}

func (h *approvalHarness) ran() int32 { return atomic.LoadInt32(h.executed) }

// requestHash is the one-shot binding the button carries — computed the way the dispatcher computes it,
// from the model's original arguments.
func requestHashFor(args map[string]any) string {
	return toolbroker.RequestHash("jira.transitionIssue", args)
}

var gatedTool = toolbroker.Tool{
	Name:             "jira.transitionIssue",
	ReplayClass:      toolbroker.ClassIrreversible,
	RequiresApproval: true,
	ApprovalLabel:    "the shared Jira service account may move tickets",
}

// TestToolApprovalGateNeverReachesExecuteWithoutAHumanDecision is RED #1 and the defining test of this
// epic. A tool declared approval_required is dispatched exactly as any other; the counter on its executor
// must read ZERO, because nobody has decided anything.
func TestToolApprovalGateNeverReachesExecuteWithoutAHumanDecision(t *testing.T) {
	h := newApprovalHarness(t, gatedTool)
	args := map[string]any{"issue": "PAL-42", "status": "Done"}

	ch, err := h.dispatch(t, 1, args)
	if !errors.Is(err, errRunAwaitingApproval) {
		t.Fatalf("dispatchTool error = %v, want errRunAwaitingApproval", err)
	}
	if n := h.ran(); n != 0 {
		t.Fatalf("the gated tool executed %d time(s) with NO human decision, want 0", n)
	}
	// And no result was delivered: the model was handed nothing to continue on. That is the difference
	// between this gate and the publication tool's pending_approval result, which the model reads as an
	// answer and keeps going from.
	if got := toolResults(ch); len(got) != 0 {
		t.Fatalf("the engine was sent %d tool.result frame(s) for an undecided call: %+v", len(got), got)
	}
	// The ledger carries the parked call, at the state §26.7 declared for it and nothing ever used.
	if got := h.callState(t); got != "approval_pending" {
		t.Fatalf("tool_call state = %q, want approval_pending", got)
	}
	// The binding is the request hash: the button authorizes THESE BYTES.
	var storedHash string
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT a.request_hash FROM approvals a WHERE a.tool_call_id = $1`, h.callID).Scan(&storedHash); err != nil {
		t.Fatalf("read the approval binding: %v", err)
	}
	if want := requestHashFor(args); storedHash != want {
		t.Fatalf("the approval is bound to %q, want the call's own request hash %q", storedHash, want)
	}
	// And it carries a deadline. 000013 forward-declared expires_at in 2023 and nothing has ever written
	// a value into it; a gate with no deadline is a run that waits forever.
	var expiresAt *time.Time
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT a.expires_at FROM approvals a WHERE a.tool_call_id = $1`, h.callID).Scan(&expiresAt); err != nil {
		t.Fatalf("read the approval deadline: %v", err)
	}
	if expiresAt == nil {
		t.Fatal("the approval has no expires_at: nothing would ever release a run parked on it")
	}
}

// TestToolApprovalParksTheRunRatherThanAnsweringWithoutADecision is RED #2. The run must be WAITING, not
// running-toward-a-terminal — because a run that answers while a human is still looking at the button is
// a run whose approval can no longer be applied (guardRunActive), which is the failure E22 measured.
func TestToolApprovalParksTheRunRatherThanAnsweringWithoutADecision(t *testing.T) {
	h := newApprovalHarness(t, gatedTool)
	if _, err := h.dispatch(t, 1, map[string]any{"issue": "PAL-42"}); !errors.Is(err, errRunAwaitingApproval) {
		t.Fatalf("dispatchTool error = %v, want errRunAwaitingApproval", err)
	}
	if got := h.runState(t); got != string(statemachines.RunWaiting) {
		t.Fatalf("run state = %q while a human owes an answer, want waiting", got)
	}
	// A parked run is still ACTIVE, so a decision can be applied to it — the exact property the publication
	// path lost by letting the run finish first.
	if _, _, err := h.spine.ToolApprovalForCall(context.Background(), h.tenant, h.callID); err != nil {
		t.Fatalf("the parked call is not readable for a decision: %v", err)
	}
	applied, err := h.spine.DecideToolApproval(context.Background(), h.tenant, coordinator.ToolApprovalDecision{
		ToolCallID: h.callID, RequestHash: requestHashFor(map[string]any{"issue": "PAL-42"}),
		DecidedBy: "slack:T1:Uapprover", Approve: true,
	})
	if err != nil || !applied {
		t.Fatalf("a decision on a PARKED run was refused (applied=%v, err=%v) — this is the 503 E22 measured", applied, err)
	}
}

// TestToolApprovalApprovedCallRunsExactlyOnceAndWakesTheRun: the approve wakes the parked run, the fresh
// attempt replays to the SAME tool.request, and the ledger consult now finds `ready`. The tool runs ONCE.
func TestToolApprovalApprovedCallRunsExactlyOnceAndWakesTheRun(t *testing.T) {
	ctx := context.Background()
	h := newApprovalHarness(t, gatedTool)
	args := map[string]any{"issue": "PAL-42", "status": "Done"}
	if _, err := h.dispatch(t, 1, args); !errors.Is(err, errRunAwaitingApproval) {
		t.Fatalf("first dispatch error = %v, want the park", err)
	}

	applied, err := h.spine.DecideToolApproval(ctx, h.tenant, coordinator.ToolApprovalDecision{
		ToolCallID: h.callID, RequestHash: requestHashFor(args), DecidedBy: "slack:T1:Uapprover", Approve: true,
	})
	if err != nil || !applied {
		t.Fatalf("DecideToolApproval(approve) = %v, %v; want applied", applied, err)
	}
	// THE WAKE: waiting -> running, and a response.run job enqueued in the same transaction, so a worker
	// opens a fresh attempt on this run.
	if got := h.runState(t); got != "running" {
		t.Fatalf("run state after the approve = %q, want running (the wake did not fire)", got)
	}
	var jobs int
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(ctx),
		`SELECT count(*) FROM durable_jobs WHERE kind='response.run' AND payload->>'run_id' = $1`, h.runID).Scan(&jobs); err != nil {
		t.Fatalf("count wake jobs: %v", err)
	}
	if jobs != 1 {
		t.Fatalf("the wake enqueued %d response.run job(s), want exactly 1", jobs)
	}

	// The fresh attempt: a NEW broker (a new process), the same call id, the same arguments.
	ch, err := h.dispatch(t, 2, args)
	if err != nil {
		t.Fatalf("post-approval dispatch error = %v", err)
	}
	if n := h.ran(); n != 1 {
		t.Fatalf("the approved tool ran %d time(s), want exactly 1", n)
	}
	if got := toolResults(ch); len(got) != 1 || got[0].replayed {
		t.Fatalf("post-approval results = %+v, want one fresh result", got)
	}
	if got := h.callState(t); got != "completed" {
		t.Fatalf("tool_call state after execution = %q, want completed", got)
	}

	// And a THIRD attempt replays the committed result without re-firing — the ordinary ledger guarantee
	// survives the detour through a human.
	if _, err := h.dispatch(t, 3, args); err != nil {
		t.Fatalf("replay dispatch error = %v", err)
	}
	if n := h.ran(); n != 1 {
		t.Fatalf("the tool ran %d time(s) across a replay, want still 1", n)
	}
}

// TestToolApprovalDoesNotRunWhenArgumentsChangedAfterTheApproval is RED #4. The approval is bound to a
// hash of (name, arguments). Different arguments are a different call, so the approval that exists
// authorizes something else — and Execute is not reached.
func TestToolApprovalDoesNotRunWhenArgumentsChangedAfterTheApproval(t *testing.T) {
	ctx := context.Background()
	h := newApprovalHarness(t, gatedTool)
	approved := map[string]any{"issue": "PAL-42", "status": "Done"}
	if _, err := h.dispatch(t, 1, approved); !errors.Is(err, errRunAwaitingApproval) {
		t.Fatalf("first dispatch error = %v, want the park", err)
	}
	if applied, err := h.spine.DecideToolApproval(ctx, h.tenant, coordinator.ToolApprovalDecision{
		ToolCallID: h.callID, RequestHash: requestHashFor(approved), DecidedBy: "slack:T1:Uapprover", Approve: true,
	}); err != nil || !applied {
		t.Fatalf("DecideToolApproval(approve) = %v, %v", applied, err)
	}

	// The same call id comes back carrying DIFFERENT arguments — a moved head, a re-planned step, or a
	// deliberate swap. Whatever produced it, the human did not look at these bytes.
	_, err := h.dispatch(t, 2, map[string]any{"issue": "PAL-99", "status": "Done"})
	if err == nil {
		t.Fatal("a call whose arguments changed after the approval was dispatched without complaint")
	}
	if !strings.Contains(err.Error(), "diverged") {
		t.Fatalf("the diverged-after-approval refusal reads %q, want it to name the divergence", err)
	}
	if n := h.ran(); n != 0 {
		t.Fatalf("the tool ran %d time(s) on arguments nobody approved, want 0", n)
	}
}

// TestToolApprovalDenyIsAnAnswerTheModelContinuesOn: a deny is not silence. The model receives the same
// {"status":"denied","reason":…} shape a before_tool hook deny produces — no second delivery path — and
// the effect never fires.
func TestToolApprovalDenyIsAnAnswerTheModelContinuesOn(t *testing.T) {
	ctx := context.Background()
	h := newApprovalHarness(t, gatedTool)
	args := map[string]any{"issue": "PAL-42"}
	if _, err := h.dispatch(t, 1, args); !errors.Is(err, errRunAwaitingApproval) {
		t.Fatalf("first dispatch error = %v, want the park", err)
	}
	if applied, err := h.spine.DecideToolApproval(ctx, h.tenant, coordinator.ToolApprovalDecision{
		ToolCallID: h.callID, RequestHash: requestHashFor(args), DecidedBy: "slack:T1:Uapprover",
		Approve: false, Reason: "PAL-42 is not ready to close",
	}); err != nil || !applied {
		t.Fatalf("DecideToolApproval(deny) = %v, %v", applied, err)
	}

	ch, err := h.dispatch(t, 2, args)
	if err != nil {
		t.Fatalf("post-deny dispatch error = %v — a deny must let the run CONTINUE, not fail it", err)
	}
	if n := h.ran(); n != 0 {
		t.Fatalf("the tool ran %d time(s) after a deny, want 0", n)
	}
	got := toolResults(ch)
	if len(got) != 1 {
		t.Fatalf("the model was sent %d result(s) after a deny, want exactly 1", len(got))
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(got[0].content), &body); err != nil {
		t.Fatalf("the deny answer is not JSON: %q", got[0].content)
	}
	if body["status"] != "denied" {
		t.Fatalf("the deny answer is %v, want the before_tool deny shape {\"status\":\"denied\",...}", body)
	}
	if reason, _ := body["reason"].(string); !strings.Contains(reason, "not ready to close") {
		t.Fatalf("the human's reason did not reach the model: %v", body)
	}
}

// TestExpiredToolApprovalCancelsTheCallAndWakesTheParkedRun is the second half of expiry, and it did not
// exist before this task. A gate whose worst behaviour is keeping shut forever what it closed is worse
// than no gate: the deadline must cancel the call AND release the run.
func TestExpiredToolApprovalCancelsTheCallAndWakesTheParkedRun(t *testing.T) {
	ctx := context.Background()
	h := newApprovalHarness(t, gatedTool)
	args := map[string]any{"issue": "PAL-42"}
	if _, err := h.dispatch(t, 1, args); !errors.Is(err, errRunAwaitingApproval) {
		t.Fatalf("first dispatch error = %v, want the park", err)
	}
	if got := h.runState(t); got != string(statemachines.RunWaiting) {
		t.Fatalf("run state = %q, want waiting before the deadline passes", got)
	}
	// Move the clock rather than wait 30 minutes: the deadline is a column, so the component test edits
	// the column. (A real 30-minute expiry is §6 leg 6, an operator leg, and is named there as such.)
	execSQL(t, h.spine.Pool(), `UPDATE approvals SET expires_at = clock_timestamp() - interval '1 minute' WHERE tool_call_id = $1`, h.callID)

	swept, err := h.spine.SweepExpiredToolApprovals(ctx)
	if err != nil {
		t.Fatalf("SweepExpiredToolApprovals error = %v", err)
	}
	if swept < 1 {
		t.Fatalf("the reaper swept %d elapsed approvals, want at least this one", swept)
	}
	if got := h.callState(t); got != "canceled" {
		t.Fatalf("tool_call state after expiry = %q, want canceled", got)
	}
	if got := h.runState(t); got == string(statemachines.RunWaiting) {
		t.Fatal("the run is STILL waiting after its approval expired: the gate closed something forever")
	}
	// The expiry is journaled on an EXISTING event type — no new type was opened for it.
	var expiredEvents int
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(ctx),
		`SELECT count(*) FROM events WHERE session_id=$1 AND type='approval.expired.v1'`, h.sessionID).Scan(&expiredEvents); err != nil {
		t.Fatalf("count expiry events: %v", err)
	}
	if expiredEvents != 1 {
		t.Fatalf("approval.expired.v1 was journaled %d time(s), want 1", expiredEvents)
	}
	// And the model LEARNS it: the woken attempt is told the call was not authorized, rather than silently
	// losing the step.
	ch, err := h.dispatch(t, 2, args)
	if err != nil {
		t.Fatalf("post-expiry dispatch error = %v", err)
	}
	if n := h.ran(); n != 0 {
		t.Fatalf("the tool ran %d time(s) after its approval expired, want 0", n)
	}
	if got := toolResults(ch); len(got) != 1 || !strings.Contains(got[0].content, "expired") {
		t.Fatalf("the model was not told the approval expired: %+v", got)
	}
	// A second sweep moves nothing — the expiry is single-winner.
	if swept, err := h.spine.SweepExpiredToolApprovals(ctx); err != nil || swept != 0 {
		t.Fatalf("second sweep = %d, %v; want 0 (already settled)", swept, err)
	}
}

// TestToolWithoutApprovalDeclaredIsBitUnchanged: the gate appears ONLY where an operator asked for one.
// A tool that declares nothing dispatches exactly as it did before this task — no park, no approvals row,
// no extra state. The whole epic's blast radius is this assertion's inverse.
func TestToolWithoutApprovalDeclaredIsBitUnchanged(t *testing.T) {
	ungated := gatedTool
	ungated.RequiresApproval = false
	h := newApprovalHarness(t, ungated)
	args := map[string]any{"issue": "PAL-42"}

	ch, err := h.dispatch(t, 1, args)
	if err != nil {
		t.Fatalf("an ungated dispatch error = %v", err)
	}
	if n := h.ran(); n != 1 {
		t.Fatalf("an ungated tool ran %d time(s), want 1", n)
	}
	if got := toolResults(ch); len(got) != 1 || got[0].replayed {
		t.Fatalf("ungated results = %+v, want one fresh result", got)
	}
	if got := h.runState(t); got != "running" {
		t.Fatalf("an ungated call parked the run (state %q)", got)
	}
	var approvals int
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM approvals WHERE tool_call_id = $1`, h.callID).Scan(&approvals); err != nil {
		t.Fatalf("count approvals: %v", err)
	}
	if approvals != 0 {
		t.Fatalf("an ungated call opened %d approval row(s), want 0", approvals)
	}
}

// TestToolApprovalScreenComesFromTheLedgerRowAndNotTheFrame closes the loop between the sweep in
// approval_display_test.go and a REAL parked call: the screen a surface would show is derived from what
// the spine stored, so what a human reads and what Execute will run are the same bytes by construction.
func TestToolApprovalScreenComesFromTheLedgerRowAndNotTheFrame(t *testing.T) {
	h := newApprovalHarness(t, gatedTool)
	args := map[string]any{"issue": "PAL-42", "comment": "ship it, no review needed"}
	if _, err := h.dispatch(t, 1, args); !errors.Is(err, errRunAwaitingApproval) {
		t.Fatalf("first dispatch error = %v, want the park", err)
	}
	parked, found, err := h.spine.ToolApprovalForCall(context.Background(), h.tenant, h.callID)
	if err != nil || !found {
		t.Fatalf("ToolApprovalForCall = %v, %v", found, err)
	}
	display := DeriveToolApprovalDisplay(parked.ToolName, gatedTool.ApprovalLabel, parked.Arguments)
	if display.Identity != "jira.transitionIssue" {
		t.Fatalf("identity = %q, want the resolved tool name", display.Identity)
	}
	if display.OperatorLabel != gatedTool.ApprovalLabel {
		t.Fatalf("operator label = %q, want the one written at registration", display.OperatorLabel)
	}
	for _, want := range []string{"PAL-42", "ship it, no review needed"} {
		if !strings.Contains(display.Arguments, want) {
			t.Fatalf("argument %q is missing from the screen:\n%s", want, display.Arguments)
		}
	}
	// The bytes on the screen are the bytes bound to the button.
	if parked.RequestHash != requestHashFor(args) {
		t.Fatalf("the stored binding %q is not this call's hash", parked.RequestHash)
	}
}
