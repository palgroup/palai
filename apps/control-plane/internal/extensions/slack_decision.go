package extensions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

// The Slack DECISION bridge (E19 T2, spec §36, SLK-004/006/007) — the first real caller of
// SlackAuthorizationPolicyFor + ApproverAuthorized, which E17 T11 recorded as an UNWIRED DECISION PATH.
//
// THE CHAIN, and every link is production code rather than a test composition:
//
//	slack.MapInteractiveApproval  (the route, after verifying the RAW form body)
//	  → Store.SlackAuthorizationPolicyFor        — the connection's allow-list, tenant-scoped
//	  → SlackAuthorizationPolicy.ApproverAuthorized — deny-by-default: an unmapped clicker decides NOTHING
//	  → coordinator.PendingApprovalForSession    — the one-shot approval the session actually has open
//	  → coordinator.AcceptCommand                — exactly ONE durable approve/deny command
//	  → coordinator.ApplyApprovalDecision        — the transition, bound to the request hash
//	  → chat.update                              — the visible message repaired in place (SLK-006)
//
// FOUR BINDINGS, all of them server-side, none of them selectable by the payload:
//
//  1. the TENANT is the resolved connection's org/project (§2/§38.6 — the events route's rule, unchanged);
//  2. the SESSION comes from the thread the button was clicked in, via the SLK-003 correlation. A hash
//     observed in one conversation therefore cannot decide another;
//  3. the CLICKER must be in the connection's allow-list. Empty list ⇒ nobody, never everybody;
//  4. the OPERATION is pinned by the one-shot request hash: it has to equal the session's pending approval's
//     hash, and ApplyApprovalDecision re-checks it under lock inside the transaction.
//
// WHY THE DECISION IS APPLIED HERE rather than left queued for the boundary pump (the native /commands
// posture): a human clicked a button and is watching the message. The pump applies a queued approve when the
// run next reaches a boundary, which for a run PARKED waiting on this very approval may be never. Both paths
// are single-winner — SetPublicationState is conditional on the from-state and applyCommandInTx on `queued` —
// so a pump that races this call finds the command already applied and does nothing.
//
// HONEST CEILING, where a reader meets it: nothing in this tree POSTS an approval message yet. This bridge
// repairs the message a click arrived on and records its ts as the thread's repair handle, so the outbound
// path (token from bot_token_ref, per-channel pacing, the bounded 429 repair) is exercised by a real caller —
// but the "post the approval message" half is driven by the credential-gated live leg (tests/live/slack) and
// by whatever composes approval UX later, which is the console's job. Enterprise Grid / org-wide install is
// out of scope: ParseInteractionTeam never produces an enterprise id.

// slackRepairMaxWait clamps ONE Retry-After honoured inside a decision. The route owes Slack an answer within
// three seconds (https://docs.slack.dev/interactivity/handling-user-interaction/, checked 2026-07-26), so a
// repair that would sleep longer than the whole ack budget is abandoned instead: the decision is already
// durable, and a stale message is a smaller failure than an unacknowledged click.
const slackRepairMaxWait = time.Second

// WithDecisions wires the interactivity half onto the admission bridge: the coordinator's approval spine, an
// HTTP client for Slack's Web API, and the API base URL (empty ⇒ https://slack.com/api). Builder-style like
// automation.TriggerStore.WithAdmitter, so a stack that mounts only the inbound half constructs unchanged and
// api.WithSlackInteractions simply is not passed there.
func (a *SlackAdmitter) WithDecisions(spine *coordinator.Store, doer slack.Doer, apiBase string) *SlackAdmitter {
	a.spine = spine
	a.doer = doer
	a.apiBase = apiBase
	if a.apiBase == "" {
		a.apiBase = "https://slack.com/api"
	}
	a.pacer = &slack.ChannelPacer{}
	return a
}

