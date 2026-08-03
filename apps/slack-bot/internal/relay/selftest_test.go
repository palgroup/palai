package relay

import (
	"context"
	"errors"
	"strings"
	"testing"

	slack "github.com/palgroup/palai/adapters/integrations/slack"
)

// The self-test's own tests (2026-08-03 plan, Task 13).
//
// THE FAKE KEEPS A MESSAGE AND THE STREAM CALLS WRITE INTO IT, rather than each call returning a canned
// answer. That is the whole reason these tests can say anything about leg 4: the question is what the
// workspace ENDS UP HOLDING after four calls, so a fake that answered the read-back directly would be
// asserting the assertion. Here the pieces travel start → append → append → stop → read, and
// fakeSelfTestStream.stopReplaces is the one switch that turns Slack's real behaviour into the one that loses text.

// fakeSelfTestSlack is relay.SelfTestSlack with a per-leg failure switch and a tiny message store.
type fakeSelfTestSlack struct {
	authErr   error
	socketErr error
	postErr   error
	readErr   error

	identity    slack.Identity
	connections int

	// messages is the workspace: ts → the text that message currently holds. fakeSelfTestStream writes here.
	messages map[string]string
	// nextTS mints message timestamps, so a stream's ts is never accidentally the parent's.
	nextTS int
	// calls records every call in order, so a test can assert the SEQUENCE and not only the outcome.
	calls []string
}

func newFakeSelfTestSlack() *fakeSelfTestSlack {
	return &fakeSelfTestSlack{
		identity:    slack.Identity{UserID: "U0BOT", User: "palai", TeamID: "T0WORK", Team: "Pallasite OU", URL: "https://pallasiteou.slack.com/"},
		connections: 1,
		messages:    map[string]string{},
	}
}

func (f *fakeSelfTestSlack) mint() string {
	f.nextTS++
	return "ts-" + string(rune('0'+f.nextTS))
}

func (f *fakeSelfTestSlack) AuthTest(context.Context) (slack.Identity, error) {
	f.calls = append(f.calls, "auth.test")
	if f.authErr != nil {
		return slack.Identity{}, f.authErr
	}
	return f.identity, nil
}

func (f *fakeSelfTestSlack) OpenSocket(context.Context) (int, error) {
	f.calls = append(f.calls, "socket")
	if f.socketErr != nil {
		return 0, f.socketErr
	}
	return f.connections, nil
}

func (f *fakeSelfTestSlack) PostMessage(_ context.Context, channel, text string) (string, error) {
	f.calls = append(f.calls, "post")
	if f.postErr != nil {
		return "", f.postErr
	}
	ts := f.mint()
	f.messages[ts] = text
	_ = channel
	return ts, nil
}

func (f *fakeSelfTestSlack) MessageText(_ context.Context, _, _, messageTS string) (string, error) {
	f.calls = append(f.calls, "read")
	if f.readErr != nil {
		return "", f.readErr
	}
	text, ok := f.messages[messageTS]
	if !ok {
		return "", errors.New("no such message")
	}
	return text, nil
}

// fakeSelfTestStream is the relay.Slack seam leg 4 drives, writing into the same message store.
type fakeSelfTestStream struct {
	parent *fakeSelfTestSlack
	// stopReplaces makes chat.stopStream REPLACE the message rather than append to it — the behaviour that
	// would make the relay silently lose text, and the one no test could reach before this file existed.
	stopReplaces bool

	startErr, appendErr, stopErr error

	recipientUserID, recipientTeamID string
	ts                               string
}

func (s *fakeSelfTestStream) StartStream(_ context.Context, _, threadTS, markdownText string) (string, error) {
	s.parent.calls = append(s.parent.calls, "startStream("+markdownText+")")
	if s.startErr != nil {
		return "", s.startErr
	}
	if threadTS == "" {
		return "", errors.New("missing_recipient_user_id")
	}
	s.ts = s.parent.mint()
	s.parent.messages[s.ts] = markdownText
	return s.ts, nil
}

func (s *fakeSelfTestStream) AppendStream(_ context.Context, _, ts, markdownText string) error {
	s.parent.calls = append(s.parent.calls, "appendStream("+markdownText+")")
	if s.appendErr != nil {
		return s.appendErr
	}
	s.parent.messages[ts] += markdownText
	return nil
}

