package slack

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"strings"
)

// Source is the source family Slack events dedupe within — the InboundEvent.Source value the canonical
// automation pipeline scopes the source-dedupe unique index by. Both transports (Events API HTTP callback
// and Socket Mode WebSocket) wrap the SAME event_callback body, so a transport switch never changes identity
// — that half IS documented (Socket Mode delivers the Events API payload verbatim inside an envelope,
// https://docs.slack.dev/apis/events-api/using-socket-mode/). What is NOT documented is that a RETRY repeats
// the event_id; see the D3 assumption on HeaderRetryNum.
const Source = "slack"

// Kind classifies a normalized Slack event so the mapping downstream can treat an edit as a correction and a
// delete as a tombstone rather than a fresh message (SLK-005), without decoding the opaque payload twice.
type Kind string

const (
	KindMessage    Kind = "message"    // a new user message / app_mention
	KindCorrection Kind = "correction" // message_changed — an edit supersedes, it is not a new turn
	KindTombstone  Kind = "tombstone"  // message_deleted — the prior content is retracted
	KindFileShare  Kind = "file_share" // a file was shared — a scoped fetch+scan happens control-plane-side
	KindOther      Kind = "other"      // any other subscribed event the mapping may still act on
)

var (
	// ErrIgnored is a well-formed event the adapter deliberately drops so no run is born: a bot's own
	// message or any bot event (SLK-008 — the loop guard). The caller ACKs 2xx and does nothing.
	ErrIgnored = errors.New("slack: event ignored (bot/self — loop guard)")
	// ErrNotAnEvent is a payload whose outer type is not event_callback (e.g. url_verification, which the
	// caller handles via ParseChallenge before verifying a normal event, or an unknown outer type).
	//
	// KNOWN MEMBER OF THIS SET, so T2 does not rediscover it: `app_rate_limited` arrives as an OUTER type,
	// not as an event_callback (https://docs.slack.dev/apis/web-api/rate-limits/, checked 2026-07-25 — a
	// workspace/app exceeding 30,000 deliveries per 60 minutes is told so with that payload). It therefore
	// lands here and the Events route answers 400 + x-slack-no-retry, i.e. the notification that we are being
	// throttled is discarded. Handling it (log + counter) is E19 plan §3.5 row D10, owned by T2.
	ErrNotAnEvent = errors.New("slack: payload is not an event_callback")
	// ErrMalformed is a structurally unusable envelope — non-JSON, or missing the team/event identity that
	// anchors dedupe and tenant correlation. The caller maps it to a 400 (authenticated client error), the
	// ParseInbound malformed shape.
	ErrMalformed = errors.New("slack: event envelope is malformed")
	// ErrNoRun is ErrIgnored's sibling and the whole of E20 T2's "few new Kinds" decision: a well-formed event
	// from the agent PANEL SURFACE — app_home_opened, app_context_changed — that is HANDLED but births no run.
	// It is an error rather than a Kind on purpose, and the purpose is fail-closed: every caller already has an
	// `err != nil` arm that refuses to admit, so a transport that never learns about the panel still cannot
	// turn a panel open into a run. A Kind would have needed every caller to remember a new branch.
	//
	// UNLIKE ErrIgnored, the Event IS returned alongside it, populated: the caller acknowledges the delivery
	// and logs which surface moved (Type, Tab, ChannelID). Returning a value with an error is unusual enough
	// that TestMapEventPanelSurfaceEventsBirthNoRun asserts it rather than leaving it to a reader's goodwill.
	ErrNoRun = errors.New("slack: panel surface event handled; no run is born")
)

