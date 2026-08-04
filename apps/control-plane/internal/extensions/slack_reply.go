package extensions

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

// The Slack RETURN LEG (E19, spec §36, SLK-006). Before this, a mention birthed a real run, the run reached
// a terminal state, and nothing wrote back: E19 T2 said so in as many words — the decision chain REPAIRS the
// message a click arrived on, and no in-process path POSTS. The loop was open, and the human who asked was
// left watching a thread that never answered.
//
// THE SHAPE, and every part of it is the tree's existing machinery rather than a second delivery model:
//
//	applyRunTransitionTx (the ONE choke point all five terminal paths share)
//	  → EnqueueTerminalSlackReply   — a durable order-to-post, committed IN the terminal transaction
//	  → SlackReplyPump.Tick         — claims it single-winner, renders it, paces it, posts it
//	  → slack.PostMessage           — the SAME sender the decision repair uses (bounded 429 repair)
//
// FOUR PROPERTIES, each earned somewhere specific rather than hoped for:
//
//  1. LOSS-LESS. The order commits with the run's terminality, so a poster that is down, restarting, or not
//     yet wired cannot lose an answer — it delivers what is already committed.
//  2. EXACTLY ONCE. UNIQUE (run_id) collapses a retried terminal transaction or a reconciled cancel to one
//     row, and ClaimDueSlackReplies claims-and-reschedules in ONE statement so two posters cannot take the
//     same row. The visible result is one message per run. E20 T1 kept that true rather than adding a second
//     writer: when the run follower has already opened a streaming message for this run, the answer CLOSES
//     that message (chat.stopStream) instead of posting beside it.
//  3. PACED. slack.ChannelPacer holds the documented Special-Tier rate BEFORE the call, so a burst of
//     terminals in one channel queues instead of asking Slack to refuse it.
//  4. NON-CORRUPTING (SLK-006). Nothing here can touch the run. A post that fails is retried and eventually
//     retired as `dead`; the canonical result stands and stays readable through /v1/responses. That is why
//     the poster is a separate loop and not a step of finalize.

// slackReplyBackoff is the base wait between attempts on one delivery; ClaimDueSlackReplies grows it
// linearly (1×, 2×, 3× …) so a Slack outage is not answered with a poll loop holding a bot token.
const slackReplyBackoff = 30 * time.Second

// SlackReplyPump drains slack_reply_deliveries. It is a system loop like the webhook and queue pumps: it
// serves every project and sits inert until a Slack-born run terminates, so it runs unconditionally rather
// than behind a flag.
type SlackReplyPump struct {
	bridge *SlackAdmitter
	tick   time.Duration
	batch  int
}

// NewSlackReplyPump wires the poster onto the bridge that already holds everything it needs — the store, the
// secret resolver, the HTTP client, the API base and the per-channel pacer. It requires WithDecisions to
// have run (that is what supplies the outbound half); Tick is a no-op otherwise, so a deployment that mounts
// only the inbound half is not broken by having this started.
func NewSlackReplyPump(bridge *SlackAdmitter) *SlackReplyPump {
	return &SlackReplyPump{bridge: bridge, tick: time.Second, batch: 16}
}

