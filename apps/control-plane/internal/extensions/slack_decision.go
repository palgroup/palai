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
//	  → coordinator.PendingToolApprovalForSession — WHICH KIND: does this hash pin an open GATED CALL?
//	  → coordinator.PendingApprovalForSession    — the one-shot approval the session actually has open
//	  → coordinator.AcceptCommand                — exactly ONE durable approve/deny command
//	  → coordinator.ApplyApprovalDecision        — the transition, bound to the request hash
//	  → chat.update                              — the visible message repaired in place (SLK-006)
//
// AND SINCE E23 T8 THE OTHER HALF OF THE FORK, for a click on a gated tool call:
//
//	  → coordinator.DecideToolApproval           — its FIRST production caller: transition, journal, WAKE
//	  → chat.update                              — the same in-place repair
//
// The fork is a lookup, not a guess: 000044's CHECK ((publication_id IS NULL) <> (tool_call_id IS NULL))
// makes an approvals row say which kind it is, and the button's one-shot hash either pins an open gated
// call in this session or it does not. A publication click takes the identical path it took before T8.
//
// FIVE BINDINGS, all of them server-side, none of them selectable by the payload:
//
//  1. the TENANT is the resolved connection's org/project (§2/§38.6 — the events route's rule, unchanged);
//  2. the SESSION comes from the thread the button was clicked in, via the SLK-003 correlation. A hash
//     observed in one conversation therefore cannot decide another;
//  3. the CHANNEL must be in the connection's allowed_channels. Empty list ⇒ every channel — the opposite of
//     the clicker list's emptiness, for the reason documented at SlackAuthorizationPolicy;
//  4. the CLICKER must be in the connection's allow-list. Empty list ⇒ nobody, never everybody;
//  5. the OPERATION is pinned by the one-shot request hash: it has to equal the session's pending approval's
//     hash, and ApplyApprovalDecision re-checks it under lock inside the transaction.
//
// WHY THE DECISION IS APPLIED HERE rather than left queued for the boundary pump (the native /commands
// posture): a human clicked a button and is watching the message. The pump applies a queued approve when the
// run next reaches a boundary, which for a run PARKED waiting on this very approval may be never. Both paths
// are single-winner — SetPublicationState is conditional on the from-state and applyCommandInTx on `queued` —
// so a pump that races this call finds the command already applied and does nothing.
//
// THE CEILING THAT USED TO STAND HERE IS GONE, and deleting it is the point of E23 T3 rather than a tidy-up.
// From E19 T2 until this epic these lines read "nothing in this tree POSTS an approval message yet" — and
// they stayed true through E22, which shipped a publication path whose buttons no human in a real workspace
// ever saw. slack_approval_post.go is the other half: the order to ask commits with the approval, a
// single-winner pump posts slack.ApprovalMessage, and this bridge repairs THAT message in place. The way to
// retire a comment is to make it false.
//
// Enterprise Grid / org-wide install remains out of scope: ParseInteractionTeam never produces an
// enterprise id.

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
	scoped := storage.ScopeToTenant(ctx, conn.Project)

	// 1. The conversation the click belongs to. No correlation ⇒ this deployment never opened a session in
	// that thread, so there is nothing for the click to decide.
	session, lastBotTS, err := a.store.threadSession(scoped, conn.Project, intent.TeamID, intent.ChannelID, intent.ThreadTS)
	switch {
	case errors.Is(err, ErrSlackThreadSessionNotFound):
		return api.SlackDecisionOutcome{Rejected: "the click's thread has no correlated session"}, nil
	case err != nil:
		return api.SlackDecisionOutcome{}, err
	}

	// 2. AUTHORIZATION — SLK-004, deny by default. Nothing below this line runs for an unmapped clicker, so
	// the guarantee is "no command was enqueued", not "a command was enqueued and then rejected".
	policy, err := a.store.SlackAuthorizationPolicyFor(ctx, conn.Project, conn.ID)
	if err != nil {
		return api.SlackDecisionOutcome{}, fmt.Errorf("read slack authorization policy: %w", err)
	}
	// 2a. SCOPE. The same allowed_channels gate the events path applies, and it is NOT redundant with it: the
	// thread above was correlated while its channel was in scope, so an operator NARROWING the allow-list —
	// which is what containing an incident looks like — would otherwise leave every already-posted button in
	// the excluded channels still live. A narrowing has to take the in-flight threads with it.
	//
	// isDM is FALSE and cannot honestly be anything else: a block_actions payload carries `channel` as
	// `{id, name}` with no channel_type anywhere
	// (https://docs.slack.dev/reference/interaction-payloads/block_actions-payload/, checked 2026-07-27), so
	// the DM exemption the EVENT path gained (E20 T2) has no authority to stand on here. A scope that cannot
	// be checked is not met. See ChannelAllowed for the consequence and the operator's escape hatch.
	if !policy.ChannelAllowed(intent.ChannelID, false) {
		return api.SlackDecisionOutcome{SessionID: session, Rejected: "the click's channel is outside the connection's allowed channels"}, nil
	}
	if !policy.ApproverAuthorized(intent.UserID) {
		// The user id is NOT logged here: it is an unauthenticated-adjacent identity from a payload, and the
		// connection id already tells an operator where to look.
		return api.SlackDecisionOutcome{SessionID: session, Rejected: "the clicking user is not an authorized approver"}, nil
	}

	tenant := coordinator.Tenant{Project: conn.Project}

	// 3. WHICH KIND OF APPROVAL THIS CLICK DECIDES (E23 T8). The two live in one table and 000044's
	// CHECK ((publication_id IS NULL) <> (tool_call_id IS NULL)) makes each row say which it is, so the
	// branch is a lookup rather than a guess: the hash the button carried either pins an OPEN GATED CALL in
	// this session or it does not.
	//
	// The generic read runs FIRST and that ordering is free of consequence, because the two reads cannot
	// both find something. PendingToolApprovalForSession matches on (session, request_hash) against
	// tool_calls at `approval_pending`; a publication's hash is not a tool call's, and a session holding
	// neither falls through to the publication branch exactly as it did before this task. Every publication
	// click therefore takes the identical path it took at E23 T7, which is the point — the shipped screen
	// and the shipped decision do not move.
	if parked, found, terr := a.spine.PendingToolApprovalForSession(ctx, tenant, session, intent.RequestHash); terr != nil {
		return api.SlackDecisionOutcome{}, fmt.Errorf("read the session's pending tool approval: %w", terr)
	} else if found {
		return a.decideToolApproval(scoped, conn, intent, session, lastBotTS, parked)
	}

	// 3a. The exact operation. The button's value has to be the hash of the approval the session actually
	// has open — a stale hash (the head moved, the request was edited) or a hash minted for another
	// operation decides nothing, and does not even become a command.
	//
	// "THE" pending approval, singular, and the limit is inherited rather than introduced: both this read and
	// ApplyApprovalDecision's own LockPendingApprovalForSession take the session's OLDEST pending publication.
	// So if a session ever holds two open approvals at once, only the older one is decidable — from Slack or
	// from the native command surface alike. Refusing here is the same verdict the coordinator would reach,
	// reached one step earlier and without leaving a pointless command behind.
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
	//
	// The command also carries WHO is deciding, in the canonical form an approver list names (E23 T2). The
	// team id is part of it because a Slack user id is unique only within its workspace — and both halves
	// come from the payload Slack SIGNED, which is the only reason a payload field is trusted here at all.
	// It rides the command rather than only the direct call below so that a boundary pump which wins the
	// race applies the SAME decision under the SAME principal.
	approver := coordinator.ApproverPrincipal(coordinator.ApproverSurfaceSlack, intent.TeamID, intent.UserID)
	payload, err := json.Marshal(map[string]string{"request_hash": intent.RequestHash, "approver": approver})
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
	//
	// LOSING THE RACE IS NOT FAILING. The command minted above is an ordinary queued command on the run, so
	// the boundary pump drains it too (it did not, while PendingBoundaryCommands filtered approve/deny out —
	// that filter is why this was the only working approval path). Whichever side gets there first applies
	// the SAME command under the SAME one-shot hash; the loser sees ErrCommandNotPending. Reporting that as
	// an error would 503 the click and tell a human their approval failed while it is durably applied.
	//
	// But an EXPIRY sweep settles a queued command too — the run terminalized, or the session closed — and
	// that one decided NOTHING. The two are indistinguishable from the error alone, so the publication's own
	// state is what separates them: a click that authorized nothing must never be drawn as approved.
	// THE SECOND GATE, and it is not the same gate as step 2a/2b. allowed_users is the CONNECTION's list
	// and carries channel scope a platform-wide list cannot express; config_policy.approvers is the
	// PROJECT's and spans every surface. Both have to be passed, and a narrowing on either side is a
	// narrowing. The check itself is inside ApplyApprovalDecision — the one throat this path and the
	// native command surface both pass through — so it cannot be forgotten by whoever adds the third.
	switch _, err := a.spine.ApplyApprovalDecision(ctx, tenant, session, pub.ResponseID, pub.RunID,
		cmdID, intent.Decision, intent.RequestHash, approver); {
	case errors.Is(err, coordinator.ErrApproverNotAuthorized):
		// The receiver worked and the click authorized nothing — a typed refusal, exactly like an unmapped
		// clicker above, and for the same reason it is not an error: a 503 would tell a human their
		// approval FAILED when in fact it was refused. The route answers 200 and tells the clicker
		// nothing (slack_interactions.go: a readable refusal is a signal an unmapped user can probe),
		// which is why the reason string is written for an operator reading a log, not for the clicker.
		return api.SlackDecisionOutcome{SessionID: session, CommandID: cmdID,
			Rejected: "the clicking user is not in the project's approver list"}, nil
	case errors.Is(err, coordinator.ErrCommandNotPending):
		state, found, serr := a.spine.PublicationState(ctx, tenant, pub.ID)
		if serr != nil {
			return api.SlackDecisionOutcome{}, fmt.Errorf("read the publication after a settled command: %w", serr)
		}
		if !found || state != decidedPublicationState(intent.Decision) {
			return api.SlackDecisionOutcome{SessionID: session, CommandID: cmdID,
				Rejected: "the command was settled before the decision could be applied"}, nil
		}
	case err != nil:
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

// slackDenyReason is what a Slack Deny hands back to the model. A denial with no reason is a wall; a
// denial with one is an instruction the agent can act on, which is the whole difference between a gate and
// an outage. It is a CONSTANT rather than a captured sentence, and that is the honest shape today: the
// modal's "Reason for denying" field is not routed to a decision (see ToolApprovalModal's ceiling), so
// there is no human sentence to carry, and inventing one would put words in an approver's mouth. WHO
// denied is recorded durably on the approval's decided_by, which is where an identity belongs.
const slackDenyReason = "a human denied this call in Slack. No reason was given — the Deny button carries none. Do not retry the same call; ask, or do something else."

// decideToolApproval answers a click on a GATED TOOL CALL (E23 T8) — the generic twin of the publication
// branch above, and the FIRST production caller of coordinator.DecideToolApproval.
//
// IT PASSES THE SAME FIVE BINDINGS as its publication sibling, because the caller has already applied
// four of them (tenant, session, channel scope, clicker allow-list) and the fifth — the one-shot hash — is
// what found this approval in the first place. What it does NOT do is mint a command, and that difference
// is structural rather than an omission: an approve/deny COMMAND exists so the boundary pump can apply a
// decision the run has not reached yet, and a publication needs that because its run keeps going. A gated
// call PARKS its run, so the only thing that can move it is this decision — DecideToolApproval does the
// transition, the journal and the wake in one transaction, and a queued command would be a second, weaker
// path to the same place. The audit trail is the approval row's decided_by plus the approval.approved /
// approval.denied journal entries, written inside that transaction.
func (a *SlackAdmitter) decideToolApproval(scoped context.Context, conn api.SlackConnectionRef, intent slack.ApprovalIntent,
	session, lastBotTS string, parked coordinator.ToolApproval) (api.SlackDecisionOutcome, error) {

	tenant := coordinator.Tenant{Project: conn.Project}
	// The canonical principal, in the one string form config_policy.approvers ever names. The team id is
	// part of it because a Slack user id is unique only within its workspace — and both halves come from
	// the payload Slack SIGNED, which is the only reason a payload field is trusted here at all.
	approver := coordinator.ApproverPrincipal(coordinator.ApproverSurfaceSlack, intent.TeamID, intent.UserID)

	out := api.SlackDecisionOutcome{SessionID: session, ToolCallID: parked.ToolCallID, Decision: intent.Decision}
	applied, err := a.spine.DecideToolApproval(scoped, tenant, coordinator.ToolApprovalDecision{
		ToolCallID:  parked.ToolCallID,
		RequestHash: intent.RequestHash,
		DecidedBy:   approver,
		Approve:     intent.Decision != "deny",
		Reason:      slackDenyReason,
	})
	switch {
	case errors.Is(err, coordinator.ErrApproverNotAuthorized):
		// THE SECOND GATE, and it is not the same gate the caller already applied. allowed_users is the
		// CONNECTION's list and carries channel scope a platform-wide list cannot express; config_policy
		// .approvers is the PROJECT's and spans every surface. Both have to be passed. The check is inside
		// DecideToolApproval — the one throat every generic surface passes through — so it cannot be
		// forgotten by whoever adds the next one, and it is the SAME function the publication path calls.
		out.Rejected = "the clicking user is not in the project's approver list"
		return out, nil
	case err != nil:
		return api.SlackDecisionOutcome{}, fmt.Errorf("apply slack tool approval decision: %w", err)
	}
	if !applied {
		// VOID, not failed: the call was decided by another click, its deadline passed, or the hash no
		// longer matches the bytes. The receiver worked; the button was simply no longer live. Answering
		// non-200 would tell a human their approval FAILED when nothing was wrong with it.
		out.Rejected = "the gated call was no longer decidable when the click arrived"
		return out, nil
	}

	// Draw the outcome on the message the human is looking at. A failure here is NOT a failure of the
	// decision (SLK-006): the call has already moved and the run has already been woken.
	//
	// The detail comes from the LEDGER ROW — the tool the executor resolved — never from the payload, and
	// it is passed as the defused text argument while the decider's id is minted separately, the E21 T3
	// split. A tool name is not model-authored today, but it travels the same wire as pub.Display and the
	// wire is what the split protects.
	out.Repaired = a.repairToolDecisionMessage(scoped, conn, intent, lastBotTS, parked)
	return out, nil
}

// repairToolDecisionMessage edits the visible gated-call question in place to reflect the decision. It is
// repairDecisionMessage's twin and shares its contract exactly: it reports whether the edit landed, never
// returns an error, and resolves the bot token at call time. See that function for WHY chat.update rather
// than response_url, and for which message is edited.
func (a *SlackAdmitter) repairToolDecisionMessage(scoped context.Context, conn api.SlackConnectionRef,
	intent slack.ApprovalIntent, lastBotTS string, parked coordinator.ToolApproval) bool {

	target := intent.MessageTS
	if target == "" {
		target = lastBotTS
	}
	if a.doer == nil || target == "" || conn.BotTokenRef == "" {
		return false
	}
	token, err := a.secrets(conn.BotTokenRef)
	if err != nil || len(token) == 0 {
		log.Printf("slack: could not resolve the bot token for connection %s; the tool decision stands but its message is stale", conn.ID)
		return false
	}
	if a.pacer != nil {
		if err := a.pacer.Wait(scoped, intent.ChannelID); err != nil {
			return false
		}
	}
	verdict := "Approved"
	if intent.Decision == "deny" {
		verdict = "Denied"
	}
	body := slack.UpdateMessage(intent.ChannelID, target, verdict+": "+parked.ToolName, intent.UserID)
	res, err := slack.PostMessage(scoped, a.doer, slack.PostRequest{
		MethodURL: a.apiBase + "/chat.update", Token: token, Body: body,
	}, slack.PostOptions{MaxWait: slackRepairMaxWait})
	if err != nil {
		log.Printf("slack: the tool decision on connection %s is durable but its message could not be repaired after %d attempt(s): %v",
			conn.ID, res.Attempts, err)
		return false
	}
	if _, err := a.store.pool.Exec(scoped, storage.Query("UpdateThreadMessageTS"),
		conn.Project, intent.TeamID, intent.ChannelID, intent.ThreadTS, target); err != nil {
		log.Printf("slack: repaired the visible tool-decision message on connection %s but could not record its ts: %v", conn.ID, err)
	}
	return true
}

// decidedPublicationState is the publication state a decision produces — what "my decision landed" looks
// like when some other path settled the command that carried it.
func decidedPublicationState(decision string) string {
	if decision == "deny" {
		return "denied"
	}
	return "approved"
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
	token, err := a.secrets(conn.BotTokenRef)
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
	//
	// The display and the ping are passed SEPARATELY (E21 T3, §3.6 D6): pub.Display is built from the run's
	// own branch name, so it is model-influenced text and is defused inside UpdateMessage, while the decider's
	// id — read off a click this path already authorized — is minted there. Concatenating them here is what
	// put a `<!channel>` branch on the wire.
	body := slack.UpdateMessage(intent.ChannelID, target, verdict+": "+pub.Display, intent.UserID)
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
		conn.Project, intent.TeamID, intent.ChannelID, intent.ThreadTS, target); err != nil {
		log.Printf("slack: repaired the visible message on connection %s but could not record its ts: %v", conn.ID, err)
	}
	return true
}