// Event is a Slack event normalized to the canonical inbound identity PLUS the Slack correlation fields the
// downstream mapping needs. Source/SourceTenant/SourceEventID/Data ARE the webhook.InboundEvent identity —
// SourceEventID is Slack's event_id, documented as globally unique across workspaces and ASSUMED (D3, see
// HeaderRetryNum) to be repeated by a redelivery, so Slack events flow through the exact source-dedupe the
// webhook seam already proves (AUT-001/AUT-009); no parallel dedupe is invented. The correlation fields
// (team/channel/thread/user) drive thread↔session (SLK-003) and the authorization/self-loop guards; Data
// stays opaque (the mapping validates the inner event later).
type Event struct {
	Source        string          // always Source ("slack")
	SourceTenant  string          // team_id — the workspace the event belongs to
	SourceEventID string          // Slack event_id — the redelivery-stable dedupe key
	Data          json.RawMessage // the inner event object, opaque to this adapter

	TeamID       string
	EnterpriseID string
	ChannelID    string
	ThreadTS     string // the thread root (thread_ts, or the message ts when it starts a thread) — the correlation key
	// MessageTS is the AFFECTED MESSAGE's own ts, and it is the handle a correction and a tombstone act on:
	// ThreadTS cannot serve, because inside a thread it names the ROOT, so retracting by it would retract the
	// wrong turn (or the whole conversation's first one).
	//
	// For an edit or a delete it is the ORIGINAL message's ts, read off the nested object — NOT the change
	// event's own top-level ts, which is a different number. That is what makes it a handle at all: an edit
	// does not change a message's ts, so the ts a correction names is the ts the original event carried.
	//
	// IT HAS A SECOND READER, and it wants the same field for the same reason: reading a thread the app was
	// invited into late (slack_thread.go) gets the triggering message back along with the earlier ones, and
	// quoting the human's own words at them as "earlier context" is confusing. This is what identifies the turn
	// to skip — an id comparison rather than a text heuristic. ThreadTS cannot serve there either: inside a
	// thread it is the root's.
	MessageTS string
	// InThread is whether Slack's OWN `thread_ts` was present, i.e. this message belongs to a thread rather
	// than standing alone at the top of a channel. ThreadTS cannot answer that: it falls back to the message's
	// own ts so the correlation key always exists, which makes a lone message and a thread root identical.
	//
	// IT IS THE RUN-BIRTH RULE'S SECOND HALF (see slackBirthsRun): a channel message with no mention births a
	// run only when it lands in a thread this app already has a session for. Without this field the check would
	// admit the `message.channels` TWIN of every top-level app_mention — Slack delivers both for one mention,
	// and by the time the twin arrives the mention has already correlated the thread, so "is this thread ours?"
	// answers yes and one message becomes two runs.
	InThread bool
	UserID   string
	// Type is the INNER event type verbatim ("app_mention", "message", "app_home_opened", …). Kind is the
	// coarse classification the run-shaping branches on; this is the identity a log line and the panel's
	// no-run branch need in order to name what arrived, and neither should be inferring it back out of Data.
	Type string
	// ChannelType is Slack's own `channel_type` on a message event — "im" for a direct message, "channel" for
	// a public channel, and so on. It is the ONLY authority for "this conversation is a DM" (see IsDM): the
	// "D…" id prefix is a convention no Slack page commits to, and a scope exemption may not rest on one.
	// Empty for events that carry no channel_type; empty is NOT a DM.
	ChannelType string
	// Tab is app_home_opened's `tab` — "home" or "messages". Only "messages" is the agent panel; the App Home
	// *home* tab is a separate surface this app subscribes to no behaviour for. Empty on every other event.
	Tab string
	// Text is WHAT THE HUMAN WROTE, with the app's own mention removed (see stripMention). It is the only
	// field of an event that belongs in a prompt: everything beside it — channel, user, team — is scope the
	// control plane enforces server-side, and repeating scope into the conversation would invite a model to
	// treat a payload string as authority. Empty for an event that carries no text (a deletion, a bare file
	// share); the caller decides what a textless event means rather than this adapter guessing.
	Text string
	// Context is app_context.entities — what the PERSON is looking at — in Slack's own relevance order,
	// filtered to this event's OWN workspace. It is DATA, and the whole of E20 T3 is what may be done with
	// it: it is recorded and described, NEVER resolved. See ContextEntity.
	//
	// Transport-invariant for free, and that is why it is populated HERE: both shipped transports call
	// MapEvent over byte-identical bodies (api/slack.go for the HTTP callback, slack_socket.go for the
	// Socket Mode envelope), so neither can carry a context the other does not.
	Context []ContextEntity
	// Files is what the message SHARED, as the payload declared it — the metadata half of SLK-005's deferred
	// file leg. It is populated whatever the subtype says, and that is the point: an @mention with an
	// attachment arrives as app_mention with NO subtype and a DM upload as message.im, so keying the parse on
	// subtype "file_share" (which is what Kind does) would see neither. Kind still classifies as it always
	// did; this field simply does not depend on it.
	//
	// EVERY FIELD BUT THE ID IS THE UPLOADER'S CLAIM. See SharedFile: the mimetype and name decide only
	// whether a file is worth fetching, and never what the bytes are.
	Files []SharedFile
	Kind  Kind
	// ActionToken authorises a Real-time Search call made with the BOT token, in the API's own words: "All
	// API calls made using a bot token require an `action_token`." It arrives with the message that started
	// the run, which is a property worth keeping rather than working around — the search is bound to THAT
	// conversation instead of being a standing power the app holds.
	//
	// IT IS A CREDENTIAL, and it is treated as one everywhere downstream: never logged, never written to a
	// table, never in argv, never in evidence. Its lifetime is UNDOCUMENTED (plan §3.5 M20a), which is the
	// reason it is never persisted — storing a credential whose expiry you cannot know is keeping something
	// you cannot tell has become rubbish.
	ActionToken string
	Retry       bool // a redelivery (X-Slack-Retry-Num set) — advisory; the dedupe is on SourceEventID
}

