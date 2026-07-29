-- Slack connection management + inbound resolution + thread↔session correlation (spec §36, E17 Task 1,
-- SLK-001..008). Create/read/list are the admin management surface (tenant-scoped by organization_id +
-- project_id). ResolveSlackConnectionByTeam is the UNAUTHENTICATED inbound path's tenant establisher (the
-- resolveInboundTrigger idiom): it is keyed by the Slack team id the callback carries and runs system-scoped
-- because there is no tenant yet — the caller still has to present a valid v0 signature over the resolved
-- connection's signing secret before anything is written. The thread queries collapse a (team, channel,
-- thread) to one canonical session.

-- name: InsertSlackConnection
INSERT INTO slack_connections (
    id, organization_id, project_id, team_id, enterprise_id, bot_user_id,
    signing_secret_ref, bot_token_ref, app_token_ref, scopes, allowed_channels, allowed_users, default_policy)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13);

-- GetSlackConnection reads a connection's metadata within scope. The secret refs are HANDLES, not values,
-- so they are safe to return to an admin read; the resolved bytes never live in this table.
-- name: GetSlackConnection
SELECT id, team_id, enterprise_id, bot_user_id, signing_secret_ref, bot_token_ref, app_token_ref, scopes, disabled
FROM slack_connections
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- GetSlackAuthorizationPolicy is the AUTHORIZATION read (SLK-004, E17 T11): the allow-lists a decision is
-- checked against before any approve/deny command is enqueued. It is deliberately SEPARATE from
-- GetSlackConnection — an authorization check needs only these two lists, never the secret-ref handles, so the
-- enforcement path never handles credential metadata it has no use for. Tenant-scoped like every other read.
-- name: GetSlackAuthorizationPolicy
SELECT allowed_channels, allowed_users
FROM slack_connections
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- ListSlackConnections pages a project's connections newest-first (the admin ListView envelope). Tenant
-- scoped by RLS; the org/project predicate is defence-in-depth. The secret refs are omitted from a list.
-- name: ListSlackConnections
SELECT id, team_id, enterprise_id, bot_user_id, disabled, created_at
FROM slack_connections
WHERE organization_id = $1 AND project_id = $2
  AND ($3::timestamptz IS NULL OR created_at >= $3)
  AND ($4::timestamptz IS NULL OR created_at <= $4)
  AND ($5::timestamptz IS NULL OR (created_at, id) < ($5, $6))
ORDER BY created_at DESC, id DESC
LIMIT $7;

-- SlackWorkspaceBoundElsewhere reports whether a (team_id, enterprise_id) is already bound in a DIFFERENT
-- org/project. It exists because this table's uniqueness is (organization_id, project_id, team_id,
-- enterprise_id) — PER TENANT, not global — while ResolveSlackConnectionByTeam below is keyed by team_id
-- ALONE and runs system-scoped. Without this check any project admin in any org could register another
-- tenant's team_id with a secret it controls, and the resolve would then pick one of the two rows: the
-- victim's own signed events start failing verification and (carrying x-slack-no-retry) are dropped for good.
-- Slack posts to ONE Request URL per app, so two tenants legitimately sharing a workspace does not exist.
-- System-scoped by necessity: the whole point is to see rows OUTSIDE the caller's tenant.
-- Only the id is selected: the caller must NOT learn which tenant holds the workspace, and a column it
-- never reads is a column that cannot be logged by accident.
-- name: SlackWorkspaceBoundElsewhere
SELECT id
FROM slack_connections
WHERE team_id = $1 AND enterprise_id = $2 AND (organization_id <> $3 OR project_id <> $4)
ORDER BY id
LIMIT 1;

