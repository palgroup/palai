-- Session journal queries (spec §21.1). A state transition and its public event
-- are appended in one transaction; the sequence is allocated by the database so it
-- is strictly increasing and gap-free per session.

-- name: AllocateSequence
INSERT INTO session_sequences (session_id, last_seq)
VALUES ($1, 1)
ON CONFLICT (session_id)
DO UPDATE SET last_seq = session_sequences.last_seq + 1
RETURNING last_seq;

-- name: AppendEvent
INSERT INTO events (id, organization_id, project_id, session_id, seq, type, payload)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: EnqueueOutbox
INSERT INTO outbox (organization_id, project_id, topic, dedupe_key, payload)
VALUES ($1, $2, $3, $4, $5);

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

-- name: ReadEventsAfter
SELECT id, seq, type, payload, created_at
FROM events
WHERE session_id = $1 AND organization_id = $2 AND project_id = $3 AND seq > $4
ORDER BY seq
LIMIT $5;
