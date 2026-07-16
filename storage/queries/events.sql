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