func (s *fakeSelfTestStream) StopStream(_ context.Context, _, ts, markdownText string) error {
	s.parent.calls = append(s.parent.calls, "stopStream("+markdownText+")")
	if s.stopErr != nil {
		return s.stopErr
	}
	if s.stopReplaces {
		s.parent.messages[ts] = markdownText
		return nil
	}
	s.parent.messages[ts] += markdownText
	return nil
}

// newSelfTestDeps builds a self-test seam whose every leg passes, plus the stream the caller can reach to break
// one. The stream is returned rather than fetched back out of Deps because NewStream is a func value.
func newSelfTestDeps(t *testing.T) (SelfTestDeps, *fakeSelfTestSlack, *fakeSelfTestStream) {
	t.Helper()
	f := newFakeSelfTestSlack()
	stream := &fakeSelfTestStream{parent: f}
	return SelfTestDeps{
		Slack: f,
		NewStream: func(recipientUserID, recipientTeamID string) Slack {
			stream.recipientUserID = recipientUserID
			stream.recipientTeamID = recipientTeamID
			return stream
		},
		Channel: "C0TEST",
	}, f, stream
}

// TestSelfTestReportsWhichLegFailed is the plan's own test: a leg reports its own result, and it carries
// SLACK'S word for what went wrong rather than one this package invented.
func TestSelfTestReportsWhichLegFailed(t *testing.T) {
	deps, f, _ := newSelfTestDeps(t)
	f.authErr = errors.New("invalid_auth")

	r, err := SelfTest(context.Background(), deps)
	if err != nil {
		t.Fatalf("SelfTest returned %v; a leg failure is a report, not an error", err)
	}
	if r.AuthOK {
		t.Fatal("AuthOK true with an invalid token")
	}
	if !strings.Contains(r.Detail, "invalid_auth") {
		t.Fatalf("detail %q does not carry Slack's own message", r.Detail)
	}
	if !strings.Contains(r.Detail, "leg 1") {
		t.Fatalf("detail %q does not name WHICH leg failed", r.Detail)
	}
}

// TestSelfTestStopsAtTheFirstFailedLeg: a token that cannot authenticate must not go on to post into
// somebody's channel. The stop is asserted on the CALLS, because a false SocketOK would also be produced by
// a leg that ran and failed.
func TestSelfTestStopsAtTheFirstFailedLeg(t *testing.T) {
	deps, f, _ := newSelfTestDeps(t)
	f.authErr = &slack.APIError{Code: "invalid_auth"}

	r, _ := SelfTest(context.Background(), deps)
	if r.SocketOK || r.PostOK || r.StreamOK {
		t.Fatalf("a later leg reported a result after leg 1 failed: %+v", r)
	}
	if len(f.calls) != 1 || f.calls[0] != "auth.test" {
		t.Fatalf("calls after a failed auth leg = %v; nothing may reach the workspace after it", f.calls)
	}
	if r.OK() {
		t.Fatal("OK() true on a failed report")
	}
}

// TestSelfTestNamesEachLegByNumber walks every leg's own failure. Each arm asserts BOTH that the leg's
// number is named and that the earlier legs stayed green — a report that collapsed to "it failed" is the
// thing this whole shape exists to prevent.
func TestSelfTestNamesEachLegByNumber(t *testing.T) {
	for _, tc := range []struct {
		name     string
		leg      string
		code     string
		break_   func(*fakeSelfTestSlack, *fakeSelfTestStream)
		wantPrev func(Report) bool
	}{
		{
			name: "socket", leg: "leg 2", code: "not_allowed_token_type",
			break_:   func(f *fakeSelfTestSlack, _ *fakeSelfTestStream) { f.socketErr = &slack.APIError{Code: "not_allowed_token_type"} },
			wantPrev: func(r Report) bool { return r.AuthOK && !r.SocketOK },
		},
		{
			name: "post", leg: "leg 3", code: "not_in_channel",
			break_:   func(f *fakeSelfTestSlack, _ *fakeSelfTestStream) { f.postErr = &slack.APIError{Code: "not_in_channel"} },
			wantPrev: func(r Report) bool { return r.AuthOK && r.SocketOK && !r.PostOK },
		},
		{
			name: "startStream", leg: "leg 4", code: "missing_scope",
			break_:   func(_ *fakeSelfTestSlack, s *fakeSelfTestStream) { s.startErr = &slack.APIError{Code: "missing_scope"} },
			wantPrev: func(r Report) bool { return r.AuthOK && r.SocketOK && r.PostOK && !r.StreamOK },
		},
		{
			name: "read-back", leg: "leg 4", code: "channel_not_found",
			break_:   func(f *fakeSelfTestSlack, _ *fakeSelfTestStream) { f.readErr = &slack.APIError{Code: "channel_not_found"} },
			wantPrev: func(r Report) bool { return r.AuthOK && r.SocketOK && r.PostOK && !r.StreamOK },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, f, stream := newSelfTestDeps(t)
			tc.break_(f, stream)

			r, err := SelfTest(context.Background(), deps)
			if err != nil {
				t.Fatalf("SelfTest returned %v", err)
			}
			if !tc.wantPrev(r) {
				t.Fatalf("report %+v does not show the earlier legs passing and this one failing", r)
			}
			if !strings.Contains(r.Detail, tc.leg) {
				t.Fatalf("detail %q does not name %s", r.Detail, tc.leg)
			}
			if !strings.Contains(r.Detail, tc.code) {
				t.Fatalf("detail %q does not carry Slack's own code %q", r.Detail, tc.code)
			}
		})
	}
}

