package slack

import (
	"encoding/json"
	"errors"
	"net/url"
	"time"
)

// The interactivity HTTP transport (E19 T2, spec §36, plan §3.5 row D8). Slack's interactivity Request URL
// receives a DIFFERENT body shape from the Events API, and the difference is a security boundary rather than
// a formatting detail.
//
// CONTRACT: https://docs.slack.dev/interactivity/handling-user-interaction/ (checked 2026-07-26) — payloads
// arrive "in the form application/x-www-form-urlencoded" and "The body of the request will contain a payload
// parameter; your app should parse this payload parameter as JSON." A 200 is required "within 3 seconds of
// receiving the payload".
//
// THE INFERENCE, WRITTEN DOWN WITH ITS SOURCE so a later reader does not have to re-derive it: the
// interactivity page above does NOT say that the v0 signature must be verified over the raw form body before
// the payload is deserialized. It never mentions verification at all. That requirement comes from the OTHER
// page — https://docs.slack.dev/authentication/verifying-requests-from-slack/ (checked 2026-07-26), which
// says the base string is built from "the raw request body, before it has been deserialized". Composing the
// two: the bytes Slack MACs are `payload=%7B%22type%22...`, NOT the JSON inside. So a receiver that
// url-decodes first and verifies the extracted JSON is verifying a string Slack never signed — and since no
// signature over it can ever match, the only way such a receiver "works" is by not checking at all. The order
// in the route (verify the RAW body, then extract) is forced by this composition, not by preference.

// ErrNotFormEncoded is a body that is not the documented single url-encoded `payload` parameter: a raw JSON
// body, an empty body, a form without the parameter, or a form carrying it more than once.
var ErrNotFormEncoded = errors.New("slack: interactivity body is not a single url-encoded payload parameter")

// ExtractInteractionPayload url-decodes the interactivity form body and returns the single `payload`
// parameter's JSON. It MUST run AFTER VerifySignature over the same raw bytes (see the inference above).
//
// More than one `payload` parameter is refused rather than resolved by taking the first: the MAC covers the
// WHOLE body, so a body carrying a decoy the receiver never reads is signed just as validly as one that does
// not — and "which of the two did we act on" is not a question a signature can answer afterwards.
func ExtractInteractionPayload(rawFormBody []byte) ([]byte, error) {
	values, err := url.ParseQuery(string(rawFormBody))
	if err != nil {
		return nil, ErrNotFormEncoded
	}
	payloads := values["payload"]
	if len(payloads) != 1 || payloads[0] == "" {
		return nil, ErrNotFormEncoded
	}
	return []byte(payloads[0]), nil
}

