// This file is Task 9 (2026-08-03 plan): the other half of the seam Run's own doc comment names —
// "does not open the Socket Mode connection (Task 9), does not decide which session a Slack thread
// maps to (Task 8)". HandleEvent is where an already-decoded, already-authenticated Slack event
// (adapters/integrations/slack's MapEvent output — decoding the Socket Mode / Events API wire and the
// self/bot loop guard both stay that package's job, upstream of this one) becomes a Palai turn.
//
// THE BINDING SEAM, measured against the running control plane before this file was written: a
// session carries neither an agent nor a repository — SessionWrite is {AutoApprovePublications,
// AutoApproveTools, Name}, DisallowUnknownFields, so agent_revision_id at session creation is a live
// 400. Both ride the RESPONSE instead (types.go's widened ResponseCreateRequest). So a Slack turn is
// TWO calls, not one:
//
//  1. Sessions.Create({Name})                                          -> session_id (first message only)
//  2. Responses.Create({SessionID, AgentRevisionID, Repository, Input}) -> the run
//
// and turn #2 recurs for every later message in the thread — as a fresh Responses.Create if the
// thread's last run has already finished, or as a Sessions.Steer into it if a relay.Run goroutine is
// still draining it (see InboundDeps.state below; that liveness is this PROCESS's own knowledge, not
// a fact this file could read back from the server).
package relay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"

	slack "github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/apps/slack-bot/internal/store"
	palai "github.com/palgroup/palai/sdks/go"
)

// ThreadStore is Task 8's store, narrowed to the three methods HandleEvent calls — the same
// "narrow to what this file needs" shape EventStream/Slack (relay.go) already use for the SDK/Slack
// surfaces.
type ThreadStore interface {
	SessionForThread(ctx context.Context, botID, teamID, channelID, threadTS string) (string, bool, error)
	BindThread(ctx context.Context, botID, teamID, channelID, threadTS, sessionID string) (string, error)
	// RebindThread is a compare-and-swap: the write lands only if the row still names oldSessionID.
	// won=false means a concurrent recovery on the SAME orphaned thread got there first — see
	// rebindOrphan, the one caller.
	RebindThread(ctx context.Context, botID, teamID, channelID, threadTS, oldSessionID, newSessionID string) (bool, error)
}

// var _ ThreadStore = (*store.Store)(nil) pins the two shapes together at COMPILE time. Without it,
// ThreadStore and *store.Store can drift silently: this file's interface briefly kept RebindThread's
// pre-review two-return-value signature after Task 8's review widened the real store to a
// compare-and-swap, and nothing caught it — no production call site wires the two together yet (that
// is a later task's job), so `go build ./...` stayed clean through the whole drift. This line is the
// only thing standing in for that missing call site until one exists.
var _ ThreadStore = (*store.Store)(nil)

// Palai is the SDK surface HandleEvent drives, flattened out of the client's nested Sessions/
// Responses resource groups so a test can substitute a fake with no HTTP round trip — the CreateResponse
// method takes the WIDENED ResponseCreateRequest (types.go: AgentID, AgentRevisionID, Repository) this
// task added, since neither of those three fields existed before it.
type Palai interface {
	CreateSession(ctx context.Context, p palai.CreateSessionParams) (*palai.Session, error)
	Steer(ctx context.Context, sessionID string, p palai.SteerParams) (*palai.Command, error)
	CreateResponse(ctx context.Context, req palai.ResponseCreateRequest) (*palai.Response, error)
	SessionEvents(ctx context.Context, sessionID string, p palai.EventsParams) (EventStream, error)
}

// palaiClient adapts *palai.Client's nested resource groups to Palai. Production constructs
// InboundDeps.Palai with this; a test constructs its own fake instead.
type palaiClient struct{ c *palai.Client }

// NewPalaiClient wraps a real SDK client as the Palai seam HandleEvent drives.
func NewPalaiClient(c *palai.Client) Palai { return palaiClient{c} }

