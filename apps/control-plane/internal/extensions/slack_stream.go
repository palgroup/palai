package extensions

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
)

// The RUN FOLLOWER (E20 T1, spec §36): while a Slack-born run is working, the thread shows it working.
//
// Before this, the sequence a human saw was: mention → nothing at all → the whole answer. The nothing could
// last as long as a model call, and it is indistinguishable from a broken integration.
//
// THE SHAPE, and every part of it is existing machinery rather than a second delivery model:
//
//	Admit (Replayed == false)
//	  → follow          — one supervised tailer per BORN run, capped by a semaphore
//	  → SetStatus       — the working indicator; chat:write, so no new scope and no panel needed (S3)
//	  → EventReader.After — the SAME journal seam the SSE endpoint tails; no new read path exists
//	  → StartStream / AppendStream — the message appears when the first step lands
//	  → (the reply pump's StopStream closes it with the answer — see slack_reply.go)
//
// HONEST CEILING, and it is the most over-claimable thing in this epic, so it is stated three ways:
//
//  1. THIS IS NOT TOKEN STREAMING. The journal has no delta event; its finest-grained output event is
//     model_step.completed.v1. E08's rule (no tools are exposed to a real provider) makes a real Slack run
//     SINGLE-STEP, so a real run has exactly ONE thing to stream.
//  2. THE JOURNAL DOES NOT CARRY THE MODEL'S WORDS. model_step.completed.v1's payload is
//     {run_id, model_request_id} — the text lives in the model request's stored result, behind a read path
//     this follower deliberately does not open. So what streams here is PROGRESS, not prose: the answer
//     itself arrives in one piece when the stream is closed. What the human actually gains is (a) a spinning
//     status while the model works and (b) a message that appears the moment the first step lands rather
//     than after the terminal transaction. Nothing more than that is claimed anywhere.
//  3. THE STREAM STATE IS DELIBERATELY NON-DURABLE. The ts and the cursor live in this process. A restart
//     loses them; that message stays in Slack's streaming state (S16(a) is unconfirmed) and the answer lands
//     as a NEW message instead. Visible, not destructive — the canonical result is untouched either way
//     (SLK-006). Migration 000042 (stream_ts + journal_cursor) is what fixes it, and its trigger is written
//     down rather than guessed: open it when this behaviour is OBSERVED in real use, with owner approval.
//     Not before (YAGNI).
//
// A multi-step, tool-driven, task-carded run streams richly — but only a FAKE ENGINE produces one today, and
// the tests that drive it say so in their names.

const (
	// slackStreamPoll is the journal tail cadence — the SSE endpoint's own PollInterval, for the same reason:
	// it is the latency a human reads as "live" without turning the database into a spin loop.
	slackStreamPoll = 500 * time.Millisecond
	// slackStreamBatch bounds one read, the SSE handler's BatchLimit halved (a follower holds its batch in a
	// goroutine that may be one of many).
	slackStreamBatch = 128
	// slackStreamBudget retires a follower whose run never reaches a terminal event — a lost terminal must not
	// pin a semaphore slot for the life of the process. The stream it opened is NOT forgotten: the reply pump
	// still closes it when the answer arrives.
	slackStreamBudget = 30 * time.Minute
	// defaultSlackStreamConcurrency caps followers in flight. The number comes from the vendor's own asymmetry
	// (S8): chat.startStream is Tier 2 (20+/min) while chat.appendStream is Tier 4 (100+/min), so STARTING
	// streams is five times scarcer than appending to them, and the real ceiling on concurrent decorated runs
	// is startStream. Eight leaves headroom under twenty for the approval messages and replies sharing the
	// workspace's budget.
	defaultSlackStreamConcurrency = 8
)

// slackStatusThinking is the working indicator, and slackLoadingMessages are what Slack rotates through
// beside it (at most ten — S15). This is the part of T1 that pays off even for a single-step run: the model
// call is where the wait is, and this is what fills it.
const slackStatusThinking = "is thinking…"

var slackLoadingMessages = []string{"reading the thread…", "asking the model…", "putting it together…"}

// slackStreamStoppedNotice is what the thread is told when the HUMAN stops the stream from Slack's own UI
// (S11). It says exactly what happened: the live view stopped, the work did not.
const slackStreamStoppedNotice = "Live updates stopped. The run is still going — its result will appear here when it finishes."

