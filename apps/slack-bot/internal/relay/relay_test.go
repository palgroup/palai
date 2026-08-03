package relay

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/integrations/slack"
	statemachines "github.com/palgroup/palai/packages/state-machines"
	palai "github.com/palgroup/palai/sdks/go"
)

// sliceStream replays a fixed, in-memory event sequence — the shape a real *palai.SessionEventStream
// has (Next/Close), without dialing anything. Next after the last event returns io.EOF, matching the
// real stream's own contract (sdks/go/sessionevents.go): EOF means "cleanly ended", which for these
// tests means the fixture ran out, not that the terminal event was mishandled.
type sliceStream struct {
	events []palai.Event
	i      int
}

// staticStream wraps a fixed event sequence as an EventStream.
func staticStream(events []palai.Event) EventStream {
	return &sliceStream{events: events}
}

func (s *sliceStream) Next() (palai.Event, error) {
	if s.i >= len(s.events) {
		return palai.Event{}, io.EOF
	}
	e := s.events[s.i]
	s.i++
	return e, nil
}

func (s *sliceStream) Close() error { return nil }

// panicOnFirstNext simulates a defect upstream of the relay (a decode bug, a nil dereference) rather
// than a normal stream error, so a test can prove Run's stop path survives a panic and not only a
// returned error.
type panicOnFirstNext struct{}

func (panicOnFirstNext) Next() (palai.Event, error) { panic("boom: simulated decode failure") }
func (panicOnFirstNext) Close() error               { return nil }

// noApprovals is the ApprovalHook for a test whose events never park a run. It is a named function
// rather than an inline literal so that a test which DOES care (TestAnApprovalRequestReachesTheBridge)
// stands out as the one place the hook is watched.
func noApprovals(context.Context, string, string, palai.Event) {}

// fakeSlack is the Slack seam's test double: it counts calls and records what text arrived, and can be
// told to fail the leading N AppendStream calls to exercise the pending-text recovery path.
type fakeSlack struct {
	started, stopped int
	appended         []string
	stoppedText      string
	// startedText is chat.startStream's own markdown_text. It is recorded because that text is the FIRST
	// thing in the message body and nothing else in this file could see it — the opening status and the
	// model's first delta fuse there exactly as two appends would, on a path emitStatus never touches.
	startedText string
	// tasks are the task_update chunks, kept apart from appended because a card is not body text: a test
	// asserting what the message BODY says must not be able to read a card and call it prose.
	tasks []slack.Task

	failAppends int
	// failStop makes the closing chat.stopStream refuse — the ONE Slack failure Run reports rather than
	// absorbs, since there is no later call to carry the text (see openStream.pending).
	failStop bool
}

func (f *fakeSlack) StartStream(ctx context.Context, channel, threadTS, markdownText string) (string, error) {
	f.started++
	f.startedText = markdownText
	return "1.1", nil
}

func (f *fakeSlack) UpdateTask(ctx context.Context, channel, ts string, task slack.Task) error {
	f.tasks = append(f.tasks, task)
	return nil
}

func (f *fakeSlack) AppendStream(ctx context.Context, channel, ts, markdownText string) error {
	if f.failAppends > 0 {
		f.failAppends--
		return errors.New("slack: append failed")
	}
	f.appended = append(f.appended, markdownText)
	return nil
}

func (f *fakeSlack) StopStream(ctx context.Context, channel, ts, markdownText string) error {
	f.stopped++
	f.stoppedText = markdownText
	if f.failStop {
		return errors.New("slack: stop failed")
	}
	return nil
}

func TestDeltasBecomeOneAppendPerWindow(t *testing.T) {
	events := []palai.Event{
		{Type: "model_step.delta.v1", Data: map[string]any{"text": "Hel"}},
		{Type: "model_step.delta.v1", Data: map[string]any{"text": "lo"}},
		{Type: "run.completed.v1", Data: map[string]any{}},
	}
	fake := &fakeSlack{}
	if err := Run(context.Background(), Deps{Events: staticStream(events), Slack: fake, OnApproval: noApprovals},
		"sess_1", "C1", "1.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fake.started != 1 || fake.stopped != 1 {
		t.Fatalf("start=%d stop=%d, want 1/1", fake.started, fake.stopped)
	}
	if got := strings.Join(fake.appended, ""); got != "Hello" {
		t.Fatalf("appended %q, want Hello", got)
	}
}