func (p palaiClient) CreateSession(ctx context.Context, params palai.CreateSessionParams) (*palai.Session, error) {
	return p.c.Sessions.Create(ctx, params)
}

func (p palaiClient) Steer(ctx context.Context, sessionID string, params palai.SteerParams) (*palai.Command, error) {
	return p.c.Sessions.Steer(ctx, sessionID, params)
}

func (p palaiClient) CreateResponse(ctx context.Context, req palai.ResponseCreateRequest) (*palai.Response, error) {
	return p.c.Responses.Create(ctx, req)
}

func (p palaiClient) SessionEvents(ctx context.Context, sessionID string, params palai.EventsParams) (EventStream, error) {
	return p.c.Sessions.Events(ctx, sessionID, params)
}

// NewChannelSlackStreamer builds the InboundDeps.NewSlack closure a real deployment uses: each call
// returns a relay.Slack bound to Slack's chat.*Stream methods (adapters/integrations/slack/stream.go)
// with the ONE thing Run's own Slack seam deliberately does not carry (S9, relay.go's Slack doc) —
// the stream's recipient — closed over. StartStream refuses before ever calling Slack when either
// recipient id is empty (stream.go's ErrNoStreamRecipient), so this must be filled from the inbound
// event's own UserID/TeamID, never invented.
func NewChannelSlackStreamer(doer slack.Doer, apiBase string, token []byte) func(recipientUserID, recipientTeamID string) Slack {
	return func(recipientUserID, recipientTeamID string) Slack {
		return &channelSlackStream{
			doer: doer, apiBase: apiBase, token: token,
			recipientUserID: recipientUserID, recipientTeamID: recipientTeamID,
		}
	}
}

// channelSlackStream implements relay.Slack over adapters/integrations/slack/stream.go's package
// functions, for one thread's recipient identity.
type channelSlackStream struct {
	doer                             slack.Doer
	apiBase                          string
	token                            []byte
	recipientUserID, recipientTeamID string
}

func (s *channelSlackStream) StartStream(ctx context.Context, channel, threadTS, markdownText string) (string, error) {
	return slack.StartStream(ctx, s.doer, s.apiBase, s.token, slack.StreamStart{
		Channel: channel, ThreadTS: threadTS,
		RecipientUserID: s.recipientUserID, RecipientTeamID: s.recipientTeamID,
		MarkdownText: markdownText,
	})
}

func (s *channelSlackStream) AppendStream(ctx context.Context, channel, ts, markdownText string) error {
	return slack.AppendStream(ctx, s.doer, s.apiBase, s.token, channel, ts, markdownText)
}

func (s *channelSlackStream) StopStream(ctx context.Context, channel, ts, markdownText string) error {
	return slack.StopStream(ctx, s.doer, s.apiBase, s.token, channel, ts, markdownText, nil)
}

// InboundDeps is HandleEvent's whole seam: Task 8's store (thread<->session), the SDK calls a Slack
// turn drives, a way to open a relay.Slack for one thread's recipient, the concurrency primitive
// relay.Run's own drain runs under, the bot's own identity, and its configured agent/repository (from
// its bots-registry row — never an environment variable, see apps/slack-bot/internal/config).
//
// RunInBackground is the seam a test needs to be deterministic: production supplies
// `func(f func()) { go f() }`; a test that does not care how a turn's stream plays out supplies a
// synchronous call so every assertion after HandleEvent returns already reflects it, and a test that
// DOES care (whether a second message steers into a still-open run) supplies its own goroutine wrapper
// with a completion signal instead of a sleep.
//
// Every field is required; HandleEvent refuses rather than doing half a job with a nil one, mirroring
// relay.Run's own Deps (relay.go).
type InboundDeps struct {
	Store           ThreadStore
	Palai           Palai
	NewSlack        func(recipientUserID, recipientTeamID string) Slack
	RunInBackground func(func())

	// BotID is this bot's own bots-registry row id (c.Bots.Get) — the key ThreadStore partitions by,
	// so two bots in the same Slack thread never share a session (Task 8's own requirement).
	BotID string
	// BotUserID is the self-loop guard: an inbound event whose UserID equals this is the bot's own
	// message and is dropped before it can become a turn.
	BotUserID string
	// AgentRevisionID / RepositoryBindingID come off the bot's registry row's config. Either may be
	// empty — an agent-less request is the server's call to refuse or default, and an empty
	// RepositoryBindingID is a deliberate non-coding bot, not an error (see types.go's
	// ResponseCreateRequest doc on what an ABSENT binding_id means for a repository-bound one).
	AgentRevisionID     string
	RepositoryBindingID string

	state *inboundState
}