// ContextEntityChannel is the ONE entity type Slack's agent pages document with a shape we can read: `value`
// is a channel id string. Every other type's value shape is undocumented on those pages (S20 found one whose
// value is an OBJECT), so no other type is described into a prompt — see slackContextNote.
const ContextEntityChannel = "slack#/types/channel_id"

// ContextEntity is one item of app_context: a thing the human has on screen.
//
// THE AUTHORITY BOUNDARY, and it is why this type exists rather than a map[string]any: the context tells us
// what the USER is looking at, while the run executes with the CONNECTION PRINCIPAL's authority. Those are
// different parties. So an entity may never (1) select a tenant — org/project come only from the resolved
// connection, (2) select a run target — default_policy, (3) widen allowed_channels, or (4) become a FETCH
// TARGET. The fourth is the heaviest: this app holds `channels:history`, so reading a channel because a
// context said the user is looking at it would hand the user the connection's authority — a confused-deputy
// read primitive. Nothing in this repo resolves an entity, and TestSlackNoCodePathResolvesAContextEntity
// keeps it that way by scanning for the method names that would.
//
// Value is populated ONLY when Slack's `value` is a JSON string. A structured value (S20) leaves it empty on
// purpose: the sole use for an invented flat id would be to resolve it.
type ContextEntity struct {
	Type   string
	Value  string
	TeamID string
}

// eventCallback is the Events API outer envelope. Only the fields the mapping needs are decoded; the inner
// event stays a RawMessage so the opaque payload is not re-parsed here.
type eventCallback struct {
	Type         string          `json:"type"`
	TeamID       string          `json:"team_id"`
	EnterpriseID string          `json:"enterprise_id"`
	APIAppID     string          `json:"api_app_id"`
	EventID      string          `json:"event_id"`
	Event        json.RawMessage `json:"event"`
	// ActionToken is read at BOTH levels for the same reason app_context is (S19), and the contradiction is
	// again Slack's own: the Real-time Search API page says "an app can obtain the action_token needed to
	// query the Real-time Search API from either a `message` event or an `app_mention` event", while
	// app_mention's reference prints an example payload whose fields are exactly {type,user,text,ts,channel,
	// event_ts} — no action_token anywhere (plan §3.5 M20b, both checked 2026-07-27). No page says which
	// level carries it. Both are read, the inner one wins, and being wrong is SILENT in the other direction:
	// a token that never arrives looks exactly like a workspace that granted no search scope.
	ActionToken string `json:"action_token"`
}