func (p *SlackReplyPump) Run(ctx context.Context) error {
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

// slackReplyOrder is one claimed delivery: where to post, what the run did, and which handle redeems the
// bot token. The token VALUE is never on this struct — it is resolved at call time and lives only inside
// the post, the inboundSecretResolver discipline the rest of this bridge follows.
type slackReplyOrder struct {
	id, project, connectionID string
	runID, responseID         string
	channelID, threadTS       string
	runState                  string
	attempt, maxAttempts      int
	botTokenRef               string
	// requesterUserID is the person this run's answer can address (E21 T3, 000043), frozen at enqueue like
	// the destination above. EMPTY is the fail-closed value every pre-000043 row carries: the answer goes out
	// with no mention rather than with an invented one.
	requesterUserID string
}

// Tick claims every due reply once and posts it, returning how many were posted. Exported for the component
// suite, which drives it directly rather than waiting on a ticker.
//
// SYSTEM-SCOPED, and narrowly: the claim is the genuinely cross-tenant statement (one poster, every
// project), while the response read that follows is scoped to the delivery's OWN tenant — a poster reading
// run output under a system scope would be a tenant boundary held open for no reason.
func (p *SlackReplyPump) Tick(ctx context.Context) (int, error) {
	a := p.bridge
	if a == nil || a.doer == nil || a.secrets == nil {
		return 0, nil // the outbound half is not wired; nothing to deliver through
	}
	rows, err := a.store.pool.Query(storage.WithSystemScope(ctx), storage.Query("ClaimDueSlackReplies"),
		p.batch, int(slackReplyBackoff/time.Second))
	if err != nil {
		return 0, fmt.Errorf("claim due slack replies: %w", err)
	}
	var orders []slackReplyOrder
	for rows.Next() {
		var o slackReplyOrder
		if err := rows.Scan(&o.id, &o.project, &o.connectionID, &o.runID, &o.responseID,
			&o.channelID, &o.threadTS, &o.runState, &o.attempt, &o.maxAttempts, &o.botTokenRef,
			&o.requesterUserID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan due slack reply: %w", err)
		}
		orders = append(orders, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("read due slack replies: %w", err)
	}

	// The claim's rows are drained BEFORE any posting: each post paces on a one-second-per-channel budget,
	// and holding a pooled connection across that would starve the pool for the sake of a loop.
	posted := 0
	for _, o := range orders {
		if p.deliver(ctx, o) {
			posted++
		}
	}
	return posted, nil
}

// deliver posts ONE reply and settles its row. It returns whether the message landed. An unrecoverable
// delivery — the attempts are spent, or the token cannot be redeemed on the last try — retires the row as
// `dead`: the canonical run result is untouched and the row stands as the audit trail of an answer Slack
// never accepted.
//
// ponytail: a PERMANENT Slack error (channel_not_found, not_in_channel, token_revoked) burns all
// max_attempts before dying, because slack.PostMessage's seam is `error` and does not carry the API error
// code back. That is the same ceiling QueueHTTPSinks documents for a permanent 4xx. Widening the seam to
// return the code is worth it the first time a deployment's logs show a workspace being retried at one
// message per 30s for a channel the bot was never in.
func (p *SlackReplyPump) deliver(ctx context.Context, o slackReplyOrder) bool {
	a := p.bridge
	tenant := coordinator.Tenant{Project: o.project}

	token, err := a.secrets(o.botTokenRef)
	if err != nil || len(token) == 0 {
		// The ref name is NOT echoed — an operator sees which connection cannot write, not the handle.
		log.Printf("slack: connection %s cannot redeem its bot token; run %s is complete but its answer is undelivered",
			o.connectionID, o.runID)
		p.retire(ctx, o, "the bot token could not be redeemed")
		return false
	}

	var projection []byte
	if o.responseID != "" {
		// Tenant-scoped, and a missing or purged response is NOT an error: retention may legitimately have
		// reaped the projection before the poster reached it. renderSlackReply then says what the run did
		// without claiming to know what it said.
		view, err := p.spine().GetResponse(ctx, tenant, o.responseID)
		if err != nil {
			log.Printf("slack: could not read response %s for the reply on run %s: %v", o.responseID, o.runID, err)
			p.retire(ctx, o, "the response could not be read")
			return false
		}
		if view.Found && !view.Purged {
			projection = view.Output
		}
	}
	answer := renderSlackReply(o.runState, projection)

	// E22 T5: the artifacts this run's answer POINTS AT are resolved now, before anything is posted, because
	// a refusal has to be visible in the message a human reads — an artifact over the ceiling is linked and
	// SAID, never silently dropped. The bytes are published after the answer lands (see below): nothing here
	// can cost the run its answer (SLK-006).
	uploads, note := p.resolveUploads(ctx, o, answer)
	answer += note

	// The documented per-channel rate, held BEFORE the call rather than recovered from after a 429 (the
	// E19 T2 pacer, reused verbatim — a second pacer would pace against a second budget and neither would
	// be the workspace's).
	if a.pacer != nil {
		if err := a.pacer.Wait(ctx, o.channelID); err != nil {
			return false // cancelled: the row stays claimed and its backoff already scheduled the retry
		}
	}

	scoped := storage.ScopeToTenant(ctx, o.project)

	// E20 T1: if this run's progress is already streaming into a message THIS process opened, the answer
	// CLOSES that message instead of posting a second one. That is what keeps "one run, one visible message"
	// true now that a run can become visible before it finishes.
	//
	// The stream is forgotten FIRST, before the call: appending to (or stopping) a stream twice is refused
	// forever, so a failure here must fall back to a plain post on the next attempt rather than retry a call
	// that can no longer succeed. The cost of being wrong is one message left in Slack's streaming state; the
	// cost of the other order is an answer nobody ever sees.
	if channel, ts, streaming := a.streams.streamFor(o.runID); streaming {
		// E20 T4: the answer becomes markdown_text plus the blocks rendered under it — the ONE call of the
		// three documented to accept blocks (the conservative reading of the contradiction cited at
		// slack.AppendStream). The tasks are read BEFORE the stream is forgotten, and the render is TOTAL:
		// prose comes back as prose with no blocks at all, so an ordinary answer looks exactly as it did.
		//
		// Nothing the MODEL wrote can become an actionable element here — that is the render's own invariant
		// and it is enforced by the sweep in adapters/integrations/slack/blocks_test.go, not by this comment.
		//
		// E21 T3: the requester frozen on THIS row is the one identity the render can address. It is passed
		// in rather than looked up, so the mention a run asks for can only ever reach the person that run's
		// own message came from — and a row from before 000043 passes '' and gets an answer with no mention.
		markdown, blocks := slack.RenderOutput(answer, a.streams.tasksFor(o.runID), o.requesterUserID)
		a.streams.forget(o.runID)
		if err := slack.StopStream(ctx, a.doer, a.apiBase, token, channel, ts, markdown, blocks); err != nil {
			log.Printf("slack: could not close run %s's stream with its answer (attempt %d/%d): %v; the next attempt posts it as a message",
				o.runID, o.attempt, o.maxAttempts, err)
			p.retire(ctx, o, "slack refused to close the stream")
			return false
		}
		if _, err := a.store.pool.Exec(scoped, storage.Query("MarkSlackReplyDelivered"), o.id, ts); err != nil {
			log.Printf("slack: closed run %s's stream but could not record its delivery: %v", o.runID, err)
		}
		p.publishUploads(ctx, o, token, uploads)
		// last_bot_message_ts is deliberately NOT moved to a streaming message's ts. That column is the
		// approval repair's chat.update handle, and whether chat.update works on a message that was streamed
		// is UNCONFIRMED (S16(b)) — handing the repair a handle we cannot promise is editable would trade a
		// working repair for an untested one. It keeps pointing at the last message we know is editable.
		return true
	}

	// E21 T6: the plain-post path is where the model's markdown was being LOST — chat.stopStream carries
	// prose in markdown_text, but this call carried it in `text`, which is Slack's narrower mrkdwn dialect.
	// ReplyMessage keeps `text` exactly as it was (Slack uses it as the notification fallback once blocks are
	// present, so no push preview changes) and puts the answer in a markdown block underneath. Same closed
	// union, same requester, same neutralisation — see adapters/integrations/slack/blocks.go.
	res, err := slack.PostMessage(ctx, a.doer, slack.PostRequest{
		MethodURL: a.apiBase + "/chat.postMessage", Token: token,
		Body: slack.ReplyMessage(o.channelID, o.threadTS, answer, o.responseID, o.requesterUserID),
	}, slack.PostOptions{})
	if err != nil {
		log.Printf("slack: reply for run %s failed on attempt %d/%d after %d call(s): %v",
			o.runID, o.attempt, o.maxAttempts, res.Attempts, err)
		p.retire(ctx, o, "slack refused the message")
		return false
	}

	if _, err := a.store.pool.Exec(scoped, storage.Query("MarkSlackReplyDelivered"), o.id, res.MessageTS); err != nil {
		// The message is VISIBLE. Failing to record that is a duplicate-message risk on the next tick, so it
		// is logged loudly — but there is nothing to undo, and un-posting is not a thing Slack offers.
		log.Printf("slack: posted the reply for run %s but could not record its delivery: %v", o.runID, err)
	}
	// The files go up only now: the row is settled, so a claimed-and-delivered delivery is never re-claimed
	// and files.completeUploadExternal — "This method can only be called once" — is called once per artifact.
	p.publishUploads(ctx, o, token, uploads)

	// The answer is now the newest visible bot message in the thread, so it becomes the SLK-006 repair
	// handle. Best-effort: the message landed either way.
	if _, err := a.store.pool.Exec(scoped, storage.Query("UpdateThreadMessageTS"),
		o.project, slackTeamOf(ctx, a, o.project, o.connectionID), o.channelID, o.threadTS, res.MessageTS); err != nil {
		log.Printf("slack: reply for run %s landed but its ts could not be recorded on the thread: %v", o.runID, err)
	}
	return true
}

// retire marks a delivery dead once its attempts are spent. Below that it does nothing: the claim already
// scheduled the next attempt, so a transient failure simply waits.
func (p *SlackReplyPump) retire(ctx context.Context, o slackReplyOrder, why string) {
	if o.attempt < o.maxAttempts {
		return
	}
	if _, err := p.bridge.store.pool.Exec(storage.ScopeToTenant(ctx, o.project),
		storage.Query("MarkSlackReplyDead"), o.id); err != nil {
		log.Printf("slack: could not retire the undeliverable reply for run %s: %v", o.runID, err)
		return
	}
	log.Printf("slack: gave up delivering the reply for run %s after %d attempts (%s); the run's result is unaffected and readable through /v1/responses",
		o.runID, o.attempt, why)
}

// spine is the coordinator the response read goes through. WithDecisions supplies it; a bridge without one
// cannot have a doer either (both are set together), so Tick has already returned.
func (p *SlackReplyPump) spine() *coordinator.Store { return p.bridge.spine }

// slackTeamOf reads the workspace id the thread belongs to. UpdateThreadMessageTS is keyed by (team,
// channel, thread) and a delivery row carries the connection rather than the team, so this is one lookup
// on a row RLS already confines. A failure yields "" — which simply matches no thread and skips the handle
// update, never a wrong one. Shared with the approval pump, which owns the other kind of delivery row.
func slackTeamOf(ctx context.Context, a *SlackAdmitter, project, connectionID string) string {
	var team string
	if err := a.store.pool.QueryRow(storage.ScopeToTenant(ctx, project),
		`SELECT team_id FROM slack_connections WHERE id = $1 AND project_id = $2`,
		connectionID, project).Scan(&team); err != nil {
		return ""
	}
	return team
}

// renderSlackReply turns a run's terminal state and its stored projection into the words posted back.
//
// WHICH TERMINAL STATES POST — all of them, and that is the decision, not an oversight. `completed` is
// obvious. The other four are not decoration: a human asked a question in a thread and is waiting, and
// silence on failure is indistinguishable from silence on "the service is down", which is precisely the
// state this whole task was opened to end. A run that failed, timed out, was cancelled, or exhausted its
// budget is a fact the asker needs, and it is a fact only we hold.
//
// (A REFUSAL is not among them because it is not a run state: an admission refusal — an unauthorized
// channel, a full queue, a missing run target — happens BEFORE a run exists, so there is no terminal
// transition and nothing to post. Those answers already go back to Slack as the route's own response,
// where the transport can retry them.)
//
// The failure text is deliberately short and carries the response id: it says what happened and where to
// look, and does not paste an internal error surface into a workspace channel.
func renderSlackReply(runState string, projection []byte) string {
	var proj struct {
		Output []contracts.ContentItem `json:"output"`
		Error  *contracts.Problem      `json:"error"`
	}
	if len(projection) > 0 {
		_ = json.Unmarshal(projection, &proj)
	}
	if runState == "completed" {
		if text := strings.TrimSpace(outputText(proj.Output)); text != "" {
			return text
		}
		return "The run completed without producing any output."
	}
	reason := map[string]string{
		"failed":          "The run failed before it could answer.",
		"canceled":        "The run was cancelled before it could answer.",
		"timed_out":       "The run timed out before it could answer.",
		"budget_exceeded": "The run stopped: its budget was exhausted.",
	}[runState]
	if reason == "" {
		reason = "The run ended without answering (" + runState + ")."
	}
	if proj.Error != nil && proj.Error.Title != "" {
		reason += " " + proj.Error.Title
	}
	return reason
}

// outputText pulls the assistant's words out of the stored output items. The canonical item is
// {"type":"message","content":…} (engines/reference output.py output_items, and finalize's own fallback
// shape); anything else — a tool item, a future type — is skipped rather than JSON-dumped into a channel,
// because a reader who wanted the raw structure has /v1/responses.
func outputText(items []contracts.ContentItem) string {
	var parts []string
	for _, item := range items {
		if item.Type() != "message" {
			continue
		}
		switch content := item["content"].(type) {
		case string:
			parts = append(parts, content)
		case []any:
			// A content ARRAY (the §25.10 richer shape): take its text fields, skip the rest.
			for _, el := range content {
				if m, ok := el.(map[string]any); ok {
					if text, ok := m["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
		}
	}
	return strings.Join(parts, "\n\n")
}