// NewInboundDeps builds InboundDeps with its dedupe/thread-liveness state initialized — the one piece
// a struct literal cannot set (state is unexported) and HandleEvent requires.
func NewInboundDeps(
	store ThreadStore,
	client Palai,
	newSlack func(recipientUserID, recipientTeamID string) Slack,
	runInBackground func(func()),
	botID, botUserID, agentRevisionID, repositoryBindingID string,
) InboundDeps {
	return InboundDeps{
		Store: store, Palai: client, NewSlack: newSlack, RunInBackground: runInBackground,
		BotID: botID, BotUserID: botUserID,
		AgentRevisionID: agentRevisionID, RepositoryBindingID: repositoryBindingID,
		state: newInboundState(),
	}
}

func (deps InboundDeps) validate() error {
	switch {
	case deps.Store == nil:
		return errors.New("relay: InboundDeps needs a Store")
	case deps.Palai == nil:
		return errors.New("relay: InboundDeps needs a Palai")
	case deps.NewSlack == nil:
		return errors.New("relay: InboundDeps needs NewSlack")
	case deps.RunInBackground == nil:
		return errors.New("relay: InboundDeps needs RunInBackground")
	case deps.state == nil:
		return errors.New("relay: InboundDeps needs state — build it with NewInboundDeps")
	case deps.BotID == "":
		return errors.New("relay: InboundDeps needs a BotID")
	}
	return nil
}

// threadKey identifies one Slack thread within one bot — the exact composite ThreadStore partitions
// by (Task 8), and the key this file's own in-memory liveness tracking (inboundState) partitions by
// too, so the two never disagree about which thread an event belongs to.
type threadKey struct{ botID, teamID, channelID, threadTS string }

// threadRun is what this PROCESS knows about a thread's most recent run: whether a relay.Run
// goroutine is still draining it, and the highest session-event sequence that goroutine has consumed
// so far. Neither fact is recoverable from the server — active-ness is purely this process's own
// in-flight state, and the sequence cursor exists only so the NEXT run in the same session can resume
// the journal from where the last one stopped reading (see startRun).
type threadRun struct {
	active  bool
	lastSeq int64
}

// inboundState is InboundDeps' own mutable half — kept out of the (copied-by-value) struct's other
// fields via a pointer, so every HandleEvent call sharing one InboundDeps value sees the same map.
type inboundState struct {
	mu   sync.Mutex
	seen map[string]struct{}
	// seenOrder is seen's insertion order, so markSeen can evict the oldest key once maxDedupeKeys is
	// reached instead of growing this map forever across a long-lived process.
	seenOrder []string
	threads   map[threadKey]threadRun
}

// maxDedupeKeys bounds the in-memory dedupe set. SLK-P5's twin arrives within the same delivery burst
// (milliseconds apart), so a modest bound is plenty — eviction only ever drops keys old enough that a
// genuine redelivery this far out is not the twin this guard exists to collapse anyway.
const maxDedupeKeys = 4096

func newInboundState() *inboundState {
	return &inboundState{seen: make(map[string]struct{}), threads: make(map[threadKey]threadRun)}
}

