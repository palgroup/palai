-- Session chaining queries (spec §9, §22.1). Every read is tenant-scoped: a session or
-- response id from another tenant matches no row, so an unknown/foreign id renders the
-- same 404 as retrieval and never leaks a cross-tenant resource's existence (§39.2).

-- SessionForCreate resolves a caller-supplied session_id within the tenant scope, returning
-- its lifecycle state so admission can append to an active session or reject a closed one.
-- name: SessionForCreate
SELECT id, state
FROM sessions
WHERE id = $1 AND project_id = $2;

-- SessionForPreviousResponse resolves the session a previous_response_id continues, joined so
-- one round-trip yields both the session id and its lifecycle state. An unknown/foreign
-- response matches no row (404).
-- name: SessionForPreviousResponse
SELECT s.id, s.state
FROM responses r
JOIN sessions s ON s.id = r.session_id
WHERE r.id = $1 AND r.project_id = $2;

-- ---------------------------------------------------------------------------------------------------
-- THE SESSION ROW (E29, migration 000048). Read this before editing either statement below.
-- ---------------------------------------------------------------------------------------------------
--
-- A session list row must render the Sessions screen — label, agent, tokens, duration — WITHOUT a
-- follow-up request per row, because a 50-row page that needs one is 50 requests. So the three
-- aggregates are LATERAL subqueries over the page, not a second round trip, and the page CTE runs
-- first: each lateral is driven at most `limit` times (≤ 101), never once per session in the project.
--
-- ONE of the four is a stored column and three are aggregates, and the split is not arbitrary:
--
--   name       STORED. Nothing anywhere held a human label for a session, so there was nothing to
--              aggregate. Falls back to the first non-retracted response's text when unset.
--   agents     AGGREGATE, and PLURAL on purpose. There is no session→agent association in this schema
--              and never has been: 000019 put agent_revision_id on `runs`. A session's runs may pin
--              different agents, or none. The honest column is therefore "the agents its runs used".
--   tokens     AGGREGATE. usage_ledger (000032) already has exactly one writer, which settles each
--              model step once by dedupe key. A counter column here would be a SECOND writer for a
--              number that already has one.
--   span       AGGREGATE. runs.created_at (InsertRun) and runs.updated_at (UpdateRunState, which
--              rewrites it on every transition) already record both ends.
--
-- THE DERIVED LABEL, and why it is filtered the way it is. `retracted_at IS NOT NULL` is EXCLUDED:
-- SessionHistory below refuses to show a retracted turn to the model because the human took the words
-- back, and a list screen must not be the loophole that puts them back on a page. A PURGED response
-- needs no filter — the reaper rewrites `input` to '{}' (responses.sql PurgeExpiredStoreFalse), which
-- is a JSON object, and the CASE below yields NULL for it.
--
-- `input` is either a bare JSON string (the prompt) or an array of content items; both shapes are live
-- in this tree. The array walk is ORDERED BY ORDINALITY before its LIMIT 1, because "the first
-- input_text" is a claim about position and jsonb_array_elements guarantees no order without it.
-- left(..., 400) bounds what crosses the wire: a page must not carry 50 whole prompts. The caller
-- normalises whitespace and truncates to the display length — one function, in Go, where runes are
-- countable.

-- SessionStateInScope is the CONTROL-PATH session read: does this session exist in my scope, and what
-- is its lifecycle state. It exists as its own statement because command acceptance used to borrow
-- GetSessionInScope for exactly this, and GetSessionInScope has since become a RENDERING read with
-- three lateral aggregates hanging off it. Running the label, the agent roll-up, the token sum and the
-- activity span inside a command transaction — to learn one word — is work nobody asked for on a path
-- that takes a row lock.
-- name: SessionStateInScope
SELECT id, state
FROM sessions
WHERE id = $1 AND project_id = $2;

-- GetSessionInScope reads a session's projection for GET /v1/sessions/{id}. An unknown or
-- foreign id matches no row (404, no existence disclosure). It renders the SAME enriched shape
-- ListSessions does, so a detail read and a list row cannot disagree. It is a RENDERING read — see
-- SessionStateInScope above before reaching for it to answer "does this session exist".
-- name: GetSessionInScope
SELECT s.id, s.state, s.created_at, s.name,
       s.auto_approve_tools, s.auto_approve_publications, s.auto_approve_set_by, s.auto_approve_set_at,
       label.derived_name,
       agg.agent_names,
       COALESCE(tok.input_tokens, 0)  AS input_tokens,
       COALESCE(tok.output_tokens, 0) AS output_tokens,
       agg.first_activity_at,
       agg.last_activity_at