// TestATerminalEventAlwaysStopsTheStream walks every run terminal branch — a stream left open renders
// as permanently "streaming" in Slack (SLK-P2), and forgetting one branch is the shape of bug this tree
// has shipped repeatedly. run.budget_exceeded.v1 is included alongside the three the plan named because
// it is measured to be in the SAME closed set the SSE endpoint itself closes on
// (apps/control-plane/api/events.go's terminalEventTypes).
//
// Each fixture carries a SECOND event after the terminal one, and the test asserts the stream never read
// it (sliceStream.i == 1): Run's defer-based stop also fires when a stream ends on its own (io.EOF), so
// a version of this test that only checked fake.stopped stayed green even with an entry deleted from
// runTerminalEvents — that fallback is real defense in depth, but it made the assertion vacuous for the
// one thing this test exists to pin, namely that the TYPE ITSELF is recognized as terminal and stops the
// loop immediately, not just that the loop stops eventually once its fixture runs out.
func TestATerminalEventAlwaysStopsTheStream(t *testing.T) {
	for _, term := range []string{"run.completed.v1", "run.failed.v1", "run.canceled.v1", "run.timed_out.v1", "run.budget_exceeded.v1"} {
		fake := &fakeSlack{}
		stream := &sliceStream{events: []palai.Event{
			{Type: term, Data: map[string]any{}},
			{Type: "model_step.delta.v1", Data: map[string]any{"text": "SHOULD-NOT-BE-READ"}},
		}}
		if err := Run(context.Background(), Deps{Events: stream, Slack: fake, OnApproval: noApprovals}, "sess_1", "C1", "1.1"); err != nil {
			t.Fatalf("Run(%s): %v", term, err)
		}
		if fake.stopped != 1 {
			t.Fatalf("%s left the stream open (stopped=%d)", term, fake.stopped)
		}
		if stream.i != 1 {
			t.Fatalf("%s: the loop read %d event(s) past the terminal one — it must stop AT the terminal, "+
				"not merely eventually", term, stream.i)
		}
	}
}

// TestAPanicStillClosesTheSlackStream is the OTHER way a loop can end without ever seeing a run
// terminal: a defect in this package itself. The stop path must still run so the Slack message does not
// stay "streaming" forever, and the panic must come back as an error rather than crashing the process a
// whole bot's worth of OTHER sessions shares.
func TestAPanicStillClosesTheSlackStream(t *testing.T) {
	fake := &fakeSlack{}
	err := Run(context.Background(), Deps{Events: panicOnFirstNext{}, Slack: fake, OnApproval: noApprovals}, "sess_1", "C1", "1.1")
	if err == nil {
		t.Fatal("Run returned nil after a panic; want the panic converted to an error")
	}
	if fake.started != 1 || fake.stopped != 1 {
		t.Fatalf("start=%d stop=%d, want 1/1 — a panic must not leave the Slack message stuck streaming",
			fake.started, fake.stopped)
	}
}

// TestAFailedAppendIsRecoveredByTheNextOne pins the guarantee relay.go documents rather than just
// claims: a delta AppendStream failed to deliver is not dropped, it rides along on the next successful
// call.
func TestAFailedAppendIsRecoveredByTheNextOne(t *testing.T) {
	events := []palai.Event{
		{Type: "model_step.delta.v1", Data: map[string]any{"text": "Hel"}}, // this AppendStream call fails
		{Type: "model_step.delta.v1", Data: map[string]any{"text": "lo"}},  // this one succeeds, carrying both
		{Type: "run.completed.v1", Data: map[string]any{}},
	}
	fake := &fakeSlack{failAppends: 1}
	if err := Run(context.Background(), Deps{Events: staticStream(events), Slack: fake, OnApproval: noApprovals},
		"sess_1", "C1", "1.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.Join(fake.appended, ""); got != "Hello" {
		t.Fatalf("appended %q, want Hello — a failed chunk must be retried, not dropped", got)
	}
	if fake.stoppedText != "" {
		t.Fatalf("stopStream carried %q, want empty — everything was eventually delivered by append", fake.stoppedText)
	}
}

