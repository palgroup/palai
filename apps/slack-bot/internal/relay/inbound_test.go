package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	slack "github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/apps/slack-bot/internal/store"
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

	// delivery mirrors migrations/0002_delivery_state.sql's four columns per thread. It is reproduced
	// FAITHFULLY rather than generously — including BeginDelivery's refusal on an unbound thread and
	// RebindThread's reset of the whole delivery row — because a fake looser than the schema lets a test
	// pass against a shape Postgres would refuse.
	delivery map[threadKey]*fakeDelivery
	// claims is approval_posts: the primary key, and nothing else it does not enforce.
	claims map[string]bool
	// failBeginDelivery makes the durable record refuse — the one store failure the inbound path is
	// supposed to REFUSE A TURN over rather than log and continue.
	failBeginDelivery error
}

// fakeDelivery is one thread's delivery row.
type fakeDelivery struct {
	lastSequence    int64
	runPending      bool
	streamTS        string
	recipientUserID string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		bound:    make(map[threadKey]string),
		delivery: make(map[threadKey]*fakeDelivery),
		claims:   make(map[string]bool),
	}
}

// row returns the delivery row for a bound thread, creating it on the first bind exactly as the real
// INSERT does (every column at its default). It answers nil for a thread with no binding at all, which is
// what makes BeginDelivery's refusal reproducible here.
func (f *fakeStore) row(key threadKey) *fakeDelivery {
	if _, bound := f.bound[key]; !bound {
		return nil
	}
	d, ok := f.delivery[key]
	if !ok {
		d = &fakeDelivery{}
		f.delivery[key] = d
	}
	return d
}

func (f *fakeStore) BeginDelivery(ctx context.Context, botID, teamID, channelID, threadTS, recipientUserID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failBeginDelivery != nil {
		return 0, f.failBeginDelivery
	}
	d := f.row(threadKey{botID: botID, teamID: teamID, channelID: channelID, threadTS: threadTS})
	if d == nil {
		return 0, fmt.Errorf("fakeStore: %s/%s/%s/%s has no binding", botID, teamID, channelID, threadTS)
	}
	d.runPending, d.streamTS, d.recipientUserID = true, "", recipientUserID
	return d.lastSequence, nil
}

func (f *fakeStore) RecordStreamTS(ctx context.Context, botID, teamID, channelID, threadTS, streamTS string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d := f.row(threadKey{botID: botID, teamID: teamID, channelID: channelID, threadTS: threadTS}); d != nil {
		d.streamTS = streamTS
	}
	return nil
}

func (f *fakeStore) RecordDelivered(ctx context.Context, botID, teamID, channelID, threadTS string, sequence int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d := f.row(threadKey{botID: botID, teamID: teamID, channelID: channelID, threadTS: threadTS}); d != nil && sequence > d.lastSequence {
		d.lastSequence = sequence // the real GREATEST: never walks backward
	}
	return nil
}

func (f *fakeStore) EndDelivery(ctx context.Context, botID, teamID, channelID, threadTS string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d := f.row(threadKey{botID: botID, teamID: teamID, channelID: channelID, threadTS: threadTS}); d != nil {
		d.runPending, d.streamTS = false, ""
	}
	return nil
}