// innerEvent is the subset of the inner event object the mapping reads to classify + correlate. bot_id (or
// a user equal to the app's own bot user) marks a self/bot event the loop guard drops.
//
// message_changed / message_deleted are special: Slack nests the AUTHOR identity (user/bot_id/ts/thread_ts)
// inside a `message` (edit) / `previous_message` (delete) object, leaving those fields empty at top level.
// nestedIdentity captures that object so the loop guard and thread-root resolution read the real values —
// otherwise the app's own SLK-006 repair (a chat.update → message_changed carrying the BOT identity nested)
// would slip past the guard and re-trigger a run.
type innerEvent struct {
	Type            string          `json:"type"`
	Subtype         string          `json:"subtype"`
	User            string          `json:"user"`
	BotID           string          `json:"bot_id"`
	Channel         string          `json:"channel"`
	ChannelType     string          `json:"channel_type"` // "im" on a DM (message.im) — see Event.ChannelType
	Tab             string          `json:"tab"`          // app_home_opened only: "home" | "messages"
	TS              string          `json:"ts"`
	ThreadTS        string          `json:"thread_ts"`
	Text            string          `json:"text"`
	Message         *nestedIdentity `json:"message"`          // message_changed nests the edited message here
	PreviousMessage *nestedIdentity `json:"previous_message"` // message_deleted nests the removed message here
	// TWO NAMES FOR ONE FIELD, and this is plan divergence S19 rather than defensive coding: the 2026-07-02
	// changelog says "the `app_context` is now sent to message.im and app_home_opened", the developing-agents
	// page says the same context is "called simply `context` in the latter", app_context_changed's own
	// reference documents `context`, and message.im's example payload shows NEITHER. No page states which key
	// message.im uses. Both are read; whichever is present wins. Being wrong in the other direction is silent
	// — a context that never arrives looks exactly like a user looking at nothing.
	AppContext *appContext `json:"app_context"`
	Context    *appContext `json:"context"`
	// See eventCallback.ActionToken: the same field is read at both levels because the two Slack pages that
	// mention it disagree about where it lives.
	ActionToken string `json:"action_token"`
	// Files is the shared-file array. Decoded on EVERY inner event, not just subtype file_share: Slack puts
	// it on app_mention and on message.im too, where there is no subtype to key on.
	Files []struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		MimeType string `json:"mimetype"`
		Size     int64  `json:"size"`
		// url_private_download is the download endpoint; url_private renders in a browser. Both need the
		// bot token and `files:read` (https://docs.slack.dev/messaging/working-with-files/, checked
		// 2026-07-27). The download form is preferred and the other is the fallback, because a file object
		// missing url_private_download but carrying url_private is still fetchable.
		URLPrivateDownload string `json:"url_private_download"`
		URLPrivate         string `json:"url_private"`
	} `json:"files"`
}

// appContext is the context object as published. `value` stays raw because its shape depends on the entity
// type (S20: `slack#/types/channel_id` is a string, `slack#/types/message_context` is an object) and a typed
// `string` here would fail the WHOLE inner event's decode for one structured entity.
type appContext struct {
	Entities []struct {
		Type   string          `json:"type"`
		Value  json.RawMessage `json:"value"`
		TeamID string          `json:"team_id"`
	} `json:"entities"`
}

// nestedIdentity is the author + thread correlation (and the words) carried inside a message_changed /
// message_deleted event.
type nestedIdentity struct {
	User     string `json:"user"`
	BotID    string `json:"bot_id"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts"`
	Text     string `json:"text"`
}

// ParseChallenge returns the url_verification challenge, if the body is that handshake. Slack POSTs it once
// when a Request URL is configured; the receiver echoes the challenge back in plaintext. The token field is
// the deprecated verification token and is ignored. A non-handshake body returns ("", false).
func ParseChallenge(body []byte) (string, bool) {
	var probe struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", false
	}
	if probe.Type != "url_verification" {
		return "", false
	}
	return probe.Challenge, true
}

