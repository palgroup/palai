package extensions

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/storage"
)

// The Slack APPROVAL POST (E23 T3, 000044 R4, spec §22.4, SLK-006). This file is the answer to a sentence
// that stood in slack_decision.go since E19 T2 and was still true at the start of this epic: nothing in
// this tree POSTED an approval message. `ApprovalMessage` has minted Approve and Deny since E19 and every
// caller of it was a test or a live smoke — so E22 could close with "the agent publishes behind a human's
// button" while, in a real workspace, no human ever saw the button. Half a control is not a control.
//
// THE SHAPE, and it is 000041's return leg statement for statement, deliberately:
//
//	coordinator.RequestPublication (the tx that opens the approval)
//	  → EnqueueApprovalMessage      — a durable order-to-ask, committed WITH the approval
//	  → SlackApprovalPump.Tick      — claims it single-winner, renders it, paces it, posts it
//	  → slack.ApprovalMessage       — its FIRST production caller
//	  → slack.PostMessage           — the same sender the decision repair and the reply pump use
//
// AND SINCE E23 T8 THE SECOND PRODUCER, which is the same shape with one word changed:
//
//	coordinator.RequestToolApproval (the tx that PARKS THE RUN)
//	  → EnqueueApprovalMessage      — the SAME statement, the SAME table, one queue
//	  → SlackApprovalPump.Tick      — the SAME loop, a second claim
//	  → slack.ToolApprovalMessage   — its FIRST production caller
//
// WHY THE SECOND ONE EXISTS AT ALL, since T1..T7 shipped without it: it did not exist, and the epic's own
// exit gate found that out and filed it as HIL-P8. `ToolApprovalMessage` had no production caller and
// `DecideToolApproval` had no production caller, so a gated MCP write tool parked its run and was released
// by the EXPIRY REAPER having asked nobody. Everything needed was already built; nothing joined it.
//
// THE SCREEN IS CHOSEN FROM THE ROW. 000044's CHECK ((publication_id IS NULL) <> (tool_call_id IS NULL))
// makes an approvals row say which kind of thing it authorizes; each claim reads that off its own join and
// RETURNS the word, and deliver() switches on it. Nothing the model wrote and nothing in a payload can
// reach that choice — which matters, because the two screens make different promises: the publication one
// names a destination, the generic one names a call and every one of its arguments.
//
// FOUR PROPERTIES, each earned rather than hoped for:
//
//  1. LOSS-LESS. The order commits with the approval, so a poster that is down cannot lose a question a
//     run is parked on.
//  2. EXACTLY ONCE. UNIQUE (approval_id) collapses a retried request to one row, and the claim IS the
//     update, so two posters cannot take the same delivery. One question, one message.
//  3. PACED. The same ChannelPacer holds the documented per-channel rate before the call.
//  4. NON-CORRUPTING (SLK-006). Nothing here can touch the approval or the run. A question Slack refuses
//     is retried and eventually retired as `dead`; the approval is still recorded, it still EXPIRES, and
//     the reaper still releases the run parked on it. That is why this is a separate loop and not a step
//     of RequestPublication — a Slack outage must cost a human the button, never the run's state.
//
// WHAT IT REFUSES TO POST, and it is the one judgement in the file: an approval that is no longer open —
// a publication past `pending_approval`, a tool call past `approval_pending`. A live Approve button for a
// question already decided is worse than no button, because it invites a click that will authorize nothing
// and report a refusal to somebody who did nothing wrong.

// slackApprovalBackoff is the base wait between attempts on one question; the claim grows it linearly, so
// a Slack outage is not answered with a poll loop holding a bot token. Shorter than the reply pump's 30s
// because a run is PARKED on this one: the reply is late, the question is blocking.
const slackApprovalBackoff = 10 * time.Second

// SlackApprovalPump drains slack_approval_deliveries. A system loop like the reply and webhook pumps: it
// serves every project and sits inert until a run parks on an approval, so it runs unconditionally rather
// than behind a flag.
type SlackApprovalPump struct {
	bridge *SlackAdmitter
	tick   time.Duration
	batch  int
}

// NewSlackApprovalPump wires the poster onto the bridge that already holds the store, the secret resolver,
// the HTTP client, the API base and the pacer. It requires WithDecisions to have run (that is what supplies
// the outbound half); Tick is a no-op otherwise, so a deployment mounting only the inbound half is not
// broken by having this started.
func NewSlackApprovalPump(bridge *SlackAdmitter) *SlackApprovalPump {
	return &SlackApprovalPump{bridge: bridge, tick: time.Second, batch: 16}
}

