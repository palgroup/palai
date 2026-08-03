// This file is Task 10 (2026-08-03 plan): the approval bridge. A gated tool call or a gated
// publication parks a run and journals approval.requested.v1 (packages/coordinator/approvals.go
// RequestToolApproval, packages/coordinator/publication.go RequestPublication); this file turns that
// event into an interactive Slack message, and a click on that message's Approve/Deny button into a
// decision through the SDK's /v1/approvals surface (c.Approvals.List/Approve/Deny — sdks/go/approvals.go).
//
// THE BRIEF'S OWN SKETCH NAMED TWO THINGS THAT DO NOT EXIST, and both are corrected here rather than
// followed, per this tree's own rule that a plan's claims are re-measured, not trusted (see
// docs/superpowers/plans's "a plan claim must be executable"):
//
//  1. There is no slack.BuildApprovalBlocks. What exists and IS exported is
//     slack.ToolApprovalMessage(channel, threadTS string, req ApprovalRequest) []byte
//     (adapters/integrations/slack/interactions.go) — it derives the screen via
//     slack.DeriveApprovalDisplay and slack.ApprovalArgumentRows internally and renders the full
//     chat.postMessage body, buttons included. approvalArgumentTable, mentioned as "exposed" in this
//     task's brief, is in fact unexported (`func approvalArgumentTable` — lowercase — blocks.go);
//     ToolApprovalMessage is the highest-level exported builder and this file uses that rather than
//     re-deriving the table by hand.
//  2. There is no slack.Interaction type. The real output of a verified interactivity payload is
//     slack.ApprovalIntent, produced by slack.MapInteractiveApproval — OnButton takes that instead.
//
// THE THIRD THING THE BRIEF GOT RIGHT AND WORTH RESTATING HERE: ToolApprovalMessage mints a THIRD
// button, "Show arguments" (slack.ActionShowArguments), that opens a modal via views.open. This file
// does not wire a handler for it — OnButton only ever branches on approve/deny — so that button will
// render on every message this file posts and do nothing observable when clicked. It decides nothing
// (MapInteractiveApproval's own doc: "It decides nothing"), so leaving it unhandled is not a security
// gap, only an incomplete affordance; wiring views.open is out of this task's scope.
package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	slack "github.com/palgroup/palai/adapters/integrations/slack"
	palai "github.com/palgroup/palai/sdks/go"
)

// ApprovalRequestedEventType is the event OnApprovalRequested handles. It is journaled
// byte-identically in name (never in payload shape — see OnApprovalRequested's own doc) for a gated
// tool call and a gated publication. This is this package's OWN copy of the coordinator's unexported
// approvalRequestedEvent constant, mirroring how relay.go's runTerminalEvents already mirrors rather
// than imports the SSE endpoint's own terminal set — the const is unexported on the other side of that
// import edge too.
const ApprovalRequestedEventType = "approval.requested.v1"

// ApprovalsPalai is the /v1/approvals surface OnApprovalRequested/OnButton drive, narrowed from
// *palai.Client's Approvals resource group the same way relay.Palai (inbound.go) narrows
// Sessions/Responses — so a test substitutes a fake with no HTTP round trip.
type ApprovalsPalai interface {
	ListApprovals(ctx context.Context, p palai.ListApprovalsParams) (*palai.Page[palai.Approval], error)
	ApproveApproval(ctx context.Context, id string, p palai.DecisionParams) (*palai.ApprovalDecisionResult, error)
	DenyApproval(ctx context.Context, id string, p palai.DecisionParams) (*palai.ApprovalDecisionResult, error)
}

// approvalsPalaiClient adapts *palai.Client's Approvals resource group to ApprovalsPalai, mirroring
// palaiClient (inbound.go).
type approvalsPalaiClient struct{ c *palai.Client }

// NewApprovalsPalaiClient wraps a real SDK client as the ApprovalsPalai seam this file drives.
func NewApprovalsPalaiClient(c *palai.Client) ApprovalsPalai { return approvalsPalaiClient{c} }