// ParseTeam reads the workspace identity out of an UNVERIFIED body. It exists because the receiver has a
// chicken-and-egg: the v0 signature can only be checked against the signing secret of the connection the
// callback belongs to, and the only thing naming that connection is the payload itself. So this read happens
// strictly BEFORE authentication and its result is a LOOKUP KEY AND NOTHING ELSE — never an identity, never a
// tenant selector, never trusted. What makes that safe is the ORDER that follows it: the resolved connection's
// secret must then verify the signature over this very body, so a forged team_id can at most select a
// connection whose secret the forger does not hold, and the verify refuses.
//
// ok is false when the body carries no team id (non-JSON, a url_verification handshake — which has no
// team_id at all — or a truncated envelope): with no lookup key there is no secret to verify against, and the
// caller must refuse rather than guess a connection.
func ParseTeam(body []byte) (teamID, enterpriseID string, ok bool) {
	var probe struct {
		TeamID       string `json:"team_id"`
		EnterpriseID string `json:"enterprise_id"`
	}
	if err := json.Unmarshal(body, &probe); err != nil || probe.TeamID == "" {
		return "", "", false
	}
	return probe.TeamID, probe.EnterpriseID, true
}

// MapEvent normalizes a verified Events API event_callback body into the canonical Event. botUserID is the
// app's own bot user id (from the connection registry): an inner event whose user IS the bot, or any event
// carrying a bot_id, is ErrIgnored so the app never answers itself (SLK-008). retry is whether Slack marked
// this a redelivery (X-Slack-Retry-Num) — recorded as advisory; identity is the event_id, so a retry
// deduplicates against the original regardless.
//
// The body MUST already have passed VerifySignature: mapping runs strictly after authentication, never
// before, so a forged payload is rejected before it is decoded.
func MapEvent(body []byte, botUserID string, retry bool) (Event, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	var outer eventCallback
	if err := dec.Decode(&outer); err != nil {
		return Event{}, ErrMalformed
	}
	if outer.Type != "event_callback" {
		return Event{}, ErrNotAnEvent
	}
	// team_id + event_id anchor tenant correlation and dedupe; without either the event is unroutable.
	if outer.TeamID == "" || outer.EventID == "" {
		return Event{}, ErrMalformed
	}
	var inner innerEvent
	if len(outer.Event) > 0 {
		if err := json.Unmarshal(outer.Event, &inner); err != nil {
			return Event{}, ErrMalformed
		}
	}
	// message_changed / message_deleted carry the author + thread INSIDE a nested object, not top-level, so
	// resolve identity from there for those subtypes. Every downstream check (loop guard, thread root, user
	// authz) must run against the nested values or it acts on empty top-level fields.
	user, botID, ts, threadTS, text := inner.User, inner.BotID, inner.TS, inner.ThreadTS, inner.Text
	if n := inner.Message; n != nil {
		user, botID, ts, threadTS, text = n.User, n.BotID, n.TS, n.ThreadTS, n.Text
	} else if n := inner.PreviousMessage; n != nil {
		// THE TEXT IS DELIBERATELY NOT TAKEN. previous_message.text is the message the human just DELETED, and
		// nothing downstream has any business with those words: a deletion retracts a turn (it names it by ts),
		// it does not say anything. Carrying them would put the retracted sentence one field access away from a
		// prompt — and it did, live: the removed text used to reach slackRunInput. Text's own contract has said
		// "empty for a deletion" since E17 T1; this is the line that makes it true.
		user, botID, ts, threadTS = n.User, n.BotID, n.TS, n.ThreadTS
		text = ""
	}

	// Loop guard (SLK-008): a bot event, or the app's own bot user, is dropped BEFORE a run can be born —
	// otherwise the app's own posted reply (or its own SLK-006 edit) would re-trigger it. Checked here (not
	// by the caller) so every transport shares one guard.
	if botID != "" || (botUserID != "" && user == botUserID) {
		return Event{}, ErrIgnored
	}

	thread := threadTS
	if thread == "" {
		thread = ts // a top-level message starts its own thread; ts is the correlation root
	}
	ev := Event{
		Source:        Source,
		SourceTenant:  outer.TeamID,
		SourceEventID: outer.EventID,
		Data:          outer.Event,
		TeamID:        outer.TeamID,
		EnterpriseID:  outer.EnterpriseID,
		ChannelID:     inner.Channel,
		ThreadTS:      thread,
		MessageTS:     ts,
		InThread:      threadTS != "",
		UserID:        user,
		Type:          inner.Type,
		ChannelType:   inner.ChannelType,
		Tab:           inner.Tab,
		Text:          stripMention(text, botUserID),
		Context:       contextEntities(inner, outer.TeamID),
		Files:         sharedFiles(inner),
		Kind:          classify(inner.Type, inner.Subtype),
		ActionToken:   cmp.Or(inner.ActionToken, outer.ActionToken),
		Retry:         retry,
	}
	if noRun(inner.Type) {
		return ev, ErrNoRun
	}
	return ev, nil
}