FROM sessions s
LEFT JOIN LATERAL (
    SELECT left(CASE jsonb_typeof(r.input)
                    WHEN 'string' THEN r.input #>> '{}'
                    WHEN 'array'  THEN (
                        SELECT el.value ->> 'text'
                        FROM jsonb_array_elements(r.input) WITH ORDINALITY AS el(value, ord)
                        WHERE el.value ->> 'text' IS NOT NULL
                        ORDER BY el.ord
                        LIMIT 1
                    )
                END, 400) AS derived_name
    FROM responses r
    WHERE r.session_id = s.id AND r.project_id = $3
      AND r.retracted_at IS NULL
    ORDER BY r.created_at, r.id
    LIMIT 1
) label ON TRUE
LEFT JOIN LATERAL (
    SELECT COALESCE(array_agg(DISTINCT ap.name ORDER BY ap.name) FILTER (WHERE ap.name IS NOT NULL),
                    ARRAY[]::text[]) AS agent_names,
           min(rn.created_at) AS first_activity_at,
           max(rn.updated_at) AS last_activity_at
    FROM runs rn
    LEFT JOIN agent_revisions ar ON ar.id = rn.agent_revision_id
    LEFT JOIN agent_profiles ap ON ap.id = ar.profile_id
    WHERE rn.session_id = s.id AND rn.project_id = $3
) agg ON TRUE
LEFT JOIN LATERAL (
    SELECT COALESCE(sum(l.quantity) FILTER (WHERE l.meter = 'model.input_tokens'), 0)  AS input_tokens,
           COALESCE(sum(l.quantity) FILTER (WHERE l.meter = 'model.output_tokens'), 0) AS output_tokens
    FROM usage_ledger l
    WHERE l.session_id = s.id AND l.organization_id = $2 AND l.project_id = $3
) tok ON TRUE
WHERE s.id = $1 AND s.project_id = $3;

-- ListSessions pages a project's sessions newest-first (spec §9.1, E13 T4). Tenant-scoped by RLS;
-- the org/project predicate is defence-in-depth. Same keyset/filter shape as ListResponses: $3
-- status, $4/$5 created_at bounds, ($6,$7) the keyset boundary, $8 the row cap.
--
-- TWO orderings, and they are load-bearing in different ways:
--
--   * The CTE's picks WHICH rows the page is — (created_at DESC, id DESC), which is TOTAL, so a clock
--     tie cannot skip or repeat a row across the boundary the cursor is minted from. This one is
--     observable and is held by TestSessionListPagesTotallyOrderedAcrossAClockTie: remove the `id`
--     tiebreaker and rows go missing between pages.
--
--   * The statement's own fixes the order they COME BACK in, and it is a GUARD rather than a
--     currently-observed behaviour — say so plainly, because a comment that overstates a defence is
--     worse than none. Deleting it changes nothing today: every lateral here is driven by a nested
--     loop over the CTE, which preserves that order, and the tie test passes without it (measured
--     2026-07-31 — three runs of TestSessionListPagesTotallyOrderedAcrossAClockTie against a real
--     Postgres, all `ok`, with this line removed). What it insures against is a PLAN change, and the
--     cost of one would not be a wrong row on screen: api/pagination.go renderPage mints next_cursor
--     from the LAST row it was handed, so a reordered result set silently pages from the wrong
--     position. No test in this tree can distinguish the two, which is exactly why the line stays.
--
-- The usage_ledger lateral filters project_id EXPLICITLY, and that is not defence-in-depth here — it
-- is the only narrowing there is. 000032 secures usage_ledger at the ORGANIZATION level with
-- has_project=false on purpose (so an org-wide budget can be summed from a project-narrowed
-- connection), and usage_ledger carries no FK to sessions, so a sibling project holding rows under the
-- same session id would be summed straight into this row.
-- name: ListSessions
WITH page AS (
    SELECT id, state, created_at, name,
           auto_approve_tools, auto_approve_publications, auto_approve_set_by, auto_approve_set_at
    FROM sessions
    WHERE project_id = $2
      AND ($3 = '' OR state = $3)
      AND ($4::timestamptz IS NULL OR created_at >= $4)
      AND ($5::timestamptz IS NULL OR created_at <= $5)
      AND ($6::timestamptz IS NULL OR (created_at, id) < ($6, $7))
    ORDER BY created_at DESC, id DESC
    LIMIT $8
)
SELECT p.id, p.state, p.created_at, p.name,
       p.auto_approve_tools, p.auto_approve_publications, p.auto_approve_set_by, p.auto_approve_set_at,
       label.derived_name,
       agg.agent_names,
       COALESCE(tok.input_tokens, 0)  AS input_tokens,
       COALESCE(tok.output_tokens, 0) AS output_tokens,
       agg.first_activity_at,
       agg.last_activity_at
