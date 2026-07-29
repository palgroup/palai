package slack

import (
	"encoding/json"
	"errors"
)

// The two interactive action ids the control plane mints into an approval message's Block Kit buttons. The
// button's `value` carries the one-shot request_hash (spec §22.4) — the exact operation the decision
// authorizes. Binding the decision to a SIGNED interactive action carrying that hash is what makes a Slack
// approval EXACT (SLK-007): a plain message that says "yes" is an ordinary event (it flows through MapEvent,
// never this path) and can never approve a high-risk operation.
const (
	ActionApprove = "palai_approve"
	ActionDeny    = "palai_deny"
)

// ErrNotApproval is any interaction that is not one of our approve/deny buttons carrying a request hash: a
// different block action, a shortcut, a view_submission, or an approve button with an empty value. It
// authorizes nothing — the caller drops it (an approval decision can ONLY come from a minted button).
var ErrNotApproval = errors.New("slack: interaction is not a bound approval action")

// ApprovalIntent is a Slack interactive approval mapped onto the one-shot approval primitive. It carries
// exactly the binding coordinator.ApplyApprovalDecision consumes: the decision kind and the RequestHash that
// pins it to the exact pending operation (a stale/mismatched hash authorizes nothing there). TeamID + UserID
// identify who clicked, in which workspace — the control plane checks that principal is a mapped, authorized
// approver before enqueueing the approve/deny command; an unmapped user is a constrained actor that cannot
// approve (SLK-004). No privilege is widened here: the intent grants nothing on its own.
type ApprovalIntent struct {
	TeamID      string
	UserID      string
	RequestHash string // the one-shot binding minted into the button value
	Decision    string // "approve" | "deny"
	ActionID    string
	// ChannelID + ThreadTS are the thread the button was clicked in — the SLK-003 correlation key. The
	// decision path resolves the session from them, so a click can only decide the conversation its own
	// thread owns; the request hash alone would let a hash observed anywhere decide anything.
	ChannelID string
	ThreadTS  string
	// MessageTS is the clicked message itself — the chat.update handle the SLK-006 single repair edits.
	MessageTS string
}

// blockActions is the subset of a Slack interactive block_actions payload the approval mapping reads.
//
// CONTRACT: https://docs.slack.dev/reference/interaction-payloads/block_actions-payload/ (checked
// 2026-07-26) — the documented top-level fields include team, user, container, channel, message, actions,
// response_url and trigger_id. `response_url` is decoded by NOTHING here on purpose; see below.
type blockActions struct {
	Type string `json:"type"`
	// TriggerID is the three-second window a modal has to be opened in (§3.5 P10). It is read by
	// MapShowArgumentsClick and by nothing on the DECISION path — a decision is bound to the request hash
	// and needs no trigger, so a payload that lost one can still approve.
	TriggerID string `json:"trigger_id"`
	User      struct {
		ID     string `json:"id"`
		TeamID string `json:"team_id"`
	} `json:"user"`
	Team struct {
		ID string `json:"id"`
	} `json:"team"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	Container struct {
		ChannelID string `json:"channel_id"`
		MessageTS string `json:"message_ts"`
		ThreadTS  string `json:"thread_ts"`
	} `json:"container"`
	Message struct {
		TS       string `json:"ts"`
		ThreadTS string `json:"thread_ts"`
	} `json:"message"`
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
		Type     string `json:"type"`
	} `json:"actions"`
}

// MapInteractiveApproval maps a VERIFIED interactive payload onto an ApprovalIntent, or returns
// ErrNotApproval if the interaction is not one of our bound approve/deny buttons. The body must already have
// passed VerifySignature (HTTP transport) or arrived over an authenticated Socket Mode connection — mapping
// never runs before authentication.
//
// It matches ONLY our two minted action ids and requires a non-empty request hash in the button value, so:
//   - a decision is bound to the exact operation (the hash), the workspace (team), and the clicker (user);
//   - a bare/foreign button, a shortcut, or a view_submission authorizes nothing;
//   - message text ("yes", "approve") never reaches here — it is an event, not an interaction.
//
// Contract for the caller: Slack's HTTP interactivity transport sends the JSON as a FORM body —
// `payload=<urlencoded JSON>`, not a raw JSON body. The receiver must therefore verify the v0 signature over
// the RAW form body (the exact bytes Slack signed) and THEN url-decode + extract `payload` to pass here —
// never verify over the extracted JSON, which is not what was signed. ExtractInteractionPayload does the
// second half and interactions.go records why the ORDER is forced, with both source pages.
//
// WHY `response_url` IS NOT READ, since it is right there in the payload and is the obvious tool: it is
// usable "up to 5 times within 30 minutes of receiving the payload"
// (https://docs.slack.dev/interactivity/handling-user-interaction/, checked 2026-07-26). An approval repair
// is not bounded by either number — the run it authorizes can outlive 30 minutes, and the message may need
// editing again when the operation finally completes. A window that expires silently would turn a late
// repair into a no-op nobody notices, so the visible message is edited with chat.update against its ts
// (SLK-006), which has no expiry and is the same handle a later terminal update uses.
func MapInteractiveApproval(body []byte) (ApprovalIntent, error) {
	var p blockActions
	if err := json.Unmarshal(body, &p); err != nil {
		return ApprovalIntent{}, ErrNotApproval
	}
	if p.Type != "block_actions" {
		return ApprovalIntent{}, ErrNotApproval
	}
	for _, a := range p.Actions {
		decision := ""
		switch a.ActionID {
		case ActionApprove:
			decision = "approve"
		case ActionDeny:
			decision = "deny"
		default:
			continue
		}
		if a.Value == "" {
			// A minted button always carries the request hash; an empty value cannot pin a decision to an
			// operation, so it authorizes nothing.
			return ApprovalIntent{}, ErrNotApproval
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
		// The thread ROOT, resolved the way MapEvent resolves it: a reply carries thread_ts, and a top-level
		// message IS its own root. Same rule on both transports, or a click and an event in one conversation
		// would correlate to two different threads.
		thread := p.Message.ThreadTS
		if thread == "" {
			thread = p.Container.ThreadTS
		}
		if thread == "" {
			thread = messageTS
		}
		return ApprovalIntent{
			TeamID:      team,
			UserID:      p.User.ID,
			RequestHash: a.Value,
			Decision:    decision,
			ActionID:    a.ActionID,
			ChannelID:   channel,
			ThreadTS:    thread,
			MessageTS:   messageTS,
		}, nil
	}
	return ApprovalIntent{}, ErrNotApproval
}