// TestTextThatNeverRecoversIsFlushedAtStop is the other half: text an AppendStream call never manages
// to deliver must still reach the message once, at chat.stopStream, rather than being silently lost —
// the concrete answer to "is the run's text still delivered some other way, or is it lost?".
func TestTextThatNeverRecoversIsFlushedAtStop(t *testing.T) {
	events := []palai.Event{
		{Type: "model_step.delta.v1", Data: map[string]any{"text": "lost"}}, // the only attempt, and it fails
		{Type: "run.completed.v1", Data: map[string]any{}},
	}
	fake := &fakeSlack{failAppends: 1}
	if err := Run(context.Background(), Deps{Events: staticStream(events), Slack: fake, OnApproval: noApprovals},
		"sess_1", "C1", "1.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.appended) != 0 {
		t.Fatalf("appended = %v, want none — the one attempt failed", fake.appended)
	}
	if fake.stoppedText != "lost" {
		t.Fatalf("stopStream text = %q, want the undelivered text flushed at close", fake.stoppedText)
	}
}

// TestToolProgressLeavesTheAnswerAlone is SLK-P6 closed, stated as the property the gap row states:
// "progress lines still ride the message body" — chat.appendStream's markdown_text IS the body, so a tool
// line the relay streamed sat ABOVE the model's answer in the finished message.
//
// The assertion has two halves and BOTH are needed. That the cards exist is not enough (the old prose could
// still be there beside them), and that the body is clean is not enough either (a relay that dropped tool
// events entirely would pass it). So: the steps are drawn as cards, AND nothing about them is in the body.
func TestToolProgressLeavesTheAnswerAlone(t *testing.T) {
	const answer = "Projede toplam 7 Swift dosyası var"
	events := []palai.Event{
		{Type: "tool_call.executing.v1", Data: map[string]any{
			"tool_call_id": "tc_1", "tool_name": "palai.workspace.file", "replay_class": "pure",
		}},
		{Type: "tool_call.progress.v1", Data: map[string]any{
			"tool_call_id": "tc_1", "progress": float64(1), "total": float64(4), "message": "reading SwiftUIListApp.swift",
		}},
		{Type: "tool_call.completed.v1", Data: map[string]any{"tool_call_id": "tc_1", "tool_name": "palai.workspace.file"}},
		{Type: "model_step.delta.v1", Data: map[string]any{"text": answer}},
		{Type: "run.completed.v1", Data: map[string]any{}},
	}
	fake := &fakeSlack{}
	if err := Run(context.Background(), Deps{Events: staticStream(events), Slack: fake, OnApproval: noApprovals},
		"sess_1", "C1", "1.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(fake.tasks) != 3 {
		t.Fatalf("drew %d task card update(s), want 3 (executing, progress, completed) — got %+v", len(fake.tasks), fake.tasks)
	}
	body := fake.startedText + strings.Join(fake.appended, "") + fake.stoppedText
	// The body is the opening status and the answer, and NOTHING about the tool. Each token below rode the
	// message body before this change; `palai.workspace.file` is checked because it is the raw name the card
	// still carries in its detail — proving the card's content did not simply get copied into the prose too.
	for _, leaked := range []string{"palai.workspace.file", "Reading files", "reading SwiftUIListApp.swift", "1/4", "▶️", "✅"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("the message body still carries %q — progress must ride the task timeline, not the "+
				"answer (SLK-P6). Body was %q", leaked, body)
		}
	}
	if !strings.Contains(body, answer) {
		t.Fatalf("the body is %q and no longer contains the answer — the model's own words must be the one "+
			"thing that DOES stay in it", body)
	}
}