// TestSelfTestDrivesTheStreamSequence pins the exact four calls the measurement depends on. If this
// sequence drifts — one append instead of two, the pieces sent in another order — the observation below
// stops being about what it claims to be about, and nothing else would notice.
func TestSelfTestDrivesTheStreamSequence(t *testing.T) {
	deps, f, stream := newSelfTestDeps(t)

	r, err := SelfTest(context.Background(), deps)
	if err != nil {
		t.Fatalf("SelfTest returned %v", err)
	}
	if !r.OK() {
		t.Fatalf("a fully working workspace did not pass every leg: %+v", r)
	}
	want := []string{
		"auth.test", "socket", "post",
		"startStream(" + selfTestOpening + ")",
		"appendStream(" + selfTestPieceOne + ")",
		"appendStream(" + selfTestPieceTwo + ")",
		"stopStream(" + selfTestPieceClose + ")",
		"read",
	}
	if strings.Join(f.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("call sequence\n got %v\nwant %v", f.calls, want)
	}
	// chat.startStream refuses without a recipient (slack.ErrNoStreamRecipient), and a self-test has no
	// human to be one — so it must be the APP, which only auth.test can name.
	if stream.recipientUserID != f.identity.UserID || stream.recipientTeamID != f.identity.TeamID {
		t.Fatalf("the stream's recipient is (%q,%q); it must come from leg 1's identity (%q,%q)",
			stream.recipientUserID, stream.recipientTeamID, f.identity.UserID, f.identity.TeamID)
	}
}

// TestSelfTestObservesAppendSemantics is the measurement's green side: against a workspace that appends,
// the verdict is `appends` and the message holds every piece.
func TestSelfTestObservesAppendSemantics(t *testing.T) {
	deps, _, _ := newSelfTestDeps(t)

	r, _ := SelfTest(context.Background(), deps)
	if r.StopStream != StopStreamAppends {
		t.Fatalf("StopStream = %q against an appending workspace, want %q", r.StopStream, StopStreamAppends)
	}
	want := selfTestOpening + selfTestPieceOne + selfTestPieceTwo + selfTestPieceClose
	if r.Streamed != want {
		t.Fatalf("Streamed = %q, want %q", r.Streamed, want)
	}
	if !strings.Contains(r.Detail, "appends") {
		t.Fatalf("a passing report %q does not say what was observed", r.Detail)
	}
}

// TestSelfTestUnprovenNamesTheRunPipeline pins the sentence a passing report is read alongside.
//
// The risk this guards is specific: an operator sees four green legs and concludes their bot works. It
// does not follow, and nothing in the four legs touches the pipeline that would make it follow — so the
// limit has to name that pipeline in words an operator can act on, not merely say "this is not a full
// test". A caveat that only subtracts reads as a disclaimer and gets skipped, which is why the constant
// states what the legs DO settle first.
func TestSelfTestUnprovenNamesTheRunPipeline(t *testing.T) {
	for _, want := range []string{"relay process is running", "@mention", "run answers", "agent_revision_id"} {
		if !strings.Contains(SelfTestUnproven, want) {
			t.Errorf("SelfTestUnproven does not name %q, so a passing run leaves it for the operator to infer:\n%s", want, SelfTestUnproven)
		}
	}
	// AND IT MUST NOT BE REACHABLE AS A CLAIM ABOUT THE LEGS THEMSELVES: the legs passed. A sentence that
	// said the credentials were unproven would be false and would send somebody back to Step 2.
	if strings.Contains(SelfTestUnproven, "the token") || strings.Contains(SelfTestUnproven, "the channel is") {
		t.Errorf("SelfTestUnproven casts doubt on a leg that PASSED:\n%s", SelfTestUnproven)
	}
}

