package slack

import (
	"bytes"
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
	UserID       string
	// Text is WHAT THE HUMAN WROTE, with the app's own mention removed (see stripMention). It is the only
	// field of an event that belongs in a prompt: everything beside it — channel, user, team — is scope the
	// control plane enforces server-side, and repeating scope into the conversation would invite a model to
	// treat a payload string as authority. Empty for an event that carries no text (a deletion, a bare file
	// share); the caller decides what a textless event means rather than this adapter guessing.
	Text  string
	Kind  Kind
	Retry bool // a redelivery (X-Slack-Retry-Num set) — advisory; the dedupe is on SourceEventID
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
	TS              string          `json:"ts"`
	ThreadTS        string          `json:"thread_ts"`
	Text            string          `json:"text"`
	Message         *nestedIdentity `json:"message"`          // message_changed nests the edited message here
	PreviousMessage *nestedIdentity `json:"previous_message"` // message_deleted nests the removed message here
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
		user, botID, ts, threadTS, text = n.User, n.BotID, n.TS, n.ThreadTS, n.Text
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
	return Event{
		Source:        Source,
		SourceTenant:  outer.TeamID,
		SourceEventID: outer.EventID,
		Data:          outer.Event,
		TeamID:        outer.TeamID,
		EnterpriseID:  outer.EnterpriseID,
		ChannelID:     inner.Channel,
		ThreadTS:      thread,
		UserID:        user,
		Text:          stripMention(text, botUserID),
		Kind:          classify(inner.Type, inner.Subtype),
		Retry:         retry,
	}, nil
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