// TestOneToolCallIsOneCardThroughout is the mechanism the whole timeline rests on: Slack advances a card
// when a later task_update repeats its task_id and DRAWS A SECOND ONE when it does not. A per-event id — a
// counter, a hash of the payload, anything not shared by the pair — renders the same step three times.
//
// tool_call_id is the id that is shared by construction: it is the ledger row's primary key, hardcoded into
// both writers' payloads (packages/coordinator/orchestration.go BeginToolCall and
// apps/control-plane/internal/execution/tool_dispatch.go).
func TestOneToolCallIsOneCardThroughout(t *testing.T) {
	events := []palai.Event{
		{Type: "tool_call.executing.v1", Data: map[string]any{"tool_call_id": "tc_1", "tool_name": "palai.workspace.shell"}},
		{Type: "tool_call.progress.v1", Data: map[string]any{"tool_call_id": "tc_1", "message": "xcodebuild"}},
		{Type: "tool_call.completed.v1", Data: map[string]any{"tool_call_id": "tc_1", "tool_name": "palai.workspace.shell"}},
		{Type: "run.completed.v1", Data: map[string]any{}},
	}
	fake := &fakeSlack{}
	if err := Run(context.Background(), Deps{Events: staticStream(events), Slack: fake, OnApproval: noApprovals},
		"sess_1", "C1", "1.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, task := range fake.tasks {
		if task.ID != "tc_1" {
			t.Fatalf("a card carried id %q, want tc_1 for every update of one call — a differing id draws a "+
				"SECOND card and the step is rendered twice", task.ID)
		}
	}
	// The statuses must PROGRESS, and the last one must not be in_progress: a step whose card never leaves
	// in_progress is a finished message that still shows the run working (the SLK-P2 shape, on a card).
	var got []string
	for _, task := range fake.tasks {
		got = append(got, task.Status)
	}
	if want := []string{"in_progress", "in_progress", "done"}; !slices.Equal(got, want) {
		t.Fatalf("card statuses were %v, want %v", got, want)
	}
	// The title is what a human reads; the raw name stays available underneath rather than being discarded.
	if fake.tasks[0].Title != "Running a command" {
		t.Fatalf("card title = %q, want a human phrase — `palai.workspace.shell` is jargon", fake.tasks[0].Title)
	}
	if fake.tasks[0].Detail != "palai.workspace.shell" {
		t.Fatalf("the opening card's detail = %q, want the raw tool name kept available", fake.tasks[0].Detail)
	}
	// AND THE TERMINAL CARRIES NO DETAIL, which is not an omission but the measured semantics: a card's
	// `details` APPENDS across updates of one id, so repeating the name here renders
	// `palai.workspace.shellpalai.workspace.shell` — which is exactly what the live workspace returned
	// before this was understood. The name is already on the card from the update above.
	if fake.tasks[2].Detail != "" {
		t.Fatalf("the completed card re-sent detail %q; `details` appends, so the name would be doubled",
			fake.tasks[2].Detail)
	}
}

// TestEveryToolTerminalResolvesItsCard walks the tool-call state machine's OWN table rather than a list
// retyped here, because the two ways this can be wrong are found by different means: a terminal the relay
// forgot is found by walking the canonical table (this test), and a terminal the table itself lacks would
// not be found by either — which is why the table is the authority and this test imports it.
//
// A terminal that maps to nothing leaves its card drawn `in_progress` forever.
func TestEveryToolTerminalResolvesItsCard(t *testing.T) {
	for _, tr := range statemachines.ToolCallTable {
		if tr.From != statemachines.ToolCallExecuting && tr.From != statemachines.ToolCallUncertain {
			continue // only a call that actually started can have a card to leave unresolved
		}
		t.Run(tr.Event, func(t *testing.T) {
			status, mapped := toolCallStatus[tr.Event]
			if !mapped {
				t.Fatalf("%s (%s -> %s) maps to no task status, so a call ending this way leaves its card "+
					"drawn in_progress in the finished message", tr.Event, tr.From, tr.To)
			}
			if tr.To != statemachines.ToolCallExecuting && status == "in_progress" {
				t.Fatalf("%s is a terminal but maps to in_progress", tr.Event)
			}
			// The mapping must also survive the adapter's own vocabulary check: a status blocks.go has not
			// been taught fails closed to `pending`, which for a finished call is the spinning-card bug
			// wearing a different name.
			if slack.TaskStatus(status) == "pending" {
				t.Fatalf("%s maps to %q, which blocks.go renders as `pending` — a finished call must not "+
					"render as not-yet-started", tr.Event, status)
			}
		})
	}
}