// markSeen reports whether key is new, recording it if so. SLK-P5: Slack delivers one top-level
// @mention as BOTH app_mention and its message.channels twin, each with its OWN event_id — so the
// dedupe key here is deliberately NOT event_id (see dedupeKey).
func (s *inboundState) markSeen(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.seen[key]; ok {
		return false
	}
	if len(s.seenOrder) >= maxDedupeKeys {
		oldest := s.seenOrder[0]
		s.seenOrder = s.seenOrder[1:]
		delete(s.seen, oldest)
	}
	s.seen[key] = struct{}{}
	s.seenOrder = append(s.seenOrder, key)
	return true
}

func (s *inboundState) isActive(tk threadKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.threads[tk].active
}

func (s *inboundState) setActive(tk threadKey, active bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.threads[tk]
	r.active = active
	s.threads[tk] = r
}

func (s *inboundState) lastSequence(tk threadKey) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.threads[tk].lastSeq
}

func (s *inboundState) setLastSequence(tk threadKey, seq int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.threads[tk]
	if seq > r.lastSeq {
		r.lastSeq = seq
	}
	s.threads[tk] = r
}

// resetSequence drops a thread's remembered cursor. Called whenever a thread (re)binds to a NEW
// session (openNewSession, rebindOrphan): a sequence number is scoped to ONE session's own event
// journal, so carrying the old session's cursor into the new one would skip the new session's own
// early events as if they were already consumed.
func (s *inboundState) resetSequence(tk threadKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.threads[tk]
	r.lastSeq = 0
	s.threads[tk] = r
}

// dedupeKey is the SLK-P5 dedupe identity: (team, channel, thread_ts, text-hash) — never event_id,
// which differs between an app_mention and its message.channels twin for the SAME human message.
func dedupeKey(teamID, channelID, threadTS, text string) string {
	sum := sha256.Sum256([]byte(text))
	return teamID + "|" + channelID + "|" + threadTS + "|" + hex.EncodeToString(sum[:])
}