FROM page p
LEFT JOIN LATERAL (
    SELECT left(CASE jsonb_typeof(r.input)
                    WHEN 'string' THEN r.input #>> '{}'
                    WHEN 'array'  THEN (
                        SELECT el.value ->> 'text'
                        FROM jsonb_array_elements(r.input) WITH ORDINALITY AS el(value, ord)
                        WHERE el.value ->> 'text' IS NOT NULL
                        ORDER BY el.ord
                        LIMIT 1
                    )
                END, 400) AS derived_name
    FROM responses r
    WHERE r.session_id = p.id AND r.project_id = $2
      AND r.retracted_at IS NULL
    ORDER BY r.created_at, r.id
    LIMIT 1
) label ON TRUE
LEFT JOIN LATERAL (
    SELECT COALESCE(array_agg(DISTINCT ap.name ORDER BY ap.name) FILTER (WHERE ap.name IS NOT NULL),
                    ARRAY[]::text[]) AS agent_names,
           min(rn.created_at) AS first_activity_at,
           max(rn.updated_at) AS last_activity_at
    FROM runs rn
    LEFT JOIN agent_revisions ar ON ar.id = rn.agent_revision_id
    LEFT JOIN agent_profiles ap ON ap.id = ar.profile_id
    WHERE rn.session_id = p.id AND rn.project_id = $2
) agg ON TRUE
LEFT JOIN LATERAL (
    SELECT COALESCE(sum(l.quantity) FILTER (WHERE l.meter = 'model.input_tokens'), 0)  AS input_tokens,
           COALESCE(sum(l.quantity) FILTER (WHERE l.meter = 'model.output_tokens'), 0) AS output_tokens
    FROM usage_ledger l
    WHERE l.session_id = p.id AND l.organization_id = $1 AND l.project_id = $2
) tok ON TRUE
ORDER BY p.created_at DESC, p.id DESC;

-- RenameSession sets a session's operator label (E29). The label is NOT unique — several sessions
-- legitimately share one — so this is a plain conditional UPDATE, and the tenant predicate makes a
-- foreign id a no-op that RETURNS no row, which the caller renders as the same 404 GetSession does.
-- name: RenameSession
UPDATE sessions
SET name = $3, updated_at = clock_timestamp()
WHERE id = $1 AND project_id = $2
RETURNING id;

-- LockSession reads and locks a session so a lifecycle transition (close) sees a stable state
-- and concurrent closes serialize (mirrors LockRun for runs).
-- name: LockSession
SELECT state
FROM sessions
WHERE id = $1 AND project_id = $2
FOR UPDATE;

-- UpdateSessionState advances a session's lifecycle state (spec §22.1).
-- name: UpdateSessionState
UPDATE sessions
SET state = $3, updated_at = clock_timestamp()
WHERE id = $1 AND project_id = $2;

-- SessionHistory returns the prior responses of a session in creation order so run.start can
-- carry them as conversation history (spec §22.2). A retained response yields the question it
-- was asked AND its stored output projection; a purged one yields NULL output with
-- purged = true, which the assembler renders as a redacted_content marker.
-- `input` is selected because a history of answers alone is not a conversation — the model
-- would see its own replies with nothing they replied to. Ordered by created_at (id tiebreak) — ponytail:
-- created_at ordering, responses carry no per-session ordinal of their own; add one if a
-- sub-microsecond clock tie ever reorders a chain.
-- A RETRACTED turn is not carried at all — not even as a redacted marker. The human deleted the message, and
-- since this query is what the model is shown (execution.historyMessages), a turn left here goes on
-- influencing every later answer in the thread. Purged and retracted are different facts with different
-- answers: purge means the CONTENT was reaped and something happened here (a marker is honest), retraction
-- means the person took the words back (nothing here is honest). The row itself survives both.
-- name: SessionHistory
SELECT input, output, purged_at IS NOT NULL AS purged
FROM responses
WHERE session_id = $1 AND project_id = $2
  AND retracted_at IS NULL
  AND created_at < (SELECT created_at FROM responses WHERE id = $3)
ORDER BY created_at, id;

-- ---------------------------------------------------------------------------------------------------
-- THE SESSION'S STANDING AUTHORIZATION (E30 T1, migration 000056, spec §22.4).
-- ---------------------------------------------------------------------------------------------------
--
-- SetSessionAutoApprove arms or disarms one session's two approval families and STAMPS THE PRINCIPAL.
-- The principal is not a log line: it is the identity the auto-decision is later made AS, so an armed
-- session can never authorize anything the person who armed it could not have authorized by clicking
-- (the decision still passes ApproverAllowed).
--
-- set_at is cleared when BOTH halves go off, so "armed at" never outlives the arming. The set_by is
-- kept in that case rather than blanked — who disarmed it is the same question as who armed it, and a
-- row that forgets is a row that cannot be audited.
-- name: SetSessionAutoApprove
UPDATE sessions
SET auto_approve_tools = $3,
    auto_approve_publications = $4,
    auto_approve_set_by = $5,
    auto_approve_set_at = CASE WHEN $3 OR $4 THEN clock_timestamp() ELSE NULL END,
    updated_at = clock_timestamp()
WHERE id = $1 AND project_id = $2
RETURNING auto_approve_tools, auto_approve_publications, auto_approve_set_by, auto_approve_set_at;

-- SessionAutoApprove reads the standing authorization the two gates consult. A session that never
-- armed anything reads (false, false, '') — the shape every session in every deployment alive before
-- 000056 has, and the shape that leaves both gates bit-unchanged.
-- name: SessionAutoApprove
SELECT auto_approve_tools, auto_approve_publications, auto_approve_set_by
FROM sessions
WHERE id = $1 AND project_id = $2;