// TestACardIsSkippedWhenNothingCanIdentifyIt: an empty task_id is not a card Slack can advance, and two
// steps sharing one would overwrite each other. Both production writers always set tool_call_id, so this is
// the malformed-event path — it must degrade to no card rather than to a broken one.
func TestACardIsSkippedWhenNothingCanIdentifyIt(t *testing.T) {
	events := []palai.Event{
		{Type: "tool_call.executing.v1", Data: map[string]any{"tool_name": "palai.workspace.file"}},
		{Type: "run.completed.v1", Data: map[string]any{}},
	}
	fake := &fakeSlack{}
	if err := Run(context.Background(), Deps{Events: staticStream(events), Slack: fake, OnApproval: noApprovals},
		"sess_1", "C1", "1.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.tasks) != 0 {
		t.Fatalf("drew %+v for an event with no tool_call_id, want no card", fake.tasks)
	}
}

// TestAStatusLineIsNeverGluedToTheTextAfterIt is the 2026-08-03 defect, stated as the property rather
// than as the one line that was reported.
//
// chat.appendStream APPENDS, so whatever a status line ends with is immediately followed by the model's
// next delta. The owner's first real answer came back reading `✅ \`palai.workspace.file\` doneProjede
// toplam 7 Swift dosyası var` — `done` and `Projede` fused. The reported line was the tool-completed one,
// but ALL FOUR status lines were missing the separator, so a test pinning only the completed line would
// have gone green over three live instances of the same bug.
//
// The assertion is structural — the rune before the answer's first character must be a newline — rather
// than `!strings.Contains(body, "doneProjede")`, which would pass the moment either half is reworded and
// says nothing about the other three lines.
func TestAStatusLineIsNeverGluedToTheTextAfterIt(t *testing.T) {
	const answer = "Projede toplam 7 Swift dosyası var"
	for _, tc := range []struct {
		name   string
		before []palai.Event
	}{
		// NOTHING BEFORE THE ANSWER, and this leg is not a filler case: the body still opens with
		// chat.startStream's own markdown_text ("Working…"), which never passes through emitStatus, so it
		// was fused to the first delta by a separate path from the one the reported bug took. E08 makes
		// this the shape of EVERY real single-step run — no tools are exposed to a real provider, so there
		// is no tool line to blame and `Working…Projede toplam…` is what the owner would see.
		{name: "the opening status"},
		{name: "the approval notice", before: []palai.Event{
			{Type: ApprovalRequestedEventType, Data: map[string]any{"request_hash": "rh_1", "tool_name": "palai.workspace.shell"}},
		}},
		// THE TOOL LINES ARE DELIBERATELY ABSENT FROM THIS TABLE and their absence is the point rather than
		// a gap: tool_call.* no longer writes body text at all — it draws a task card — so a subtest for it
		// here would be asserting a separator between two things that never touch. What replaced the
		// coverage is TestToolProgressLeavesTheAnswerAlone, which pins the stronger property (no tool prose
		// in the body at all). If a tool line ever returns to the body, it belongs back in this table.
	} {
		// A SUBTEST PER LINE, not one loop body: with a shared t.Fatalf the first failing line ends the
		// whole test, so a sweep meant to prove several lines would only ever report one of them.
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeSlack{}
			events := append(append([]palai.Event{}, tc.before...),
				palai.Event{Type: "model_step.delta.v1", Data: map[string]any{"text": answer}},
				palai.Event{Type: "run.completed.v1", Data: map[string]any{}},
			)
			if err := Run(context.Background(), Deps{Events: staticStream(events), Slack: fake, OnApproval: noApprovals},
				"sess_1", "C1", "1.1"); err != nil {
				t.Fatalf("Run: %v", err)
			}
			// startedText FIRST: it is the message body's opening bytes, so a test that joined only the
			// appends would measure a separator the reader never sees.
			body := fake.startedText + strings.Join(fake.appended, "") + fake.stoppedText

			// Guard the measurement itself: if the status line rendered nothing the answer sits at index 0
			// and the check below would read out of range, and if the answer never arrived the whole
			// assertion is about an empty body. Either way the fixture — not the property — is what broke.
			idx := strings.Index(body, answer)
			if idx <= 0 {
				t.Fatalf("the body is %q — want the status line THEN the answer, so this test measures a "+
					"separator that actually has two sides", body)
			}
			if body[idx-1] != '\n' {
				t.Fatalf("the answer begins %q — a status line and the model's next word fused into one, "+
					"which is exactly what `doneProjede` was in the owner's channel", body[max(0, idx-24):idx+12])
			}
		})
	}
}