func (p approvalsPalaiClient) ListApprovals(ctx context.Context, params palai.ListApprovalsParams) (*palai.Page[palai.Approval], error) {
	return p.c.Approvals.List(ctx, params)
}

func (p approvalsPalaiClient) ApproveApproval(ctx context.Context, id string, params palai.DecisionParams) (*palai.ApprovalDecisionResult, error) {
	return p.c.Approvals.Approve(ctx, id, params)
}

func (p approvalsPalaiClient) DenyApproval(ctx context.Context, id string, params palai.DecisionParams) (*palai.ApprovalDecisionResult, error) {
	return p.c.Approvals.Deny(ctx, id, params)
}

// ApprovalSlack is the outbound half of the bridge: posting an approval's message and repairing it in
// place once a decision is known — the two chat.* calls this bridge needs, each body already carrying
// its own channel/ts (built by slack.ToolApprovalMessage / slack.UpdateMessage), narrowed the same
// way relay.Slack (relay.go) narrows the same package for the streaming path.
type ApprovalSlack interface {
	// PostMessage sends body (built by buildApprovalMessage) and returns the ts Slack assigns —
	// unused by this file today (a decided click carries its own message ts, see OnButton) but
	// returned rather than discarded because a caller integrating this into a store IS the kind of
	// thing a later task does with it.
	PostMessage(ctx context.Context, body []byte) (ts string, err error)
	// UpdateMessage repairs the message body already names (channel + ts baked in by
	// slack.UpdateMessage) — the SLK-006 single-repair pattern.
	UpdateMessage(ctx context.Context, body []byte) error
}

// webAPIApprovalSlack is ApprovalSlack over the real Slack Web API, mirroring channelSlackStream
// (inbound.go): same Doer/apiBase/token shape, no recipient fields — an approval message is not a
// stream and carries no recipient_user_id/recipient_team_id.
type webAPIApprovalSlack struct {
	doer    slack.Doer
	apiBase string
	token   []byte
}

// NewApprovalSlack builds the production ApprovalSlack.
func NewApprovalSlack(doer slack.Doer, apiBase string, token []byte) ApprovalSlack {
	return &webAPIApprovalSlack{doer: doer, apiBase: apiBase, token: token}
}

func (s *webAPIApprovalSlack) PostMessage(ctx context.Context, body []byte) (string, error) {
	res, err := slack.PostMessage(ctx, s.doer,
		slack.PostRequest{MethodURL: s.apiBase + "/chat.postMessage", Token: s.token, Body: body}, slack.PostOptions{})
	if err != nil {
		return "", err
	}
	return res.MessageTS, nil
}

func (s *webAPIApprovalSlack) UpdateMessage(ctx context.Context, body []byte) error {
	_, err := slack.PostMessage(ctx, s.doer,
		slack.PostRequest{MethodURL: s.apiBase + "/chat.update", Token: s.token, Body: body}, slack.PostOptions{})
	return err
}

// ApprovalDeps is the approval bridge's whole seam.
//
// AllowedApprovers IS THE ONLY PER-HUMAN AUTHORIZATION BOUNDARY THIS PATH HAS, and that is worth
// stating in the struct doc rather than only at the check site. It is resolved by the caller from
// this bot's OWN bots-registry row config (c.Bots.Get) — never an environment variable, mirroring
// InboundDeps's identical rule. The reason it carries this much weight: DecideApproval's deciding
// principal (apps/control-plane/internal/store/approvals.go) is `key:<api_key_id>` — this BOT's own
// API key, the same one for every click this process ever forwards, because Palai has no per-human
// identity to check server-side (known-gaps HIL-P2). The server's ConfigPolicy.ApproverAllowed check
// therefore authorizes THIS BOT AS A WHOLE, once, for its key — it cannot tell one Slack clicker from
// another. Without this list (or with a caller that consulted it too late), every member of the
// channel who can see the message could decide it.
type ApprovalDeps struct {
	Palai ApprovalsPalai
	Slack ApprovalSlack
	// AllowedApprovers are the Slack user ids permitted to decide through this bot.
	AllowedApprovers []string
}