-- ResolveSlackConnectionByTeam establishes the tenant for a signed inbound callback, keyed by the Slack
-- team + enterprise id. System-scoped (there is no tenant to scope by yet); the signature over the returned
-- signing_secret_ref is the auth. A disabled connection still resolves so the caller can reject explicitly.
-- Note for the future webhook receiver: this resolve is UNAUTHENTICATED (one DB query per request), so the
-- caller must bound the request body size BEFORE the HMAC verify and treat the pre-parse team_id as a lookup
-- key ONLY — never as trust — or it is an edge rate-limit / amplification hole.
-- default_policy rides along because it carries the RUN TARGET for events on this connection (E19 T1): the
-- agent_revision_id the admission pins and the principal the run belongs to. Reading it here keeps the whole
-- unauthenticated resolve at ONE query, and it is the column's stated purpose ("default run policy for events
-- on this connection") rather than a new column — E19 takes no migration.
-- ORDER BY id LIMIT 2 is load-bearing, not tidiness: the predicate is NOT unique (see the per-tenant index
-- above), and a QueryRow over an unordered multi-row result silently takes whichever row came first and can
-- flip between requests. The caller reads BOTH rows and refuses the ambiguity outright — deciding which of
-- two tenants an event belongs to is not a decision this query is allowed to make by accident.
-- name: ResolveSlackConnectionByTeam
SELECT id, organization_id, project_id, signing_secret_ref, bot_token_ref, app_token_ref, bot_user_id, disabled,
       default_policy
FROM slack_connections
WHERE team_id = $1 AND enterprise_id = $2
ORDER BY id
LIMIT 2;

