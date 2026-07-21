-- Trigger management + delivery pipeline (spec §20.2.2, E11 Task 2). Writes are the management surface
-- (create trigger / revise / create+advance delivery); reads resolve the ACTIVE revision (highest
-- revision_number — there is no publish flag, AGT-002 pin-at-accept) and drive dedupe / correlation /
-- concurrency. A revise always INSERTs a new immutable revision — no statement here rewrites a revision's
-- config columns. Every statement is tenant-scoped by (organization_id, project_id).

-- name: InsertTrigger
INSERT INTO triggers (id, organization_id, project_id, name, type)
VALUES ($1, $2, $3, $4, $5);

-- TriggerForDelivery verifies a trigger is in scope and returns whether it is enabled (a disabled
-- trigger rejects new deliveries).
-- name: TriggerForDelivery
SELECT enabled FROM triggers WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- InsertTriggerRevision creates a NEW immutable revision (revise = new INSERT, the 000019 discipline).
-- revision_number is the trigger's next monotonic number, computed in-statement. At most one run target
-- is pinned (the CHECK enforces it). Returns the revision_number.
-- ponytail: the MAX+1 subselect can race two concurrent revises to the same number; the
-- UNIQUE(trigger_id, revision_number) then rejects the loser (retry on 23505 if concurrent revise
-- throughput ever matters — a human authoring cadence does not).
-- name: InsertTriggerRevision
INSERT INTO trigger_revisions (
    id, organization_id, project_id, trigger_id, revision_number,
    agent_revision_id, run_template_revision_id, input_mapping,
    dedupe_key_expr, correlation_mode, correlation_key_expr, concurrency_policy)
VALUES ($1, $2, $3, $4,
        (SELECT COALESCE(MAX(revision_number), 0) + 1 FROM trigger_revisions WHERE trigger_id = $4),
        $5, $6, $7, $8, $9, $10, $11)
RETURNING revision_number;

-- ActiveTriggerRevision resolves the trigger's ACTIVE revision (highest revision_number) — the revision
-- a new delivery pins at accept. Returns the revision id + number.
-- name: ActiveTriggerRevision
SELECT id, revision_number
FROM trigger_revisions
WHERE trigger_id = $1 AND organization_id = $2 AND project_id = $3
ORDER BY revision_number DESC
LIMIT 1;

-- GetTriggerRevision loads a revision's full config for the delivery pipeline (mapping, dedupe/
-- correlation exprs, correlation mode, concurrency policy, and the run-target pin). Tenant-scoped.
-- name: GetTriggerRevision
SELECT agent_revision_id, run_template_revision_id, input_mapping,
       dedupe_key_expr, correlation_mode, correlation_key_expr, concurrency_policy
FROM trigger_revisions
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- InsertDelivery accepts a delivery, PINNING trigger_revision_id at accept (AGT-002) and recording the
-- accepting principal (so a deferred resume admits under the same principal). Born 'received' (the
-- state-machine genesis); the pipeline advances it from here.
-- name: InsertTriggerDelivery
INSERT INTO trigger_deliveries (id, organization_id, project_id, trigger_id, trigger_revision_id, principal_id)
VALUES ($1, $2, $3, $4, $5, $6);

-- RecordDeliveryAdmitted records the born run's coordinates (response/run/session) + the mapped canonical
-- input on the delivery and advances it to 'admitted'. The delivery is now tied to a session, so the
-- run_created transition (SetDeliveryState) rides the run's own journal.
-- name: RecordDeliveryAdmitted
UPDATE trigger_deliveries
SET state = 'admitted', response_id = $4, run_id = $5, session_id = $6, mapped_input = $7, updated_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- SetDeliveryCorrelationHash records the delivery's correlation-key HASH (only the hash is stored, never
-- the raw key — spec §20.2.2). The hash is (project, trigger_revision, source_tenant)-scoped by its input.
-- name: SetDeliveryCorrelationHash
UPDATE trigger_deliveries
SET correlation_key_hash = $4, updated_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- FindCorrelatedSession resolves the session a bounded_key_reuse / reject_if_active delivery correlates
-- onto: the most recent OTHER delivery (of this trigger, in scope) that carries the same correlation hash
-- and a resolved session. Only THIS tenant's deliveries are queried, so a correlation can never reach a
-- foreign session (authz is not bypassed).
-- name: FindCorrelatedSession
SELECT session_id FROM trigger_deliveries
WHERE trigger_id = $1 AND organization_id = $2 AND project_id = $3
  AND correlation_key_hash = $4 AND session_id <> '' AND id <> $5
ORDER BY received_at DESC
LIMIT 1;

-- GetDeliveryPin reads a delivery's pinned revision + state (the AGT-002 assertion + pipeline read).
-- name: GetDeliveryPin
SELECT trigger_revision_id, state
FROM trigger_deliveries
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- SetDeliveryState advances a delivery's state (the SM transition persisted; the caller computes the
-- legal transition via the TriggerDelivery table, this only writes it). Bumps updated_at.
-- name: SetDeliveryState
UPDATE trigger_deliveries
SET state = $4, updated_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- SetDeliveryReason advances a delivery's state AND records a human reason (a reject/skip/fail).
-- name: SetDeliveryReason
UPDATE trigger_deliveries
SET state = $4, reason = $5, updated_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- ClaimCanonicalDelivery makes a delivery the LIVE canonical row for its (trigger, dedupe_key): it sets
-- the dedupe_key + advances to 'deduplicated'. The partial UNIQUE index
-- (trigger_deliveries_dedupe_canonical_idx) rejects a second live canonical for the same key with a
-- 23505, so a duplicate loses HERE (at the DB, race-free) rather than in an app-code check-then-set.
-- name: ClaimCanonicalDelivery
UPDATE trigger_deliveries
SET dedupe_key = $4, state = 'deduplicated', updated_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3
RETURNING id;

-- FindCanonicalDelivery resolves the surviving canonical original a duplicate links to (AUT-001
-- original-linkage): the earliest live canonical row for the (trigger, dedupe_key).
-- name: FindCanonicalDelivery
SELECT id FROM trigger_deliveries
WHERE trigger_id = $1 AND organization_id = $2 AND project_id = $3
  AND dedupe_key = $4 AND duplicate_of IS NULL
ORDER BY received_at
LIMIT 1;

-- MarkDeliveryDuplicate links a losing delivery to its canonical original and terminalizes it
-- 'duplicate'. duplicate_of is set, so the dedupe_key it also records stays exempt from the canonical
-- index (WHERE duplicate_of IS NULL).
-- name: MarkDeliveryDuplicate
UPDATE trigger_deliveries
SET state = 'duplicate', duplicate_of = $4, dedupe_key = $5, reason = $6, updated_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3;