// isNotFound reports whether err is the SDK's typed 404 — the shape a call against a session id
// ThreadStore is still holding, but that Palai has since closed or forgotten, comes back as (Task 8's
// package doc: "an ORPHANED row is the expected steady state").
func isNotFound(err error) bool {
	var apiErr *palai.APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

// HandleEvent turns one already-decoded, already-authenticated Slack event into a Palai turn: it
// drops the app's own messages and a redelivered twin, resolves (or opens) the thread's session, and
// either steers a live run or starts a new one — draining that run's events into the Slack thread on
// its own goroutine so this call returns as soon as the turn has been durably ACCEPTED by Palai, not
// once the run has finished.
func HandleEvent(ctx context.Context, deps InboundDeps, ev slack.Event) error {
	if err := deps.validate(); err != nil {
		return err
	}

	// The self-loop guard: MapEvent already drops the app's own posts and any bot_id event upstream of
	// this call using the CONNECTION registry's botUserID, but the BOT registry row (this file's own
	// identity) is a separate record, so this file keeps the same guard rather than trusting a caller
	// it does not control to have applied an equivalent one.
	if ev.UserID != "" && ev.UserID == deps.BotUserID {
		return nil
	}

	// SLK-005: an edit supersedes a prior turn and a deletion retracts one — neither is a new thing a
	// human said, so neither should ever reach Palai as fresh input.
	if ev.Kind != slack.KindMessage {
		return nil
	}

	key := dedupeKey(ev.TeamID, ev.ChannelID, ev.ThreadTS, ev.Text)
	if !deps.state.markSeen(key) {
		return nil // the app_mention/message.channels twin of a message already handled
	}

	tk := threadKey{botID: deps.BotID, teamID: ev.TeamID, channelID: ev.ChannelID, threadTS: ev.ThreadTS}

	sessionID, err := deps.resolveSession(ctx, tk)
	if err != nil {
		return err
	}

	if deps.state.isActive(tk) {
		_, err := deps.Palai.Steer(ctx, sessionID, palai.SteerParams{Message: ev.Text})
		switch {
		case err == nil:
			return nil // delivered into the still-open run
		case !isNotFound(err):
			return fmt.Errorf("relay: steer session %s: %w", sessionID, err)
		}
		// Orphan (Task 8's expected steady state): the session this process believes is still
		// streaming no longer exists server-side. Recover and fall through to starting a fresh run.
		sessionID, err = deps.rebindOrphan(ctx, tk, sessionID)
		if err != nil {
			return err
		}
		deps.state.setActive(tk, false)
	}

	return deps.startRun(ctx, tk, sessionID, ev)
}

// resolveSession returns tk's bound session, opening one via Sessions.Create + BindThread if this is
// the thread's first event.
func (deps InboundDeps) resolveSession(ctx context.Context, tk threadKey) (string, error) {
	sessionID, found, err := deps.Store.SessionForThread(ctx, tk.botID, tk.teamID, tk.channelID, tk.threadTS)
	if err != nil {
		return "", fmt.Errorf("relay: resolve thread session: %w", err)
	}
	if found {
		return sessionID, nil
	}
	return deps.openNewSession(ctx, tk)
}

// openNewSession opens a session for a thread that has never been bound, and BINDS it — never
// overwriting a concurrent winner (BindThread's own ON CONFLICT DO NOTHING semantics, Task 8).
func (deps InboundDeps) openNewSession(ctx context.Context, tk threadKey) (string, error) {
	sess, err := deps.Palai.CreateSession(ctx, palai.CreateSessionParams{Name: tk.channelID + "/" + tk.threadTS})
	if err != nil {
		return "", fmt.Errorf("relay: create session: %w", err)
	}
	bound, err := deps.Store.BindThread(ctx, tk.botID, tk.teamID, tk.channelID, tk.threadTS, sess.ID)
	if err != nil {
		return "", fmt.Errorf("relay: bind thread: %w", err)
	}
	deps.state.resetSequence(tk)
	return bound, nil
}

// rebindOrphan opens a REPLACEMENT session for a thread whose stored session (oldSessionID — the
// exact value the caller just got a 404 against, never anything else) no longer resolves server-side,
// and tries to move the thread's mapping onto it via RebindThread's compare-and-swap.
//
// Two events on the SAME orphaned thread can reach this call at once: both read the same oldSessionID,
// both open a new Palai session here, and both call RebindThread. Only one write lands (won=true); the
// loser's session is a live one on Palai's side that this call just orphaned itself. The PLATFORM CAN
// close it — close_session is a real, accepted command kind (apps/control-plane/api/commands.go:341)
// — but this SDK does not surface it (Sessions only has Create/Steer/Events; `grep -n
// 'close_session\|CloseSession' sdks/go/*.go` is empty), and adding it is not this task's scope. So the
// best THIS CODE can do today is never bind or use the loser's session: re-read the mapping and hand
// the WINNER's session back to the caller instead — still correct (the thread ends up talking to A
// session either way), just not the one this particular call minted. The loser's session stays live and
// unreferenced on Palai's side until Sessions.Close exists to close it.
func (deps InboundDeps) rebindOrphan(ctx context.Context, tk threadKey, oldSessionID string) (string, error) {
	sess, err := deps.Palai.CreateSession(ctx, palai.CreateSessionParams{Name: tk.channelID + "/" + tk.threadTS})
	if err != nil {
		return "", fmt.Errorf("relay: recreate orphaned session: %w", err)
	}
	won, err := deps.Store.RebindThread(ctx, tk.botID, tk.teamID, tk.channelID, tk.threadTS, oldSessionID, sess.ID)
	if err != nil {
		return "", fmt.Errorf("relay: rebind orphaned thread: %w", err)
	}
	if !won {
		winnerID, found, selErr := deps.Store.SessionForThread(ctx, tk.botID, tk.teamID, tk.channelID, tk.threadTS)
		if selErr != nil {
			return "", fmt.Errorf("relay: resolve winning session after a lost rebind race: %w", selErr)
		}
		if !found {
			return "", fmt.Errorf("relay: lost a rebind race on %s/%s but no row was found afterward", tk.channelID, tk.threadTS)
		}
		deps.state.resetSequence(tk)
		return winnerID, nil
	}
	deps.state.resetSequence(tk)
	return sess.ID, nil
}

// startRun opens a new run against sessionID and hands its events to Run on a background goroutine.
// A 404 on the create (the session ThreadStore resolved is itself orphaned — the same failure mode
// rebindOrphan handles for Steer, reached here when NO run was active in this process, e.g. a restart)
// recovers the same way: a fresh session, rebound, retried once.
func (deps InboundDeps) startRun(ctx context.Context, tk threadKey, sessionID string, ev slack.Event) error {
	_, err := deps.createResponse(ctx, sessionID, ev.Text)
	if isNotFound(err) {
		sessionID, err = deps.rebindOrphan(ctx, tk, sessionID)
		if err != nil {
			return err
		}
		_, err = deps.createResponse(ctx, sessionID, ev.Text)
	}
	if err != nil {
		return fmt.Errorf("relay: create response for session %s: %w", sessionID, err)
	}

	stream, err := deps.Palai.SessionEvents(ctx, sessionID, palai.EventsParams{AfterSequence: deps.state.lastSequence(tk)})
	if err != nil {
		return fmt.Errorf("relay: open session %s event stream: %w", sessionID, err)
	}

	runDeps := Deps{
		Events: &sequenceTrackingStream{EventStream: stream, record: func(seq int64) { deps.state.setLastSequence(tk, seq) }},
		Slack:  deps.NewSlack(ev.UserID, ev.TeamID),
	}
	// Set BEFORE handing off to RunInBackground, not inside the deferred goroutine: a caller that
	// supplies a SYNCHRONOUS RunInBackground (a test not exercising the steer path) would otherwise
	// see isActive still false immediately after HandleEvent returns, which is backwards — the run
	// this call just opened genuinely IS the thread's live one from this point on.
	deps.state.setActive(tk, true)
	deps.RunInBackground(func() {
		defer deps.state.setActive(tk, false)
		// context.WithoutCancel: ctx may end (a socket dispatch loop's own request scope) the instant
		// HandleEvent returns, but the run this starts outlives that scope by design — the same reason
		// relay.go's own stop() detaches from ctx to close the Slack message it opened.
		_ = Run(context.WithoutCancel(ctx), runDeps, sessionID, ev.ChannelID, ev.ThreadTS)
	})
	return nil
}

// createResponse builds and sends the turn's Responses.Create call — call #2 of the two-call turn
// this file's doc comment describes. AgentRevisionID/Repository are omitted (not sent as empty
// strings) when the bot's row does not carry them, so a caller inspecting the wire body sees an
// absent field, matching what "the server's own comment" (types.go) says an absence means.
func (deps InboundDeps) createResponse(ctx context.Context, sessionID, text string) (*palai.Response, error) {
	req := palai.ResponseCreateRequest{
		Input:     text,
		SessionID: &sessionID,
	}
	if deps.AgentRevisionID != "" {
		agentRevisionID := deps.AgentRevisionID
		req.AgentRevisionID = &agentRevisionID
	}
	if deps.RepositoryBindingID != "" {
		req.Repository = map[string]any{"binding_id": deps.RepositoryBindingID}
	}
	return deps.Palai.CreateResponse(ctx, req)
}

// sequenceTrackingStream wraps a session's EventStream and records the sequence of the last event it
// delivered. The NEXT run started in the SAME session (startRun) resumes the journal from that
// sequence rather than from 0 — opening from 0 would replay the run that just finished, whose own
// terminal event Run would see FIRST and stop on immediately, never reaching the new run's events.
type sequenceTrackingStream struct {
	EventStream
	record func(sequence int64)
}

func (s *sequenceTrackingStream) Next() (palai.Event, error) {
	event, err := s.EventStream.Next()
	if err == nil {
		s.record(int64(event.Sequence))
	}
	return event, err
}
