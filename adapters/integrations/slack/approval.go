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
}

// blockActions is the subset of a Slack interactive block_actions payload the approval mapping reads.
type blockActions struct {
	Type string `json:"type"`
	User struct {
		ID     string `json:"id"`
		TeamID string `json:"team_id"`
	} `json:"user"`
	Team struct {
		ID string `json:"id"`
	} `json:"team"`
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
// never verify over the extracted JSON, which is not what was signed.
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
		return ApprovalIntent{
			TeamID:      team,
			UserID:      p.User.ID,
			RequestHash: a.Value,
			Decision:    decision,
			ActionID:    a.ActionID,
		}, nil
	}
	return ApprovalIntent{}, ErrNotApproval
}
