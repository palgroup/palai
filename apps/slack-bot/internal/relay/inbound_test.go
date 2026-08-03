package relay

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	slack "github.com/palgroup/palai/adapters/integrations/slack"
	palai "github.com/palgroup/palai/sdks/go"
)

// fakeStore is ThreadStore's test double: an in-memory map keyed the same way Task 8's real Postgres
// table is (bot_id, team_id, channel_id, thread_ts), with BindThread's ON-CONFLICT-DO-NOTHING and
// RebindThread's real compare-and-swap semantics reproduced — a test simulates a lost race by
// pre-seeding bound[key] to a value OTHER than the oldSessionID the code under test will pass, exactly
// as a concurrent winner would have left it.
type fakeStore struct {
	mu         sync.Mutex
	bound      map[threadKey]string
	binds      int
	rebinds    int
	rebindsWon int
}

func newFakeStore() *fakeStore { return &fakeStore{bound: make(map[threadKey]string)} }

func (f *fakeStore) SessionForThread(ctx context.Context, botID, teamID, channelID, threadTS string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.bound[threadKey{botID: botID, teamID: teamID, channelID: channelID, threadTS: threadTS}]
	return id, ok, nil
}

func (f *fakeStore) BindThread(ctx context.Context, botID, teamID, channelID, threadTS, sessionID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.binds++
	key := threadKey{botID: botID, teamID: teamID, channelID: channelID, threadTS: threadTS}
	if existing, ok := f.bound[key]; ok {
		return existing, nil
	}
	f.bound[key] = sessionID
	return sessionID, nil
}

func (f *fakeStore) RebindThread(ctx context.Context, botID, teamID, channelID, threadTS, oldSessionID, newSessionID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rebinds++
	key := threadKey{botID: botID, teamID: teamID, channelID: channelID, threadTS: threadTS}
	if f.bound[key] != oldSessionID {
		return false, nil // the real compare-and-swap: only wins if the row still names oldSessionID
	}
	f.bound[key] = newSessionID
	f.rebindsWon++
	return true, nil
}

// fakePalai is Palai's test double: it counts calls, records the last request each method saw, can be
// told to 404 its NEXT Steer/CreateResponse call (the orphan-recovery trigger), and hands back a
// caller-supplied EventStream (default: an immediately-EOF one, so a run under test finishes at once).
type fakePalai struct {
	mu sync.Mutex

	sessionsCreated int
	sessionSeq      int
	nextSessionID   string

	responses     int
	lastCreateReq palai.ResponseCreateRequest

	steers       int
	lastSteerMsg string

	failNextResponseNotFound bool
	failNextSteerNotFound    bool

	stream func() EventStream
	// sessionEventsParams records every EventsParams SessionEvents was called with, in call order —
	// what proves (or disproves) AfterSequence resumption: an implementation that hardcoded
	// AfterSequence: 0 would satisfy every OTHER assertion in this file identically, so a test that
	// cares about resumption must inspect this rather than just observing the run complete.
	sessionEventsParams []palai.EventsParams

	// onCreateSession, if set, runs after a session is minted but before CreateSession returns — the
	// exact window rebindOrphan's own CreateSession-then-RebindThread ordering opens up for a
	// concurrent recovery on the same thread to land first. A test uses it to simulate exactly that.
	onCreateSession func()
}

func (f *fakePalai) CreateSession(ctx context.Context, p palai.CreateSessionParams) (*palai.Session, error) {
	f.mu.Lock()
	f.sessionsCreated++
	id := f.nextSessionID
	if id == "" {
		f.sessionSeq++
		id = fmt.Sprintf("ses_%d", f.sessionSeq)
	}
	hook := f.onCreateSession
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return &palai.Session{ID: id, Object: "session", Status: "active"}, nil
}

func (f *fakePalai) Steer(ctx context.Context, sessionID string, p palai.SteerParams) (*palai.Command, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNextSteerNotFound {
		f.failNextSteerNotFound = false
		return nil, &palai.APIError{Status: http.StatusNotFound}
	}
	f.steers++
	f.lastSteerMsg = p.Message
	return &palai.Command{ID: "cmd_1", SessionID: sessionID, Status: "queued"}, nil
}