// slackRunTerminalEvents ends a follower. It mirrors api.terminalEventTypes (unexported there); the SSE
// endpoint closes on exactly these five, and a follower that ended on a different set would either hang or
// abandon a stream mid-run.
var slackRunTerminalEvents = map[string]bool{
	"run.completed.v1":       true,
	"run.failed.v1":          true,
	"run.canceled.v1":        true,
	"run.timed_out.v1":       true,
	"run.budget_exceeded.v1": true,
}

// SlackStreamFollower tails born runs and decorates their threads. It is mounted through WithStreaming and is
// nil-safe throughout: a deployment that does not wire it admits and answers exactly as it did before.
type SlackStreamFollower struct {
	bridge *SlackAdmitter
	events api.EventReader
	sup    *coordinator.Supervisor
	// sem caps followers in flight. Over the cap a run is NOT queued and NOT dropped — it simply goes
	// undecorated, and its answer posts as a plain message through the unchanged reply pump.
	sem chan struct{}

	poll   time.Duration
	budget time.Duration

	// open maps a run to the streaming message opened for it, so the reply pump can CLOSE that message with
	// the answer instead of posting a second one. In-process on purpose (ceiling 3 above).
	mu   sync.Mutex
	open map[string]slackOpenStream
}

type slackOpenStream struct{ channel, ts string }

// WithStreaming mounts the follower. events is the SAME api.EventReader the SSE endpoint reads through — the
// production store — so no second journal read path exists. maxConcurrent <= 0 takes the default.
//
// It requires WithDecisions to have run (that is what supplies the outbound client, the API base and the
// pacer); without it every method here is inert, which is the same posture the reply pump takes.
func (a *SlackAdmitter) WithStreaming(events api.EventReader, sup *coordinator.Supervisor, maxConcurrent int) *SlackAdmitter {
	if maxConcurrent <= 0 {
		maxConcurrent = defaultSlackStreamConcurrency
	}
	a.streams = &SlackStreamFollower{
		bridge: a, events: events, sup: sup,
		sem:  make(chan struct{}, maxConcurrent),
		poll: slackStreamPoll, budget: slackStreamBudget,
		open: map[string]slackOpenStream{},
	}
	return a
}

// slackStreamTarget is one run to follow: where to write, which journal to tail, and which handle redeems the
// bot token. The token VALUE is not on it — it is resolved inside the follower and lives only there.
type slackStreamTarget struct {
	org, project        string
	sessionID, runID    string
	channel, threadTS   string
	recipientUser, team string
	botTokenRef         string
}

// follow starts ONE follower for a freshly born run.
//
// EXACTLY-ONCE, and note where the guarantee actually comes from: it is NOT re-derived here. Admit calls this
// only when the admission's idempotency reservation reported Replayed == false, so a Slack redelivery — which
// replays onto the same response and births no second run — reaches this function zero times. One run, at
// most one StartStream, enforced upstream of anything this file could get wrong.
func (f *SlackStreamFollower) follow(ctx context.Context, conn api.SlackConnectionRef, ev slack.Event, sessionID, runID string) {
	if f == nil || f.bridge == nil || f.bridge.doer == nil || f.bridge.secrets == nil || f.events == nil {
		return // the outbound half is not wired; the run answers exactly as it did before
	}
	select {
	case f.sem <- struct{}{}:
	default:
		// OVER THE CAP: skipped, not queued and not dropped. The answer still lands — as a plain message,
		// through the reply pump that has always posted it.
		log.Printf("slack: %d run streams already in flight; run %s runs undecorated (its answer still posts)", cap(f.sem), runID)
		return
	}
	target := slackStreamTarget{
		org: conn.Org, project: conn.Project, sessionID: sessionID, runID: runID,
		channel: ev.ChannelID, threadTS: ev.ThreadTS,
		// S9: chat.startStream needs BOTH recipient ids when it writes to a channel, and both are already on
		// the event. Absent, StartStream refuses without calling and the run goes undecorated.
		recipientUser: ev.UserID, team: ev.TeamID,
		botTokenRef: conn.BotTokenRef,
	}
	// context.WithoutCancel: the caller's context dies with the Slack ack (a 3-second budget), and the run it
	// is following outlives that by definition. The follower is bounded by its own budget instead.
	go func() {
		defer func() { <-f.sem }()
		f.sup.Supervise(context.WithoutCancel(ctx), "slack-stream", func(c context.Context) error {
			f.tail(c, target)
			// ALWAYS nil, and this is load-bearing rather than laziness: Supervise RESTARTS a loop that
			// returns an error, and a restarted follower would open a SECOND stream for one run. The panic
			// guard and the named loop are what this call is here for; the restart is what must not happen.
			return nil
		})
	}()
}