-- SlackRunPrincipalInScope confirms the principal named by default_policy belongs to the connection's OWN
-- org/project. Without it a project admin could name a FOREIGN principal and have Slack-born runs booked
-- against another tenant's identity: idempotency_records.principal_id is a bare FK to principals(id), so the
-- database would accept it. System-scoped like the resolve above (the caller has no tenant yet) and therefore
-- explicit about org AND project in the predicate.
-- name: SlackRunPrincipalInScope
SELECT 1 FROM principals WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- CorrelateThreadSession claims the (team, channel, thread) -> session mapping single-winner. A first event
-- inserts its session; a later event in the SAME thread hits the unique index (23505) and inserts nothing,
-- so the caller reads the canonical session with GetThreadSession — one session per thread (SLK-003), race
-- included. RETURNING id lets the caller tell a fresh claim from a reuse.
-- name: CorrelateThreadSession
INSERT INTO slack_thread_sessions (
    id, organization_id, project_id, connection_id, team_id, channel_id, thread_ts, session_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (organization_id, project_id, team_id, channel_id, thread_ts) DO NOTHING
RETURNING id;

-- GetThreadSession reads the canonical session a (team, channel, thread) already resolved to — the reuse
-- path (a second thread event, or a web-console attach joining the SAME session).
-- name: GetThreadSession
SELECT session_id, last_bot_message_ts
FROM slack_thread_sessions
WHERE organization_id = $1 AND project_id = $2 AND team_id = $3 AND channel_id = $4 AND thread_ts = $5;

-- DeleteThreadSession drops a correlation whose session is no longer usable (closed/reaped), so the thread
-- can open a fresh one on the next event. session_id is in the predicate on purpose: by the time the repair
-- runs, another event may already have re-correlated the thread, and deleting THAT row would undo a healthy
-- correlation. Without this the failure is permanent — a chained admission onto a dead session is refused
-- every time, and nothing else in the tree ever clears the row.
-- name: DeleteThreadSession
DELETE FROM slack_thread_sessions
WHERE organization_id = $1 AND project_id = $2 AND team_id = $3 AND channel_id = $4 AND thread_ts = $5
  AND session_id = $6;

-- UpdateThreadMessageTS records the visible bot message ts the rate-limited live-output repair edits
-- (message-ts reconciliation, SLK-006). Idempotent — it just overwrites the handle.
-- name: UpdateThreadMessageTS
UPDATE slack_thread_sessions
SET last_bot_message_ts = $6
WHERE organization_id = $1 AND project_id = $2 AND team_id = $3 AND channel_id = $4 AND thread_ts = $5;

-- ============================================================================
-- The RETURN LEG (000041): a terminal run's answer, posted back into the thread the mention came from.
-- ============================================================================

-- EnqueueTerminalSlackReply runs INSIDE the run's terminal transaction (coordinator.applyRunTransitionTx),
-- exactly like EnqueueTerminalQueueDeliveries. That placement is the loss-lessness claim and it is the only
-- reason this is a statement rather than a call from the poster: the run's terminality and the recorded
-- intent to answer commit TOGETHER, so no crash window exists in which a run finished and the human is owed
-- a reply nobody recorded.
--
-- EXACTLY ONCE is UNIQUE (run_id) + ON CONFLICT DO NOTHING. A retried terminal transaction, a cancel that
-- reconciles onto an already-terminal run, or a reclaimer re-driving the transition all present the same
-- run id and insert nothing the second time. The poster delivers what this committed; it never decides
-- whether a reply is owed.
--
-- The destination is COPIED here, not looked up later: repairDeadCorrelation legitimately deletes a thread
-- correlation whose session went bad, and an answer that lost its address because the next message repaired
-- the thread would be an answer nobody ever sees.
--
-- A DISABLED connection enqueues nothing — an operator turning an integration off means it stops writing to
-- their workspace, including for runs already in flight. A project with no Slack thread for the session (i.e.
-- every non-Slack run in the deployment) inserts zero rows, which is the common case and why this is one
-- indexed lookup on slack_thread_sessions (session_id) rather than a scan.
--
-- THE REQUESTER IS FROZEN HERE TOO (000043), for the reason the destination is: the source can legitimately
-- disappear. slack_message_turns hangs off the response with ON DELETE CASCADE, so a retention purge between
-- this transaction and the eighth delivery attempt would take the id with it, and a mention lost to an
-- unrelated schedule is a notification nobody can account for. The subquery is scoped to the SAME tenant as
-- the enqueue and ordered, so it is a deterministic read rather than whichever row the planner returned; no
-- match — a response that was never a Slack turn, or the empty response id a run with no projection carries —
-- yields '', which the renderer treats as "send the words, mention nobody".
-- name: EnqueueTerminalSlackReply
INSERT INTO slack_reply_deliveries
    (id, organization_id, project_id, connection_id, run_id, response_id, channel_id, thread_ts, run_state,
     requester_user_id)
SELECT 'sdel_' || replace(gen_random_uuid()::text, '-', ''),
       t.organization_id, t.project_id, t.connection_id, $3, $4, t.channel_id, t.thread_ts, $6,
       COALESCE((SELECT m.requester_user_id
                   FROM slack_message_turns m
                  WHERE m.organization_id = $1 AND m.project_id = $2 AND m.response_id = $4
                  ORDER BY m.created_at, m.id
                  LIMIT 1), '')
  FROM slack_thread_sessions t
  JOIN slack_connections c ON c.id = t.connection_id AND NOT c.disabled
 WHERE t.organization_id = $1 AND t.project_id = $2 AND t.session_id = $5
ON CONFLICT (run_id) DO NOTHING;

-- ClaimDueSlackReplies takes the next due replies AND schedules their retry in ONE statement. That is
-- deliberately stronger than the queue outbox's DueQueueDeliveries, which documents itself as safe for a
-- single pump only (its FOR UPDATE SKIP LOCKED locks die at statement end, so two pumps read the same row):
-- here the claim IS the update, so two posters can never take the same delivery, and a poster that dies
-- mid-post retries after the backoff instead of the next tick.
--
-- The attempt is counted at CLAIM time, not after the post. A post that crashes the process between the
-- Slack call and the mark must not be free — an uncounted attempt is an infinite retry against a workspace,
-- and a message Slack may already have accepted.
--
-- $2 is the backoff seconds. System-scoped by necessity (one poster serves every project); the connection
-- join both supplies the bot token handle and re-checks `disabled`, so an operator who turns an integration
-- off stops its pending replies without deleting anything.
-- name: ClaimDueSlackReplies
UPDATE slack_reply_deliveries d
   SET attempt_count = d.attempt_count + 1,
       -- Linear backoff: d.attempt_count on the right is the OLD value, so the waits grow
       -- $2, 2*$2, 3*$2 … A flat retry against a Slack that is down is a poll loop with a token.
       next_attempt_at = clock_timestamp() + ($2::int * (d.attempt_count + 1) * interval '1 second'),
       updated_at = clock_timestamp()
  FROM slack_connections c
 WHERE d.id IN (
           SELECT p.id
             FROM slack_reply_deliveries p
            WHERE p.state = 'pending' AND p.next_attempt_at <= clock_timestamp()
            ORDER BY p.next_attempt_at
            FOR UPDATE SKIP LOCKED
            LIMIT $1)
   AND c.id = d.connection_id AND NOT c.disabled
RETURNING d.id, d.organization_id, d.project_id, d.connection_id, d.run_id, d.response_id,
          d.channel_id, d.thread_ts, d.run_state, d.attempt_count, d.max_attempts, c.bot_token_ref,
          d.requester_user_id;

-- MarkSlackReplyDelivered closes a delivery and records the ts Slack assigned the visible message — the
-- handle any later repair of THIS message edits (the SLK-006 idiom).
-- name: MarkSlackReplyDelivered
UPDATE slack_reply_deliveries
   SET state = 'delivered', message_ts = $2, updated_at = clock_timestamp()
 WHERE id = $1;

-- MarkSlackReplyDead retires a delivery that has spent its attempts, or that failed for a reason no retry
-- can fix. The row STAYS as the audit trail of an answer Slack never accepted; the canonical result is
-- untouched and still readable through /v1/responses (SLK-006 — a Slack failure never erases a run).
-- name: MarkSlackReplyDead
UPDATE slack_reply_deliveries
   SET state = 'dead', updated_at = clock_timestamp()
 WHERE id = $1;

-- ============================================================================
-- Connection REPAIR (E19): update + delete, the surface a mis-registered binding needs.
-- ============================================================================

-- UpdateSlackConnection revises a binding's operational fields. Every parameter is NULLABLE and COALESCEs
-- to the stored value, so a PATCH carries only what it changes.
--
-- team_id AND enterprise_id ARE NOT HERE, and their absence is a security property rather than an omission.
-- The workspace a connection is bound to is the one thing the cross-tenant squat refusal
-- (SlackWorkspaceBoundElsewhere) guards at registration; a revise that could move it would let an admin
-- register a workspace they own and then re-point the row at someone else's, arriving at exactly the state
-- that check exists to prevent — and arriving there through a statement that never runs the check. Immutable
-- here means the attack has no statement to ride. Re-binding a workspace is DELETE + register, which does
-- run it.
-- name: UpdateSlackConnection
UPDATE slack_connections
   SET bot_user_id = COALESCE($4::text, bot_user_id),
       signing_secret_ref = COALESCE($5::text, signing_secret_ref),
       bot_token_ref = COALESCE($6::text, bot_token_ref),
       app_token_ref = COALESCE($7::text, app_token_ref),
       scopes = COALESCE($8::text, scopes),
       allowed_channels = COALESCE($9::jsonb, allowed_channels),
       allowed_users = COALESCE($10::jsonb, allowed_users),
       default_policy = COALESCE($11::jsonb, default_policy),
       disabled = COALESCE($12::boolean, disabled)
 WHERE id = $1 AND organization_id = $2 AND project_id = $3
RETURNING id;

-- DeleteSlackConnectionThreads drops the thread↔session correlations a binding owns. It runs FIRST in the
-- delete transaction because slack_thread_sessions.connection_id is a plain FK (000035) — without this the
-- delete fails on any workspace that has ever been used, i.e. exactly the ones an operator needs to remove.
-- The sessions and runs those threads pointed at are NOT touched: they are canonical results that happen to
-- have arrived over Slack, and unbinding a workspace does not un-happen them.
-- name: DeleteSlackConnectionThreads
DELETE FROM slack_thread_sessions
WHERE connection_id = $1 AND organization_id = $2 AND project_id = $3;

-- DeleteSlackConnection removes the binding within the caller's tenant. RETURNING id separates "deleted" from
-- "no such connection HERE" — a foreign id must answer 404 rather than 204, or the response tells an outsider
-- that someone else's connection exists. Undelivered replies cascade with it (000041): a workspace we are no
-- longer bound to is one we must not post into.
-- name: DeleteSlackConnection
DELETE FROM slack_connections
WHERE id = $1 AND organization_id = $2 AND project_id = $3
RETURNING id;

-- ============================================================================
-- The TURN HANDLE (000042): which turn a Slack message became, so an edit can supersede it and a deletion
-- can retract it. SLK-005 classified both kinds from the start; these are what let the classification DO
-- something other than shape a prompt.
-- ============================================================================

-- RecordSlackMessageTurn writes the handle for one admitted message. ON CONFLICT DO NOTHING is load bearing
-- twice over: a redelivery replays onto the SAME response (SLK-002) and must not re-point the handle, and
-- Slack delivers a top-level mention TWICE (app_mention plus its message.channels twin) under ONE message
-- ts — the first one is the turn, the second is not a second turn.
--
-- requester_user_id (000043) rides here because this is the ONE write that happens with the event in hand and
-- names the turn the run belongs to. It is SCOPE, not conversation — it never enters the prompt (slack_admit)
-- — and it exists so the reply pump, which posts minutes later and across restarts, can address the person
-- who asked. The ON CONFLICT above means a redelivery does not re-point it either: the FIRST writer of a turn
-- is its requester, exactly as the first response is its turn.
-- name: RecordSlackMessageTurn
INSERT INTO slack_message_turns (
    id, organization_id, project_id, connection_id, team_id, channel_id, message_ts, response_id, session_id,
    requester_user_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (organization_id, project_id, team_id, channel_id, message_ts) DO NOTHING;

-- RetractSlackMessageTurn withdraws the turn a deleted message opened: the response stays (what was said and
-- that it was withdrawn are both facts an operator may need), but SessionHistory stops carrying it, so the
-- model is no longer shown words the human took back.
--
-- The tenant appears on BOTH sides of the join. The turn row is already tenant-scoped by RLS, but a query
-- that writes to responses through a join must say whose responses it may write, or it is trusting a join
-- key to be a scope check.
-- retracted_at IS NULL keeps it idempotent: a redelivered deletion is a no-op rather than a second timestamp.
-- name: RetractSlackMessageTurn
UPDATE responses r
SET retracted_at = clock_timestamp(), updated_at = clock_timestamp()
FROM slack_message_turns t
WHERE t.organization_id = $1 AND t.project_id = $2
  AND t.team_id = $3 AND t.channel_id = $4 AND t.message_ts = $5
  AND r.id = t.response_id AND r.organization_id = $1 AND r.project_id = $2
  AND r.retracted_at IS NULL
RETURNING r.id;

-- SupersedeSlackMessageTurn rewrites the stored turn to the corrected text. The turn is REPLACED rather than
-- appended to, because that is what an edit is: the human did not say a second thing, they changed what they
-- said, and SLK-005 has called it a correction since E17 T1.
--
-- to_jsonb($6::text) is what keeps the column a JSON STRING. responses.input IS the prompt (run.start hands
-- it to the engine verbatim and model_dispatch serialises anything that is not a string), so writing an
-- object here would put raw JSON in front of the model — the defect ed44544 fixed on the way in.
--
-- A TURN THAT CARRIES AN IMAGE IS STORED AS A CONTENT ARRAY (E20), and there the words are one ITEM of it, so
-- the whole value cannot be overwritten: an edit rewrites the caption, and blanking the array would take the
-- image out of the conversation while it is still sitting in the thread for everyone to see. jsonb_set
-- replaces the text of item 0 and leaves the image_ref items alone. Item 0 is the words by construction —
-- slackRunInput puts them first and is the only writer of these rows — and the CASE keeps a plain text turn
-- on the string path, so this is not the "raw JSON at the model" defect: a content array is a shape
-- model_dispatch decodes (decodeContentParts), not an envelope it stringifies.
--
-- A RETRACTED turn is left alone: the words were withdrawn, and an edit arriving afterwards must not put
-- them back. Slack does not send one, but the guard is one predicate and the alternative is unrecoverable.
-- name: SupersedeSlackMessageTurn
UPDATE responses r
SET input = CASE jsonb_typeof(r.input)
                WHEN 'array' THEN jsonb_set(r.input, '{0,text}', to_jsonb($6::text))
                ELSE to_jsonb($6::text)
            END,
    updated_at = clock_timestamp()
FROM slack_message_turns t
WHERE t.organization_id = $1 AND t.project_id = $2
  AND t.team_id = $3 AND t.channel_id = $4 AND t.message_ts = $5
  AND r.id = t.response_id AND r.organization_id = $1 AND r.project_id = $2
  AND r.retracted_at IS NULL
RETURNING r.id;

-- ============================================================================
-- The APPROVAL MESSAGE (000044 R4, E23 T3): the question a human answers, posted into the thread the run
-- was born in. It follows the return leg above STATEMENT FOR STATEMENT on purpose — an operator should be
-- able to read one delivery model and recognise both.
-- ============================================================================

-- EnqueueApprovalMessage runs INSIDE coordinator.RequestPublication's transaction, exactly where
-- EnqueueTerminalSlackReply runs inside the terminal one. That placement is the whole loss-lessness claim:
-- the approval and the order to ask about it commit together, so no crash window exists in which a run is
-- parked waiting for a question nobody recorded.
--
-- EXACTLY ONCE is UNIQUE (approval_id) + ON CONFLICT DO NOTHING — keyed on the APPROVAL and not the run,
-- because one run can owe a human several separate answers and slack_reply_deliveries' UNIQUE (run_id)
-- could not have expressed that without weakening a shipped promise.
--
-- The destination is COPIED, for 000041's reason: repairDeadCorrelation legitimately deletes a thread
-- correlation whose session went bad, and a question that lost its address is a run that waits forever.
-- A DISABLED connection enqueues nothing, and a session with no Slack thread inserts zero rows — which is
-- every non-Slack run in the deployment, and why this is one indexed lookup rather than a scan.
-- name: EnqueueApprovalMessage
INSERT INTO slack_approval_deliveries
    (id, organization_id, project_id, connection_id, approval_id, run_id, response_id, channel_id, thread_ts)
SELECT 'sapr_' || replace(gen_random_uuid()::text, '-', ''),
       t.organization_id, t.project_id, t.connection_id, $3, $4, $5, t.channel_id, t.thread_ts
  FROM slack_thread_sessions t
  JOIN slack_connections c ON c.id = t.connection_id AND NOT c.disabled
 WHERE t.organization_id = $1 AND t.project_id = $2 AND t.session_id = $6
ON CONFLICT (approval_id) DO NOTHING;

-- ClaimDueApprovalMessages claims the next due questions AND schedules their retry in ONE statement, so two
-- posters can never take the same row and a poster that dies mid-post retries after the backoff. The
-- attempt is counted at CLAIM time for the reply pump's reason: an uncounted attempt is an infinite retry
-- against a workspace, and a message Slack may already have accepted.
--
-- WHAT IT RETURNS IS WHAT THE HUMAN READS, and it is read LIVE from the publication rather than frozen on
-- the delivery row: p.display is the resolved destination (publicationDisplay — the binding's remote, the
-- run's branch, the exact head), a.request_hash is the one-shot binding the button's value carries, and
-- p.state is what lets the poster refuse to post a live button for a question already decided.
--
-- ponytail: the join is to PUBLICATIONS, so a GATED TOOL CALL's approval (000044 R1, publication_id NULL)
-- is never claimed. Nothing enqueues one today — T3 wires the publication producer only, so a second claim
-- now would be a statement with no rows to claim. Whoever wires the second producer gives this a SECOND
-- claim — joining nothing, returning the approval id, the request hash and the ledger row's name and
-- arguments — and posts slack.ToolApprovalMessage. Not one claim serving both: the two screens carry
-- different guarantees (see the note at execution/approval.go), and merging them is the generic display
-- this epic refuses.
-- name: ClaimDueApprovalMessages
UPDATE slack_approval_deliveries d
   SET attempt_count = d.attempt_count + 1,
       next_attempt_at = clock_timestamp() + ($2::int * (d.attempt_count + 1) * interval '1 second'),
       updated_at = clock_timestamp()
  FROM slack_connections c, approvals a, publications p
 WHERE d.id IN (
           SELECT q.id
             FROM slack_approval_deliveries q
            WHERE q.state = 'pending' AND q.next_attempt_at <= clock_timestamp()
            ORDER BY q.next_attempt_at
            FOR UPDATE SKIP LOCKED
            LIMIT $1)
   AND c.id = d.connection_id AND NOT c.disabled
   AND a.id = d.approval_id
   AND p.id = a.publication_id
RETURNING d.id, d.organization_id, d.project_id, d.connection_id, d.run_id,
          d.channel_id, d.thread_ts, d.attempt_count, d.max_attempts, c.bot_token_ref,
          a.request_hash, p.display, p.state;

-- MarkApprovalMessageDelivered closes a delivery and records the ts Slack assigned — the handle the
-- decision's chat.update repairs in place, so the message a human clicked is the message that changes.
-- name: MarkApprovalMessageDelivered
UPDATE slack_approval_deliveries
   SET state = 'delivered', message_ts = $2, updated_at = clock_timestamp()
 WHERE id = $1;

-- MarkApprovalMessageDead retires a question Slack never accepted, or one whose publication was decided
-- before it could be asked. The row STAYS as the audit trail, and SLK-006 holds: the approval is still
-- recorded, it still expires, and the run parked on it is still released by the reaper. A delivery failure
-- costs a human the button, never the canonical state.
-- name: MarkApprovalMessageDead
UPDATE slack_approval_deliveries
   SET state = 'dead', updated_at = clock_timestamp()
 WHERE id = $1;