// TestToolCallProgressWithNoMessageIsSkipped: an MCP progress notification is advisory and often carries
// no human-readable message (packages/coordinator/mcp_progress.go AppendToolProgress) — that must not
// render a bare, content-free line into the thread.
//
// SINCE PROGRESS BECAME A CARD the stake is higher than a stray line, which is why the fixture now runs the
// notification AFTER a named executing event: re-sending the card with an empty detail would BLANK what the
// previous update wrote, so a messageless notification would erase the step's own description. The card
// must be left exactly as it was.
func TestToolCallProgressWithNoMessageIsSkipped(t *testing.T) {
	events := []palai.Event{
		{Type: "tool_call.executing.v1", Data: map[string]any{"tool_call_id": "tc_1", "tool_name": "palai.workspace.shell"}},
		{Type: "tool_call.progress.v1", Data: map[string]any{"tool_call_id": "tc_1", "progress": float64(1), "total": float64(4)}},
		{Type: "run.completed.v1", Data: map[string]any{}},
	}
	fake := &fakeSlack{}
	if err := Run(context.Background(), Deps{Events: staticStream(events), Slack: fake, OnApproval: noApprovals},
		"sess_1", "C1", "1.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.appended) != 0 {
		t.Fatalf("appended = %v, want none — a messageless progress notification renders nothing", fake.appended)
	}
	if len(fake.tasks) != 1 {
		t.Fatalf("drew %d card update(s), want 1 — a messageless notification must not re-draw the card "+
			"with an empty detail, which would erase what the step said it was doing: %+v", len(fake.tasks), fake.tasks)
	}
}

// TestAProgressNotificationDoesNotRenameItsStep is the other half of the same payload gap, and it is the
// one a reader would actually notice. A progress event carries {tool_call_id, progress, total, message} and
// NO tool_name, so a title re-derived from each event alone renames a live step to the no-name fallback the
// instant it reports progress: "Running a command" becomes "a tool" mid-run.
func TestAProgressNotificationDoesNotRenameItsStep(t *testing.T) {
	events := []palai.Event{
		{Type: "tool_call.executing.v1", Data: map[string]any{"tool_call_id": "tc_1", "tool_name": "palai.workspace.shell"}},
		{Type: "tool_call.progress.v1", Data: map[string]any{"tool_call_id": "tc_1", "message": "xcodebuild -scheme App"}},
		{Type: "run.completed.v1", Data: map[string]any{}},
	}
	fake := &fakeSlack{}
	if err := Run(context.Background(), Deps{Events: staticStream(events), Slack: fake, OnApproval: noApprovals},
		"sess_1", "C1", "1.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.tasks) != 2 {
		t.Fatalf("drew %d card update(s), want 2", len(fake.tasks))
	}
	if fake.tasks[1].Title != "Running a command" {
		t.Fatalf("the progress update retitled the step to %q — a card must keep the title it was drawn "+
			"with when the event carries no tool_name", fake.tasks[1].Title)
	}
	// The detail is separated from the one already on the card. `details` APPENDS (measured against the live
	// workspace), so without this the card reads `palai.workspace.shellxcodebuild -scheme App` — the
	// `doneProjede` defect again, on the card instead of in the body.
	if fake.tasks[1].Detail != "\nxcodebuild -scheme App" {
		t.Fatalf("progress detail = %q, want the message with a leading separator — a card's details field "+
			"appends, so a second detail fuses with the first without one", fake.tasks[1].Detail)
	}
	// The FIRST detail takes no separator: it has nothing in front of it to be separated from.
	if fake.tasks[0].Detail != "palai.workspace.shell" {
		t.Fatalf("the first detail = %q, want no leading separator", fake.tasks[0].Detail)
	}
}

