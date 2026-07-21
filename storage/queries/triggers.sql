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

-- InsertDelivery accepts a delivery, PINNING trigger_revision_id at accept (AGT-002). It is born in the
-- 'received' state (the state-machine genesis); the pipeline advances it from here.
-- name: InsertDelivery
INSERT INTO trigger_deliveries (id, organization_id, project_id, trigger_id, trigger_revision_id)
VALUES ($1, $2, $3, $4, $5);

-- GetDeliveryPin reads a delivery's pinned revision + state (the AGT-002 assertion + pipeline read).
-- name: GetDeliveryPin
SELECT trigger_revision_id, state
FROM trigger_deliveries
WHERE id = $1 AND organization_id = $2 AND project_id = $3;
