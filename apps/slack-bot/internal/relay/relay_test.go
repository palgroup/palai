package relay

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

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

	failAppends int
	// failStop makes the closing chat.stopStream refuse — the ONE Slack failure Run reports rather than
	// absorbs, since there is no later call to carry the text (see openStream.pending).
	failStop bool
}

func (f *fakeSlack) StartStream(ctx context.Context, channel, threadTS, markdownText string) (string, error) {
	f.started++
	return "1.1", nil
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

// TestToolCallEventsRenderReadableProgress is requirement 2: tool_call.* events must become progress a
// human can read, not a bare event dump — the owner flagged UI quality as a priority.
func TestToolCallEventsRenderReadableProgress(t *testing.T) {
	events := []palai.Event{
		{Type: "tool_call.executing.v1", Data: map[string]any{
			"tool_call_id": "tc_1", "tool_name": "xcodebuild", "replay_class": "irreversible",
		}},
		{Type: "tool_call.progress.v1", Data: map[string]any{
			"tool_call_id": "tc_1", "progress": float64(1), "total": float64(4), "message": "compiling AppDelegate.swift",
		}},
		{Type: "tool_call.completed.v1", Data: map[string]any{"tool_call_id": "tc_1", "tool_name": "xcodebuild"}},
		{Type: "run.completed.v1", Data: map[string]any{}},
	}
	fake := &fakeSlack{}
	if err := Run(context.Background(), Deps{Events: staticStream(events), Slack: fake, OnApproval: noApprovals},
		"sess_1", "C1", "1.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := strings.Join(fake.appended, "")
	for _, want := range []string{"xcodebuild", "compiling AppDelegate.swift", "1/4", "done"} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered progress %q does not mention %q — a bare event dump is not acceptable", got, want)
		}
	}
}

// TestToolCallProgressWithNoMessageIsSkipped: an MCP progress notification is advisory and often carries
// no human-readable message (packages/coordinator/mcp_progress.go AppendToolProgress) — that must not
// render a bare, content-free line into the thread.
func TestToolCallProgressWithNoMessageIsSkipped(t *testing.T) {
	events := []palai.Event{
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
