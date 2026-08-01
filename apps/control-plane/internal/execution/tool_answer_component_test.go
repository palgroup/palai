//go:build component

package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/packages/coordinator"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
	"github.com/palgroup/palai/storage"
)

// THE WEDGE THIS FILE FENCES, AND THE ONE IT MUST NOT REMOVE.
//
// Reproduced on a native stack on 2026-08-01, same agent revision and same repository binding, one
// variable changed each time:
//
//	file read "repo/README"           (exists)  -> response completed in 13s, tool_call `completed`
//	file read "README"                (missing) -> response in_progress at 152s, run `running`, WEDGED
//	file read "../../../../etc/passwd" (refused) -> same wedge; THE SECURITY CONTROL TOOK THE RUN DOWN
//	shell `false`                     (exit 1)  -> completed; a non-zero EXIT CODE is a result FIELD
//
// Every test below drives the REAL dispatchTool against a REAL migrated spine and the REAL FileTool
// against a REAL directory on disk — no fake tool stands in for the one whose errors caused this — and
// every one of them asserts the same three things a wedge violates: the attempt did NOT die, the durable
// row reached `completed` rather than being left `executing` for the reconciler to escalate, and exactly
// one tool.result carrying the refusal reached the engine.
//
// The FAULT half is fenced just as hard (TestAToolFaultStillAbortsTheAttemptAndLeavesItsRowUncommitted).
// Without it this file would pass just as well against a change that turned EVERY error into a result,
// which is the outcome the whole distinction exists to prevent.

// answerWorkspace lays out a real allocation with a real file in it and returns the allocation root
// (what ExecEnv.WorkspaceRoot is) plus an OUTSIDE-the-root canary the traversal leg must never read.
func answerWorkspace(t *testing.T) (root, canaryPath, canaryBody string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "alloc")
	if err := workspace.Prepare(root); err != nil {
		t.Fatalf("workspace.Prepare() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, workspace.RepoDir, "README"), []byte("Hello World!\n"), 0o644); err != nil {
		t.Fatalf("seed README: %v", err)
	}
	// The canary sits OUTSIDE the allocation root, which is what `../../…` is reaching for. Its body is
	// unique so its presence anywhere in the delivered result or the committed row is unambiguous.
	canaryBody = "canary-" + redeliveryID("secret")
	canaryPath = filepath.Join(base, "outside.txt")
	if err := os.WriteFile(canaryPath, []byte(canaryBody), 0o600); err != nil {
		t.Fatalf("seed canary: %v", err)
	}
	return root, canaryPath, canaryBody
}

// fileBroker is the SHIPPED file tool behind the SHIPPED broker — the object under test, not a stand-in.
func fileBroker() *toolbroker.Broker { return toolbroker.New(tools.FileTool()) }

// answerAttempt is ledgerAttempt with a real workspace root bound to the attempt, so o.execEnv hands the
// file tool the same ExecEnv production hands it.
func answerAttempt(cs *coordinator.Store, broker *toolbroker.Broker, tenant coordinator.Tenant, sessionID, runID, root string, fence uint64) (*Orchestrator, *attemptState, *recordingChannel) {
	orch, st, ch := ledgerAttempt(cs, broker, tenant, sessionID, runID, fence)
	st.attempt.WorkspaceHostPath = root
	return orch, st, ch
}