// Decide runs the chain above for a VERIFIED interactive approval. It is api.SlackInteractionsAPI's
// implementation. Every refusal is a TYPED outcome rather than an error: an error means the receiver failed
// (503 to Slack), while a refusal means the receiver worked and the click authorized nothing.
func (a *SlackAdmitter) Decide(ctx context.Context, conn api.SlackConnectionRef, intent slack.ApprovalIntent) (api.SlackDecisionOutcome, error) {
	if a.spine == nil {
		// Unreachable through the router (the route is mounted only where this is wired), but a nil spine must
		// never fail OPEN into "approved without checking".
		return api.SlackDecisionOutcome{}, errors.New("extensions: slack decisions are not wired")
	}
	// slack_thread_sessions is FORCE-RLS and threadSession does not scope its own context: an unscoped read
	// sees NO rows, which here would silently report every click as coming from an uncorrelated thread.
	scoped := storage.ScopeToTenant(ctx, conn.Org, conn.Project)

	// 1. The conversation the click belongs to. No correlation ⇒ this deployment never opened a session in
	// that thread, so there is nothing for the click to decide.
	session, lastBotTS, err := a.store.threadSession(scoped, conn.Org, conn.Project, intent.TeamID, intent.ChannelID, intent.ThreadTS)
	switch {
	case errors.Is(err, ErrSlackThreadSessionNotFound):
		return api.SlackDecisionOutcome{Rejected: "the click's thread has no correlated session"}, nil
	case err != nil:
		return api.SlackDecisionOutcome{}, err
	}

	// 2. AUTHORIZATION — SLK-004, deny by default. Nothing below this line runs for an unmapped clicker, so
	// the guarantee is "no command was enqueued", not "a command was enqueued and then rejected".
	policy, err := a.store.SlackAuthorizationPolicyFor(ctx, conn.Org, conn.Project, conn.ID)
	if err != nil {
		return api.SlackDecisionOutcome{}, fmt.Errorf("read slack authorization policy: %w", err)
	}
	if !policy.ApproverAuthorized(intent.UserID) {
		// The user id is NOT logged here: it is an unauthenticated-adjacent identity from a payload, and the
		// connection id already tells an operator where to look.
		return api.SlackDecisionOutcome{SessionID: session, Rejected: "the clicking user is not an authorized approver"}, nil
	}

	// 3. The exact operation. The button's value has to be the hash of the approval the session actually has
	// open — a stale hash (the head moved, the request was edited) or a hash minted for another operation
	// decides nothing, and does not even become a command.
	//
	// "THE" pending approval, singular, and the limit is inherited rather than introduced: both this read and
	// ApplyApprovalDecision's own LockPendingApprovalForSession take the session's OLDEST pending publication.
	// So if a session ever holds two open approvals at once, only the older one is decidable — from Slack or
	// from the native command surface alike. Refusing here is the same verdict the coordinator would reach,
	// reached one step earlier and without leaving a pointless command behind.
	tenant := coordinator.Tenant{Organization: conn.Org, Project: conn.Project}
	pub, found, err := a.spine.PendingApprovalForSession(ctx, tenant, session)
	if err != nil {
		return api.SlackDecisionOutcome{}, fmt.Errorf("read the session's pending approval: %w", err)
	}
	if !found {
		return api.SlackDecisionOutcome{SessionID: session, Rejected: "the session has no pending approval"}, nil
	}
	if pub.RequestHash == "" || pub.RequestHash != intent.RequestHash {
		return api.SlackDecisionOutcome{SessionID: session, Rejected: "the button's request hash does not match the pending approval"}, nil
	}

	// 4. Exactly ONE durable command. AcceptCommand is the same entry POST /v1/sessions/{id}/commands takes,
	// so the session-lifecycle gate and the no_pending_approval rule apply identically.
	payload, err := json.Marshal(map[string]string{"request_hash": intent.RequestHash})
	if err != nil {
		return api.SlackDecisionOutcome{}, err
	}
	cmdID := newID("cmd")
	acc, err := a.spine.AcceptCommand(ctx, tenant, session, coordinator.CommandInput{
		CommandID: cmdID, Kind: intent.Decision, Payload: payload,
	})
	if err != nil {
		return api.SlackDecisionOutcome{}, fmt.Errorf("accept slack approval command: %w", err)
	}
	if acc.SessionNotFound {
		return api.SlackDecisionOutcome{SessionID: session, Rejected: "the correlated session no longer exists"}, nil
	}
	if acc.State != "queued" {
		// A durably REJECTED command (a closed session, no pending approval by the time it committed). The
		// command row stands as the audit trail of the attempt; nothing was authorized.
		return api.SlackDecisionOutcome{SessionID: session, CommandID: cmdID,
			Rejected: "the command was refused at accept (" + acc.State + ")"}, nil
	}

	// 5. Apply it — bound to the hash, single-winner, in one transaction with settling the command.
	if _, err := a.spine.ApplyApprovalDecision(ctx, tenant, session, pub.ResponseID, pub.RunID,
		cmdID, intent.Decision, intent.RequestHash); err != nil {
		return api.SlackDecisionOutcome{}, fmt.Errorf("apply slack approval decision: %w", err)
	}

	// 6. Draw the outcome on the message the human is looking at. A failure here is NOT a failure of the
	// decision (SLK-006): the publication has already moved and the command is settled.
	out := api.SlackDecisionOutcome{
		SessionID: session, PublicationID: pub.ID, CommandID: cmdID, Decision: intent.Decision,
	}
	out.Repaired = a.repairDecisionMessage(scoped, conn, intent, lastBotTS, pub)
	return out, nil
}