func (f *fakePalai) CreateResponse(ctx context.Context, req palai.ResponseCreateRequest) (*palai.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNextResponseNotFound {
		f.failNextResponseNotFound = false
		return nil, &palai.APIError{Status: http.StatusNotFound}
	}
	f.responses++
	f.lastCreateReq = req
	sessionID := ""
	if req.SessionID != nil {
		sessionID = *req.SessionID
	}
	return &palai.Response{ID: "resp_1", SessionID: sessionID, Status: "queued"}, nil
}

func (f *fakePalai) SessionEvents(ctx context.Context, sessionID string, p palai.EventsParams) (EventStream, error) {
	f.mu.Lock()
	f.sessionEventsParams = append(f.sessionEventsParams, p)
	supplier := f.stream
	f.mu.Unlock()
	if supplier != nil {
		return supplier(), nil
	}
	return staticStream(nil), nil // no events: Run's loop hits io.EOF on its first Next() and returns
}

// channelStream is a blocking EventStream a test drives by hand — the shape needed to prove "a second
// message steers into a still-open run" deterministically: Next() blocks until the test pushes an
// event (or closes the stream), so the run under test stays "active" for exactly as long as the test
// needs it to, with no sleep anywhere.
type channelStream struct {
	events chan palai.Event
	done   chan struct{}
}

func newChannelStream() *channelStream {
	return &channelStream{events: make(chan palai.Event), done: make(chan struct{})}
}

func (c *channelStream) Next() (palai.Event, error) {
	select {
	case e, ok := <-c.events:
		if !ok {
			return palai.Event{}, io.EOF
		}
		return e, nil
	case <-c.done:
		return palai.Event{}, io.EOF
	}
}

func (c *channelStream) Close() error {
	select {
	case <-c.done:
	default:
		close(c.done)
	}
	return nil
}

// newTestDeps builds InboundDeps over fakes with a SYNCHRONOUS RunInBackground: a test that does not
// care how a turn's stream plays out gets deterministic assertions immediately after HandleEvent
// returns, with no goroutine left running past the test. Tests that DO care (steering into a still-open
// run) override RunInBackground themselves.
func newTestDeps(t *testing.T) (InboundDeps, *fakePalai, *fakeStore) {
	t.Helper()
	fp := &fakePalai{}
	fs := newFakeStore()
	deps := NewInboundDeps(
		fs, fp,
		func(recipientUserID, recipientTeamID string) Slack { return &fakeSlack{} },
		func(f func()) { f() },
		func(context.Context, string, string, palai.Event) {},
		"bot_1", "U_BOT", "rev_1", "rbd_1",
	)
	return deps, fp, fs
}

// TestATopLevelMentionIsDeliveredOnce is SLK-P5: Slack sends both app_mention and its message.channels
// twin for the SAME human message, each with its OWN event_id but the SAME MessageTS (the affected
// message's own ts — a property of the message, not the delivery). A dedupe keyed on event_id would
// not collapse them; one keyed on MessageTS does.
func TestATopLevelMentionIsDeliveredOnce(t *testing.T) {
	deps, fp, _ := newTestDeps(t)
	mention := slack.Event{Type: "app_mention", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.001",
		SourceEventID: "Ev1", UserID: "U2", Text: "hi"}
	twin := slack.Event{Type: "message", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.001",
		SourceEventID: "Ev2", UserID: "U2", Text: "hi"}

	if err := HandleEvent(context.Background(), deps, mention); err != nil {
		t.Fatalf("HandleEvent(mention): %v", err)
	}
	if err := HandleEvent(context.Background(), deps, twin); err != nil {
		t.Fatalf("HandleEvent(twin): %v", err)
	}
	if fp.responses != 1 {
		t.Fatalf("responses = %d, want 1: the twin was delivered as a second turn", fp.responses)
	}
	if fp.sessionsCreated != 1 {
		t.Fatalf("sessionsCreated = %d, want 1", fp.sessionsCreated)
	}

	// The other direction, and the exact regression a text-keyed dedupe (this file's earlier version)
	// introduced: a LATER, genuinely different message that happens to share the same TEXT ("yes",
	// answering two different bot questions) must NOT be swallowed. Different MessageTS, same text.
	again := slack.Event{Type: "message", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.002",
		SourceEventID: "Ev4", UserID: "U2", Text: "hi"}
	if err := HandleEvent(context.Background(), deps, again); err != nil {
		t.Fatalf("HandleEvent(again): %v", err)
	}
	if fp.responses != 2 {
		t.Fatalf("responses = %d, want 2 — a later message with the SAME text but a DIFFERENT MessageTS must not be swallowed", fp.responses)
	}
}