// ParseInteractionTeam reads the workspace id out of an interactivity payload so the receiver can resolve
// WHICH connection's signing secret to verify against. It is the ParseTeam sibling and carries the same
// warning: this runs BEFORE authentication, so the value is a LOOKUP KEY and never an identity. What makes it
// safe is that the resolved connection's secret must then MAC this very body.
//
// CONTRACT: https://docs.slack.dev/reference/interaction-payloads/block_actions-payload/ (checked
// 2026-07-26) — the payload carries `team: {id, domain}` and `user: {id, username, team_id}`.
//
// CEILING: enterpriseID is always empty. Enterprise Grid / org-wide install is out of scope for this phase
// (a Grid payload carries `enterprise` and `is_enterprise_install`), so a connection registered with an
// enterprise id does not resolve on this route. Stated rather than half-implemented.
func ParseInteractionTeam(payload []byte) (teamID, enterpriseID string, ok bool) {
	var probe struct {
		Team struct {
			ID string `json:"id"`
		} `json:"team"`
		User struct {
			TeamID string `json:"team_id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return "", "", false
	}
	team := probe.Team.ID
	if team == "" {
		team = probe.User.TeamID
	}
	if team == "" {
		return "", "", false
	}
	return team, "", true
}

// ParseAppRateLimited reports whether a body is Slack telling us our app has exceeded its Events API delivery
// budget, and returns the workspace + the minute the throttling started. It is an OUTER type like
// url_verification — not an event_callback — so MapEvent refuses it, and without this branch the route would
// answer 400 + x-slack-no-retry and discard the one signal that says we are being throttled.
//
// CONTRACT: https://docs.slack.dev/apis/web-api/rate-limits/ (checked 2026-07-26) — Events API delivery is
// capped at "30,000 per workspace/team per app per 60 minutes"; past that the app receives an
// `app_rate_limited` event with token, type, team_id, minute_rate_limited and api_app_id.
func ParseAppRateLimited(body []byte) (teamID string, minuteRateLimited int64, ok bool) {
	var probe struct {
		Type              string `json:"type"`
		TeamID            string `json:"team_id"`
		MinuteRateLimited int64  `json:"minute_rate_limited"`
	}
	if err := json.Unmarshal(body, &probe); err != nil || probe.Type != "app_rate_limited" {
		return "", 0, false
	}
	return probe.TeamID, probe.MinuteRateLimited, true
}

// ApprovalMessage builds the chat.postMessage body for an approval request: the authoritative detail of the
// operation plus the two minted buttons, each carrying the one-shot request hash in its `value` (which is
// what makes the resulting decision EXACT — see MapInteractiveApproval).
//
// HONEST CEILING, stated where a reader meets it: this is the MINIMUM Block Kit surface — one section of
// text and two buttons. A rich approval UI (diffs, arguments, reviewer lists, modals) is the console's job,
// not the transport's. Nothing here degrades if that is built later; it is simply not built here.
func ApprovalMessage(channel, threadTS, detail, requestHash string) []byte {
	body := map[string]any{
		"channel": channel,
		// text is the notification/fallback string, so the detail is legible without Block Kit rendering.
		"text": "Approval requested: " + detail,
		"blocks": []any{
			map[string]any{
				"type": "section",
				"text": map[string]any{"type": "mrkdwn", "text": "*Approval requested*\n" + detail},
			},
			map[string]any{
				"type": "actions",
				"elements": []any{
					map[string]any{
						"type":      "button",
						"action_id": ActionApprove,
						"text":      map[string]any{"type": "plain_text", "text": "Approve"},
						"style":     "primary",
						"value":     requestHash,
					},
					map[string]any{
						"type":      "button",
						"action_id": ActionDeny,
						"text":      map[string]any{"type": "plain_text", "text": "Deny"},
						"style":     "danger",
						"value":     requestHash,
					},
				},
			},
		},
	}
	if threadTS != "" {
		body["thread_ts"] = threadTS
	}
	raw, _ := json.Marshal(body)
	return raw
}

// MaxMessageText is how much of a run's answer one reply carries.
//
// CONTRACT: https://docs.slack.dev/changelog/2018-truncating-really-long-messages (checked 2026-07-27) — the
// `text` field of every message-posting method accepts up to 40,000 characters; beyond that Slack "may also
// automatically split the content into multiple messages". The formatting guidance
// (https://docs.slack.dev/messaging/formatting-message-text/) puts the readable ceiling far lower, at about
// 4,000, and points longer content at a file upload.
//
// So the budget below is NOT the 40,000 limit, and the reason is the SPLIT rather than the size: a message
// Slack divides into several is several visible messages with one ts, which quietly breaks the one-run
// one-message claim the delivery row is keyed on, and leaves later repairs pointing at a fragment. Truncating
// ourselves keeps one message, one handle, and one honest marker telling the reader where the whole answer
// is. Threading the overflow instead was rejected for the same reason plus a second: it multiplies a
// per-channel-rate-limited write by however long the model was feeling. A file upload needs files:write,
// which this app is not granted.
const MaxMessageText = 3900

// ThreadReply builds the chat.postMessage body that answers IN THE THREAD the question was asked in.
// threadTS is the thread root from the SLK-003 correlation, never anything a payload named.
//
// Plain text, no Block Kit: a model's answer is prose the model chose, and wrapping attacker-influenced
// text in mrkdwn blocks adds rendering surface for nothing. Slack's own mrkdwn still applies to `text`, but
// the message stays one field rather than a structure whose shape a run could steer.
//
// Over-long text is truncated with a marker naming the response, so the reader is told the answer was cut
// and where the whole of it lives — silence about truncation is how a wrong answer gets read as a complete
// one. responseID may be empty (a run with no stored response), in which case the marker just says it was
// truncated.
func ThreadReply(channel, threadTS, text, responseID string) []byte {
	// The model's words are on this path, so the broadcast tokens in them are defused before anything else
	// happens (see NeutralizeBroadcasts). It runs BEFORE the truncation below so the escape can never be cut
	// in half — a `&lt;` sliced at the marker would put a raw `<` back on the wire.
	text = NeutralizeBroadcasts(text)
	if n := len([]rune(text)); n > MaxMessageText {
		marker := "\n\n_… truncated_"
		if responseID != "" {
			marker = "\n\n_… truncated; the full answer is response " + responseID + "_"
		}
		text = string([]rune(text)[:MaxMessageText-len([]rune(marker))]) + marker
	}
	body := map[string]any{"channel": channel, "text": text}
	if threadTS != "" {
		body["thread_ts"] = threadTS
	}
	raw, _ := json.Marshal(body)
	return raw
}

// UpdateMessage builds the chat.update body that REPAIRS an already-visible message in place: same channel,
// the message's own ts, new text and no buttons (the decision is made; the buttons must not invite a second
// click). Editing the visible message is the SLK-006 repair — one message per thread, never a second post.
//
// THE TEXT AND THE PING ARE TWO ARGUMENTS (E21 T3, plan §3.6 D6), and that split is the fix rather than
// decoration. This was the second outbound path outside the broadcast defence, and what it interpolates is
// not incidental: the decision repair names a publication display built from THE RUN'S OWN BRANCH NAME, and
// `<!channel>` is a valid git ref. Neutralizing the whole string would have taken the decider's ping with it;
// keeping the whole string live left a run able to broadcast. So text is defused and mentionUserID — an id
// the caller read off a click it already authorized, never off model output — is minted HERE, after.
//
// Empty mentionUserID mints nothing, so a repair with nobody to ping is a repair with no mention.
func UpdateMessage(channel, messageTS, text, mentionUserID string) []byte {
	text = NeutralizeBroadcasts(text)
	if mentionUserID != "" {
		text += " (<@" + mentionUserID + ">)"
	}
	raw, _ := json.Marshal(map[string]any{
		"channel": channel,
		"ts":      messageTS,
		"text":    text,
		"blocks": []any{
			map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": text}},
		},
	})
	return raw
}

// ---------------------------------------------------------------------------------------------------
// E23 T4 — THE APPROVAL SCREEN BECOMES A DOCUMENT.
//
// ApprovalMessage above is the publication-shaped screen and is left BYTE-IDENTICAL: the plan keeps the
// publication display and the generic tool display separate on purpose (T3), because the publication's is
// derived from a destination and the generic one from a call, and folding them into a single "generic
// display" would drag the stronger one down to the weaker one's level.
//
// What follows is the generic one, and every widget in it traces to a measured row of plan §3.5 rather
// than to a preference:
//
//	markdown (P14, 12,000 cumulative) — the identity and the operator's one sentence.
//	table    (P13, 100 rows / 20 columns / 10,000 characters) — one row per argument. THE screen §2 asks
//	         for, and it was already built: E20 wrote the block, E21 measured the limits, E23 brings only
//	         the contents.
//	actions  (P6, max 25 elements) — three buttons where there were two. Twenty-two short of the ceiling,
//	         so the third is a choice and not a squeeze.
//	alert    (P12, 200 characters, modal-only) — E21 called this block "structurally dead" and the reason
//	         was correct: there was no modal. There is one now, so the rejection expired with its reason.
//	input    (P11, dispatch_action defaults false) — E21 refused it; E23 accepts it INSIDE the modal, for
//	         a deny reason, and never sets dispatch_action, so it dispatches nothing.
//
// AND THE REJECTIONS, one line each, each with its measurement:
//
//	card — `body` is 200 characters (P7). A tool call's arguments do not fit, and an approval screen that
//	       truncates is precisely the screen this epic exists to prevent.
//	context_actions / feedback_buttons / icon_button — they would be the first actionable element in this
//	       tree with no one-shot binding (P8/P9): a 👍 has no request hash, no publication, nothing to
//	       bind to. Having BUILT the authorization path is not a reason to hand it out.
//	carousel / container — an approval screen is a document, not a gallery (P15).
//
// CEILING, where a reader meets it: these blocks are validated SYNTACTICALLY against the published
// references, never visually (E20/E21 unchanged) — how they LOOK in a real workspace is §6 leg 1.

// ActionShowArguments is the third button. It is minted here with the other two and carries the SAME
// one-shot request hash, so the modal it opens can only ever show the call that button was minted for.
//
// It decides nothing, and MapInteractiveApproval refuses it (its switch matches only approve/deny), which
// is asserted from both directions in approval_widgets_test.go rather than trusted.
const ActionShowArguments = "palai_show_arguments"

// The modal's identifiers. callback_id names the view so a later view_submission can be recognized as
// OURS; the block/action ids name the deny-reason field inside it.
const (
	ApprovalModalCallbackID    = "palai_approval_arguments"
	ApprovalDenyReasonBlockID  = "palai_deny_reason"
	ApprovalDenyReasonActionID = "palai_deny_reason_text"
)

// The vendor's limits for the two surfaces this file mints.
//
// CONTRACT: https://docs.slack.dev/reference/block-kit/blocks/actions-block/ (§3.5 P6, fetched
// 2026-07-29) — "There is a maximum of 25 elements in each action block."
// https://docs.slack.dev/surfaces/modals/ + https://docs.slack.dev/reference/methods/views.open/ (§3.5
// P10, fetched 2026-07-29) — a modal holds "Max of 100 blocks", its title has a "max length of 24
// characters", views.open needs "_No scopes required_", and — the sentence that shapes the whole path —
// "A trigger_id will expire 3 seconds after it's sent to your app, so you'll want to use it quickly."
const (
	MaxActionElements = 25
	MaxModalBlocks    = 100
	MaxModalTitleText = 24
)

// ApprovalRequest is everything the two surfaces need, taken as the LEDGER ROW'S OWN FIELDS rather than as
// a pre-rendered screen. The rendering happens inside, once, so the message a human reads in the channel
// and the modal they open from it cannot disagree — not by discipline, by construction.
type ApprovalRequest struct {
	// ApprovalID and RequestHash are the BINDING, and the only two values that ride private_metadata.
	ApprovalID  string
	RequestHash string
	// Identity is the tool name resolved by the lookup that will EXECUTE the call — never a name off the
	// model's frame, or the human would be authorizing whatever the model chose to call it.
	Identity string
	// OperatorLabel is the one human sentence on the screen, written at registration. Empty renders as
	// NoOperatorLabel, out loud.
	OperatorLabel string
	// Arguments are the ledger row's committed bytes: what the broker will send, not what was proposed.
	Arguments []byte
	// ExpiresAt is the approval's deadline; zero means the modal carries no expiry alert.
	ExpiresAt time.Time
}

// ToolApprovalMessage builds the chat.postMessage body for a gated tool call: who is being authorized to
// do what, every argument in a table, and the three buttons.
//
// The buttons all carry the request hash for the reason ApprovalMessage's two already did — the decision
// is bound to the BYTES (tool_calls.request_hash is toolbroker.RequestHash(name, args)), so an argument
// edited after the click authorizes nothing.
func ToolApprovalMessage(channel, threadTS string, req ApprovalRequest) []byte {
	display := DeriveApprovalDisplay(req.Identity, req.OperatorLabel, req.Arguments)
	budget := MaxMarkdownText
	body := map[string]any{
		"channel": channel,
		// The fallback/notification string. The identity and the operator's label only — never an argument,
		// because a notification preview is rendered in places Block Kit is not.
		"text": "Approval requested: " + NeutralizeBroadcasts(req.Identity),
		"blocks": []any{
			markdownBlock("**Approval requested**\n`"+display.Identity+"`\n"+display.OperatorLabel, &budget),
			approvalArgumentTable(ApprovalArgumentRows(req.Arguments)),
			map[string]any{
				"type": "actions",
				"elements": []any{
					map[string]any{
						"type":      "button",
						"action_id": ActionApprove,
						"text":      map[string]any{"type": "plain_text", "text": "Approve"},
						"style":     "primary",
						"value":     req.RequestHash,
					},
					map[string]any{
						"type":      "button",
						"action_id": ActionDeny,
						"text":      map[string]any{"type": "plain_text", "text": "Deny"},
						"style":     "danger",
						"value":     req.RequestHash,
					},
					map[string]any{
						"type":      "button",
						"action_id": ActionShowArguments,
						"text":      map[string]any{"type": "plain_text", "text": "Show arguments"},
						"value":     req.RequestHash,
					},
				},
			},
		},
	}
	if threadTS != "" {
		body["thread_ts"] = threadTS
	}
	raw, _ := json.Marshal(body)
	return raw
}

// ToolApprovalModal builds the views.open body: the same call, in full.
//
// THE THREE-SECOND RULE IS THE SHAPE OF THIS FUNCTION AND NOT A COMMENT ON IT. `trigger_id` dies three
// seconds after Slack sends it (P10) and the interactivity route owes Slack a 200 in the same three
// seconds (E19), so views.open has to happen INSIDE the ack budget. That is why this takes the trigger id
// and a struct and does nothing else: it performs no I/O, makes no decision, and the caller's only job
// between the click and this call is ONE ledger read. A modal that wrote to the database would be a modal
// that missed its own trigger.
//
// interactivity_pointer is NOT used, and its absence is asserted rather than assumed: P17 could not be
// confirmed on either vendor page — whether that alternative shares trigger_id's three seconds is unknown
// — and an unconfirmed row does not enter the code. The measurement is §6 leg 2.
//
// private_metadata carries the BINDING ONLY (P18). The arguments are not in it; they are in the ledger,
// which is where this modal read them from. P18 is unconfirmed too — nobody could measure the field's
// documented size limit — and the cheapest way around an unmeasured limit is to put almost nothing in it.
func ToolApprovalModal(triggerID string, req ApprovalRequest) []byte {
	display := DeriveApprovalDisplay(req.Identity, req.OperatorLabel, req.Arguments)
	budget := MaxMarkdownText

	blocks := []any{
		markdownBlock("**Approval requested**\n`"+display.Identity+"`\n"+display.OperatorLabel, &budget),
	}
	if display.Truncated {
		blocks = append(blocks, alertBlock("warning",
			"These arguments are longer than the screen holds and were cut. The button is bound to the FULL bytes on the tool call, not to what is shown."))
	}
	if !req.ExpiresAt.IsZero() {
		blocks = append(blocks, alertBlock("info",
			"This approval expires at "+req.ExpiresAt.UTC().Format(time.RFC3339)+". After that the call is canceled and the run is released."))
	}
	blocks = append(blocks, approvalArgumentTable(ApprovalArgumentLeaves(req.Arguments)))

	// The deny reason (P11). `dispatch_action` is never set, so it defaults to false and this element
	// dispatches nothing — which is what keeps it out of the actionable family E21 refused.
	//
	// CEILING, and it is the honest one: NOTHING IN THIS TREE ROUTES view_submission TO A DECISION YET.
	// coordinator.DecideToolApproval has no production caller at all (measured 2026-07-29), so neither this
	// field nor the channel's own Deny button decides a tool approval today. The field is therefore labelled
	// and hinted for what it IS — a reason written next to a decision made on the message — rather than for
	// what a submit button would imply. The submit exists only because Slack requires one when a view holds
	// an input block, and it is named accordingly.
	blocks = append(blocks, map[string]any{
		"type":     "input",
		"block_id": ApprovalDenyReasonBlockID,
		"optional": true,
		"label":    map[string]any{"type": "plain_text", "text": "Reason for denying"},
		"hint": map[string]any{"type": "plain_text",
			"text": "The decision itself is the Approve or Deny button on the message in the channel."},
		"element": map[string]any{
			"type":        "plain_text_input",
			"action_id":   ApprovalDenyReasonActionID,
			"multiline":   true,
			"placeholder": map[string]any{"type": "plain_text", "text": "why this should not run"},
		},
	})

	if len(blocks) > MaxModalBlocks {
		blocks = blocks[:MaxModalBlocks]
	}
	metadata, _ := json.Marshal(map[string]string{
		"approval_id": req.ApprovalID, "request_hash": req.RequestHash,
	})
	raw, _ := json.Marshal(map[string]any{
		"trigger_id": triggerID,
		"view": map[string]any{
			"type":             "modal",
			"callback_id":      ApprovalModalCallbackID,
			"title":            map[string]any{"type": "plain_text", "text": modalTitle("Tool call arguments")},
			"submit":           map[string]any{"type": "plain_text", "text": "Done"},
			"close":            map[string]any{"type": "plain_text", "text": "Back"},
			"private_metadata": string(metadata),
			"blocks":           blocks,
		},
	})
	return raw
}

// modalTitle enforces the documented 24-character title (P10) here rather than trusting a literal to stay
// short: a title one character over is a view Slack refuses, and a refused view is a dead button.
func modalTitle(text string) string {
	if runes := []rune(text); len(runes) > MaxModalTitleText {
		return string(runes[:MaxModalTitleText])
	}
	return text
}

// ErrNotShowArguments is any interaction that is not our Show-arguments button carrying both a request
// hash and a usable trigger id. It opens nothing.
var ErrNotShowArguments = errors.New("slack: interaction is not a bound show-arguments action")

// ShowArgumentsIntent is a click on the third button, mapped. It carries NO decision field — not an empty
// one, not a defaulted one — because the type a value has is the cheapest place to make a whole class of
// mistake impossible: there is nothing on this struct a caller could accidentally treat as an answer.
//
// Every other field is a BINDING, and each is the same binding the decision path checks (slack_decision.go
// steps 1-3): the thread names the session, the channel is scoped, the user is checked against the
// approver list, and the request hash pins the exact call. Opening a document is a smaller act than
// deciding, but it still shows a human somebody else's arguments, so it passes the same gates.
type ShowArgumentsIntent struct {
	TeamID      string
	UserID      string
	RequestHash string
	// TriggerID is the three-second window. Empty is not mappable: calling views.open without one burns
	// budget on a request Slack will reject.
	TriggerID string
	ChannelID string
	ThreadTS  string
	MessageTS string
}

// MapShowArgumentsClick maps a VERIFIED interactive payload onto a ShowArgumentsIntent. Like
// MapInteractiveApproval it runs strictly after authentication, matches ONLY our own action id, and
// refuses anything missing a binding.
func MapShowArgumentsClick(body []byte) (ShowArgumentsIntent, error) {
	var p blockActions
	if err := json.Unmarshal(body, &p); err != nil {
		return ShowArgumentsIntent{}, ErrNotShowArguments
	}
	if p.Type != "block_actions" || p.TriggerID == "" {
		return ShowArgumentsIntent{}, ErrNotShowArguments
	}
	for _, a := range p.Actions {
		if a.ActionID != ActionShowArguments || a.Value == "" {
			continue
		}
		team := p.Team.ID
		if team == "" {
			team = p.User.TeamID
		}
		channel := p.Channel.ID
		if channel == "" {
			channel = p.Container.ChannelID
		}
		messageTS := p.Message.TS
		if messageTS == "" {
			messageTS = p.Container.MessageTS
		}
		// The thread ROOT, resolved exactly as MapInteractiveApproval and MapEvent resolve it — one rule on
		// every transport, or a click and an event in one conversation would correlate to two sessions.
		thread := p.Message.ThreadTS
		if thread == "" {
			thread = p.Container.ThreadTS
		}
		if thread == "" {
			thread = messageTS
		}
		return ShowArgumentsIntent{
			TeamID: team, UserID: p.User.ID, RequestHash: a.Value, TriggerID: p.TriggerID,
			ChannelID: channel, ThreadTS: thread, MessageTS: messageTS,
		}, nil
	}
	return ShowArgumentsIntent{}, ErrNotShowArguments
}