// TestSelfTestCatchesReplaceSemantics IS THE REASON LEG 4 EXISTS.
//
// A workspace whose chat.stopStream replaces rather than appends erases text that earlier appends already
// delivered — the relay flushes only UNCONFIRMED residue as the closing markdown_text (openStream.pending,
// relay.go), so the loss is silent: no duplicate, no error, just a shorter message than the run produced.
// This test drives exactly that workspace and requires the leg to FAIL and to name it.
func TestSelfTestCatchesReplaceSemantics(t *testing.T) {
	deps, _, stream := newSelfTestDeps(t)
	stream.stopReplaces = true

	r, err := SelfTest(context.Background(), deps)
	if err != nil {
		t.Fatalf("SelfTest returned %v", err)
	}
	if r.StreamOK {
		t.Fatal("leg 4 passed against a workspace that erases everything streamed before the close")
	}
	if r.StopStream != StopStreamReplaces {
		t.Fatalf("StopStream = %q, want %q — the verdict is the thing this leg exists to record", r.StopStream, StopStreamReplaces)
	}
	if !strings.Contains(r.Detail, "leg 4") {
		t.Fatalf("detail %q does not name the leg", r.Detail)
	}
	// The three legs before it are untouched: the credentials are fine, the behaviour is not.
	if !r.AuthOK || !r.SocketOK || !r.PostOK {
		t.Fatalf("report %+v blamed an earlier leg for a stopStream behaviour", r)
	}
}

// TestClassifyStopStreamIsNotDefeatedBySubstring guards the comparison itself.
//
// This tree records defeated path/membership comparisons as a repeating defect, and the quiet version of it
// is here: the closing piece is CONTAINED in the appended result, so `strings.Contains(final, close)` is
// true under both semantics — a substring test would report `appends` on precisely the workspace that is
// losing text, which is worse than not testing at all.
func TestClassifyStopStreamIsNotDefeatedBySubstring(t *testing.T) {
	appended := selfTestOpening + selfTestPieceOne + selfTestPieceTwo + selfTestPieceClose
	if !strings.Contains(appended, selfTestPieceClose) {
		t.Fatal("the premise of this guard is gone: the closing piece is no longer a substring of the appended result")
	}
	for _, tc := range []struct {
		final string
		want  StopStreamSemantics
	}{
		{appended, StopStreamAppends},
		{selfTestPieceClose, StopStreamReplaces},
		// Trailing/leading whitespace is Slack's own trim on read-back, not a different outcome.
		{"  " + appended + "\n", StopStreamAppends},
		// Anything else is its OWN answer and must not be folded into either: "Slack did something third"
		// is exactly as design-relevant as replace.
		{selfTestOpening + selfTestPieceClose, StopStreamUnrecognised},
		{"", StopStreamUnrecognised},
	} {
		if got := classifyStopStream(tc.final); got != tc.want {
			t.Errorf("classifyStopStream(%q) = %q, want %q", tc.final, got, tc.want)
		}
	}
}

// TestSelfTestRefusesAnIncompleteSeam: the error return is for a caller's bug and nothing else, and an
// empty channel is the one worth a name — a self-test that defaulted would post into somebody's workspace
// at a place nobody chose.
func TestSelfTestRefusesAnIncompleteSeam(t *testing.T) {
	full, _, _ := newSelfTestDeps(t)
	for _, tc := range []struct {
		name string
		deps SelfTestDeps
		want string
	}{
		{"no slack", SelfTestDeps{NewStream: full.NewStream, Channel: "C0TEST"}, "needs a Slack"},
		{"no stream", SelfTestDeps{Slack: full.Slack, Channel: "C0TEST"}, "needs NewStream"},
		{"no channel", SelfTestDeps{Slack: full.Slack, NewStream: full.NewStream}, "needs a Channel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := SelfTest(context.Background(), tc.deps)
			if err == nil {
				t.Fatal("an incomplete seam was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v does not say %q", err, tc.want)
			}
			if r.OK() {
				t.Fatal("a refused self-test reported a passing run")
			}
		})
	}
}
