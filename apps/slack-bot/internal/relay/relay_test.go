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

	// stopAfter makes AppendStream answer Slack's `stopped_by_user` from the Nth call onward, which is what
	// the human pressing stop on the streaming card actually produces. It is a THRESHOLD rather than a
	// one-shot flag because the refusal is permanent: a double that refused once and then accepted again
	// would let a relay that keeps writing into a stopped stream pass.
	stopAfter int
	// stopCards makes UpdateTask refuse the same way, so a test can reproduce the ordering where a CARD is
	// the first call the stop refuses (a run drawing cards between deltas).
	stopCards bool
	// stopOnClose makes ONLY the closing chat.stopStream refuse, which is what a stop pressed after the last
	// append actually produces — no later append exists to discover it.
	stopOnClose bool
	// appendErr is an arbitrary AppendStream failure, used to prove a NEIGHBOURING Slack refusal is not read
	// as the stop button.
	appendErr error
	// posted are the plain chat.postMessage bodies — the path an answer takes once the stream is stopped.
	// Kept apart from appended and tasks for the same reason those two are apart from each other: a test
	// asserting the answer reached the human must not be able to read a stream append and call it a message.
	posted []string
	// failPost makes that last path fail too, which is the one loss this relay still has no answer for and
	// therefore must REPORT.
	failPost bool
}

func (f *fakeSlack) StartStream(ctx context.Context, channel, threadTS, markdownText string) (string, error) {
	f.started++
	f.startedText = markdownText
	return "1.1", nil
}

func (f *fakeSlack) UpdateTask(ctx context.Context, channel, ts string, task slack.Task) error {
	if f.stopCards {
		return &slack.APIError{Code: slack.CodeStoppedByUser}
	}
	f.tasks = append(f.tasks, task)
	return nil
}

func (f *fakeSlack) AppendStream(ctx context.Context, channel, ts, markdownText string) error {
	if f.stopAfter > 0 && len(f.appended)+1 >= f.stopAfter {
		return &slack.APIError{Code: slack.CodeStoppedByUser}
	}
	if f.appendErr != nil {
		return f.appendErr
	}
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
	if f.stopOnClose || f.stopAfter > 0 || f.stopCards {
		// Slack refuses the CLOSE on a stopped stream too, not only the appends — the whole reason the
		// answer needs another path. It is derived from the same knobs rather than opted into separately
		// because a double whose close SUCCEEDED on a stopped stream would be a fiction, and code written
		// against it would leave the answer inside a call Slack never accepted.
		return &slack.APIError{Code: slack.CodeStoppedByUser}
	}
	if f.failStop {
		return errors.New("slack: stop failed")
	}
	return nil
}

func (f *fakeSlack) PostMessage(ctx context.Context, channel, threadTS, markdownText string) error {
	if f.failPost {
		return errors.New("slack: chat.postMessage failed")
	}
	f.posted = append(f.posted, markdownText)
	return nil
}

