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
// RebindThread's always-overwrites semantics reproduced so a test can tell the two apart.
type fakeStore struct {
	mu      sync.Mutex
	bound   map[threadKey]string
	binds   int
	rebinds int
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

func (f *fakeStore) RebindThread(ctx context.Context, botID, teamID, channelID, threadTS, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rebinds++
	f.bound[threadKey{botID: botID, teamID: teamID, channelID: channelID, threadTS: threadTS}] = sessionID
	return nil
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
}

func (f *fakePalai) CreateSession(ctx context.Context, p palai.CreateSessionParams) (*palai.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessionsCreated++
	id := f.nextSessionID
	if id == "" {
		f.sessionSeq++
		id = fmt.Sprintf("ses_%d", f.sessionSeq)
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
		"bot_1", "U_BOT", "rev_1", "rbd_1",
	)
	return deps, fp, fs
}

// TestATopLevelMentionIsDeliveredOnce is SLK-P5: Slack sends both app_mention and its message.channels
// twin for the SAME human message, each with its OWN event_id — a dedupe keyed on event_id would not
// collapse them, so the guard must key on the message itself instead.
func TestATopLevelMentionIsDeliveredOnce(t *testing.T) {
	deps, fp, _ := newTestDeps(t)
	mention := slack.Event{Type: "app_mention", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", SourceEventID: "Ev1", UserID: "U2", Text: "hi"}
	twin := slack.Event{Type: "message", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", SourceEventID: "Ev2", UserID: "U2", Text: "hi"}

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
}

// TestTheBotIgnoresItsOwnMessages pins the self-loop guard: an event whose UserID is the bot's own
// (BotUserID, from the bots-registry row — a separate identity from whatever the connection-level
// MapEvent guard upstream may already have applied) must never become a turn.
func TestTheBotIgnoresItsOwnMessages(t *testing.T) {
	deps, fp, _ := newTestDeps(t)
	self := slack.Event{Type: "message", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", SourceEventID: "Ev3", UserID: deps.BotUserID, Text: "an answer"}

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
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", UserID: "U2", Text: "ship it"}

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
}

// TestRepositoryIsOmittedWhenTheBotIsNotRepositoryBound pins the OTHER half of the silent-failure the
// plan calls out: a bot whose registry row carries no RepositoryBindingID must send NO repository
// field at all (never an empty binding_id), so a caller inspecting the wire body can tell "not a
// coding bot" apart from "forgot to configure one".
func TestRepositoryIsOmittedWhenTheBotIsNotRepositoryBound(t *testing.T) {
	deps, fp, _ := newTestDeps(t)
	deps.RepositoryBindingID = ""

	ev := slack.Event{Type: "message", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", UserID: "U2", Text: "hi"}
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
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", UserID: "U2", Text: "start"}
	if err := HandleEvent(context.Background(), deps, first); err != nil {
		t.Fatalf("HandleEvent(first): %v", err)
	}

	second := slack.Event{Type: "message", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", UserID: "U2", Text: "and also this"}
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
func TestASecondMessageAfterTheRunFinishedStartsAFreshResponseOnTheSameSession(t *testing.T) {
	deps, fp, _ := newTestDeps(t)

	first := slack.Event{Type: "app_mention", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", UserID: "U2", Text: "one"}
	if err := HandleEvent(context.Background(), deps, first); err != nil {
		t.Fatalf("HandleEvent(first): %v", err)
	}
	firstSessionID := *fp.lastCreateReq.SessionID

	second := slack.Event{Type: "message", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", UserID: "U2", Text: "two"}
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
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", UserID: "U2", Text: "hi"}
	if err := HandleEvent(context.Background(), deps, ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	if fp.sessionsCreated != 1 {
		t.Fatalf("sessionsCreated = %d, want 1 (the replacement session)", fp.sessionsCreated)
	}
	if fp.responses != 1 {
		t.Fatalf("responses = %d, want 1 (the retried create, after recovery)", fp.responses)
	}
	if fs.rebinds != 1 {
		t.Fatalf("rebinds = %d, want 1", fs.rebinds)
	}
	sessionID, found, err := fs.SessionForThread(context.Background(), "bot_1", "T1", "C1", "1.1")
	if err != nil || !found || sessionID == "ses_stale" {
		t.Fatalf("thread mapping = (%s,%v), want it moved off the stale session", sessionID, found)
	}
}

// TestAnEditOrDeleteNeverBecomesATurn is SLK-005 at the turn boundary: MapEvent classifies an edit as
// KindCorrection and a deletion as KindTombstone specifically so downstream code does not have to
// re-decode the payload to tell them apart from a new message — HandleEvent must actually use that.
func TestAnEditOrDeleteNeverBecomesATurn(t *testing.T) {
	deps, fp, _ := newTestDeps(t)
	edit := slack.Event{Type: "message", Kind: slack.KindCorrection,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", UserID: "U2", Text: "edited text"}
	if err := HandleEvent(context.Background(), deps, edit); err != nil {
		t.Fatalf("HandleEvent(edit): %v", err)
	}
	del := slack.Event{Type: "message", Kind: slack.KindTombstone,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.2", UserID: "U2"}
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
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", UserID: "U2", Text: "hi"}
	if err := HandleEvent(context.Background(), incomplete, ev); err == nil {
		t.Fatal("HandleEvent with incomplete InboundDeps succeeded")
	}
	if fp.sessionsCreated != 0 {
		t.Fatalf("sessionsCreated = %d, want 0 — an incomplete Deps must refuse before doing anything", fp.sessionsCreated)
	}
}