func (deps ApprovalDeps) validate() error {
	switch {
	case deps.Palai == nil:
		return errors.New("relay: ApprovalDeps needs a Palai")
	case deps.Slack == nil:
		return errors.New("relay: ApprovalDeps needs a Slack")
	}
	return nil
}

// approvalListPageSize/maxApprovalListPages bound findPendingApproval's scan of GET /v1/approvals.
const (
	approvalListPageSize = 100
	maxApprovalListPages = 20
)

// ErrApprovalNotFound is findPendingApproval finding no open (pending) approval with the request hash
// an approval.requested.v1 event named — the row has not committed visibly yet, or (see OnButton's own
// 404/409 handling) it was already decided by the time this ran.
var ErrApprovalNotFound = errors.New("relay: no open approval matches this request hash")

// OnApprovalRequested turns one approval.requested.v1 event into the Slack message a human decides
// from. channel/threadTS are NOT read off ev — nothing in a Palai event names a Slack thread, that
// mapping is this bot's own (Task 8's ThreadStore, keyed the other direction: thread -> session, with
// no reverse query) — so the caller wiring this bridge into a run's event loop must supply them the
// same way relay.Run itself takes them as explicit parameters, not derive them here.
//
// THE EVENT DOES NOT CARRY WHAT THIS FUNCTION NEEDS TO RENDER A SCREEN, and the two kinds that share
// this event type do not even agree on what they DO carry — measured directly against the two
// journalling call sites:
//
//   - packages/coordinator/approvals.go RequestToolApproval's payload:
//     {tool_call_id, approval_id, tool_name, request_hash, run_id}
//   - packages/coordinator/publication.go RequestPublication's payload (line 269-272):
//     {publication_id, operation, branch, request_hash, display} — NO approval_id.
//
// request_hash is the one field both share, so it is the only thing this function reads off ev.data;
// everything else — the approval's own id, and the already-derived Identity/OperatorLabel/Arguments/
// Truncated GET /v1/approvals computes server-side (store/approvals.go) — comes from listing the
// tenant's open approvals and matching this hash (findPendingApproval).
func OnApprovalRequested(ctx context.Context, deps ApprovalDeps, channel, threadTS string, ev palai.Event) error {
	if err := deps.validate(); err != nil {
		return err
	}
	if ev.Type != ApprovalRequestedEventType {
		return fmt.Errorf("relay: OnApprovalRequested called with a %q event, not %q", ev.Type, ApprovalRequestedEventType)
	}
	requestHash, _ := ev.Data["request_hash"].(string)
	if requestHash == "" {
		return fmt.Errorf("relay: %s event for session %s carries no request_hash", ev.Type, ev.SessionID)
	}

	approval, err := findPendingApproval(ctx, deps, requestHash)
	if err != nil {
		return err
	}

	if _, err := deps.Slack.PostMessage(ctx, buildApprovalMessage(channel, threadTS, approval)); err != nil {
		return fmt.Errorf("relay: post approval message for %s: %w", approval.ID, err)
	}
	return nil
}

// findPendingApproval scans GET /v1/approvals (oldest first, per-page) for the row carrying
// requestHash. It is a scan rather than a filtered lookup because the route accepts no
// ?request_hash= filter (sdks/go/approvals.go's ListApprovalsParams doc: only after/limit/
// created_after/created_before are honoured) — this bridge is the first caller that has needed one.
func findPendingApproval(ctx context.Context, deps ApprovalDeps, requestHash string) (palai.Approval, error) {
	after := ""
	for page := 0; page < maxApprovalListPages; page++ {
		result, err := deps.Palai.ListApprovals(ctx, palai.ListApprovalsParams{After: after, Limit: approvalListPageSize})
		if err != nil {
			return palai.Approval{}, fmt.Errorf("relay: list approvals: %w", err)
		}
		for _, a := range result.Data {
			if a.RequestHash == requestHash {
				return a, nil
			}
		}
		if !result.HasMore || result.NextCursor == nil {
			break
		}
		after = *result.NextCursor
	}
	return palai.Approval{}, ErrApprovalNotFound
}

