package extensions

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/palgroup/palai/packages/contracts"
)

// S11's guarantee is a NEGATIVE one, and a negative is proven by absence rather than by behaviour: when Slack
// answers an append with `stopped_by_user`, the stream stops and THE RUN DOES NOT.
//
// Testing that behaviourally can only ever show that one path did not cancel a run. This shows that no path
// can: the follower does not reach a single run-control call, so there is nothing for a future edit to reach
// past. The reason it matters is the plan's §2 invariant — `stopped_by_user` carries no authenticated actor,
// while every run-control call in this tree stands behind a verified principal. Wiring the first to the
// second would be inventing an authorization path out of a UI affordance, and it would be a two-line change
// in a file nobody re-reads.
//
// If cancelling from Slack is ever wanted, it earns its own authorization path exactly the way approve/deny
// did — and this test is what makes adding it a deliberate act.
// It scans the PARSED file rather than its bytes, and that distinction is the whole difference between a
// guard and a decoration: a raw substring sweep fails on the sentence above (which names AcceptCommand in
// order to say the follower must not call it), so the only way to keep it green would be to stop writing the
// reason down. Walking identifiers instead means prose is free and a CALL is not.
func TestSlackStreamNeverControlsTheRun(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "slack_stream.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse the follower's source: %v", err)
	}
	// The run-control surface of packages/coordinator. Naming them individually (rather than matching a
	// pattern) means a NEW control call must be added here consciously.
	forbidden := map[string]bool{
		"AcceptCommand": true, "ApplyRunTransition": true, "CancelRunReconciled": true,
		"PauseRun": true, "InterruptRun": true,
	}
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if forbidden[sel.Sel.Name] {
			t.Fatalf("the Slack run follower calls %s. A stream stopping is not run control: `stopped_by_user` "+
				"carries no authenticated actor, and run control in this tree stands behind a verified principal.", sel.Sel.Name)
		}
		// It has no coordinator command spine to reach one THROUGH, either: the follower holds an
		// api.EventReader (read-only) and the outbound Slack client, and that is the whole of it.
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "coordinator" && sel.Sel.Name == "Store" {
			t.Fatal("the Slack run follower holds a coordinator.Store; it READS the journal, it does not write to the run")
		}
		return true
	})
}

// slackStreamLine is TOTAL: every event type it does not know maps to silence rather than to a dump of an
// internal payload into a workspace channel.
func TestSlackStreamLineIsTotalAndSaysOnlyWhatTheJournalKnows(t *testing.T) {
	// A MODEL STEP IS SILENT, and that is the fix for the status that was landing inside the answer.
	// markdown_text is the message BODY ("This text is what will be appended to the message received so far",
	// chat.appendStream, checked 2026-07-27), so "_Working on it…_" was rendering in front of the model's
	// words: `_Working on it..._2 + 2 = 4.` in a real channel. A status has its own surface and is already set
	// there before the first journal read. A real run is single-step, so it now streams nothing at all.
	if got := slackStreamLine(contracts.Event{Type: "model_step.completed.v1"}); got != "" {
		t.Fatalf("a model step produced %q, want silence — a status in markdown_text is a status in the answer", got)
	}

	// A task event is the ONLY kind carrying text a human wrote, and it is echoed with its status.
	line := slackStreamLine(contracts.Event{
		Type: "task.updated.v1",
		Data: map[string]any{"title": "Write the migration", "status": "complete"},
	})
	if !strings.Contains(line, "Write the migration") || !strings.Contains(line, "complete") {
		t.Fatalf("task line = %q, want the title and its status", line)
	}
	// A task with no title has nothing to say.
	if got := slackStreamLine(contracts.Event{Type: "task.created.v1", Data: map[string]any{"status": "pending"}}); got != "" {
		t.Fatalf("a titleless task produced %q, want silence", got)
	}

	// Unknown, internal and future event types are silent — including ones whose payloads are the kind of
	// thing that must never reach a channel.
	for _, quiet := range []string{
		"config.revised.v1", "attempt.recovering.v1", "checkpoint.rejected.v1",
		"run.queued.v1", "some.future.event.v9", "",
	} {
		if got := slackStreamLine(contracts.Event{Type: quiet, Data: map[string]any{"secret": "x"}}); got != "" {
			t.Fatalf("event %q produced %q, want silence", quiet, got)
		}
	}
}

// The run filter is what keeps a follow-up message in a thread from replaying the PREVIOUS answer's progress:
// one Slack thread is one session across many runs (SLK-003), so the session's journal is shared.
func TestSlackStreamReadsTheRunItIsFollowing(t *testing.T) {
	if got := slackEventRunID(contracts.Event{Data: map[string]any{"run_id": "run_a", "state": "completed"}}); got != "run_a" {
		t.Fatalf("run id = %q, want run_a", got)
	}
	if got := slackEventRunID(contracts.Event{Data: map[string]any{}}); got != "" {
		t.Fatalf("an event with no run id read as %q, want empty (it is not any run's business)", got)
	}
	if got := slackEventRunID(contracts.Event{Data: map[string]any{"run_id": 42}}); got != "" {
		t.Fatalf("a non-string run id read as %q, want empty", got)
	}
}