// TestTheBotIgnoresItsOwnMessages pins the self-loop guard: an event whose UserID is the bot's own
// (BotUserID, from the bots-registry row — a separate identity from whatever the connection-level
// MapEvent guard upstream may already have applied) must never become a turn.
func TestTheBotIgnoresItsOwnMessages(t *testing.T) {
	deps, fp, _ := newTestDeps(t)
	self := slack.Event{Type: "message", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.001",
		SourceEventID: "Ev3", UserID: deps.BotUserID, Text: "an answer"}

	if err := HandleEvent(context.Background(), deps, self); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if fp.responses != 0 || fp.sessionsCreated != 0 {
		t.Fatal("the bot answered itself; the self-loop guard is BotUserID")
	}
}

// TestFirstMessageInAThreadOpensASessionThenARun pins the two-call turn (Sessions.Create then
// Responses.Create), that the second call carries the bot's own AgentRevisionID/RepositoryBindingID,
// that Input is the event's own text, that the thread ends up bound in the store, and that the Slack
// stream this run opens is given the EVENT's recipient identity, not an invented one.
func TestFirstMessageInAThreadOpensASessionThenARun(t *testing.T) {
	deps, fp, fs := newTestDeps(t)
	var gotRecipientUser, gotRecipientTeam string
	deps.NewSlack = func(recipientUserID, recipientTeamID string) Slack {
		gotRecipientUser, gotRecipientTeam = recipientUserID, recipientTeamID
		return &fakeSlack{}
	}

	ev := slack.Event{Type: "app_mention", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.001", UserID: "U2", Text: "ship it"}

	if err := HandleEvent(context.Background(), deps, ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	if fp.sessionsCreated != 1 {
		t.Fatalf("sessionsCreated = %d, want 1 (call #1 of the two-call turn)", fp.sessionsCreated)
	}
	if fp.responses != 1 {
		t.Fatalf("responses = %d, want 1 (call #2)", fp.responses)
	}
	req := fp.lastCreateReq
	if req.SessionID == nil || *req.SessionID == "" {
		t.Fatal("Responses.Create carried no session_id")
	}
	if req.AgentRevisionID == nil || *req.AgentRevisionID != "rev_1" {
		t.Fatalf("AgentRevisionID = %v, want rev_1", req.AgentRevisionID)
	}
	if got := req.Repository["binding_id"]; got != "rbd_1" {
		t.Fatalf("Repository binding_id = %v, want rbd_1", got)
	}
	if req.Input != "ship it" {
		t.Fatalf("Input = %v, want the event's own text", req.Input)
	}
	if gotRecipientUser != "U2" || gotRecipientTeam != "T1" {
		t.Fatalf("stream recipient = (%s,%s), want the event's own (UserID,TeamID) = (U2,T1)", gotRecipientUser, gotRecipientTeam)
	}
	sessionID, found, err := fs.SessionForThread(context.Background(), "bot_1", "T1", "C1", "1.1")
	if err != nil || !found || sessionID == "" {
		t.Fatalf("thread not bound after the first message: found=%v err=%v", found, err)
	}
	if len(fp.sessionEventsParams) != 1 || fp.sessionEventsParams[0].AfterSequence != 0 {
		t.Fatalf("SessionEvents calls = %v, want exactly one with AfterSequence=0 (a brand-new session has nothing to resume past)", fp.sessionEventsParams)
	}
}

// TestRepositoryIsOmittedWhenTheBotIsNotRepositoryBound pins the OTHER half of the silent-failure the
// plan calls out: a bot whose registry row carries no RepositoryBindingID must send NO repository
// field at all (never an empty binding_id), so a caller inspecting the wire body can tell "not a
// coding bot" apart from "forgot to configure one".
func TestRepositoryIsOmittedWhenTheBotIsNotRepositoryBound(t *testing.T) {
	deps, fp, _ := newTestDeps(t)
	deps.RepositoryBindingID = ""

	ev := slack.Event{Type: "message", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.001", UserID: "U2", Text: "hi"}
	if err := HandleEvent(context.Background(), deps, ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if fp.lastCreateReq.Repository != nil {
		t.Fatalf("Repository = %v, want nil (an unbound bot must not attach a workspace)", fp.lastCreateReq.Repository)
	}
}

// TestASecondMessageWhileARunIsOpenSteersIntoIt is the "test case, not a comment" the brief calls for:
// while a thread's run is still streaming, a second message must be delivered via Sessions.Steer into
// the SAME run, never as a second Responses.Create.
func TestASecondMessageWhileARunIsOpenSteersIntoIt(t *testing.T) {
	deps, fp, _ := newTestDeps(t)
	stream := newChannelStream()
	fp.stream = func() EventStream { return stream }

	done := make(chan struct{})
	deps.RunInBackground = func(f func()) {
		go func() {
			f()
			close(done)
		}()
	}

	first := slack.Event{Type: "app_mention", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.001", UserID: "U2", Text: "start"}
	if err := HandleEvent(context.Background(), deps, first); err != nil {
		t.Fatalf("HandleEvent(first): %v", err)
	}

	second := slack.Event{Type: "message", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.002", UserID: "U2", Text: "and also this"}
	if err := HandleEvent(context.Background(), deps, second); err != nil {
		t.Fatalf("HandleEvent(second): %v", err)
	}

	if fp.responses != 1 {
		t.Fatalf("responses = %d, want 1 — the second message must steer, not start a new run", fp.responses)
	}
	if fp.steers != 1 {
		t.Fatalf("steers = %d, want 1", fp.steers)
	}
	if fp.lastSteerMsg != "and also this" {
		t.Fatalf("steered message = %q, want the second event's own text", fp.lastSteerMsg)
	}

	// Let the run finish and wait for cleanup, so nothing outlives the test.
	stream.events <- palai.Event{Type: "run.completed.v1", Sequence: 3, Data: map[string]any{}}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("relay.Run never finished after its terminal event")
	}
}

// TestASecondMessageAfterTheRunFinishedStartsAFreshResponseOnTheSameSession is the other branch of the
// same choice: once a thread's run has already finished, the NEXT message must open a fresh
// Responses.Create — reusing the thread's session (Sessions.Create must NOT run again), not a Steer
// into a run that no longer exists.
//
// It ALSO pins AfterSequence resumption, which no other test in this file can: the first run's fixture
// carries real, non-zero sequence numbers (an implementation hardcoding AfterSequence: 0 would satisfy
// every assertion in this file EXCEPT the ones below identically — an empty first stream never proves
// resumption happened, only that a stream was opened at all). The second run must resume PAST the
// first run's own terminal event, or it would see that stale terminal event first and stop immediately
// without ever reaching the second run's events (sequenceTrackingStream's own doc comment, inbound.go).
func TestASecondMessageAfterTheRunFinishedStartsAFreshResponseOnTheSameSession(t *testing.T) {
	deps, fp, _ := newTestDeps(t)

	firstRunEvents := []palai.Event{
		{Type: "model_step.delta.v1", Sequence: 5, Data: map[string]any{"text": "ok"}},
		{Type: "run.completed.v1", Sequence: 6, Data: map[string]any{}},
	}
	streamCalls := 0
	fp.stream = func() EventStream {
		streamCalls++
		if streamCalls == 1 {
			return staticStream(firstRunEvents)
		}
		return staticStream(nil) // the second run's own stream; its content is not this test's concern
	}

	first := slack.Event{Type: "app_mention", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.001", UserID: "U2", Text: "one"}
	if err := HandleEvent(context.Background(), deps, first); err != nil {
		t.Fatalf("HandleEvent(first): %v", err)
	}
	firstSessionID := *fp.lastCreateReq.SessionID

	second := slack.Event{Type: "message", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.002", UserID: "U2", Text: "two"}
	if err := HandleEvent(context.Background(), deps, second); err != nil {
		t.Fatalf("HandleEvent(second): %v", err)
	}

	if fp.sessionsCreated != 1 {
		t.Fatalf("sessionsCreated = %d, want 1 — a second message in the SAME thread must reuse the session", fp.sessionsCreated)
	}
	if fp.responses != 2 {
		t.Fatalf("responses = %d, want 2 — the finished run must not be steered into", fp.responses)
	}
	if fp.steers != 0 {
		t.Fatalf("steers = %d, want 0", fp.steers)
	}
	if *fp.lastCreateReq.SessionID != firstSessionID {
		t.Fatalf("second response's session_id = %s, want %s (the same thread's session)", *fp.lastCreateReq.SessionID, firstSessionID)
	}
	if len(fp.sessionEventsParams) != 2 {
		t.Fatalf("SessionEvents was called %d time(s), want 2 (one per run)", len(fp.sessionEventsParams))
	}
	if got := fp.sessionEventsParams[0].AfterSequence; got != 0 {
		t.Fatalf("first run's AfterSequence = %d, want 0 (a brand-new session)", got)
	}
	if got := fp.sessionEventsParams[1].AfterSequence; got != 6 {
		t.Fatalf("second run's AfterSequence = %d, want 6 (the first run's own terminal event's sequence) — "+
			"an implementation that never resumes would send 0 here too and every OTHER assertion in "+
			"this test would still pass", got)
	}
}

// TestAnOrphanedSessionIsRecoveredWithANewSessionAndRebind is Task 8's expected steady state, exercised
// end to end: a thread's stored session_id no longer resolves server-side (404 on the very call that
// would use it), and HandleEvent must open a replacement, REBIND the thread to it (not just bind — the
// row already exists), and retry the turn on the replacement rather than surfacing the 404 to the
// caller.
func TestAnOrphanedSessionIsRecoveredWithANewSessionAndRebind(t *testing.T) {
	deps, fp, fs := newTestDeps(t)
	if _, err := fs.BindThread(context.Background(), "bot_1", "T1", "C1", "1.1", "ses_stale"); err != nil {
		t.Fatalf("seed BindThread: %v", err)
	}
	fp.failNextResponseNotFound = true

	ev := slack.Event{Type: "app_mention", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.001", UserID: "U2", Text: "hi"}
	if err := HandleEvent(context.Background(), deps, ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	if fp.sessionsCreated != 1 {
		t.Fatalf("sessionsCreated = %d, want 1 (the replacement session)", fp.sessionsCreated)
	}
	if fp.responses != 1 {
		t.Fatalf("responses = %d, want 1 (the retried create, after recovery)", fp.responses)
	}
	if fs.rebinds != 1 || fs.rebindsWon != 1 {
		t.Fatalf("rebinds=%d rebindsWon=%d, want 1/1", fs.rebinds, fs.rebindsWon)
	}
	sessionID, found, err := fs.SessionForThread(context.Background(), "bot_1", "T1", "C1", "1.1")
	if err != nil || !found || sessionID == "ses_stale" {
		t.Fatalf("thread mapping = (%s,%v), want it moved off the stale session", sessionID, found)
	}
}

// TestALostRebindRaceUsesTheWinnersSession is the CAS's other branch (store/threads.go's RebindThread
// review round): two events on the same orphaned thread can both open a replacement session and both
// call RebindThread with the same oldSessionID, but the row can only move once. This simulates the
// LOSING side — the store already carries a DIFFERENT session (the concurrent winner's) by the time
// this call's compare-and-swap runs — and asserts the loser abandons the session it just minted (never
// writes it anywhere, never uses it for the turn) and uses the winner's session instead.
func TestALostRebindRaceUsesTheWinnersSession(t *testing.T) {
	deps, fp, fs := newTestDeps(t)
	if _, err := fs.BindThread(context.Background(), "bot_1", "T1", "C1", "1.1", "ses_stale"); err != nil {
		t.Fatalf("seed BindThread: %v", err)
	}
	fp.failNextResponseNotFound = true

	// Simulate a concurrent winner landing in the exact window rebindOrphan opens: after THIS call has
	// already read "ses_stale" (resolveSession, before HandleEvent even starts here) and minted its own
	// replacement session, but before its RebindThread compare-and-swap runs. onCreateSession fires
	// synchronously inside that window.
	fp.onCreateSession = func() {
		fs.mu.Lock()
		fs.bound[threadKey{botID: "bot_1", teamID: "T1", channelID: "C1", threadTS: "1.1"}] = "ses_winner"
		fs.mu.Unlock()
	}

	ev := slack.Event{Type: "app_mention", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.001", UserID: "U2", Text: "hi"}
	if err := HandleEvent(context.Background(), deps, ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	if fs.rebinds != 1 || fs.rebindsWon != 0 {
		t.Fatalf("rebinds=%d rebindsWon=%d, want 1/0 (this call must lose the race)", fs.rebinds, fs.rebindsWon)
	}
	if fp.responses != 1 {
		t.Fatalf("responses = %d, want 1 (the turn still completes, on the winner's session)", fp.responses)
	}
	if got := *fp.lastCreateReq.SessionID; got != "ses_winner" {
		t.Fatalf("Responses.Create session_id = %s, want ses_winner — the loser must not use the session it minted", got)
	}
	sessionID, found, err := fs.SessionForThread(context.Background(), "bot_1", "T1", "C1", "1.1")
	if err != nil || !found || sessionID != "ses_winner" {
		t.Fatalf("thread mapping = (%s,%v), want it left at ses_winner, untouched by the loser", sessionID, found)
	}
}

// TestAnEditOrDeleteNeverBecomesATurn is SLK-005 at the turn boundary: MapEvent classifies an edit as
// KindCorrection and a deletion as KindTombstone specifically so downstream code does not have to
// re-decode the payload to tell them apart from a new message — HandleEvent must actually use that.
func TestAnEditOrDeleteNeverBecomesATurn(t *testing.T) {
	deps, fp, _ := newTestDeps(t)
	edit := slack.Event{Type: "message", Kind: slack.KindCorrection,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.001", UserID: "U2", Text: "edited text"}
	if err := HandleEvent(context.Background(), deps, edit); err != nil {
		t.Fatalf("HandleEvent(edit): %v", err)
	}
	del := slack.Event{Type: "message", Kind: slack.KindTombstone,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.2", MessageTS: "100.002", UserID: "U2"}
	if err := HandleEvent(context.Background(), deps, del); err != nil {
		t.Fatalf("HandleEvent(delete): %v", err)
	}
	if fp.sessionsCreated != 0 || fp.responses != 0 {
		t.Fatal("an edit or a deletion must never become a new turn (SLK-005)")
	}
}

// TestHandleEventRefusesIncompleteDeps mirrors relay.Run's own TestRunRefusesIncompleteDeps: a
// half-built InboundDeps must be refused before it does anything, not fail partway through.
func TestHandleEventRefusesIncompleteDeps(t *testing.T) {
	_, fp, fs := newTestDeps(t)
	incomplete := InboundDeps{Store: fs, Palai: fp} // no NewSlack, no RunInBackground, no state, no BotID

	ev := slack.Event{Type: "message", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.001", UserID: "U2", Text: "hi"}
	if err := HandleEvent(context.Background(), incomplete, ev); err == nil {
		t.Fatal("HandleEvent with incomplete InboundDeps succeeded")
	}
	if fp.sessionsCreated != 0 {
		t.Fatalf("sessionsCreated = %d, want 0 — an incomplete Deps must refuse before doing anything", fp.sessionsCreated)
	}
}