func (f *fakeStore) PendingDeliveries(ctx context.Context, botID string) ([]store.PendingRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.PendingRun
	for key, d := range f.delivery {
		if key.botID != botID || !d.runPending {
			continue
		}
		out = append(out, store.PendingRun{
			TeamID: key.teamID, ChannelID: key.channelID, ThreadTS: key.threadTS,
			SessionID: f.bound[key], LastSequence: d.lastSequence,
			StreamTS: d.streamTS, RecipientUserID: d.recipientUserID,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ThreadTS < out[j].ThreadTS })
	return out, nil
}

func (f *fakeStore) ThreadForSession(ctx context.Context, botID, sessionID string) (store.PendingRun, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var found []store.PendingRun
	for key, id := range f.bound {
		if key.botID != botID || id != sessionID {
			continue
		}
		p := store.PendingRun{TeamID: key.teamID, ChannelID: key.channelID, ThreadTS: key.threadTS, SessionID: id}
		if d := f.delivery[key]; d != nil {
			p.LastSequence, p.StreamTS, p.RecipientUserID = d.lastSequence, d.streamTS, d.recipientUserID
		}
		found = append(found, p)
	}
	switch len(found) {
	case 0:
		return store.PendingRun{}, false, nil
	case 1:
		return found[0], true, nil
	default:
		return store.PendingRun{}, false, store.ErrAmbiguousSession
	}
}

func (f *fakeStore) ClaimApprovalPost(ctx context.Context, botID, approvalID, channelID, threadTS string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := botID + "|" + approvalID
	if f.claims[key] {
		return false, nil
	}
	f.claims[key] = true
	return true, nil
}

func (f *fakeStore) ReleaseApprovalPost(ctx context.Context, botID, approvalID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.claims, botID+"|"+approvalID)
	return nil
}

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
	// The real UPDATE resets the whole delivery row in the same statement, because a cursor means nothing
	// outside the journal it came from and the pending run it named was on the session that no longer
	// exists. A double that skipped this would let a resume-from-a-dead-session bug pass.
	delete(f.delivery, key)
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

	// searchGrants records every GrantSlackSearchAuthority call IN ORDER, including the ones carrying an
	// empty token. Order and emptiness are both load-bearing: the withdraw is a call, not an omission.
	searchGrants  []palai.SlackSearchAuthorityParams
	failNextGrant error

	failNextResponseNotFound bool
	failNextSteerNotFound    bool
	// failNextEventsNotFound / failNextEvents are the two ways attaching to a session's journal fails —
	// see SessionEvents for why they are separate knobs and not one.
	failNextEventsNotFound bool
	failNextEvents         error

	// stream is handed the CONTEXT SessionEvents was opened with, because that context is not incidental
	// to the fake — *palai.SessionEventStream reads its connection under it, and a stream double that
	// ignores it cannot reproduce (or refute) the 2026-08-03 defect. See ctxBoundStream.
	stream func(ctx context.Context) EventStream
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
	// The two ways attaching to a journal fails are kept APART, because the relay's recovery path treats
	// them as opposites: a 404 means the answer is gone and the debt should be written off, anything else
	// means try again later. A double with one knob for both would let a recovery that wrote off a
	// transient failure pass.
	notFound, other := f.failNextEventsNotFound, f.failNextEvents
	f.failNextEventsNotFound, f.failNextEvents = false, nil
	f.mu.Unlock()
	if notFound {
		return nil, &palai.APIError{Status: http.StatusNotFound}
	}
	if other != nil {
		return nil, other
	}
	if supplier != nil {
		if stream := supplier(ctx); stream != nil {
			return stream, nil
		}
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

// ctxBoundStream is an EventStream carrying *palai.SessionEventStream's REAL cancellation semantics, which
// is the whole point of it: the SDK reads its connection under the context Sessions.Events was opened with
// and closes its delivery channel when that context ends, and Next() turns a closed channel into io.EOF
// (sdks/go/sessionevents.go). A stream double that ignored the context would render the events regardless
// and report a green run for a bot that renders nothing.
//
// release gates when the relay may start reading, so the test can put the caller's cancellation strictly
// BEFORE the first Next() — which is the real ordering (socket.dispatch's `defer cancel()` fires as
// HandleEvent returns, before the goroutine is scheduled) and makes the outcome deterministic instead of a
// race between the two.
type ctxBoundStream struct {
	ctx     context.Context
	release chan struct{}
	events  []palai.Event
	i       int
}

func (c *ctxBoundStream) Next() (palai.Event, error) {
	<-c.release
	if c.ctx.Err() != nil {
		return palai.Event{}, io.EOF // the SDK's own answer for a stream whose context has ended
	}
	if c.i >= len(c.events) {
		return palai.Event{}, io.EOF
	}
	c.i++
	return c.events[c.i-1], nil
}

func (c *ctxBoundStream) Close() error { return nil }

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
		func(error, string, string, string) {},
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
	// InThread: true because it is a genuine Slack reply under the mention's own thread, and birthsRun
	// requires that (plus the correlation the mention already established) for a non-mention message.
	again := slack.Event{Type: "message", Kind: slack.KindMessage, InThread: true,
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

	// app_mention, not a bare "message": this thread has never been correlated, and an unaddressed
	// message in an uncorrelated thread must not birth a run at all (birthsRun) — which would make the
	// assertion below pass vacuously (a zero-value ResponseCreateRequest also has a nil Repository).
	ev := slack.Event{Type: "app_mention", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.001", UserID: "U2", Text: "hi"}
	if err := HandleEvent(context.Background(), deps, ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if fp.responses != 1 {
		t.Fatalf("responses = %d, want 1 — the turn must actually have been created for the Repository check below to mean anything", fp.responses)
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
	fp.stream = func(context.Context) EventStream { return stream }

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

	// InThread: true — a genuine reply in the thread `first` opened, which birthsRun requires (alongside
	// the correlation `first` already established) for a follow-up that is not itself a mention or a DM.
	second := slack.Event{Type: "message", Kind: slack.KindMessage, InThread: true,
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
	fp.stream = func(context.Context) EventStream {
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

	// InThread: true — a genuine reply in the thread `first` opened; see the identical note in
	// TestASecondMessageWhileARunIsOpenSteersIntoIt.
	second := slack.Event{Type: "message", Kind: slack.KindMessage, InThread: true,
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
	// already read "ses_stale" (the correlation read HandleEvent does before deciding birth) and minted
	// its own replacement session, but before its RebindThread compare-and-swap runs. onCreateSession
	// fires synchronously inside that window.
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

// TestOrdinaryChannelChatterNeverBirthsARun is the run-birth rule's headline case, and the literal bug it
// restores against: the app subscribes to Slack's message.channels, so a plain top-level message — not an
// @mention, not a DM, not a reply inside a thread the bot already holds — must be acknowledged in silence,
// not become a Palai turn just because it is shaped like a message. Before birthsRun existed, this exact
// event opened a session and started a run.
func TestOrdinaryChannelChatterNeverBirthsARun(t *testing.T) {
	deps, fp, fs := newTestDeps(t)
	// A genuine top-level channel message: no thread_ts on the wire, so MapEvent sets InThread=false and
	// uses the message's own ts as ThreadTS (its would-be correlation root) — see inbound.go's MapEvent
	// doc ("a top-level message starts its own thread; ts is the correlation root").
	chatter := slack.Event{Type: "message", Kind: slack.KindMessage, ChannelType: "channel",
		TeamID: "T1", ChannelID: "C1", ThreadTS: "200.001", MessageTS: "200.001", UserID: "U2", Text: "anyone up for lunch?"}

	if err := HandleEvent(context.Background(), deps, chatter); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if fp.sessionsCreated != 0 || fp.responses != 0 {
		t.Fatalf("sessionsCreated=%d responses=%d, want 0/0 — ordinary channel chatter must never birth a run", fp.sessionsCreated, fp.responses)
	}
	if _, found, err := fs.SessionForThread(context.Background(), "bot_1", "T1", "C1", "200.001"); err != nil || found {
		t.Fatalf("thread bound=%v err=%v, want unbound — a non-birthing event must leave no trace at all", found, err)
	}
}

// TestAThreadReplyTheBotWasNeverPartOfDoesNotBirthARun is the OTHER half of the default clause
// (ev.InThread && correlated): being inside a genuine Slack thread is not enough on its own. Two humans
// can carry on a thread under a message the bot was never addressed in — the app is in the channel, so
// it still receives every reply — and none of those replies may open a run just because they arrived with
// a thread_ts.
func TestAThreadReplyTheBotWasNeverPartOfDoesNotBirthARun(t *testing.T) {
	deps, fp, fs := newTestDeps(t)
	reply := slack.Event{Type: "message", Kind: slack.KindMessage, ChannelType: "channel", InThread: true,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "1.2", UserID: "U2", Text: "agreed, noon works"}

	if err := HandleEvent(context.Background(), deps, reply); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if fp.sessionsCreated != 0 || fp.responses != 0 {
		t.Fatalf("sessionsCreated=%d responses=%d, want 0/0 — a thread the bot never correlated must not birth a run", fp.sessionsCreated, fp.responses)
	}
	if _, found, err := fs.SessionForThread(context.Background(), "bot_1", "T1", "C1", "1.1"); err != nil || found {
		t.Fatalf("thread bound=%v err=%v, want unbound", found, err)
	}
}

// TestADMWithoutAMentionBirthsARunEvenUncorrelated pins the DM half of birthsRun against the same
// uncorrelated-thread shape TestOrdinaryChannelChatterNeverBirthsARun refuses: a DM needs no @mention and
// no prior correlation, because there is nothing else a direct message to the app could be addressed to.
// ev.IsDM() (ChannelType == "im") is the authority, never an id prefix.
func TestADMWithoutAMentionBirthsARunEvenUncorrelated(t *testing.T) {
	deps, fp, _ := newTestDeps(t)
	dm := slack.Event{Type: "message", Kind: slack.KindMessage, ChannelType: "im",
		TeamID: "T1", ChannelID: "D1", ThreadTS: "300.001", MessageTS: "300.001", UserID: "U2", Text: "what's the status?"}

	if err := HandleEvent(context.Background(), deps, dm); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if fp.sessionsCreated != 1 || fp.responses != 1 {
		t.Fatalf("sessionsCreated=%d responses=%d, want 1/1 — a DM must birth a run with no @mention and no prior correlation", fp.sessionsCreated, fp.responses)
	}
}

// TestTheEventStreamOutlivesTheEnvelopeThatOpenedIt is the regression for the defect that swallowed a real
// answer on 2026-08-03: startRun detached the Run call from the caller's context but opened the EVENT
// STREAM under it, and the caller is socket.dispatch, whose `defer cancel()` fires the instant HandleEvent
// returns — before the relay goroutine has read anything.
//
// WHAT MAKES IT WORTH A TEST OF ITS OWN is that every OTHER signal stayed green. The turn was accepted, the
// run completed server-side with its answer journalled, Run took its ordinary io.EOF exit and returned NIL,
// and the Slack message was opened and closed correctly — just empty. Nothing failed; the relay simply
// never saw an event. So neither HandleEvent's return value nor OnRunFailed can catch this, and the only
// thing that can is asserting the events actually reached Slack after the caller's context ended.
func TestTheEventStreamOutlivesTheEnvelopeThatOpenedIt(t *testing.T) {
	deps, fp, _ := newTestDeps(t)
	slackStub := &fakeSlack{}
	deps.NewSlack = func(recipientUserID, recipientTeamID string) Slack { return slackStub }

	release := make(chan struct{})
	fp.stream = func(streamCtx context.Context) EventStream {
		return &ctxBoundStream{ctx: streamCtx, release: release, events: []palai.Event{
			{Type: "model_step.delta.v1", Sequence: 13, Data: map[string]any{"text": "Projede toplam 7 Swift dosyası var"}},
			{Type: "run.completed.v1", Sequence: 21, Data: map[string]any{}},
		}}
	}

	done := make(chan struct{})
	deps.RunInBackground = func(f func()) { go func() { f(); close(done) }() }

	// A context shaped like the one socket.dispatch hands the handler.
	ctx, cancel := context.WithCancel(context.Background())
	ev := slack.Event{Type: "app_mention", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.001", UserID: "U2",
		Text: "bu projede kaç tane Swift dosyası var?"}
	if err := HandleEvent(ctx, deps, ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	cancel()       // socket.dispatch's `defer cancel()`: the envelope is handled, its scope is over
	close(release) // only now may the relay read — so the cancellation is strictly first

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay goroutine never finished")
	}

	if got := strings.Join(slackStub.appended, ""); got != "Projede toplam 7 Swift dosyası var" {
		t.Fatalf("Slack received %q, want the run's own text — the relay read %d event(s) after the envelope's context ended",
			got, len(slackStub.appended))
	}
	if slackStub.stopped != 1 {
		t.Fatalf("the stream was closed %d time(s), want 1", slackStub.stopped)
	}
}

// TestAFailedRunIsReportedAndNotDiscarded is the regression for the one defect this file has actually
// shipped: startRun wrote `_ = Run(...)`, so every way a relay can fail AFTER the turn is accepted —
// the Slack stream refusing, the event stream erroring, a panic unwinding through Run's defer — ended in
// a discarded error. On 2026-08-03 that happened for real: a run completed on the control plane with its
// answer journalled, the thread kept saying "Working…", and the bot's log said nothing at all for the
// whole episode, so the failure had to be found by reading code rather than by reading output.
//
// The assertion is deliberately on the HOOK and not on the run's own success: HandleEvent's own return
// value stays nil in this scenario (the turn WAS accepted — Palai has the run), which is exactly why
// nothing else in this file can catch a regression here.
func TestAFailedRunIsReportedAndNotDiscarded(t *testing.T) {
	deps, _, _ := newTestDeps(t)
	// A Slack whose closing stopStream refuses: Run absorbs a failed APPEND into pending and tries again,
	// but a failed STOP is terminal — there is no later call to carry the text — so it becomes Run's
	// returned error, the value under test here.
	deps.NewSlack = func(recipientUserID, recipientTeamID string) Slack { return &fakeSlack{failStop: true} }

	var (
		reported                        []error
		gotSession, gotChannel, gotThre string
	)
	deps.OnRunFailed = func(err error, sessionID, channel, threadTS string) {
		reported = append(reported, err)
		gotSession, gotChannel, gotThre = sessionID, channel, threadTS
	}

	ev := slack.Event{Type: "app_mention", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.001", UserID: "U2", Text: "hi"}
	// RunInBackground is synchronous here (newTestDeps), so the run has already finished and reported by
	// the time this returns.
	if err := HandleEvent(context.Background(), deps, ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	if len(reported) != 1 {
		t.Fatalf("OnRunFailed fired %d time(s), want 1 — the error relay.Run returned was discarded", len(reported))
	}
	if !strings.Contains(reported[0].Error(), "stop stream") {
		t.Fatalf("reported error = %v, want the stopStream failure that ended the run", reported[0])
	}
	// The three ids are what makes the report actionable: an operator holding a thread that stopped needs
	// to be able to go from it to the session, and from the session to the run.
	if gotChannel != "C1" || gotThre != "1.1" {
		t.Fatalf("reported channel/thread = %s/%s, want C1/1.1", gotChannel, gotThre)
	}
	bound, found, err := deps.Store.SessionForThread(context.Background(), "bot_1", "T1", "C1", "1.1")
	if err != nil || !found {
		t.Fatalf("thread not bound: found=%v err=%v", found, err)
	}
	if gotSession != bound {
		t.Fatalf("reported session = %q, want the thread's own %q", gotSession, bound)
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

func (f *fakePalai) GrantSlackSearchAuthority(_ context.Context, _ string, p palai.SlackSearchAuthorityParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searchGrants = append(f.searchGrants, p)
	if err := f.failNextGrant; err != nil {
		f.failNextGrant = nil
		return err
	}
	return nil
}

// grantsSeen returns a copy under the lock.
func (f *fakePalai) grantsSeen() []palai.SlackSearchAuthorityParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]palai.SlackSearchAuthorityParams(nil), f.searchGrants...)
}

// THE ADDRESSED TURN HANDS OVER ITS TOKEN, and the tokenless one HANDS OVER AN EMPTY STRING rather than
// staying silent. The second half is the property with teeth.
//
// Slack attaches an action_token to the message that ADDRESSES the app and to nothing else — measured live
// on 2026-08-04 against team T0AMPM5JX8U: an app_mention and the message.channels twin of the same message
// each carried one, while a human's thread REPLY, a file_share, an edit and every bot-authored message
// carried none. A thread is mostly replies. So a relay that called the grant only when it HAD a token would
// leave the opening @mention's authority standing for every later reply in that thread — a search power
// nobody's message granted. The withdraw is a call, which is why this asserts on the call and not merely on
// the absence of a second grant.
func TestATokenlessTurnSendsTheWithdrawRatherThanStayingSilent(t *testing.T) {
	deps, fp, _ := newTestDeps(t)

	addressed := slack.Event{Type: "app_mention", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.001",
		SourceEventID: "Ev1", UserID: "U2", Text: "what did we say about the release?",
		ActionToken: "xoxa-the-turns-token"}
	if err := HandleEvent(context.Background(), deps, addressed); err != nil {
		t.Fatalf("HandleEvent(addressed): %v", err)
	}

	// The follow-up: a human reply in the same thread, carrying no token, as Slack actually delivers it.
	reply := slack.Event{Type: "message", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.002", InThread: true,
		SourceEventID: "Ev2", UserID: "U2", Text: "and the one before that?"}
	if err := HandleEvent(context.Background(), deps, reply); err != nil {
		t.Fatalf("HandleEvent(reply): %v", err)
	}

	grants := fp.grantsSeen()
	if len(grants) != 2 {
		t.Fatalf("the control plane was told about %d turns, want 2 — a turn that carries no action_token "+
			"must still be reported, because saying nothing leaves the previous turn's authority standing: %+v",
			len(grants), grants)
	}
	if grants[0].ActionToken != "xoxa-the-turns-token" {
		t.Fatalf("the addressed turn granted %q, want the message's own token", grants[0].ActionToken)
	}
	if grants[0].TeamID != "T1" {
		t.Fatalf("the grant named team %q, want T1 — the workspace is the pacer's key", grants[0].TeamID)
	}
	if grants[1].ActionToken != "" {
		t.Fatalf("the tokenless reply granted %q, want an empty token: an empty action_token is what "+
			"WITHDRAWS the earlier grant, and sending anything else re-authorises the search",
			grants[1].ActionToken)
	}
	if grants[1].SessionID != grants[0].SessionID {
		t.Fatalf("the withdraw named session %q but the grant named %q: a withdraw aimed at a different "+
			"session leaves the real one authorised", grants[1].SessionID, grants[0].SessionID)
	}
}

// A FAILED GRANT DOES NOT FAIL THE TURN. The tool is simply never offered and the model answers from the
// thread — which is exactly how a workspace that never granted search:read.public behaves. Losing the whole
// answer because an optional capability could not be armed would be a far worse trade.
func TestAFailedSearchGrantStillDeliversTheTurn(t *testing.T) {
	deps, fp, _ := newTestDeps(t)
	fp.failNextGrant = errors.New("the control plane refused the grant")

	ev := slack.Event{Type: "app_mention", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.001",
		SourceEventID: "Ev1", UserID: "U2", Text: "hi", ActionToken: "tok"}
	if err := HandleEvent(context.Background(), deps, ev); err != nil {
		t.Fatalf("HandleEvent: %v — a search grant this turn could not arm must not cost the answer", err)
	}
	if fp.responses != 1 {
		t.Fatalf("responses = %d, want 1: the run was never created", fp.responses)
	}
}
