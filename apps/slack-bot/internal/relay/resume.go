// This file is the half of the relay that only ever runs because a PREVIOUS process stopped existing.
//
// THE DEFECT IT CLOSES, stated as what it did to a person. This bot's answer came from one place: a
// relay.Run goroutine draining a live event stream (relay.go). Nothing else delivered it, and how far that
// goroutine had got was known only to a map in this process's memory (inbound.go's inboundState). So a bot
// that was restarted mid-run — which on this tree happens several times an hour while it is being worked on
// — did not delay that thread's answer, it destroyed it: the run completed on the control plane, its text
// was journalled, and the Slack thread kept whatever the dead process had last written, usually "Working…".
// The operator experienced it as a thread that stopped talking. The old in-process bridge this relay
// replaced had no such hole, because it enqueued the reply inside the run's own terminal transaction
// (packages/coordinator/store.go EnqueueTerminalSlackReply) and a separate pump delivered it.
//
// WHAT MAKES RECOVERY POSSIBLE OVER A PUBLIC API, measured rather than assumed (2026-08-04, against the
// running control plane):
//
//   - GET /v1/sessions/{id}/events?after_sequence=N is a REPLAY of the durable journal followed by a tail,
//     and it closes cleanly at the run's terminal (apps/control-plane/api/events.go:68-73,132-155). Against
//     ses_56a78aa98ab2931549988426d1650586: after_sequence=0 delivered 28 events ending run.completed.v1
//     with the server closing the connection; after_sequence=10 delivered 18, the first of them sequence 11.
//     So the answer to a run that finished while this process was dead is STILL THERE to be fetched — the
//     recovery below is not a race against the run, it is a read of a log.
//   - A Slack stream is addressed by (channel, ts) and the bot token, and belongs to no connection: one curl
//     invocation opened a stream in C0B8BSXETHV, a second appended to it, a third closed it, all ok, and the
//     finished message read "half one, by process A. HALF TWO, BY PROCESS B AFTER A DIED. closed by process
//     C." So recovery finishes the message a reader is ALREADY looking at rather than posting a second one
//     underneath it — and the card that would otherwise say "streaming" forever gets closed.
//
// NO NEW CONTROL-PLANE ROUTE IS NEEDED FOR ANY OF THIS, and that was the choice worth measuring. The other
// design on the table was for the control plane to grow an "undelivered work" queue for bots. It is not
// needed: the journal already IS that queue, and the only thing missing was a bot that remembered where it
// had got to — which is a column in the bot's own database (migrations/0002_delivery_state.sql), not a new
// public surface with its own auth, pagination and retention story.
package relay

import (
	"context"
	"fmt"

	"github.com/palgroup/palai/apps/slack-bot/internal/store"
	palai "github.com/palgroup/palai/sdks/go"
)

// RecoverPendingRuns finishes every run this bot was rendering when it last stopped.
//
// It is called ONCE at boot, before the Socket Mode connection opens (apps/slack-bot/main.go), and it
// returns as soon as the recoveries have been HANDED OFF — each one drains on its own goroutine through
// RunInBackground, exactly like a live turn, so a recovery of a run that is still working does not hold the
// socket shut behind it.
//
// A THREAD IS RECOVERED EVEN IF ITS RUN FINISHED HOURS AGO, and that is the case that matters most rather
// than an edge: the common shape is a bot killed during a run that then completed perfectly well
// server-side. The journal read below returns that run's remaining events immediately and its terminal
// closes the stream, so the whole recovery is a few hundred milliseconds and the answer lands in the
// message it was always meant to land in.
//
// IT NEVER STARTS A RUN and never speaks for one. Everything here is a read of what the control plane has
// already recorded plus the Slack calls that render it, so a recovery cannot double-charge a model, cannot
// re-execute a tool, and cannot ask a human a question twice — the run whose events these are is the same
// run, still identified the same way.
func RecoverPendingRuns(ctx context.Context, deps InboundDeps) error {
	if err := deps.validate(); err != nil {
		return err
	}
	pending, err := deps.Store.PendingDeliveries(ctx, deps.BotID)
	if err != nil {
		return fmt.Errorf("relay: list the runs left unfinished by the previous process: %w", err)
	}
	if len(pending) == 0 {
		return nil
	}
	deps.logf("relay: %d thread(s) were left mid-answer by a previous process; finishing them", len(pending))
	for _, p := range pending {
		deps.resume(ctx, p)
	}
	return nil
}

