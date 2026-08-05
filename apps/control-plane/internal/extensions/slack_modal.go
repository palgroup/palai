package extensions

import (
	"context"
	"errors"
	"fmt"

	"github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

// THE MODAL (E23 T4, plan §3.5 P10). A Show-arguments click opens a view holding the full argument dump,
// and this file is the whole of what happens between the click and the view.
//
// THE THREE-SECOND RULE IS THE ARCHITECTURE HERE, not a caution written beside it. Two independent
// deadlines run from the same instant:
//
//	the route owes Slack a 200 "within 3 seconds of receiving the payload"
//	  (https://docs.slack.dev/interactivity/handling-user-interaction/, E19, inherited); and
//	"A trigger_id will expire 3 seconds after it's sent to your app, so you'll want to use it quickly"
//	  (https://docs.slack.dev/surfaces/modals/, §3.5 P10, fetched 2026-07-29).
//
// They compose into one constraint: views.open has to be CALLED INSIDE THE ACK BUDGET, which is why the
// route calls this before it writes its 200 rather than handing the work to a goroutine. And that in turn
// is why this function DOES NOT WRITE TO THE DATABASE — not as a policy, as a budget. It performs exactly
// three reads (the thread's session, the connection's authorization policy, the pending approval), one
// secret resolution and one outbound call. A row written here would be a lock taken here, and a lock taken
// here is a trigger_id that expired while we waited for it.
//
// AUTHORIZATION IS THE SAME FIVE-BINDING CHAIN THE DECISION USES, minus the decision. Opening a document is
// a smaller act than deciding, but it still shows one human another human's pending command — arguments a
// run assembled, a ticket id, a branch name — so it passes the same gates: the tenant is the resolved
// connection's, the session comes from the thread the click happened in, the channel must be in scope, the
// clicker must be an authorized approver, and the request hash must pin an approval that is actually open.
// A refusal here opens NOTHING and tells the clicker nothing, for the reason the route documents.
//
// CEILING, stated where a reader meets it: `views.open` is the ONLY thing this path does with the trigger.
// If the ack is delayed by anything — a slow policy read, a stalled secret backend, an outbound Slack that
// is pacing us — the trigger expires and the modal simply does not open. The message and its three buttons
// are unaffected; the human loses the document, not the decision. That failure mode is bounded by the
// route's own budget rather than by hope.

// OpenApprovalArguments serves a verified Show-arguments click: it re-derives the approval screen from the
// ledger and opens it as a modal. It implements api.SlackInteractionsAPI.
//
// The return is a TYPED refusal string like Decide's, never an error, for everything that means "the
// receiver worked and nothing opened". An error is reserved for the receiver actually failing, which the
// route answers with a 503.
func (a *SlackAdmitter) OpenApprovalArguments(ctx context.Context, conn api.SlackConnectionRef, intent slack.ShowArgumentsIntent) (string, error) {
	if a.spine == nil {
		return "", errors.New("extensions: slack decisions are not wired")
	}
	if a.doer == nil || a.secrets == nil || conn.BotTokenRef == "" {
		// Nothing to open the modal WITH. Not a fault: a deployment that mounted the inbound half only is a
		// deployment where this button is inert, and saying so beats a 503 the clicker cannot act on.
		return "the connection has no outbound token for views.open", nil
	}
	// slack_thread_sessions is FORCE-RLS; an unscoped read sees no rows and would report every click as
	// uncorrelated.
	scoped := storage.ScopeToTenant(ctx, conn.Project)

	// 1. The conversation the click belongs to.
	session, _, err := a.store.threadSession(scoped, conn.Project, intent.TeamID, intent.ChannelID, intent.ThreadTS)
	switch {
	case errors.Is(err, ErrSlackThreadSessionNotFound):
		return "the click's thread has no correlated session", nil
	case err != nil:
		return "", err
	}

	// 2. AUTHORIZATION, deny by default and identical to the decision path's. The channel gate is not
	// redundant with the correlation above: an operator NARROWING allowed_channels is what containing an
	// incident looks like, and a narrowing has to take the already-posted buttons in those channels with it.
	policy, err := a.store.SlackAuthorizationPolicyFor(ctx, conn.Project, conn.ID)
	if err != nil {
		return "", fmt.Errorf("read slack authorization policy: %w", err)
	}
	// isDM is FALSE for the reason slack_decision.go records: a block_actions payload carries no
	// channel_type, so the DM exemption the event path gained has nothing to stand on here. A scope that
	// cannot be checked is not met.
	if !policy.ChannelAllowed(intent.ChannelID, false) {
		return "the click's channel is outside the connection's allowed channels", nil
	}
	if !policy.ApproverAuthorized(intent.UserID) {
		return "the clicking user is not an authorized approver", nil
	}

	// 3. The exact call, pinned by the hash the button carried. A hash that opens nothing shows nothing —
	// including to someone who is authorized, because "there is no such pending approval" and "you may not
	// see it" have to look the same to a clicker.
	tenant := coordinator.Tenant{Project: conn.Project}
	parked, found, err := a.spine.PendingToolApprovalForSession(ctx, tenant, session, intent.RequestHash)
	if err != nil {
		return "", fmt.Errorf("read the session's pending tool approval: %w", err)
	}
	if !found {
		return "the button's request hash matches no open approval in this session", nil
	}

	// 4. The operator's label, resolved through the SAME lookup the executor resolves the call through, so
	// the sentence a human reads belongs to the tool that will actually run. A tool that no longer resolves
	// — deleted revision, unpublished set — renders as "(no operator label)" rather than blocking the view:
	// the arguments are the authority on this screen and they came off the ledger row.
	label := ""
	if tool, ok, lerr := a.store.LookupTool(ctx, conn.Project, parked.RunID, parked.ToolName); lerr == nil && ok {
		label = tool.ApprovalLabel
	}

	expires := parked.ExpiresAt
	req := slack.ApprovalRequest{
		ApprovalID:    parked.ApprovalID,
		RequestHash:   parked.RequestHash,
		Identity:      parked.ToolName,
		OperatorLabel: label,
		Arguments:     parked.Arguments,
	}
	if expires != nil {
		req.ExpiresAt = *expires
	}

	token, err := a.secrets(conn.BotTokenRef)
	if err != nil || len(token) == 0 {
		// The ref name is never echoed; an operator sees WHICH connection could not answer.
		return "the connection's bot token could not be resolved", nil
	}

	// 5. views.open, with the trigger id, inside the caller's budget. NO PACER: the per-channel pacing the
	// posting paths hold is a write-rate rule, and waiting on it here would spend the trigger's three
	// seconds queueing behind messages. views.open is Tier 4 and needs no scopes (P10).
	//
	// MaxWait is zero deliberately — a retry after a Retry-After would land after the trigger is dead, so a
	// 429 here is a modal that does not open rather than a modal that opens late.
	if _, err := slack.PostMessage(ctx, a.doer, slack.PostRequest{
		MethodURL: a.apiBase + "/views.open", Token: token, Body: slack.ToolApprovalModal(intent.TriggerID, req),
	}, slack.PostOptions{}); err != nil {
		// A modal that failed to open is not a failed receiver: the decision buttons are untouched and the
		// approval is unchanged, so the clicker is told nothing and an operator gets one line.
		return "views.open did not succeed", nil
	}
	return "", nil
}