// buildApprovalMessage renders one approval row into a chat.postMessage body via
// slack.ToolApprovalMessage — see the file doc for why that function and not a hand-rolled one.
//
// EVERY STRING slack.ToolApprovalMessage PUTS ON THE SCREEN IS RE-DERIVED THROUGH
// slack.DeriveApprovalDisplay / slack.ApprovalArgumentRows HERE, and for a PUBLICATION approval that
// re-derivation is not redundant — it is the FIRST time these bytes are defused at all. GET
// /v1/approvals derives a TOOL row's Identity/OperatorLabel/Arguments through
// slack.DeriveApprovalDisplay server-side (store/approvals.go:64), but a PUBLICATION row's (line 90)
// is `Identity: p.Operation, OperatorLabel: p.Display, Arguments: string(p.Arguments)` — the raw
// columns, untouched by NeutralizeBroadcasts. p.Display is exactly the kind of string
// interactions.go's own UpdateMessage doc warns is run-controlled ("the decision repair names a
// publication display built from THE RUN'S OWN BRANCH NAME, and `<!channel>` is a valid git ref").
// Passing approval.Identity/OperatorLabel/Arguments through ToolApprovalMessage — which neutralizes
// all three again before rendering — closes that gap for the Slack surface this bridge owns. For a
// TOOL row it is a second, idempotent pass over already-defused text: NeutralizeBroadcasts only
// rewrites a literal `<!`/`<@`, and defused text (`&lt;!`/`&lt;@`) contains neither.
//
// The approve/deny/show-arguments buttons all carry the SAME value: approvalActionValue(approval.ID,
// approval.RequestHash), not the bare request hash slack.ApprovalRequest's own doc names — see
// OnButton's doc for why this bridge packs both ids into the one string a button carries, and why
// that is safe as a private convention between this function and OnButton alone.
func buildApprovalMessage(channel, threadTS string, approval palai.Approval) []byte {
	return slack.ToolApprovalMessage(channel, threadTS, slack.ApprovalRequest{
		ApprovalID:    approval.ID,
		RequestHash:   approvalActionValue(approval.ID, approval.RequestHash),
		Identity:      approval.Identity,
		OperatorLabel: approval.OperatorLabel,
		Arguments:     []byte(approval.Arguments),
	})
}

// approvalActionValue packs the two ids a decision needs into the one string a Block Kit button
// carries. Approvals.Approve/Deny (sdks/go/approvals.go) take the approval's own id as a URL path
// segment AND its request hash as a MANDATORY body field — DecisionParams.RequestHash, which the
// server refuses empty with 400 "an approval id alone authorizes nothing" (verified live against the
// running control plane). `|` is safe as a separator because neither half is Slack- or human-authored:
// both come straight off the GET /v1/approvals row findPendingApproval just read, never off anything
// a click could have carried in.
func approvalActionValue(approvalID, requestHash string) string {
	return approvalID + "|" + requestHash
}

// parseApprovalActionValue reverses approvalActionValue. ok is false for anything that is not exactly
// two non-empty halves — an empty approval id or request hash authorizes nothing, matching how
// slack.MapInteractiveApproval already refuses an empty button value for the same reason. Splitting
// on the FIRST `|` only (strings.Cut) is deliberate: approvalID is ours and never contains one, so
// whatever remains — even if a request hash somehow did — is still the whole second half.
func parseApprovalActionValue(value string) (approvalID, requestHash string, ok bool) {
	approvalID, requestHash, found := strings.Cut(value, "|")
	if !found || approvalID == "" || requestHash == "" {
		return "", "", false
	}
	return approvalID, requestHash, true
}

// ErrApproverNotAllowed is an unlisted Slack user's click. OnButton returns it before ever calling
// deps.Palai — see OnButton's own doc for why the ORDER, not just the outcome, is the property this
// file's test asserts.
var ErrApproverNotAllowed = errors.New("relay: this Slack user is not an allowed approver for this bot")