// TestAnApprovalRequestReachesTheBridge is the wiring Task 12.5 found missing: approvals.go's
// OnApprovalRequested — the only thing that mints an approve/deny button — had NO caller anywhere in
// this process, so a gated tool call parked the run and nobody was ever asked. Run sees the event on the
// session stream and is the one place that also holds the thread it must be posted into.
func TestAnApprovalRequestReachesTheBridge(t *testing.T) {
	requested := palai.Event{Type: ApprovalRequestedEventType, SessionID: "sess_1",
		Data: map[string]any{"request_hash": "rh_1", "tool_name": "xcodebuild"}}
	events := []palai.Event{requested, {Type: "run.completed.v1", Data: map[string]any{}}}

	var gotChannel, gotThread string
	var gotEvents []palai.Event
	fake := &fakeSlack{}
	deps := Deps{Events: staticStream(events), Slack: fake,
		OnApproval: func(_ context.Context, channel, threadTS string, ev palai.Event) {
			gotChannel, gotThread = channel, threadTS
			gotEvents = append(gotEvents, ev)
		}}
	if err := Run(context.Background(), deps, "sess_1", "C1", "1.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(gotEvents) != 1 {
		t.Fatalf("the approval hook fired %d time(s), want exactly 1 — a parked run nobody is asked about is a hang", len(gotEvents))
	}
	if gotEvents[0].Data["request_hash"] != "rh_1" {
		t.Fatalf("the hook received %v, want the event carrying request_hash rh_1 (the ONE field both approval kinds share)", gotEvents[0].Data)
	}
	// The thread, not the stream's own ts: the approval message is a separate chat.postMessage into the
	// same thread, and posting it against the stream's ts would put it in the wrong place.
	if gotChannel != "C1" || gotThread != "1.1" {
		t.Fatalf("the hook was given channel=%q thread=%q, want C1/1.1 — nothing in a Palai event names a Slack thread", gotChannel, gotThread)
	}
	// The stream says why it went quiet. A run that parks with no word in the open message reads as a hang.
	if got := strings.Join(fake.appended, ""); !strings.Contains(got, "approval") {
		t.Fatalf("the open stream said %q; a parked run must say it is waiting on a person", got)
	}
}

func TestRunRefusesIncompleteDeps(t *testing.T) {
	fake := &fakeSlack{}
	if err := Run(context.Background(), Deps{Slack: fake, OnApproval: noApprovals}, "sess_1", "C1", "1.1"); err == nil {
		t.Fatal("Run with a nil Events succeeded")
	}
	// A nil hook is refused for the same reason, and it is the newer half: a relay that renders a run
	// but drops its approval requests looks entirely healthy right up to the first gated call.
	if err := Run(context.Background(), Deps{Events: staticStream(nil), Slack: fake}, "sess_1", "C1", "1.1"); err == nil {
		t.Fatal("Run with a nil OnApproval succeeded")
	}
	if fake.started != 0 {
		t.Fatalf("StartStream was called %d time(s) with incomplete Deps", fake.started)
	}
}