// tail is one run's whole visible life: set the status, walk the journal, open the stream when there is
// something to show, and clear the status when the run ends. Every Slack failure inside it is logged and
// abandoned rather than raised — SLK-006's invariant is that nothing Slack does can touch the run, and the
// answer's delivery is the reply pump's job either way.
func (f *SlackStreamFollower) tail(ctx context.Context, tg slackStreamTarget) {
	a := f.bridge
	token, err := a.secrets(tg.org, tg.botTokenRef)
	if err != nil || len(token) == 0 {
		// The ref name is not echoed — an operator sees which run went undecorated, not the handle.
		log.Printf("slack: run %s cannot be followed; its connection's bot token could not be redeemed", tg.runID)
		return
	}

	// The status FIRST, before any journal read: it is the one thing that helps a single-step run, and it is
	// the whole visible win on a workspace that has not enabled the agent panel.
	if err := slack.SetStatus(ctx, a.doer, a.apiBase, token, tg.channel, tg.threadTS, slackStatusThinking, slackLoadingMessages); err != nil {
		log.Printf("slack: could not set the working status for run %s: %v", tg.runID, err)
	}
	defer func() {
		// Clearing is best-effort but not optional: a thread left saying "is thinking…" after the answer
		// landed is a claim the next reader has no way to check.
		if err := slack.SetStatus(context.WithoutCancel(ctx), a.doer, a.apiBase, token, tg.channel, tg.threadTS, "", nil); err != nil {
			log.Printf("slack: could not clear the working status for run %s: %v", tg.runID, err)
		}
	}()

	var (
		cursor   int64
		steps    int
		streamTS string
		deadline = time.Now().Add(f.budget)
	)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return
		}
		batch, err := f.events.After(ctx, tg.org, tg.project, tg.sessionID, cursor, slackStreamBatch)
		if err != nil {
			log.Printf("slack: could not tail the journal for run %s: %v", tg.runID, err)
			return
		}
		for _, event := range batch {
			cursor = int64(event.Sequence)
			// A Slack THREAD is one session across many runs (SLK-003), so the session's journal carries every
			// earlier run's events too. Filtering on the run id is what keeps a follow-up message from
			// replaying the previous answer's progress into a new stream.
			if slackEventRunID(event) != tg.runID {
				continue
			}
			if slackRunTerminalEvents[event.Type] {
				return // the reply pump closes the stream with the answer
			}
			line := slackStreamLine(event, &steps)
			if line == "" {
				continue
			}
			if streamTS == "" {
				ts, err := slack.StartStream(ctx, a.doer, a.apiBase, token, slack.StreamStart{
					Channel: tg.channel, ThreadTS: tg.threadTS,
					RecipientUserID: tg.recipientUser, RecipientTeamID: tg.team,
					MarkdownText: line,
				})
				if err != nil {
					// No stream. The run is undecorated and its answer posts plainly — the same outcome as
					// being over the concurrency cap, reached a different way.
					log.Printf("slack: could not open a stream for run %s: %v", tg.runID, err)
					return
				}
				streamTS = ts
				f.remember(tg.runID, tg.channel, ts)
				continue
			}
			// The documented per-channel rate, held BEFORE the call (the E19 T2 pacer, reused — a second
			// pacer would pace against a second budget and neither would be the workspace's).
			if a.pacer != nil {
				if err := a.pacer.Wait(ctx, tg.channel); err != nil {
					return
				}
			}
			if err := slack.AppendStream(ctx, a.doer, a.apiBase, token, tg.channel, streamTS, line); err != nil {
				if slack.APIErrorCode(err) == slack.CodeStoppedByUser {
					f.stoppedByUser(ctx, tg, token)
					return
				}
				log.Printf("slack: could not append to run %s's stream: %v", tg.runID, err)
				return
			}
		}
		if len(batch) < slackStreamBatch {
			if sleepCtx(ctx, f.poll) != nil {
				return
			}
		}
	}
	log.Printf("slack: stopped following run %s after %s without a terminal event; its answer will still post", tg.runID, f.budget)
}