// OnButton turns one decoded, already-authenticated Slack approve/deny click into a decision.
//
// click is a slack.ApprovalIntent — MapInteractiveApproval's own output type, not a slack.Interaction
// (no such type exists; see the file doc). Its RequestHash field, on a click THIS bridge minted the
// button for, holds approvalActionValue's composite string rather than a bare hash — a different wire
// convention on the same shared field than the OLDER admitter-based Slack integration embedded in the
// control plane uses for the SAME struct. That is safe only because this bridge is also the only
// reader of its own buttons' values; it must never be assumed elsewhere in this package.
//
// THE ALLOW-LIST CHECK RUNS BEFORE deps.Palai IS EVER CALLED, and the ordering is load-bearing, not
// tidy (see ApprovalDeps's own doc on why AllowedApprovers is the only per-human boundary this path
// has): a version that called the API first and checked the allow-list on the way out would still let
// an unauthorized click reach the server on every attempt, race or not.
//
// A deny sends no Reason: this bridge decides from a channel-message button, never a modal, so there
// is no free-text field to lose in the first place. That sidesteps rather than papers over the known
// platform gap (HIL-P10, sdks/go/approvals.go's DecisionParams doc): a publication denial's Reason is
// dropped server-side (ApplyApprovalDecision, packages/coordinator/publication.go, takes no reason
// parameter at all) while a tool denial's reaches the model verbatim. Since this file never offers a
// reason box to type into, it can never show a human a box that silently swallows their words.
func OnButton(ctx context.Context, deps ApprovalDeps, click slack.ApprovalIntent) error {
	if err := deps.validate(); err != nil {
		return err
	}
	if !allowed(deps.AllowedApprovers, click.UserID) {
		return fmt.Errorf("relay: %w: %s", ErrApproverNotAllowed, click.UserID)
	}

	approvalID, requestHash, ok := parseApprovalActionValue(click.RequestHash)
	if !ok {
		return fmt.Errorf("relay: button value %q is not a bound approval/request-hash pair", click.RequestHash)
	}

	var result *palai.ApprovalDecisionResult
	var err error
	switch click.Decision {
	case "approve":
		result, err = deps.Palai.ApproveApproval(ctx, approvalID, palai.DecisionParams{RequestHash: requestHash})
	case "deny":
		result, err = deps.Palai.DenyApproval(ctx, approvalID, palai.DecisionParams{RequestHash: requestHash})
	default:
		return fmt.Errorf("relay: %q is not a decision this bridge makes", click.Decision)
	}

	if err != nil {
		if isAlreadyDecided(err) {
			// NEVER SURFACE "no such approval": a decided PUBLICATION 404s (PublicationApprovalByID
			// filters WHERE p.state = 'pending_approval', so a decided row falls out of the lookup
			// entirely) and a decided TOOL call 409s (ToolApprovalByID carries no state filter at
			// all) — two different codes for the SAME fact, that nothing is left for this clicker to
			// decide, most often because someone else's click landed first. Repair the message rather
			// than error the caller.
			return deps.Slack.UpdateMessage(ctx, slack.UpdateMessage(click.ChannelID, click.MessageTS,
				"This approval was already decided — refresh the thread to see by whom. No action was taken from this click.", ""))
		}
		return fmt.Errorf("relay: %s approval %s: %w", click.Decision, approvalID, err)
	}

	return deps.Slack.UpdateMessage(ctx, slack.UpdateMessage(click.ChannelID, click.MessageTS,
		fmt.Sprintf("Decision recorded: %s.", result.Decision), click.UserID))
}

// allowed reports whether userID is one of this bot's configured approvers: an exact,
// case-sensitive match against the Slack user id, never a substring or prefix match — the class of
// comparison this tree has shipped defeated more than once elsewhere (see path/membership-comparison
// history in this repo's plan discipline notes).
func allowed(list []string, userID string) bool {
	for _, id := range list {
		if id == userID {
			return true
		}
	}
	return false
}

// isAlreadyDecided reports whether err is either half of the split Approvals.Approve/Deny's own doc
// measures: 409 approval_not_decidable for a tool call already decided, or 404 for a publication
// already decided. Both collapse to one branch here because they mean the same thing to a human.
func isAlreadyDecided(err error) bool {
	var apiErr *palai.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Status == http.StatusConflict || apiErr.Status == http.StatusNotFound
}
