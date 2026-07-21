-- Session journal queries (spec §21.1). A state transition and its public event
-- are appended in one transaction; the sequence is allocated by the database so it
-- is strictly increasing and gap-free per session.

-- name: AllocateSequence
INSERT INTO session_sequences (session_id, last_seq)
VALUES ($1, 1)
ON CONFLICT (session_id)
DO UPDATE SET last_seq = session_sequences.last_seq + 1
RETURNING last_seq;

-- AppendEvent journals one event. response_id names the owning response for run-scoped
-- events so the retention purge is per-response (spec §22.2); session-scoped events pass
-- NULL and are left untouched by the purge (they carry no customer content).
-- name: AppendEvent
INSERT INTO events (id, organization_id, project_id, session_id, response_id, seq, type, payload)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: EnqueueOutbox
INSERT INTO outbox (organization_id, project_id, topic, dedupe_key, payload)
VALUES ($1, $2, $3, $4, $5);

-- ChildLifecycleEventExists reports whether a child-lifecycle event of a given type was already
-- journaled for a specific child_run_id under a response (E10 T8, DET-001 exactly-once fold): a
-- detached parent that restores re-emits the child.request and re-folds the terminal child, so the
-- child.completed.v1 journal is guarded by this so the parent's stream carries it EXACTLY once even
-- across repeated restores. Tenant-scoped.
-- name: ChildLifecycleEventExists
SELECT EXISTS (
    SELECT 1 FROM events
    WHERE response_id = $1 AND organization_id = $2 AND project_id = $3
      AND type = $4 AND payload->>'child_run_id' = $5
);

-- Journal reads for the resumable event stream (spec §21.1). Every read is
-- tenant-scoped: a session_id from another tenant matches no row, so a caller
-- cannot stream another tenant's journal by guessing an ID.

-- name: SessionExistsInScope
SELECT EXISTS (
    SELECT 1 FROM sessions
    WHERE id = $1 AND organization_id = $2 AND project_id = $3
);

-- name: EventSequenceInScope
SELECT seq
FROM events
WHERE id = $1 AND session_id = $2 AND organization_id = $3 AND project_id = $4;

-- name: CurrentJournalSequence
-- The current transcript boundary: the highest event seq in the session's journal, or 0 for an
-- empty journal (spec §26.1 — where the canonical transcript stands when a checkpoint is cut).
-- COALESCE so an empty session returns 0, not NULL.
SELECT COALESCE(MAX(seq), 0)
FROM events
WHERE session_id = $1 AND organization_id = $2 AND project_id = $3;

-- name: ReadEventsAfter
SELECT id, seq, type, payload, created_at
FROM events
WHERE session_id = $1 AND organization_id = $2 AND project_id = $3 AND seq > $4
ORDER BY seq
LIMIT $5;
