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
		// Set HERE because chat.startStream is the only call that accepts it, and because it OPENS THE
		// STREAM IN CHUNK MODE — a whole-stream decision, not a formatting hint (see slack.StartStream). It
		// is why every method below sends chunks: on this stream `markdown_text` is refused.
		//
		// PLAN rather than TIMELINE, because it is the difference between a finished answer preceded by a
		// column of ticks and one preceded by a single collapsed container. Measured on the same card
		// sequence (2026-08-04): timeline keeps four loose `task_card` blocks, plan keeps one `plan` block
		// holding the same four tasks, and — the part no page states — a plan-mode stream that draws NO tasks
		// keeps NO container at all, so the body-only run E08 makes every real single-step run into is
		// unchanged by this.
		TaskDisplayMode: slack.TaskDisplayModePlan,
	})
}

// AppendStream carries the MODEL'S OWN WORDS, and on a chunk-mode stream even those must travel as a chunk —
// `markdown_text` is refused with streaming_mode_mismatch. The relay above is unaware of any of this: it
// still calls AppendStream with a string, and the mode stays the wiring's business.
func (s *channelSlackStream) AppendStream(ctx context.Context, channel, ts, markdownText string) error {
	return slack.AppendStreamChunks(ctx, s.doer, s.apiBase, s.token, channel, ts,
		[]map[string]any{slack.MarkdownChunk(markdownText)})
}

// StopStream closes the stream, flushing any text the relay never managed to deliver. It goes through the
// CHUNK closer for the reason StopStreamChunks documents: closing a chunk stream with markdown_text does not
// close it, and a stream that never closes reads as permanently "streaming" (SLK-P2).
func (s *channelSlackStream) StopStream(ctx context.Context, channel, ts, markdownText string) error {
	var chunks []map[string]any
	if markdownText != "" {
		chunks = append(chunks, slack.MarkdownChunk(markdownText))
	}
	return slack.StopStreamChunks(ctx, s.doer, s.apiBase, s.token, channel, ts, chunks)
}

func (s *channelSlackStream) UpdateTask(ctx context.Context, channel, ts string, task slack.Task) error {
	chunk := slack.TaskUpdateChunk(task)
	if chunk == nil {
		return nil // unrenderable (no title and no id) — TaskUpdateChunk's own refusal, not a wire error
	}
	return slack.AppendStreamChunks(ctx, s.doer, s.apiBase, s.token, channel, ts, []map[string]any{chunk})
}

// RunFailedHook is what happens to the error a background relay.Run returns.
//
// IT EXISTS BECAUSE THAT ERROR WAS ONCE DISCARDED — `_ = Run(...)` — and on 2026-08-03 that cost a real
// answer: a run completed on the control plane with its text journalled, and the Slack thread kept saying
// "Working…" forever while the bot's log stayed completely silent for the whole episode. There was nothing
// to read, so there was nothing to fix.
//
// LIKE ApprovalHook IT RETURNS NOTHING, and for the same kind of reason: it fires on the goroutine startRun
// handed to RunInBackground, long after HandleEvent returned to a caller that has already acknowledged the
// Slack envelope. There is nobody left to return an error to and nothing left to retry into — the whole of
// the available response is to say it out loud where an operator reads. Production (apps/slack-bot/dispatch.go)
// does exactly that.
//
// The three ids ride alongside the error because the error itself cannot carry them: relay.Run wraps its
// failures with the SESSION id only, and an operator looking at a thread that stopped needs the channel and
// thread ts to find the session, not the other way round.
type RunFailedHook func(err error, sessionID, channel, threadTS string)

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
	// OnApproval is handed straight to every relay.Run this file starts (see Deps.ApprovalHook). It is
	// forwarded rather than called here because an approval request arrives on the RUN's event stream,
	// which only Run reads — HandleEvent has returned long before the first one lands.
	OnApproval ApprovalHook
	// OnRunFailed is what happens to the error relay.Run returns. Required, for the reason RunFailedHook's
	// own doc gives.
	OnRunFailed RunFailedHook

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

	// Images is the optional image leg (images.go). NIL IS A SUPPORTED DEPLOYMENT, unlike every field
	// above it: with no leg the relay behaves exactly as it did before images existed, so validate() does
	// not require one. What it must never do is fail quietly — see ImageLeg.Ready and its boot line.
	Images *ImageLeg
	// Logf is where the image leg's per-file refusals go. Optional; nil discards them, which is what a
	// test that is not asserting on logs wants.
	Logf func(string, ...any)

	state *inboundState
}