// resume finishes one thread's interrupted run.
//
// THE CONTEXT IS DETACHED FROM THE CALLER'S for the reason startRun's own detach comment gives at length,
// and the mistake it records is the same one waiting here: *palai.SessionEventStream reads under the
// context it was OPENED with, so a stream opened under a boot-scoped ctx and drained on a goroutine
// delivers zero events and reports io.EOF — which relay.Run treats as a clean exit, so the recovery would
// report success and deliver nothing. That failure is invisible by construction; only the detach prevents it.
func (deps InboundDeps) resume(ctx context.Context, p store.PendingRun) {
	tk := threadKey{botID: deps.BotID, teamID: p.TeamID, channelID: p.ChannelID, threadTS: p.ThreadTS}
	relayCtx := context.WithoutCancel(ctx)

	stream, err := deps.Palai.SessionEvents(relayCtx, p.SessionID, palai.EventsParams{AfterSequence: p.LastSequence})
	if err != nil {
		if isNotFound(err) {
			// THE SESSION IS GONE, so there is no answer left to deliver and no amount of retrying will
			// find one — this is Task 8's expected steady state (store's package doc), reached here by a
			// session closed or purged while this bot was down. Close the abandoned stream so the thread
			// does not keep a card spinning forever over a run nobody can finish, then clear the record:
			// leaving it would make every future boot re-attempt a read that can only 404.
			deps.closeAbandonedStream(relayCtx, p, "the session this run belonged to no longer exists")
			if endErr := deps.Store.EndDelivery(relayCtx, tk.botID, tk.teamID, tk.channelID, tk.threadTS); endErr != nil {
				deps.logf("relay: could not clear the delivery record for the vanished session %s: %v", p.SessionID, endErr)
			}
			deps.logf("relay: %s/%s was mid-answer on session %s, which no longer exists; its stream has been closed and nothing further will arrive",
				p.ChannelID, p.ThreadTS, p.SessionID)
			return
		}
		// Anything else is transient — the control plane restarting, a network blip. The record is LEFT
		// PENDING deliberately, so the next start tries again rather than this being the moment the answer
		// was quietly given up on.
		deps.logf("relay: could not re-attach to session %s for %s/%s; the answer is still owed and the next start will try again: %v",
			p.SessionID, p.ChannelID, p.ThreadTS, err)
		return
	}

	// The recipient is what the PREVIOUS process recorded, not anything invented here: slack.StartStream
	// refuses an empty recipient before it ever calls Slack, so a row written before this column existed —
	// or by a path that never learned the user id — cannot open a message. It can still finish one, which is
	// why this is only checked on the branch that would have to open one.
	if p.StreamTS == "" && p.RecipientUserID == "" {
		_ = stream.Close()
		deps.logf("relay: %s/%s was left mid-answer on session %s with neither an open message nor a recorded recipient, so there is nothing to render into; clearing it",
			p.ChannelID, p.ThreadTS, p.SessionID)
		if endErr := deps.Store.EndDelivery(relayCtx, tk.botID, tk.teamID, tk.channelID, tk.threadTS); endErr != nil {
			deps.logf("relay: could not clear that record: %v", endErr)
		}
		return
	}

	runDeps := Deps{
		Events:     &sequenceTrackingStream{EventStream: stream, record: func(seq int64) { deps.state.setLastSequence(tk, seq) }},
		Slack:      deps.NewSlack(p.RecipientUserID, p.TeamID),
		OnApproval: deps.OnApproval,
		Delivery:   deps.delivery(tk),
		// EMPTY MEANS OPEN A NEW MESSAGE, which is the run that was accepted but never got its
		// chat.startStream away. Non-empty means finish the one already on screen.
		ResumeTS: p.StreamTS,
	}
	deps.state.setLastSequence(tk, p.LastSequence)
	deps.state.setActive(tk, true)
	deps.logf("relay: finishing the answer for %s/%s (session %s) from sequence %d%s",
		p.ChannelID, p.ThreadTS, p.SessionID, p.LastSequence, resumeTarget(p.StreamTS))
	deps.RunInBackground(func() {
		defer deps.state.setActive(tk, false)
		if runErr := Run(relayCtx, runDeps, p.SessionID, p.ChannelID, p.ThreadTS); runErr != nil {
			deps.OnRunFailed(runErr, p.SessionID, p.ChannelID, p.ThreadTS)
		}
	})
}

// closeAbandonedStream shuts a message whose run can never be finished, so the thread stops showing a live
// card for something that will never arrive. It says why in the message itself: a stream that simply closes
// with nothing added reads as an answer that was given, and this is the opposite of one.
//
// Its failure is logged and dropped. There is nothing further to try, and the caller is already in the
// middle of reporting a worse problem than an un-closed card.
func (deps InboundDeps) closeAbandonedStream(ctx context.Context, p store.PendingRun, why string) {
	if p.StreamTS == "" {
		return
	}
	err := deps.NewSlack(p.RecipientUserID, p.TeamID).StopStream(ctx, p.ChannelID, p.StreamTS,
		"\n\n⚠️ This answer was interrupted and cannot be finished — "+why+".")
	if err != nil {
		deps.logf("relay: could not close the abandoned stream on %s/%s: %v", p.ChannelID, p.StreamTS, err)
	}
}

// resumeTarget renders which of the two recoveries a log line is about, so an operator reading the boot
// output can tell "finishing the message already on screen" from "opening one, because the previous process
// never got that far" — two different stories about the same thread.
func resumeTarget(streamTS string) string {
	if streamTS == "" {
		return " into a new message (the previous process never opened one)"
	}
	return " into the message it already opened (" + streamTS + ")"
}