// stoppedByUser handles S11 — the human pressed stop in Slack's UI.
//
// THE DECISION, and it is the plan's §2 invariant rather than a convenience: the stream stops, THE RUN DOES
// NOT. `stopped_by_user` arrives as an API error code on an append; it carries no authenticated actor, no
// approver identity and no command. Run control in this tree is AcceptCommand behind a verified principal,
// and nothing about this error is that. So the honest thing — and the only thing that does not invent an
// authorization path — is to stop writing and say so.
//
// The stream is FORGOTTEN, so the answer arrives as a plain message: appending to a stopped stream is
// refused forever, and the reply pump must not spend its attempts discovering that.
func (f *SlackStreamFollower) stoppedByUser(ctx context.Context, tg slackStreamTarget, token []byte) {
	a := f.bridge
	f.forget(tg.runID)
	log.Printf("slack: the user stopped run %s's live stream; the run continues and its answer will post as a message", tg.runID)
	if a.pacer != nil {
		if err := a.pacer.Wait(ctx, tg.channel); err != nil {
			return
		}
	}
	if _, err := slack.PostMessage(ctx, a.doer, slack.PostRequest{
		MethodURL: a.apiBase + "/chat.postMessage", Token: token,
		Body: slack.ThreadReply(tg.channel, tg.threadTS, slackStreamStoppedNotice, ""),
	}, slack.PostOptions{}); err != nil {
		log.Printf("slack: could not tell run %s's thread that its live stream was stopped: %v", tg.runID, err)
	}
}

// remember/streamFor/forget are the in-process handoff to the reply pump: the pump CLOSES the message this
// follower opened rather than posting a second one, which is what keeps "one run, one visible message" true
// once streaming exists.
func (f *SlackStreamFollower) remember(runID, channel, ts string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.open[runID] = slackOpenStream{channel: channel, ts: ts}
}

// streamFor reports the message opened for a run, if this process opened one. A nil follower, another
// replica's run, or a restart all answer "no" — and "no" means the answer posts as a plain message, which is
// exactly what happened before streaming existed.
func (f *SlackStreamFollower) streamFor(runID string) (channel, ts string, ok bool) {
	if f == nil {
		return "", "", false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.open[runID]
	return s.channel, s.ts, ok
}

func (f *SlackStreamFollower) forget(runID string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.open, runID)
}

// slackEventRunID reads the run a journal event belongs to. Every event this follower acts on carries it in
// its payload (run.*.v1 write {"run_id","state"}; model_step/tool_call write it beside their own ids), and an
// event without one is not this run's business.
func slackEventRunID(event contracts.Event) string {
	id, _ := event.Data["run_id"].(string)
	return id
}

// slackStreamLine maps ONE journal event to the line the thread shows for it, or "" for an event a human
// gains nothing from. It is TOTAL: an unknown event type is silently skipped rather than dumped into a
// workspace channel.
//
// WHAT IT CANNOT DO, restated because it is the ceiling and not an oversight: none of these events carries
// the model's OUTPUT. model_step.completed.v1's payload is {run_id, model_request_id}; tool_call's is
// {run_id, tool_call_id}. So these lines describe PROGRESS. The words the model wrote arrive when the stream
// is closed with the run's answer.
//
// steps is the caller's step counter — a real run reaches 1 and stops there (E08: no tools are exposed to a
// real provider, so a real run is single-step); higher numbers only ever appear under a fake engine.
func slackStreamLine(event contracts.Event, steps *int) string {
	switch event.Type {
	case "model_step.completed.v1":
		*steps++
		if *steps == 1 {
			return "_Working on it…_"
		}
		return fmt.Sprintf("_…step %d_", *steps)
	case "tool_call.executing.v1":
		// The payload carries the call id, not the tool's NAME, so the line cannot name it. Saying "a tool"
		// is true; inventing a name from the id would not be.
		return "• running a tool…"
	case "tool_call.completed.v1", "tool_call.reconciled_completed.v1":
		return "• tool finished"
	case "task.created.v1", "task.updated.v1":
		return slackTaskLine(event.Data)
	case "approval.requested.v1":
		return "• waiting for an approval…"
	case "child.requested.v1":
		return "• delegated to a sub-agent…"
	}
	return ""
}

// slackTaskLine renders a durable task event. These are the ONLY journal events that carry human-readable
// text of their own (title + status), which is why a multi-step run reads well and a single-step one has
// nothing beyond its opener.
//
// The status is echoed as the model wrote it, marked as a quotation rather than as our own word for it: task
// statuses are free strings in this tree, and mapping them onto a fixed vocabulary is E20 T4's job (with a
// fail-closed default), not something to guess here.
func slackTaskLine(data map[string]any) string {
	title, _ := data["title"].(string)
	if title == "" {
		return ""
	}
	if status, _ := data["status"].(string); status != "" {
		return fmt.Sprintf("• %s — %s", title, status)
	}
	return "• " + title
}

// sleepCtx is a cancelable pause. (slack.PostOptions has its own; this one is for the tail loop.)
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