// NewInboundDeps builds InboundDeps with its dedupe/thread-liveness state initialized — the one piece
// a struct literal cannot set (state is unexported) and HandleEvent requires.
func NewInboundDeps(
	store ThreadStore,
	client Palai,
	newSlack func(recipientUserID, recipientTeamID string) Slack,
	runInBackground func(func()),
	onApproval ApprovalHook,
	onRunFailed RunFailedHook,
	botID, botUserID, agentRevisionID, repositoryBindingID string,
) InboundDeps {
	return InboundDeps{
		Store: store, Palai: client, NewSlack: newSlack, RunInBackground: runInBackground,
		OnApproval: onApproval, OnRunFailed: onRunFailed,
		BotID: botID, BotUserID: botUserID,
		AgentRevisionID: agentRevisionID, RepositoryBindingID: repositoryBindingID,
		state: newInboundState(),
	}
}

// WithImages mounts the image leg (images.go) and the log sink its per-file refusals go to.
//
// IT IS A BUILDER RATHER THAN TWO MORE PARAMETERS ON NewInboundDeps, and the reason is the one the old
// in-process bridge's WithFileFetch gave for the same shape: this half is genuinely optional, so a
// constructor that demanded it would force every caller that does not fetch images — every test of the
// text path, and any deployment without the leg — to pass two nils to say "as before".
func (deps InboundDeps) WithImages(leg *ImageLeg, logf func(string, ...any)) InboundDeps {
	deps.Images, deps.Logf = leg, logf
	return deps
}