// IsDM reports whether the event came from a direct message with the app — the ONE fact E20 T2's scope
// widening turns on (SlackAuthorizationPolicy.ChannelAllowed exempts a DM from allowed_channels, because a
// DM's scope is Slack's own invitation model: whoever can DM the bot is already a workspace member).
//
// CONTRACT: https://docs.slack.dev/reference/events/message.im/ (checked 2026-07-27) — the message.im inner
// event carries "channel_type":"im" beside a "D…" channel id, and the event requires the `im:history` scope.
// The FIELD is the authority, never the id prefix: no Slack page commits to "D…" meaning DM, and an
// exemption resting on an undocumented prefix is an exemption resting on nothing.
func (e Event) IsDM() bool { return e.ChannelType == "im" }

// contextEntities normalizes app_context into the ordered, own-workspace-only list Event carries.
//
// THE WORKSPACE FILTER RUNS HERE, at map time, and that placement is the guarantee: an entity from another
// team must "not even reach the description", which is only structural if the value never exists in our
// types at all — a filter one layer up is a filter someone can forget to call. An entity with NO team_id is
// dropped for the same reason a scope that cannot be checked is not met (ChannelAllowed's rule): a workspace
// we cannot prove is ours is not ours.
//
// CONTRACT: https://docs.slack.dev/reference/events/app_context_changed/ (checked 2026-07-27) — each entity
// is {type, value, team_id} and "the entities are ordered by relevance". The order is Slack's; we preserve
// it and make no claim about it (see the honest ceiling on slackContextNote).
func contextEntities(inner innerEvent, teamID string) []ContextEntity {
	ctx := inner.AppContext
	if ctx == nil || len(ctx.Entities) == 0 {
		ctx = inner.Context // S19: the same object under the other published name
	}
	if ctx == nil || teamID == "" {
		return nil
	}
	var out []ContextEntity
	for _, e := range ctx.Entities {
		if e.TeamID == "" || e.TeamID != teamID {
			continue
		}
		var value string
		// A non-string value (S20) leaves this empty rather than erroring: the entity is still RECORDED, it
		// is simply not describable, and inventing a flat id for it would only ever serve a resolver.
		_ = json.Unmarshal(e.Value, &value)
		out = append(out, ContextEntity{Type: e.Type, Value: value, TeamID: e.TeamID})
	}
	return out
}

// noRun names the agent panel's SURFACE events — the ones that describe what a human is LOOKING AT rather
// than something they said. They carry no message, so admitting one would spend a model call on an empty
// prompt, and they arrive every time the panel is opened.
//
// CONTRACT: https://docs.slack.dev/ai/developing-agents/ (checked 2026-07-27) — the agent messaging
// experience subscribes to app_home_opened, app_context_changed and message.im. Only the third is a
// conversation. app_home_opened requires no scopes at all
// (https://docs.slack.dev/reference/events/app_home_opened/, checked 2026-07-27) and app_context_changed
// carries neither a channel nor a user (https://docs.slack.dev/reference/events/app_context_changed/,
// checked 2026-07-27), which is a second way of saying neither one is a turn in a conversation.
//
// message.im is DELIBERATELY ABSENT from this list: a DM message is a message, so it keeps KindMessage /
// KindCorrection / KindTombstone and travels the unchanged admission bridge.
func noRun(innerType string) bool {
	switch innerType {
	case "app_home_opened", "app_context_changed":
		return true
	}
	return false
}

