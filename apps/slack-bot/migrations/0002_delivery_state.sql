-- 0002 makes DELIVERY durable — the property this bot lost when the relay replaced the control plane's
-- in-process Slack bridge, and the one an operator felt as "Working…" that never finished.
--
-- WHAT WAS LOST, precisely. The old side enqueued a run's terminal reply INSIDE the run's own terminal
-- transaction (packages/coordinator/store.go EnqueueTerminalSlackReply) and a separate pump delivered it,
-- so a pump that crashed delayed an answer rather than losing one. The relay's answer came only from the
-- LIVE stream a relay.Run goroutine was draining (relay.go), and its only record of how far that goroutine
-- had got was a map in memory (relay/inbound.go's inboundState). Kill the process and that map goes with
-- it: nothing anywhere named the thread whose answer was half-written, so nothing could finish it.
--
-- The four columns below are that map, written down. They live ON thread_sessions rather than in a table of
-- their own because they are one-to-one with a thread by construction: a thread has at most one run being
-- rendered into it at a time (relay/inbound.go steers into a live one rather than opening a second), so a
-- separate table would carry the same primary key and buy nothing but a join.
--
-- WHY A CURSOR IS ENOUGH TO FINISH AN ANSWER, and this is the measurement the whole design rests on:
-- GET /v1/sessions/{id}/events?after_sequence=N is a REPLAY of the durable journal followed by a tail, and
-- it closes cleanly at the run's terminal event (apps/control-plane/api/events.go:68-73,132-155). Measured
-- live against the running control plane on 2026-08-04, session ses_56a78aa98ab2931549988426d1650586:
--   after_sequence=0  -> 28 events, ending run.completed.v1, server closed the connection (curl exit 0)
--   after_sequence=10 -> 18 events, FIRST one sequence 11, ending run.completed.v1
-- So a restarted process that remembers N gets exactly the part it never delivered — whether the run is
-- still going or finished hours ago. No new control-plane route is needed for any of it.
ALTER TABLE thread_sessions
    -- last_sequence is the highest session-event sequence whose rendering SLACK HAS CONFIRMED. It is the
    -- durable twin of inboundState.lastSeq, and it is the de-duplication mechanism as well as the resume
    -- point: an event at or below it is never sent again, so a recovered run cannot repeat text a reader
    -- has already seen. It is scoped to session_id and is reset to 0 whenever a thread rebinds to a new
    -- session (store.RebindThread), for the same reason inboundState.resetSequence exists — a sequence
    -- number means nothing outside the journal it came from.
    ADD COLUMN IF NOT EXISTS last_sequence BIGINT NOT NULL DEFAULT 0,

    -- run_pending is the marker that says a run was accepted and has NOT been rendered to its terminal.
    -- It is set the moment POST /v1/responses returns (relay/inbound.go startRun) — BEFORE the Slack
    -- stream is opened, which is the window a `stream_ts`-only design would lose: a process killed between
    -- the accepted run and chat.startStream has a live run on the control plane and, without this column,
    -- nothing at all naming the thread it belongs to.
    ADD COLUMN IF NOT EXISTS run_pending BOOLEAN NOT NULL DEFAULT false,

    -- stream_ts is the chat.startStream message this thread's pending run is being written into, empty
    -- when there is none yet.
    --
    -- IT IS RESUMABLE FROM ANOTHER PROCESS, which is why recovery finishes the ORIGINAL message rather than
    -- posting a second one. Measured live on 2026-08-04 in C0B8BSXETHV: one curl invocation opened a stream,
    -- a SECOND, separate invocation appended to it (ok:true) and a THIRD closed it (ok:true); the finished
    -- message read "half one, by process A. HALF TWO, BY PROCESS B AFTER A DIED. closed by process C."
    -- A stream is addressed by (channel, ts) and the bot token — nothing about it belongs to the connection
    -- that opened it. So the recovered half lands in the same message, in order, with no seam a reader sees,
    -- and the "streaming forever" card SLK-P2 warns about gets closed instead of abandoned.
    ADD COLUMN IF NOT EXISTS stream_ts TEXT NOT NULL DEFAULT '',

    -- recipient_user_id is the human chat.startStream is addressed to. It is stored because a recovery that
    -- has to OPEN the stream (the run_pending-but-no-stream_ts window above) cannot invent one:
    -- slack.StartStream refuses an empty recipient before it ever calls Slack (ErrNoStreamRecipient), so a
    -- recovery without this column could only ever finish runs that had already got past chat.startStream —
    -- exactly the runs least in need of it. team_id is already a column of this table, so only the user id
    -- is new here.
    ADD COLUMN IF NOT EXISTS recipient_user_id TEXT NOT NULL DEFAULT '';

-- pending_deliveries answers "which threads owe somebody an answer" without scanning the table. The
-- predicate is partial so the index holds only the rows recovery cares about — normally none, since a
-- finished run clears the flag.
CREATE INDEX IF NOT EXISTS thread_sessions_pending
    ON thread_sessions (bot_id) WHERE run_pending;

-- thread_sessions_by_session serves the ONE reverse lookup this schema needs: an approval names a
-- session_id and nothing else (GET /v1/approvals), and the bot has to find the thread to ask in. Every
-- other access in this schema is by the primary key.
CREATE INDEX IF NOT EXISTS thread_sessions_by_session
    ON thread_sessions (bot_id, session_id);

-- approval_posts is the at-most-once record of which approvals this bot has already put a question about
-- into Slack. It exists because the approval question now has TWO producers and they overlap by design:
-- the live event (relay.go's approval.requested.v1 arm) asks the instant a run parks, and the sweep
-- (relay/approvals.go SweepApprovals) asks for anything still open that nobody was asked about — which is
-- exactly what a restart leaves behind. Without this table the ordinary case would post twice: the live
-- arm posts, the sweep runs a few seconds later, GET /v1/approvals still lists the row (it is open until
-- somebody clicks), and the same question appears again with a second pair of buttons.
--
-- It is a CLAIM, not a log: a poster inserts before posting and DELETES the row if the post fails, so a
-- failed post is retried by the next sweep rather than being silently marked as asked. The row that
-- survives therefore means "a human can see this question", not "we tried once".
--
-- approval_id is the control plane's own id off the GET /v1/approvals row, never anything derived from
-- Slack input — the same restriction thread_sessions.session_id carries (see internal/store/threads.go).
CREATE TABLE IF NOT EXISTS approval_posts (
    bot_id      TEXT NOT NULL,
    approval_id TEXT NOT NULL,
    channel_id  TEXT NOT NULL,
    thread_ts   TEXT NOT NULL,
    posted_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (bot_id, approval_id)
);