// nonThinkingTasks strips the "palai.thinking" card's own updates out of a recorded sequence, isolating
// what most of the tests below actually assert: the TOOL card's shape. Run draws that card unconditionally —
// in_progress the instant it starts, resolved by whatever ends the run — regardless of what the fixture
// under test is about, so a raw len(fake.tasks) count written before that card existed now over-counts by
// exactly two (one in_progress, one terminal) on every fixture that runs to a terminal event. The thinking
// card's OWN behaviour — that it opens eagerly, tracks model steps, dedupes repeats, and resolves on every
// run terminal — is pinned separately by the TestThinkingCard* tests below, not by narrowing here.
func nonThinkingTasks(tasks []slack.Task) []slack.Task {
	var out []slack.Task
	for _, task := range tasks {
		if task.ID == thinkingTaskID {
			continue
		}
		out = append(out, task)
	}
	return out
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

// TestTheShapeARealRunActuallyProduces is the fixture that matches PRODUCTION, and it exists because every
// other card test in this file feeds a tool_call.progress.v1 that a real run never sends.
//
// MEASURED 2026-08-04, whole journal, all time:
//
//	SELECT count(*) FROM events WHERE type='tool_call.progress.v1'  ->  0
//
// and that zero is STRUCTURAL, not a quiet week: the event's only writer is the MCP progress sink
// (apps/control-plane/internal/execution/mcp_progress.go, journalling a remote server's tools/call
// notifications), while palai.workspace.* are dispatched by the control plane itself and emit none. So a
// real run's card is executing -> completed with NOTHING in between, and this test is what that looks like.
//
// Confirmed end to end by the live leg (realrun_live_test.go, run 2026-08-04): one real tool call, two
// updates sharing tcall_15621b7abb4be6ab84654e78, and the workspace kept
// `status=complete title="Running a command" details=<nil>`.
func TestTheShapeARealRunActuallyProduces(t *testing.T) {
	const answer = "Bu projede toplam 151 adet Swift dosyası var."
	events := []palai.Event{
		{Type: "tool_call.executing.v1", Data: map[string]any{"tool_call_id": "tcall_1", "tool_name": "palai.workspace.shell"}},
		{Type: "tool_call.completed.v1", Data: map[string]any{"tool_call_id": "tcall_1", "tool_name": "palai.workspace.shell"}},
		{Type: "model_step.delta.v1", Data: map[string]any{"text": answer}},
		{Type: "run.completed.v1", Data: map[string]any{}},
	}
	fake := &fakeSlack{}
	if err := Run(context.Background(), Deps{Events: staticStream(events), Slack: fake, OnApproval: noApprovals},
		"sess_1", "C1", "1.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	tasks := nonThinkingTasks(fake.tasks)
	if len(tasks) != 2 {
		t.Fatalf("drew %d card update(s), want 2 (executing, completed) — got %+v", len(tasks), tasks)
	}
	// ONE card, resolved, and its whole content is a human phrase. No detail line at all: nothing in a real
	// run has anything to put there, and the raw name that used to fill it was the owner's complaint.
	for i, task := range tasks {
		if task.ID != "tcall_1" || task.Title != "Running a command" || task.Detail != "" {
			t.Fatalf("update %d = %+v, want one card titled `Running a command` with no detail", i, task)
		}
	}
	if tasks[1].Status != "done" {
		t.Fatalf("the card ended %q, want done — a step left mid-flight in a finished message is the SLK-P2 "+
			"shape on a card", tasks[1].Status)
	}
	body := fake.startedText + strings.Join(fake.appended, "") + fake.stoppedText
	if !strings.Contains(body, answer) || strings.Contains(body, "palai.workspace.shell") {
		t.Fatalf("body = %q, want the answer and nothing about the tool", body)
	}
}

// TestToolProgressLeavesTheAnswerAlone is SLK-P6 closed, stated as the property the gap row states:
// "progress lines still ride the message body" — chat.appendStream's markdown_text IS the body, so a tool
// line the relay streamed sat ABOVE the model's answer in the finished message.
//
// The assertion has two halves and BOTH are needed. That the cards exist is not enough (the old prose could
// still be there beside them), and that the body is clean is not enough either (a relay that dropped tool
// events entirely would pass it). So: the steps are drawn as cards, AND nothing about them is in the body.
//
// THE FIXTURE IS RICHER THAN PRODUCTION AND THAT IS DELIBERATE HERE, but it must not be read as a picture of
// a real run: the tool_call.progress.v1 below is journalled ZERO times in this deployment and cannot be
// journalled for a built-in tool at all (see TestTheShapeARealRunActuallyProduces for the measurement and
// for the shape production really produces). It is included because SLK-P6 is about progress lines
// specifically, so the strongest form of "progress does not ride the body" needs a progress event to exist.
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

	tasks := nonThinkingTasks(fake.tasks)
	if len(tasks) != 3 {
		t.Fatalf("drew %d task card update(s), want 3 (executing, progress, completed) — got %+v", len(tasks), tasks)
	}
	body := fake.startedText + strings.Join(fake.appended, "") + fake.stoppedText
	// SCOPE, because the name of this list once over-claimed: these tokens must be absent FROM THE MESSAGE
	// BODY. That is not the same as absent from the reader's screen — a card is on screen too, and this
	// loop says nothing about it. The card's own content is asserted separately below, which is the
	// distinction the owner's "is the raw tool name supposed to be there?" question turned on: this test was
	// green the whole time the name was visible, and it was right to be, because it is a test about prose.
	for _, inBody := range []string{"palai.workspace.file", "Reading files", "reading SwiftUIListApp.swift", "1/4", "▶️", "✅"} {
		if strings.Contains(body, inBody) {
			t.Fatalf("the message body still carries %q — progress must ride the task timeline, not the "+
				"answer (SLK-P6). Body was %q", inBody, body)
		}
	}
	if !strings.Contains(body, answer) {
		t.Fatalf("the body is %q and no longer contains the answer — the model's own words must be the one "+
			"thing that DOES stay in it", body)
	}
	// AND THE CARD ITSELF says the step in human words, with nothing repeating it in machine words. A tool
	// this tree has a phrase for must not ALSO show its registered name: the owner read that as noise, and
	// no assertion above this one can see it.
	if tasks[0].Title != "Reading files" {
		t.Fatalf("card title = %q, want the human phrase", tasks[0].Title)
	}
	for _, task := range tasks {
		if strings.Contains(task.Title+task.Detail, "palai.workspace.file") {
			t.Fatalf("a card carried the raw tool name (%+v) though `Reading files` already said it — the "+
				"name belongs on a card only as toolTitle's fallback for a tool with no phrase", task)
		}
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
	// SCOPED TO THIS CALL'S OWN CARD: Run also draws a SEPARATE "palai.thinking" card (a different id, by
	// design — see thinkingTaskID) for the model's own working time, which this fixture's model_step-less
	// events never touch directly but which still opens at Run's start and resolves at the run terminal.
	// Mixing it in here would be asserting the wrong claim: this test is that ONE tool call's updates never
	// drift onto a second id, not that the whole stream draws only one id ever.
	tasks := nonThinkingTasks(fake.tasks)
	for _, task := range tasks {
		if task.ID != "tc_1" {
			t.Fatalf("a card carried id %q, want tc_1 for every update of one call — a differing id draws a "+
				"SECOND card and the step is rendered twice", task.ID)
		}
	}
	// The statuses must PROGRESS, and the last one must not be in_progress: a step whose card never leaves
	// in_progress is a finished message that still shows the run working (the SLK-P2 shape, on a card).
	var got []string
	for _, task := range tasks {
		got = append(got, task.Status)
	}
	if want := []string{"in_progress", "in_progress", "done"}; !slices.Equal(got, want) {
		t.Fatalf("card statuses were %v, want %v", got, want)
	}
	// The title is what a human reads; the raw name stays available underneath rather than being discarded.
	if tasks[0].Title != "Running a command" {
		t.Fatalf("card title = %q, want a human phrase — `palai.workspace.shell` is jargon", tasks[0].Title)
	}
	// NO LIFECYCLE EVENT PUTS A DETAIL ON THE CARD. The opening update once wrote the raw tool name there
	// and the owner read it as noise twice: `Running a command` with `palai.workspace.shell` under it says
	// the same thing in machine words, and on a tool that reports no progress that was the card's only
	// line. Only progress writes a detail now — see tasks[1] below.
	if tasks[0].Detail != "" || tasks[2].Detail != "" {
		t.Fatalf("a lifecycle update carried a detail (open %q, terminal %q); the title already says what "+
			"the step is, and the raw name survives as toolTitle's fallback for unmapped tools",
			tasks[0].Detail, tasks[2].Detail)
	}
	if tasks[1].Detail != "xcodebuild" {
		t.Fatalf("the progress update's detail = %q, want the notification's message — progress is the one "+
			"thing that has something to say under the title", tasks[1].Detail)
	}
}

// TestConsecutiveProgressLinesAreSeparated is the LAST reachable home for the appending-details rule, and it
// is written as its own test because the fixtures that used to cover it no longer can: a card's `details`
// APPENDS (measured against the live workspace), and now that no lifecycle event writes one, the only way
// two details ever meet on one card is two progress notifications.
//
// Without the separator the live workspace returned `palai.workspace.fileSwiftUIListApp.swift (3/7)` — the
// `doneProjede` defect, on the card instead of in the body.
//
// REACHABLE ONLY BY AN MCP TOOL, and that ceiling belongs on the test rather than in a report nobody reads:
// tool_call.progress.v1 has one writer (the MCP progress sink) and is journalled zero times in this
// deployment, so no built-in palai.workspace.* tool can ever reach this code. The rule is still real — an
// MCP server that reports progress twice would hit it — but this test is NOT evidence about what a Slack
// answer looks like today. TestTheShapeARealRunActuallyProduces is.
func TestConsecutiveProgressLinesAreSeparated(t *testing.T) {
	events := []palai.Event{
		{Type: "tool_call.executing.v1", Data: map[string]any{"tool_call_id": "tc_1", "tool_name": "palai.workspace.file"}},
		{Type: "tool_call.progress.v1", Data: map[string]any{"tool_call_id": "tc_1", "message": "SwiftUIListApp.swift"}},
		{Type: "tool_call.progress.v1", Data: map[string]any{"tool_call_id": "tc_1", "message": "ContentView.swift"}},
		{Type: "run.completed.v1", Data: map[string]any{}},
	}
	fake := &fakeSlack{}
	if err := Run(context.Background(), Deps{Events: staticStream(events), Slack: fake, OnApproval: noApprovals},
		"sess_1", "C1", "1.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	tasks := nonThinkingTasks(fake.tasks)
	if len(tasks) != 3 {
		t.Fatalf("drew %d card update(s), want 3", len(tasks))
	}
	// The FIRST detail has nothing in front of it, so it takes no separator; the second does.
	if tasks[1].Detail != "SwiftUIListApp.swift" {
		t.Fatalf("first progress detail = %q, want no leading separator", tasks[1].Detail)
	}
	if tasks[2].Detail != "\nContentView.swift" {
		t.Fatalf("second progress detail = %q, want a leading separator — `details` appends, so without one "+
			"the two fuse into `SwiftUIListApp.swiftContentView.swift`", tasks[2].Detail)
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

// TestAnUnmappedToolStillShowsItsName is the half that makes dropping the raw name from the detail LOSSLESS
// rather than merely quieter, and it is the reason that change was safe: a tool this tree has no phrase for
// — every MCP server contributes them — is titled with its own registered name. So the name disappears
// exactly where a human sentence already said the same thing, and stays exactly where it is the most
// informative thing the card can show.
func TestAnUnmappedToolStillShowsItsName(t *testing.T) {
	events := []palai.Event{
		{Type: "tool_call.executing.v1", Data: map[string]any{"tool_call_id": "tc_1", "tool_name": "acme.jira.create_ticket"}},
		{Type: "run.completed.v1", Data: map[string]any{}},
	}
	fake := &fakeSlack{}
	if err := Run(context.Background(), Deps{Events: staticStream(events), Slack: fake, OnApproval: noApprovals},
		"sess_1", "C1", "1.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	tasks := nonThinkingTasks(fake.tasks)
	if len(tasks) != 1 || tasks[0].Title != "acme.jira.create_ticket" {
		t.Fatalf("cards = %+v, want one titled with the unmapped tool's own name — a card that showed "+
			"neither a phrase nor the name would say nothing at all", tasks)
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
	// SCOPED to non-thinking cards: Run's own "palai.thinking" card still opens and resolves regardless of
	// this event, since it tracks the run's wall clock, not this tool call's identifiability. What this
	// test pins is that the UNIDENTIFIABLE tool_call.executing.v1 itself draws nothing.
	if tasks := nonThinkingTasks(fake.tasks); len(tasks) != 0 {
		t.Fatalf("drew %+v for an event with no tool_call_id, want no card", tasks)
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
//
// "THE OPENING STATUS" LEG IS GONE, and that is a removal, not a rename: this table used to carry a case
// with nothing in `before`, on the theory that the body still opened with chat.startStream's own
// markdown_text ("Working…\n"), which never passed through emitStatus and so fused to the first delta by a
// path this test's separator check did not cover. Run no longer sends that text AT ALL — StartStream now
// opens with "" (relay.go's Run, and see adapters/integrations/slack's TestAnEmptyOpeningSendsNoChunkAtAll
// for the wire-level proof that an empty opening carries no chunk and no markdown_text either) — so with
// nothing in `before` the body is JUST the answer: idx would be 0, and `body[idx-1]` would be reading
// before the start of the string. Keeping the case and loosening the guard to tolerate idx==0 would stop
// testing a separator at all, since there is no longer a first half for it to separate. The defect this
// leg guarded against — a leftover status word gluing to the model's own text — cannot recur on this path
// because there is no longer a status word on it; the property is enforced structurally now, not by a test.
// The approval leg below still exercises a REAL status line (emitStatus's guaranteed trailing newline) and
// stays for exactly that reason.
func TestAStatusLineIsNeverGluedToTheTextAfterIt(t *testing.T) {
	const answer = "Projede toplam 7 Swift dosyası var"
	for _, tc := range []struct {
		name   string
		before []palai.Event
	}{
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
	tasks := nonThinkingTasks(fake.tasks)
	if len(tasks) != 1 {
		t.Fatalf("drew %d card update(s), want 1 — a messageless notification must not re-draw the card "+
			"with an empty detail, which would erase what the step said it was doing: %+v", len(tasks), tasks)
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
	tasks := nonThinkingTasks(fake.tasks)
	if len(tasks) != 2 {
		t.Fatalf("drew %d card update(s), want 2", len(tasks))
	}
	if tasks[1].Title != "Running a command" {
		t.Fatalf("the progress update retitled the step to %q — a card must keep the title it was drawn "+
			"with when the event carries no tool_name", tasks[1].Title)
	}
	// This progress line is the card's FIRST detail (the opening update writes none), so it takes no
	// separator. Two progress lines meeting on one card is TestConsecutiveProgressLinesAreSeparated.
	if tasks[1].Detail != "xcodebuild -scheme App" {
		t.Fatalf("progress detail = %q, want the notification's message", tasks[1].Detail)
	}
}

// thinkingTasks is nonThinkingTasks' complement: the "palai.thinking" card's own updates, in the order Run
// sent them. The tests below are the ones this file was missing before this change — every test above
// scopes IT out to isolate a tool card's own claim, and nothing pinned the card's own mechanism anywhere.
func thinkingTasks(tasks []slack.Task) []slack.Task {
	var out []slack.Task
	for _, task := range tasks {
		if task.ID == thinkingTaskID {
			out = append(out, task)
		}
	}
	return out
}

// TestThinkingCardOpensImmediatelyAndTracksModelSteps pins the two halves of Run's own doc comment ("THE
// MESSAGE OPENS WITH AN EMPTY BODY AND A LIVE CARD") that no test measured before this one: the card is
// in_progress before this package has read a single event — proven here by a fixture whose FIRST event is
// already mid-run (a repeated model_step.created.v1, never itself producing a fresh send) — and it toggles
// once per model_step.created/completed pair, across TWO steps.
//
// THE SECOND STEP IS NOT DECORATION: with only one step, model_step.completed.v1 and the trailing
// run.completed.v1 both map to "done", so a version of this test ending after one step cannot tell "the step
// resolved the card" apart from "the step did nothing and the run terminal fixed it anyway" — the two bugs
// produce the SAME final sequence. The second created/completed pair forces the card back to in_progress
// and then done AGAIN before the run ends, so if model_step.completed stopped being mapped, the sequence
// would collapse from four entries to two (see the perturbation this test was checked against, noted in the
// PR/commit that added it) rather than merely reading the same by coincidence.
//
// THE REPEATED "created" AT THE START IS THE DEDUPE CASE: modelStepStatus's own doc says a real run never
// sends two in a row with no "completed" between them, but setThinking's guard against resending an
// unchanged status is real code, not a hypothetical. Without the guard this fixture would send in_progress
// an extra time for each repeat instead of collapsing them.
func TestThinkingCardOpensImmediatelyAndTracksModelSteps(t *testing.T) {
	events := []palai.Event{
		{Type: "model_step.created.v1", Data: map[string]any{}},
		{Type: "model_step.created.v1", Data: map[string]any{}}, // dedupe: already in_progress, no resend
		{Type: "tool_call.executing.v1", Data: map[string]any{"tool_call_id": "tc_1", "tool_name": "palai.workspace.shell"}},
		{Type: "tool_call.completed.v1", Data: map[string]any{"tool_call_id": "tc_1", "tool_name": "palai.workspace.shell"}},
		{Type: "model_step.completed.v1", Data: map[string]any{}}, // step 1 ends: in_progress -> done
		{Type: "model_step.created.v1", Data: map[string]any{}},   // step 2 begins: done -> in_progress
		{Type: "model_step.completed.v1", Data: map[string]any{}}, // step 2 ends: in_progress -> done
		{Type: "run.completed.v1", Data: map[string]any{}},        // dedupe: the step already left the card "done"
	}
	fake := &fakeSlack{}
	if err := Run(context.Background(), Deps{Events: staticStream(events), Slack: fake, OnApproval: noApprovals},
		"sess_1", "C1", "1.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	thinking := thinkingTasks(fake.tasks)
	var got []string
	for _, task := range thinking {
		got = append(got, task.Status)
	}
	if want := []string{"in_progress", "done", "in_progress", "done"}; !slices.Equal(got, want) {
		t.Fatalf("thinking card statuses were %v, want %v — either a created/completed pair failed to toggle "+
			"the card, or a repeat sent a status the dedupe guard should have skipped", got, want)
	}
	for _, task := range thinking {
		if task.Title != thinkingTitle {
			t.Fatalf("thinking card title = %q, want the fixed headline %q — it must not be rewritten per "+
				"state (see thinkingTitle's own doc)", task.Title, thinkingTitle)
		}
		if task.Detail != "" {
			t.Fatalf("thinking card carried a detail %q — nothing in this product journals the model's "+
				"reasoning content, only the interval (see the doc above modelStepStatus)", task.Detail)
		}
	}
	// The tool call's own card is untouched by any of this: two different ids, tracked independently.
	if toolTasks := nonThinkingTasks(fake.tasks); len(toolTasks) != 2 || toolTasks[0].ID != "tc_1" {
		t.Fatalf("the tool call's card = %+v, want its usual executing/completed pair, unaffected by the "+
			"thinking card sharing the same message", toolTasks)
	}
}

// TestThinkingCardResolvesOnEveryRunTerminal walks runTerminalThinking's OWN table rather than a copy typed
// here, for the same reason TestEveryToolTerminalResolvesItsCard walks the tool-call state machine's table:
// a terminal type added to the map without also being exercised here would ship untested, and a hardcoded
// list here could silently drift from what Run actually consults.
//
// EVERY FIXTURE HAS NO MODEL STEP AT ALL — the exact shape runTerminalThinking's own doc names as the reason
// a run terminal must be able to resolve this card unassisted: a provisioning failure or a cancel while
// queued never emits a model_step.* event, so if the terminal itself did not resolve the card nothing ever
// would, and Slack stores an in_progress card left open at chat.stopStream as an ERROR (measured 2026-08-04),
// inventing a failure a run that simply had no steps never had.
func TestThinkingCardResolvesOnEveryRunTerminal(t *testing.T) {
	for term, want := range runTerminalThinking {
		t.Run(term, func(t *testing.T) {
			fake := &fakeSlack{}
			events := []palai.Event{{Type: term, Data: map[string]any{}}}
			if err := Run(context.Background(), Deps{Events: staticStream(events), Slack: fake, OnApproval: noApprovals},
				"sess_1", "C1", "1.1"); err != nil {
				t.Fatalf("Run: %v", err)
			}
			thinking := thinkingTasks(fake.tasks)
			var got []string
			for _, task := range thinking {
				got = append(got, task.Status)
			}
			if wantSeq := []string{"in_progress", want}; !slices.Equal(got, wantSeq) {
				t.Fatalf("%s left the thinking card at %v, want %v", term, got, wantSeq)
			}
			// The mapped status must also survive blocks.go's own vocabulary check: an unmapped status
			// fails closed to "pending" there, which on a FINISHED run is the spinning-card bug again.
			if slack.TaskStatus(want) == "pending" {
				t.Fatalf("%s maps the thinking card to %q, which blocks.go renders as pending — a finished "+
					"run must not leave its card looking not-yet-started", term, want)
			}
		})
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