// stripMention removes the app's OWN mention from a message and trims what is left. A user addressing the
// bot writes "<@Ubot> merhaba"; the mention is how Slack routes the message to us, not part of what was
// said, and leaving it in makes the model's first token a user id it has no business reasoning about.
//
// CONTRACT: https://docs.slack.dev/messaging/formatting-message-text/ (checked 2026-07-27) — a user mention
// is "<@" + the user id + ">", optionally "<@Uxxx|label>" in older payloads. Only the id equal to botUserID
// is removed: another person's mention is CONTENT ("ask <@U999> about it" is a sentence), and a run whose
// prompt silently lost a name would be answering a different question than the one asked.
//
// An unterminated "<@Ubot" is left alone — it is then literal text a human typed, not a mention.
// botUserID empty (a connection registered without one) strips nothing, which is fail-soft by construction:
// the worst case is the model reading one extra token, never a lost message.
func stripMention(text, botUserID string) string {
	if text == "" || botUserID == "" {
		return strings.TrimSpace(text)
	}
	open := "<@" + botUserID
	var b strings.Builder
	for {
		i := strings.Index(text, open)
		if i < 0 {
			break
		}
		// The match must END the mention here — "<@Ubot>" or "<@Ubot|label>" — or "<@Ubotany" would eat a
		// DIFFERENT user whose id merely starts with ours.
		rest := text[i+len(open):]
		end := strings.IndexByte(rest, '>')
		if end < 0 || rest == "" || (rest[0] != '>' && rest[0] != '|') {
			break // not our mention (or unterminated): the rest is literal text
		}
		head, tail := text[:i], rest[end+1:]
		// Take ONE adjacent space with the mention, the way deleting a word does: "hey <@Ubot> ship" must
		// become "hey ship", not "hey  ship" and not "heyship". Only a plain space is eaten — a newline or a
		// tab is layout the human wrote (a code block, a list), and flattening it would rewrite the message.
		switch {
		case strings.HasPrefix(tail, " "):
			tail = tail[1:]
		case strings.HasSuffix(head, " "):
			head = head[:len(head)-1]
		}
		b.WriteString(head)
		text = tail
	}
	b.WriteString(text)
	return strings.TrimSpace(b.String())
}

// classify maps a Slack (type, subtype) pair onto the coarse Kind the downstream mapping branches on. An
// edit and a delete are their own kinds so a correction supersedes rather than starting a fresh turn and a
// tombstone retracts (SLK-005), instead of both being treated as new messages.
// sharedFiles normalizes the inner event's `files` array, keeping payload order (the fetch caller's
// candidate selection is order-stable, and that stability is what keeps a redelivery a replay rather than an
// idempotency conflict). A file with no id is dropped: there is nothing to identify or attribute it by.
//
// The download URL is url_private_download where present, url_private otherwise. Both are fetched the same
// way and both are checked against the Slack file-host allow-list before the token is presented (FetchImage).
func sharedFiles(inner innerEvent) []SharedFile {
	if len(inner.Files) == 0 {
		return nil
	}
	out := make([]SharedFile, 0, len(inner.Files))
	for _, f := range inner.Files {
		if f.ID == "" {
			continue
		}
		download := f.URLPrivateDownload
		if download == "" {
			download = f.URLPrivate
		}
		out = append(out, SharedFile{ID: f.ID, Name: f.Name, MimeType: f.MimeType, Size: f.Size, DownloadURL: download})
	}
	return out
}

func classify(typ, subtype string) Kind {
	switch subtype {
	case "message_changed":
		return KindCorrection
	case "message_deleted":
		return KindTombstone
	case "file_share":
		return KindFileShare
	}
	switch typ {
	case "message", "app_mention":
		return KindMessage
	default:
		return KindOther
	}
}