// repairDecisionMessage edits the visible approval message in place to reflect the decision, with the bot
// token resolved from bot_token_ref at call time. It reports whether the edit landed and never returns an
// error: by the time it runs the decision is durable, and a Slack delivery problem must not be able to
// undo it or to fail the ack.
//
// WHICH MESSAGE, since SLK-006 names last_bot_message_ts as the repair handle: the CLICKED message is by
// construction our own bot message (only our minted buttons reach this path), so it is the one edited, and it
// is then RECORDED as the thread's last_bot_message_ts. That keeps the invariant the column exists for — one
// visible bot message per thread, repaired in place, never duplicated by a second post — and it points a
// later coalesced update at the message the human is actually looking at. An already-recorded handle is used
// as-is when the click carries no message ts.
//
// chat.update, NOT response_url: that URL is usable "up to 5 times within 30 minutes of receiving the
// payload" (https://docs.slack.dev/interactivity/handling-user-interaction/, checked 2026-07-26), and an
// approval's message may need editing again when the operation it authorized finally completes — well past
// both bounds. A repair that silently expires is worse than one that visibly fails.
func (a *SlackAdmitter) repairDecisionMessage(scoped context.Context, conn api.SlackConnectionRef, intent slack.ApprovalIntent, lastBotTS string, pub coordinator.Publication) bool {
	target := intent.MessageTS
	if target == "" {
		target = lastBotTS
	}
	if a.doer == nil || target == "" || conn.BotTokenRef == "" {
		return false
	}
	token, err := a.secrets(conn.Org, conn.BotTokenRef)
	if err != nil || len(token) == 0 {
		// The ref name is not echoed anywhere; an operator sees WHICH connection could not reply.
		log.Printf("slack: could not resolve the bot token for connection %s; the decision stands but its message is stale", conn.ID)
		return false
	}
	// The documented per-channel rate, held BEFORE the call rather than recovered from after a 429.
	if a.pacer != nil {
		if err := a.pacer.Wait(scoped, intent.ChannelID); err != nil {
			return false
		}
	}
	verdict := "Approved"
	if intent.Decision == "deny" {
		verdict = "Denied"
	}
	// The authoritative detail comes from the PUBLICATION, never from the payload: what was decided is what
	// the control plane had pending, not what a Slack message claimed.
	body := slack.UpdateMessage(intent.ChannelID, target, verdict+": "+pub.Display+" (<@"+intent.UserID+">)")
	res, err := slack.PostMessage(scoped, a.doer, slack.PostRequest{
		MethodURL: a.apiBase + "/chat.update", Token: token, Body: body,
	}, slack.PostOptions{MaxWait: slackRepairMaxWait})
	if err != nil {
		log.Printf("slack: the decision on connection %s is durable but its message could not be repaired after %d attempt(s): %v",
			conn.ID, res.Attempts, err)
		return false
	}
	// Record the repaired message as the thread's single visible handle (SLK-006). A failure to record is
	// logged, not fatal: the edit already landed.
	if _, err := a.store.pool.Exec(scoped, storage.Query("UpdateThreadMessageTS"),
		conn.Org, conn.Project, intent.TeamID, intent.ChannelID, intent.ThreadTS, target); err != nil {
		log.Printf("slack: repaired the visible message on connection %s but could not record its ts: %v", conn.ID, err)
	}
	return true
}