// deliveredAnswer decodes the ONE tool.result the attempt sent and returns its refusal fields. It fails
// the test when the count is not one, because "no result" is precisely the wedge and "two results" would
// be a double delivery.
func deliveredAnswer(t *testing.T, ch *recordingChannel) (code, message, raw string) {
	t.Helper()
	got := toolResults(ch)
	if len(got) != 1 {
		t.Fatalf("tool.results delivered = %d, want exactly 1 (0 is the wedge: the model was told nothing)", len(got))
	}
	var probe struct {
		Status string `json:"status"`
		Error  struct {
			Code    string `json:"code"`
			Tool    string `json:"tool"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(got[0].content), &probe); err != nil {
		t.Fatalf("delivered result is not JSON: %v (%s)", err, got[0].content)
	}
	if probe.Status != "error" {
		t.Fatalf("delivered result status = %q, want %q: %s", probe.Status, "error", got[0].content)
	}
	return probe.Error.Code, probe.Error.Message, got[0].content
}

// toolRow reads the durable row's state and committed result.
func toolRow(t *testing.T, cs *coordinator.Store, callID string) (state, result string) {
	t.Helper()
	var res *string
	if err := cs.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state, result::text FROM tool_calls WHERE id=$1`, callID).Scan(&state, &res); err != nil {
		t.Fatalf("read tool_calls row %s: %v", callID, err)
	}
	if res != nil {
		result = *res
	}
	return state, result
}

// runState reads the run's durable state, which is the wedge's own symptom: a wedged run sits `running`.
func runState(t *testing.T, cs *coordinator.Store, runID string) string {
	t.Helper()
	var state string
	if err := cs.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM runs WHERE id=$1`, runID).Scan(&state); err != nil {
		t.Fatalf("read runs row %s: %v", runID, err)
	}
	return state
}

// TestAMissingFileIsAnsweredToTheModelInsteadOfWedgingTheRun is leg B of the reproduction — the most
// ordinary thing a coding agent does wrong. The model asks for `README` when the file is at
// `repo/README`; before this change the attempt died, the pre-written `executing` row was escalated to
// manual_resolution, and the run sat `running` forever.
func TestAMissingFileIsAnsweredToTheModelInsteadOfWedgingTheRun(t *testing.T) {
	ctx := context.Background()
	cs, tenant, sessionID, runID := openLedgerSpine(t)
	root, _, _ := answerWorkspace(t)
	callID := redeliveryID("tc")

	orch, st, ch := answerAttempt(cs, fileBroker(), tenant, sessionID, runID, root, 1)
	err := orch.dispatchTool(ctx, st, toolRequestFrame(callID, "palai.workspace.file",
		map[string]any{"op": "read", "path": "README"}))
	if err != nil {
		t.Fatalf("dispatchTool for a missing file error = %v, want nil (an absent file is an ANSWER, not a fault)", err)
	}

	code, message, _ := deliveredAnswer(t, ch)
	if code != toolbroker.AnswerNotFound {
		t.Fatalf("answer code = %q, want %q", code, toolbroker.AnswerNotFound)
	}
	if !strings.Contains(message, "README") {
		t.Fatalf("answer message = %q, want it to name the path the model asked for", message)
	}

	// The durable row is CLOSED. `executing` here is the wedge: it is what the next attempt's consult
	// classifies `uncertain` and what the reconciler escalates to manual_resolution.
	state, committed := toolRow(t, cs, callID)
	if state != "completed" {
		t.Fatalf("tool_calls state = %q, want completed (executing/uncertain/manual_resolution IS the wedge)", state)
	}
	if !isAnswerResult(committed) {
		t.Fatalf("committed result = %s, want the same refusal the model was handed", committed)
	}
	// The model can still be told what actually IS there, on the same run — which is the point of not
	// killing it. The correction runs through the same dispatcher and comes back a normal result.
	orch2, st2, ch2 := answerAttempt(cs, fileBroker(), tenant, sessionID, runID, root, 1)
	if err := orch2.dispatchTool(ctx, st2, toolRequestFrame(redeliveryID("tc"), "palai.workspace.file",
		map[string]any{"op": "read", "path": "repo/README"})); err != nil {
		t.Fatalf("corrected read error = %v, want nil", err)
	}
	corrected := toolResults(ch2)
	if len(corrected) != 1 || isAnswerResult(corrected[0].content) || !strings.Contains(corrected[0].content, "Hello World!") {
		t.Fatalf("corrected read delivered %+v, want the file's real content (non-vacuity: the fixture IS readable)", corrected)
	}
	if got := runState(t, cs, runID); got != "running" {
		t.Fatalf("run state = %q, want running (the run must survive both calls)", got)
	}
}

// TestARefusedTraversalIsAnsweredAndTheOutsideFileIsNeverRead is leg D, and it is the leg that matters
// most: the security control WORKED and took the run down with it. It must go on working — the bytes
// outside the allocation must appear in nothing — while the run stays alive and the model is told why.
func TestARefusedTraversalIsAnsweredAndTheOutsideFileIsNeverRead(t *testing.T) {
	ctx := context.Background()
	cs, tenant, sessionID, runID := openLedgerSpine(t)
	root, canaryPath, canaryBody := answerWorkspace(t)
	callID := redeliveryID("tc")

	// Three shapes of the same attack. The canary-bearing ones come FIRST so the leak assertion below is
	// REACHABLE: `../../../../etc/passwd` climbs past a macOS temp root into a directory that does not
	// exist, so it MISSES rather than escapes and proves the least about containment. It is kept because
	// it is the exact string from the reproduction, not because it is the sharp case.
	for _, target := range []string{"../outside.txt", canaryPath, "../../../../etc/passwd"} {
		orch, st, ch := answerAttempt(cs, fileBroker(), tenant, sessionID, runID, root, 1)
		id := callID + "-" + fmt.Sprint(len(target))
		err := orch.dispatchTool(ctx, st, toolRequestFrame(id, "palai.workspace.file",
			map[string]any{"op": "read", "path": target}))
		if err != nil {
			t.Fatalf("dispatchTool(%q) error = %v, want nil (a refusal must not end the run)", target, err)
		}
		// THE LEAK ASSERTION RUNS FIRST, AND THE ORDER IS THE POINT RATHER THAN THE STYLE.
		//
		// MEASURED by deleting BOTH containment guards in WorkspaceFS.resolve: a path that no longer
		// escapes merely MISSES, so the answer code changes from `refused` to `not_found` — and with the
		// code assertion first this test failed saying "answer code = not_found, want refused", which
		// reads as a naming problem and is in fact a read of somebody else's disk. An assertion that
		// fails for a reason unrelated to what it claims is worse than one that does not fail at all,
		// because it sends the next reader to the wrong file. With this order it says what happened:
		//
		//   traversal "../outside.txt" LEAKED the out-of-workspace file into the result:
		//   {"content":"canary-secret_95d83823f1e72179","path":"../outside.txt","size":30}
		//
		// THE REFUSAL STILL REFUSES. The canary body is unique to this test; its presence in either the
		// delivered bytes or the committed row would mean the read happened.
		state, committed := toolRow(t, cs, id)
		raw := ""
		if got := toolResults(ch); len(got) == 1 {
			raw = got[0].content
		}
		if strings.Contains(raw, canaryBody) || strings.Contains(committed, canaryBody) {
			t.Fatalf("traversal %q LEAKED the out-of-workspace file into the result: %s", target, raw)
		}
		if strings.Contains(raw, "root:") || strings.Contains(committed, "root:") {
			t.Fatalf("traversal %q returned /etc/passwd content: %s", target, raw)
		}
		code, _, _ := deliveredAnswer(t, ch)
		if code != toolbroker.AnswerRefused {
			t.Fatalf("traversal %q answer code = %q, want %q", target, code, toolbroker.AnswerRefused)
		}
		if state != "completed" {
			t.Fatalf("traversal %q left tool_calls state %q, want completed", target, state)
		}
	}
	if got := runState(t, cs, runID); got != "running" {
		t.Fatalf("run state after three refused traversals = %q, want running", got)
	}
	// Non-vacuity: the canary really is readable through the OS, so "not found in the result" is a
	// statement about the refusal rather than about an empty file.
	if body, err := os.ReadFile(canaryPath); err != nil || string(body) != canaryBody {
		t.Fatalf("canary fixture unreadable (%v): the leak assertions above would be vacuous", err)
	}
}

// TestAToolFaultStillAbortsTheAttemptAndLeavesItsRowUncommitted is the negative that keeps the two
// halves apart. A tool whose Exec returns a PLAIN error — no toolbroker.Answer anywhere — must behave
// exactly as it did before this change: the attempt fails, no tool.result is delivered, and the durable
// row is left `executing` for the reconciler. If this ever goes green-by-delivery, somebody has widened
// "answer" to mean "every error", which would swallow a fence violation and let a poisoned attempt talk.
func TestAToolFaultStillAbortsTheAttemptAndLeavesItsRowUncommitted(t *testing.T) {
	ctx := context.Background()
	cs, tenant, sessionID, runID := openLedgerSpine(t)
	callID := redeliveryID("tc")

	broker := toolbroker.New(toolbroker.Tool{
		Name: "fault.reversible", InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		ReplayClass: toolbroker.ClassReversible, // same class as the file tool, so the pre-write happens
		Invoke: func(map[string]any) (map[string]any, error) {
			return nil, errors.New("connection reset by peer")
		},
	})
	orch, st, ch := ledgerAttempt(cs, broker, tenant, sessionID, runID, 1)
	err := orch.dispatchTool(ctx, st, toolRequestFrame(callID, "fault.reversible", map[string]any{}))
	if err == nil {
		t.Fatal("dispatchTool for an UNCLASSIFIED error returned nil; an unclassified error must still abort the attempt")
	}
	if _, isAnswer := toolbroker.AsAnswer(err); isAnswer {
		t.Fatalf("a plain error was classified as an answer: %v", err)
	}
	if got := toolResults(ch); len(got) != 0 {
		t.Fatalf("a fault delivered %+v to the model, want nothing", got)
	}
	if state, _ := toolRow(t, cs, callID); state != "executing" {
		t.Fatalf("tool_calls state after a fault = %q, want executing (the reconciler's to resolve)", state)
	}
}

// TestTheToolErrorBudgetEndsARunThatKeepsBeingRefused is the bound. Sixteen is the shipped number; this
// drives two so the loop is short, and asserts the run reaches a NAMED terminal rather than either
// wedging (the old behaviour) or looping forever (the new behaviour's own risk).
func TestTheToolErrorBudgetEndsARunThatKeepsBeingRefused(t *testing.T) {
	ctx := context.Background()
	cs, tenant, sessionID, runID := openLedgerSpine(t)
	root, _, _ := answerWorkspace(t)
	t.Setenv("PALAI_TOOL_ERROR_BUDGET", "2")

	orch, st, ch := answerAttempt(cs, fileBroker(), tenant, sessionID, runID, root, 1)
	// Two refusals are inside the budget and are answered.
	var ids []string
	for i := 0; i < 2; i++ {
		id := redeliveryID("tc")
		ids = append(ids, id)
		if err := orch.dispatchTool(ctx, st, toolRequestFrame(id, "palai.workspace.file",
			map[string]any{"op": "read", "path": fmt.Sprintf("nope-%d.txt", i)})); err != nil {
			t.Fatalf("refusal %d error = %v, want nil (inside the budget)", i+1, err)
		}
	}
	if got := toolResults(ch); len(got) != 2 {
		t.Fatalf("results delivered inside the budget = %d, want 2", len(got))
	}
	// The third passes it: the sentinel is raised, and NOTHING more is delivered.
	third := redeliveryID("tc")
	err := orch.dispatchTool(ctx, st, toolRequestFrame(third, "palai.workspace.file",
		map[string]any{"op": "read", "path": "nope-3.txt"}))
	if !errors.Is(err, errToolAnswerBudget) {
		t.Fatalf("over-budget dispatchTool error = %v, want errToolAnswerBudget", err)
	}
	if got := toolResults(ch); len(got) != 2 {
		t.Fatalf("results delivered after the budget = %d, want still 2 (the model is handed nothing more)", len(got))
	}
	// The over-budget call's row is still CLOSED — the refusal that was produced is recorded, so nothing
	// is left for the reconciler and an operator can read exactly what kept being refused.
	if state, _ := toolRow(t, cs, third); state != "completed" {
		t.Fatalf("over-budget tool_calls state = %q, want completed (committed before the stop)", state)
	}
	// And the run reaches a NAMED terminal rather than sitting `running`.
	st.responseID = "" // the seeded run has no response row; FinalizeResponse is exercised by the API tier
	if err := orch.failToolAnswerBudget(ctx, st); err != nil && !strings.Contains(err.Error(), "response") {
		t.Fatalf("failToolAnswerBudget error = %v", err)
	}
	if got := runState(t, cs, runID); got != "failed" {
		t.Fatalf("run state after the budget = %q, want failed (NAMED terminal, not `running`)", got)
	}
}

// TestAnExplicitlyUnboundedBudgetIsHonoured is the budget's own negative: the guard above must be the
// budget doing the work rather than the third call being special. `0` is unbounded and has to be
// WRITTEN, which is the idiom PALAI_BACKGROUND_MAX_WALL_TIME already uses in this tree.
func TestAnExplicitlyUnboundedBudgetIsHonoured(t *testing.T) {
	ctx := context.Background()
	cs, tenant, sessionID, runID := openLedgerSpine(t)
	root, _, _ := answerWorkspace(t)
	t.Setenv("PALAI_TOOL_ERROR_BUDGET", "0")

	orch, st, ch := answerAttempt(cs, fileBroker(), tenant, sessionID, runID, root, 1)
	for i := 0; i < 5; i++ {
		if err := orch.dispatchTool(ctx, st, toolRequestFrame(redeliveryID("tc"), "palai.workspace.file",
			map[string]any{"op": "read", "path": fmt.Sprintf("nope-%d.txt", i)})); err != nil {
			t.Fatalf("refusal %d under an unbounded budget error = %v, want nil", i+1, err)
		}
	}
	if got := toolResults(ch); len(got) != 5 {
		t.Fatalf("results under an unbounded budget = %d, want 5", len(got))
	}
}

// TestAReplayedRefusalCountsAgainstTheSameBudget closes the reset the counter would otherwise have. A
// reclaimed attempt replays the committed transcript through the ledger consult; if only FRESH
// executions counted, a crash loop would walk through the bound one attempt at a time forever.
func TestAReplayedRefusalCountsAgainstTheSameBudget(t *testing.T) {
	ctx := context.Background()
	cs, tenant, sessionID, runID := openLedgerSpine(t)
	root, _, _ := answerWorkspace(t)
	t.Setenv("PALAI_TOOL_ERROR_BUDGET", "1")
	callID := redeliveryID("tc")

	// Attempt one: one refusal, inside the budget, committed.
	orch1, st1, _ := answerAttempt(cs, fileBroker(), tenant, sessionID, runID, root, 1)
	if err := orch1.dispatchTool(ctx, st1, toolRequestFrame(callID, "palai.workspace.file",
		map[string]any{"op": "read", "path": "nope.txt"})); err != nil {
		t.Fatalf("first refusal error = %v, want nil", err)
	}
	// A FRESH process (new broker, empty in-memory cache) re-dispatches the same call id. It replays off
	// the durable row — and that replay is one refusal in front of the model, so it counts.
	orch2, st2, ch2 := answerAttempt(cs, fileBroker(), tenant, sessionID, runID, root, 2)
	if err := orch2.dispatchTool(ctx, st2, toolRequestFrame(callID, "palai.workspace.file",
		map[string]any{"op": "read", "path": "nope.txt"})); err != nil {
		t.Fatalf("replayed refusal error = %v, want nil (1 is inside a budget of 1)", err)
	}
	if got := toolResults(ch2); len(got) != 1 || !got[0].replayed {
		t.Fatalf("replay delivered %+v, want one LABELED replayed refusal", got)
	}
	if st2.toolAnswerErrors != 1 {
		t.Fatalf("replayed refusals counted = %d, want 1 (a replay must not be free)", st2.toolAnswerErrors)
	}
	// A second refusal on that same attempt is now over the budget of 1.
	if err := orch2.dispatchTool(ctx, st2, toolRequestFrame(redeliveryID("tc"), "palai.workspace.file",
		map[string]any{"op": "read", "path": "still-nope.txt"})); !errors.Is(err, errToolAnswerBudget) {
		t.Fatalf("second refusal after a counted replay error = %v, want errToolAnswerBudget", err)
	}
}