func (p *SlackApprovalPump) Run(ctx context.Context) error {
	ticker := time.NewTicker(p.tick)
	defer ticker.Stop()
	for {
		if _, err := p.Tick(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// The two screens, named by the value the CLAIM returns rather than by anything a caller decides. A
// delivery row points at an approvals row, and 000044's CHECK ((publication_id IS NULL) <> (tool_call_id
// IS NULL)) makes that row say which kind of thing is being approved; each claim reads it off its own join
// and returns the word. So the switch in deliver() is a switch on a COLUMN — nothing the model emitted,
// nothing in the payload text, and nothing this process inferred.
const (
	approvalKindPublication = "publication"
	approvalKindToolCall    = "tool_call"
)

// slackApprovalOrder is one claimed question: where to ask, what to show, and which handle redeems the bot
// token. The token VALUE is never on this struct — it is resolved at call time and lives only inside the
// post, the inboundSecretResolver discipline the rest of this bridge follows.
type slackApprovalOrder struct {
	id, org, project, connectionID string
	runID                          string
	channelID, threadTS            string
	attempt, maxAttempts           int
	botTokenRef                    string
	// kind is the discriminator, straight out of the claim that matched. deliver() switches on it and
	// refuses anything else rather than falling through to a default screen.
	kind string
	// requestHash is the one-shot binding every button's value carries, on both screens.
	requestHash string
	// display is the RESOLVED DESTINATION (publicationDisplay), read live off the publication rather than
	// frozen here: what a human reads has to be what will run, and a copy is a thing that can drift.
	// PUBLICATION ORDERS ONLY.
	display string
	// approvalID, toolName and arguments are the GATED CALL's own facts, read live off the ledger row for
	// the same reason: the render happens once, from these, in slack.ToolApprovalMessage. TOOL ORDERS ONLY.
	approvalID, toolName string
	arguments            []byte
	// state gates the post: only a question still open is worth asking. It is the PUBLICATION's state on a
	// publication order (`pending_approval`) and the TOOL CALL's on a tool order (`approval_pending`) —
	// two different vocabularies for one idea, which is why openState() names it rather than a literal.
	state string
}

// openState is the state this order's kind calls "still worth asking about". The two machines spell it
// differently (§26.7's tool-call state vs the publication lifecycle), and a poster that compared against
// one literal would silently treat every order of the other kind as already decided.
func (o slackApprovalOrder) openState() string {
	if o.kind == approvalKindToolCall {
		return "approval_pending"
	}
	return "pending_approval"
}

// Tick claims every due question once and posts it, returning how many were posted. Exported for the
// component suite, which drives it directly rather than waiting on a ticker.
//
// SYSTEM-SCOPED, and narrowly: the claim is the genuinely cross-tenant statement (one poster, every
// project). Everything after it is scoped to the delivery's own tenant.
func (p *SlackApprovalPump) Tick(ctx context.Context) (int, error) {
	a := p.bridge
	if a == nil || a.doer == nil || a.secrets == nil {
		return 0, nil // the outbound half is not wired; nothing to ask through
	}
	orders, err := p.claimPublications(ctx)
	if err != nil {
		return 0, err
	}
	// The second producer's claim (E23 T8). Two statements rather than one for the reason the SQL records:
	// the two screens carry different guarantees, and a claim serving both would have to return the union of
	// two row shapes and then choose. Each takes only its own kind, so neither can starve the other.
	toolOrders, err := p.claimToolCalls(ctx)
	if err != nil {
		return 0, err
	}
	orders = append(orders, toolOrders...)

	// Drained BEFORE any posting: each post paces on a per-channel budget, and holding a pooled connection
	// across that would starve the pool for the sake of a loop.
	posted := 0
	for _, o := range orders {
		if p.deliver(ctx, o) {
			posted++
		}
	}
	return posted, nil
}

// claimPublications takes this tick's batch of publication questions.
func (p *SlackApprovalPump) claimPublications(ctx context.Context) ([]slackApprovalOrder, error) {
	rows, err := p.bridge.store.pool.Query(storage.WithSystemScope(ctx), storage.Query("ClaimDueApprovalMessages"),
		p.batch, int(slackApprovalBackoff/time.Second))
	if err != nil {
		return nil, fmt.Errorf("claim due approval messages: %w", err)
	}
	defer rows.Close()
	var orders []slackApprovalOrder
	for rows.Next() {
		var o slackApprovalOrder
		if err := rows.Scan(&o.id, &o.org, &o.project, &o.connectionID, &o.runID,
			&o.channelID, &o.threadTS, &o.attempt, &o.maxAttempts, &o.botTokenRef,
			&o.requestHash, &o.display, &o.state, &o.kind); err != nil {
			return nil, fmt.Errorf("scan due approval message: %w", err)
		}
		orders = append(orders, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read due approval messages: %w", err)
	}
	return orders, nil
}

// claimToolCalls takes this tick's batch of gated-tool-call questions. The arguments come back as the
// ledger row's text and are carried as bytes: the render is slack.ToolApprovalMessage's job, once, and a
// screen decoded here would be a second place the same JSON is interpreted.
func (p *SlackApprovalPump) claimToolCalls(ctx context.Context) ([]slackApprovalOrder, error) {
	rows, err := p.bridge.store.pool.Query(storage.WithSystemScope(ctx), storage.Query("ClaimDueToolApprovalMessages"),
		p.batch, int(slackApprovalBackoff/time.Second))
	if err != nil {
		return nil, fmt.Errorf("claim due tool approval messages: %w", err)
	}
	defer rows.Close()
	var orders []slackApprovalOrder
	for rows.Next() {
		var (
			o    slackApprovalOrder
			args string
		)
		if err := rows.Scan(&o.id, &o.org, &o.project, &o.connectionID, &o.runID,
			&o.channelID, &o.threadTS, &o.attempt, &o.maxAttempts, &o.botTokenRef,
			&o.requestHash, &o.approvalID, &args, &o.toolName, &o.state, &o.kind); err != nil {
			return nil, fmt.Errorf("scan due tool approval message: %w", err)
		}
		o.arguments = []byte(args)
		orders = append(orders, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read due tool approval messages: %w", err)
	}
	return orders, nil
}

// deliver posts ONE question and settles its row. It returns whether the message landed.
func (p *SlackApprovalPump) deliver(ctx context.Context, o slackApprovalOrder) bool {
	a := p.bridge

	// A question already answered is not asked. The thing moved between the enqueue and this tick — a
	// boundary-pump approve, a deny from the native command surface, or the expiry reaper — so posting a
	// live button now would invite a click that authorizes nothing.
	if o.state != o.openState() {
		p.retire(ctx, o, "the approval was decided before the question could be asked")
		return false
	}

	// THE SCREEN, CHOSEN FROM THE ROW, and it is built FIRST — before a credential is redeemed and before
	// any pacer budget is spent — because a message this poster cannot construct is a message it must not
	// pay for. A publication order gets E19's two-button ApprovalMessage, whose bytes have not moved since
	// it shipped; a tool-call order gets T4's three-button argument table. The choice is the `kind` the
	// claim returned, so it is a column and not an inference, and an order whose kind is neither posts
	// NOTHING rather than defaulting to a screen that would describe it wrongly.
	//
	// EVERY ARGUMENT TO BOTH IS INFRASTRUCTURE-DERIVED. The publication detail is publicationDisplay's
	// resolved destination; the tool identity is the name the EXECUTOR resolved and the arguments are the
	// ledger row's committed bytes, never the model's frame; and both buttons' values are the one-shot
	// request hash. Not one character comes from the model — which is what makes interactions.go's
	// single-mint rule meaningful rather than decorative, since this is the path that puts a mint in front
	// of a human.
	body, ok := p.screen(ctx, o)
	if !ok {
		p.retire(ctx, o, "the delivery names no screen this poster can build")
		return false
	}

	token, err := a.secrets(o.botTokenRef)
	if err != nil || len(token) == 0 {
		// The ref name is NOT echoed — an operator sees which connection cannot write, not the handle.
		log.Printf("slack: connection %s cannot redeem its bot token; run %s is parked on an approval nobody can be shown",
			o.connectionID, o.runID)
		p.retire(ctx, o, "the bot token could not be redeemed")
		return false
	}

	// The documented per-channel rate, held BEFORE the call rather than recovered from after a 429 (the
	// E19 T2 pacer, reused — a second pacer would pace against a second budget and neither would be the
	// workspace's).
	if a.pacer != nil {
		if err := a.pacer.Wait(ctx, o.channelID); err != nil {
			return false // cancelled: the row stays claimed and its backoff already scheduled the retry
		}
	}
	res, err := slack.PostMessage(ctx, a.doer, slack.PostRequest{
		MethodURL: a.apiBase + "/chat.postMessage", Token: token, Body: body,
	}, slack.PostOptions{})
	if err != nil {
		log.Printf("slack: the approval question for run %s failed on attempt %d/%d after %d call(s): %v",
			o.runID, o.attempt, o.maxAttempts, res.Attempts, err)
		p.retire(ctx, o, "slack refused the message")
		return false
	}

	scoped := storage.ScopeToTenant(ctx, o.project)
	if _, err := a.store.pool.Exec(scoped, storage.Query("MarkApprovalMessageDelivered"), o.id, res.MessageTS); err != nil {
		// The message is VISIBLE. Failing to record that is a duplicate-question risk on the next tick, so it
		// is logged loudly — but there is nothing to undo, and un-posting is not a thing Slack offers.
		log.Printf("slack: asked the approval question for run %s but could not record its delivery: %v", o.runID, err)
	}
	// The question is now the newest visible bot message in the thread, so it becomes the SLK-006 repair
	// handle: a decision that arrives without a message ts still edits THIS message in place. Best-effort —
	// the message landed either way, and the click itself carries the ts in the ordinary case.
	if _, err := a.store.pool.Exec(scoped, storage.Query("UpdateThreadMessageTS"),
		o.project, slackTeamOf(ctx, a, o.org, o.project, o.connectionID), o.channelID, o.threadTS, res.MessageTS); err != nil {
		log.Printf("slack: the approval question for run %s landed but its ts could not be recorded on the thread: %v", o.runID, err)
	}
	return true
}

// screen renders the Block Kit body this order's KIND calls for, or reports that it cannot. The default is
// deliberately not a screen: an unrecognized kind means the claim and this switch have drifted apart, and
// posting the publication screen for a tool call would put a two-button message with no arguments in front
// of a human about to authorize a side effect. Silence is the honest failure there.
func (p *SlackApprovalPump) screen(ctx context.Context, o slackApprovalOrder) ([]byte, bool) {
	switch o.kind {
	case approvalKindPublication:
		return slack.ApprovalMessage(o.channelID, o.threadTS, o.display, o.requestHash), true
	case approvalKindToolCall:
		// The OPERATOR'S LABEL, resolved through the SAME lookup the modal and the executor resolve the call
		// through, so the one human sentence on the screen belongs to the tool that will actually run. A tool
		// that no longer resolves renders as "(no operator label)" rather than blocking the question: the
		// arguments are the authority on this screen and they came off the ledger row.
		label := ""
		if tool, ok, err := p.bridge.store.LookupTool(ctx, o.project, o.runID, o.toolName); err == nil && ok {
			label = tool.ApprovalLabel
		}
		return slack.ToolApprovalMessage(o.channelID, o.threadTS, slack.ApprovalRequest{
			ApprovalID:    o.approvalID,
			RequestHash:   o.requestHash,
			Identity:      o.toolName,
			OperatorLabel: label,
			Arguments:     o.arguments,
		}), true
	default:
		log.Printf("slack: delivery %s names approval kind %q, which this poster cannot render; run %s stays parked until its approval expires",
			o.id, o.kind, o.runID)
		return nil, false
	}
}

// retire marks a question dead once its attempts are spent, or immediately when re-asking cannot help.
// Below that it does nothing: the claim already scheduled the next attempt, so a transient failure waits.
func (p *SlackApprovalPump) retire(ctx context.Context, o slackApprovalOrder, why string) {
	if o.state == o.openState() && o.attempt < o.maxAttempts {
		return
	}
	if _, err := p.bridge.store.pool.Exec(storage.ScopeToTenant(ctx, o.project),
		storage.Query("MarkApprovalMessageDead"), o.id); err != nil {
		log.Printf("slack: could not retire the undeliverable approval question for run %s: %v", o.runID, err)
		return
	}
	log.Printf("slack: gave up asking run %s's approval question after %d attempt(s) (%s); the approval is still recorded, still expires, and the reaper still releases the run",
		o.runID, o.attempt, why)
}