// logf is the image leg's log sink, defaulted to a discard so a caller that set none does not crash inside
// a fetch failure — which would turn "one image did not attach" into "the whole turn was lost".
func (deps InboundDeps) logf(format string, args ...any) {
	if deps.Logf != nil {
		deps.Logf(format, args...)
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
	case deps.OnApproval == nil:
		return errors.New("relay: InboundDeps needs OnApproval — a run that parks on a human nobody is asked is a hang")
	case deps.OnRunFailed == nil:
		return errors.New("relay: InboundDeps needs OnRunFailed — a run whose failure nobody reports is a thread that stops with no trace anywhere")
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

// dedupeKey is the SLK-P5 dedupe identity: (team, channel, messageTS) — messageTS is slack.Event's own
// MessageTS, the AFFECTED MESSAGE's ts (MapEvent populates it on every event,
// adapters/integrations/slack/inbound.go:90,365), NOT event_id and NOT the message text.
//
// event_id differs between an app_mention and its message.channels twin because they are two
// DELIVERIES of one message; messageTS does not, because it is a property of the message ITSELF, and
// two genuinely different messages never share a ts. An earlier version of this key hashed the text
// instead — that has a real collision a human hits: typing the same short reply twice in one thread
// ("yes", "ok", "retry", answering two different bot questions) would have silently swallowed the
// second one, with no run, no error, nothing on screen. messageTS has no such collision.
func dedupeKey(teamID, channelID, messageTS string) string {
	return teamID + "|" + channelID + "|" + messageTS
}

// isNotFound reports whether err is the SDK's typed 404 — the shape a call against a session id
// ThreadStore is still holding, but that Palai has since closed or forgotten, comes back as (Task 8's
// package doc: "an ORPHANED row is the expected steady state").
func isNotFound(err error) bool {
	var apiErr *palai.APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

// birthsRun is THE RUN-BIRTH RULE, restored from the old in-process bridge's slackBirthsRun
// (apps/control-plane/internal/extensions/slack_admit.go) — this relay shares no code with it, but must
// not diverge from what it decided. Without this rule ANY message in a channel the app is invited into
// becomes a turn: the app subscribes to Slack's message.channels, so every human talking in that channel
// silently starts a Palai run whether or not they ever addressed the bot. That was this relay's own bug
// until this function existed — HandleEvent's Kind filter above only asked "is this shaped like a
// message", never "was it said TO us".
//
// correlated is exactly slackBirthsRun's `ours`: whether THIS bot already has a session bound to ev's
// thread (deps.Store.SessionForThread's own `found`), regardless of whether that session is still alive
// server-side — a dead one is recovered downstream by the orphan path (rebindOrphan) that already
// existed, not by this check, so the thread stays "ours" through that recovery exactly as it did before.
//
// THE RULE, mirroring slackBirthsRun's three live clauses (its first, for corrections/tombstones, has no
// counterpart here: HandleEvent's Kind filter has already dropped both before this runs):
//
//   - A DM IS ALWAYS A TURN — Slack's own channel_type is the authority (ev.IsDM()), never a channel id
//     prefix. A screenshot or question dragged into a DM has nothing else it could be addressed to.
//   - AN app_mention IS ALWAYS A TURN — somebody typed the bot's name; that is the invitation, and it
//     needs no prior relationship with the thread.
//   - ANYTHING ELSE IS A TURN ONLY AS A FOLLOW-UP: it must be inside a Slack thread (ev.InThread — Slack's
//     own thread_ts was present, see MapEvent) AND that thread must already be one of ours (correlated).
//     Both halves are load-bearing. Without `correlated`, every reply in every thread in a channel the
//     app sits in opens a run. Without `InThread`, a top-level message that merely reuses an old thread's
//     root ts as its own correlation key (HandleEvent's threadKey, built off ev.ThreadTS regardless of
//     InThread) would qualify too.
//
// A file share takes no clause of its own, for the same reason slackBirthsRun's doc gives: SLK-005
// classifies it so the fetch can be scoped, which is a statement about what to DO with the file, not
// about whether the human was talking. It follows the ordinary rule above — dropped into a DM, always a
// turn; dropped into a thread this bot already holds, a turn; dropped into a channel this bot merely sits
// in, nothing.
func birthsRun(ev slack.Event, correlated bool) bool {
	switch {
	case ev.IsDM(), ev.Type == "app_mention":
		return true
	default:
		return ev.InThread && correlated
	}
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

	// Only KindMessage becomes a turn — every other Kind is dropped here, each for its own reason, not
	// one blanket "not a message":
	//   - KindCorrection (SLK-005): message_changed supersedes a prior turn rather than starting a new
	//     one — the human revised what they already said, they did not say something new.
	//   - KindTombstone (SLK-005): message_deleted retracts a turn — again nothing new was said.
	//   - KindOther: anything MapEvent did not classify as a conversational turn at all.
	//
	// KindFileShare IS ADMITTED, and only when it actually carries files. It used to be dropped here with
	// the others, on the reading that a bare file share was handled "control-plane-side" — which was true
	// of the OLD in-process bridge and is not true of this process, so the drop had become a silent hole:
	// a screenshot uploaded into a DM (Slack's `message.im` with subtype `file_share`) reached this line
	// and stopped, and the person who dropped it in got no answer at all.
	//
	// A file share IS a message with an attachment — Slack's subtype is how a client renders it, not a
	// different kind of turn — so it takes the same path. The `len(ev.Files) > 0` guard is what keeps this
	// from being a wider change than it says: a file_share with nothing on it admits nothing new.
	//
	// It cannot double-answer the ordinary case. An @mention carrying an attachment arrives as
	// `app_mention` with NO subtype (KindMessage) and its `message.channels` twin arrives as file_share;
	// both carry the same message ts, so the dedupe above collapses them exactly as it already collapses
	// the twin of a text-only mention — whichever lands first becomes the turn, and both carry Files.
	if ev.Kind != slack.KindMessage && !(ev.Kind == slack.KindFileShare && len(ev.Files) > 0) {
		return nil
	}

	// MessageTS is expected to be non-empty for every KindMessage event (Slack always sends a ts on a
	// message-shaped event); IF it somehow is not, dedupe is SKIPPED rather than applied with an empty
	// key — an empty key would collapse every such event in the channel into one dedupe slot, silently
	// swallowing real messages, which is worse than occasionally admitting an undeduped twin.
	if ev.MessageTS != "" {
		key := dedupeKey(ev.TeamID, ev.ChannelID, ev.MessageTS)
		if !deps.state.markSeen(key) {
			return nil // the app_mention/message.channels twin of a message already handled
		}
	}

	tk := threadKey{botID: deps.BotID, teamID: ev.TeamID, channelID: ev.ChannelID, threadTS: ev.ThreadTS}

	// The correlation read has to happen BEFORE a session can be opened, not after: the old shape here
	// (resolveSession) minted a session for every thread's first event unconditionally, mention or DM or
	// not — which is the literal bug birthsRun exists to close. See birthsRun.
	sessionID, correlated, err := deps.Store.SessionForThread(ctx, tk.botID, tk.teamID, tk.channelID, tk.threadTS)
	if err != nil {
		return fmt.Errorf("relay: resolve thread session: %w", err)
	}
	if !birthsRun(ev, correlated) {
		return nil // ordinary channel chatter the bot was never addressed in — see birthsRun
	}
	if !correlated {
		sessionID, err = deps.openNewSession(ctx, tk)
		if err != nil {
			return err
		}
	}

	if deps.state.isActive(tk) {
		// A steer carries a STRING, so a picture attached to it cannot ride along; the text says so rather
		// than the file vanishing. See steerImageNote for the contract that makes this a ceiling.
		steerText := ev.Text
		if len(ev.Files) > 0 {
			steerText += steerImageNote
		}
		_, err := deps.Palai.Steer(ctx, sessionID, palai.SteerParams{Message: steerText})
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
	// The images are fetched and uploaded ONCE, before the create, and the resulting input is reused on the
	// orphan retry below. Fetching inside the retry instead would pay Slack twice and — worse — mint a
	// SECOND set of artifacts for one message, leaving the first set attached to nothing.
	//
	// THE ARTIFACTS ARE WRITTEN BEFORE THE RUN IS CREATED, which is forced rather than tidy: the input has
	// to NAME them, so they must have ids by the time it is built. The control plane closes the other half
	// — it attaches every artifact the input names inside the same transaction that inserts the run, so
	// there is no window where the bytes exist attached to nothing (see coordinator.AdmitResponse).
	var artifactIDs []string
	var skipped int
	if len(ev.Files) > 0 {
		artifactIDs, skipped = deps.Images.attach(ctx, ev.Files, deps.logf)
	}
	input := runInput(ev.Text, artifactIDs, skipped)

	_, err := deps.createResponse(ctx, sessionID, input)
	if isNotFound(err) {
		sessionID, err = deps.rebindOrphan(ctx, tk, sessionID)
		if err != nil {
			return err
		}
		_, err = deps.createResponse(ctx, sessionID, input)
	}
	if err != nil {
		return fmt.Errorf("relay: create response for session %s: %w", sessionID, err)
	}

	// ONE CONTEXT FOR THE WHOLE RELAY, and it is detached from the caller's — the event stream and the Run
	// that drains it are two halves of one thing that outlives this call, so they cannot be opened under
	// two different lifetimes.
	//
	// THE STREAM WAS THE HALF THAT GOT THIS WRONG, and it cost a whole answer on 2026-08-03. Only the Run
	// call below was detached; the stream above it was opened with the caller's ctx, and the caller is
	// socket.dispatch, whose `defer cancel()` fires the INSTANT HandleEvent returns — which is before the
	// goroutine has read a single event. *palai.SessionEventStream reads its connection under the context
	// it was opened with (sdks/go/sessionevents.go: streamCtx derives from that ctx), and a cancelled one
	// closes its items channel, which Next() reports as io.EOF.
	//
	// io.EOF IS RUN'S CLEAN EXIT, which is what made this invisible rather than merely broken: Run stopped
	// at its first Next(), closed the Slack message it had just opened with nothing appended, and returned
	// NIL. The thread kept saying "Working…" while the run completed server-side with its answer
	// journalled, and no error existed anywhere to be logged — the report below could not have caught this
	// one, because there was nothing to report.
	//
	// Measured, against the live control plane on the very session that failed (21 events, 7 carrying
	// text): opened with the caller's ctx and cancelled the way dispatch cancels it, the stream delivers 0
	// events and ends in io.EOF; opened detached, it delivers all 21.
	//
	// Nothing leaks: Run closes the stream on every exit path (its own `defer deps.Events.Close()`), and
	// Close cancels the stream's context, so the detached lifetime ends exactly when the run does.
	relayCtx := context.WithoutCancel(ctx)

	stream, err := deps.Palai.SessionEvents(relayCtx, sessionID, palai.EventsParams{AfterSequence: deps.state.lastSequence(tk)})
	if err != nil {
		return fmt.Errorf("relay: open session %s event stream: %w", sessionID, err)
	}

	runDeps := Deps{
		Events:     &sequenceTrackingStream{EventStream: stream, record: func(seq int64) { deps.state.setLastSequence(tk, seq) }},
		Slack:      deps.NewSlack(ev.UserID, ev.TeamID),
		OnApproval: deps.OnApproval,
	}
	// Set BEFORE handing off to RunInBackground, not inside the deferred goroutine: a caller that
	// supplies a SYNCHRONOUS RunInBackground (a test not exercising the steer path) would otherwise
	// see isActive still false immediately after HandleEvent returns, which is backwards — the run
	// this call just opened genuinely IS the thread's live one from this point on.
	deps.state.setActive(tk, true)
	deps.RunInBackground(func() {
		defer deps.state.setActive(tk, false)
		if runErr := Run(relayCtx, runDeps, sessionID, ev.ChannelID, ev.ThreadTS); runErr != nil {
			deps.OnRunFailed(runErr, sessionID, ev.ChannelID, ev.ThreadTS)
		}
	})
	return nil
}

// createResponse builds and sends the turn's Responses.Create call — call #2 of the two-call turn
// this file's doc comment describes. AgentRevisionID/Repository are omitted (not sent as empty
// strings) when the bot's row does not carry them, so a caller inspecting the wire body sees an
// absent field, matching what "the server's own comment" (types.go) says an absence means.
//
// input is `any` because §25.10 makes it two shapes: a bare string for a text-only turn (what this relay
// has always sent) and a content array when the turn carries images. runInput decides which; this call
// stays indifferent to it.
func (deps InboundDeps) createResponse(ctx context.Context, sessionID string, input any) (*palai.Response, error) {
	req := palai.ResponseCreateRequest{
		Input:     input,
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
