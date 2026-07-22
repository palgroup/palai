-- Trigger management + delivery pipeline (spec §20.2.2, E11 Task 2). Writes are the management surface
-- (create trigger / revise / create+advance delivery); reads resolve the ACTIVE revision (highest
-- revision_number — there is no publish flag, AGT-002 pin-at-accept) and drive dedupe / correlation /
-- concurrency. A revise always INSERTs a new immutable revision — no statement here rewrites a revision's
-- config columns. Every statement is tenant-scoped by (organization_id, project_id).

-- name: InsertTrigger
INSERT INTO triggers (id, organization_id, project_id, name, type, created_by)
VALUES ($1, $2, $3, $4, $5, $6);

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
-- output_mapping + callback_endpoint_id (T6, callback output shaping + delivery) ride the SAME immutable
-- INSERT — the columns are pre-provisioned in 000021, so T6 adds behavior with no migration.
INSERT INTO trigger_revisions (
    id, organization_id, project_id, trigger_id, revision_number,
    agent_revision_id, run_template_revision_id, input_mapping,
    dedupe_key_expr, correlation_mode, correlation_key_expr, concurrency_policy,
    output_mapping, callback_endpoint_id)
VALUES ($1, $2, $3, $4,
        (SELECT COALESCE(MAX(revision_number), 0) + 1 FROM trigger_revisions WHERE trigger_id = $4),
        $5, $6, $7, $8, $9, $10, $11, $12, $13)
RETURNING revision_number;

-- WebhookEndpointInScope verifies a callback endpoint belongs to the revising tenant (spec §39.2). The
-- callback_endpoint_id FK is GLOBAL (any webhook_endpoints row), so without this app-side scope check a
-- revise could name another tenant's endpoint and leak the run result to a foreign URL. Returns the id
-- when in scope; no row otherwise.
-- name: WebhookEndpointInScope
SELECT id FROM webhook_endpoints WHERE id = $1 AND organization_id = $2 AND project_id = $3;

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

-- RecordDeliveryMapped advances a delivery to 'mapped' and stores BOTH the mapped canonical input and the
-- correlation-key hash. Storing them here (not only at admit/defer) makes a delivery that crashes after
-- mapping a RECOVERABLE remnant: the reconciler re-runs the concurrency decision from the stored state
-- without the (now gone) source payload.
-- name: RecordDeliveryMapped
UPDATE trigger_deliveries
SET state = 'mapped', mapped_input = $4, correlation_key_hash = $5, updated_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- DeferDelivery gates a mapped delivery behind a busy key: state → 'deferred' (its mapped_input + hash are
-- already stored, so the reconciler can admit it FIFO once the gate opens). A reason records why.
-- name: DeferDelivery
UPDATE trigger_deliveries
SET state = 'deferred', reason = $4, updated_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- KeyHasActiveRun reports whether a (trigger, correlation-key) group already has a delivery whose run is
-- non-terminal — the queue/singleton "busy" gate. A trigger-wide gate (singleton) passes '' for the hash
-- arg semantics by the caller instead (it uses TriggerHasActiveRun).
-- name: KeyHasActiveRun
SELECT 1
FROM trigger_deliveries d
JOIN runs r ON r.id = d.run_id AND r.organization_id = d.organization_id AND r.project_id = d.project_id
WHERE d.trigger_id = $1 AND d.organization_id = $2 AND d.project_id = $3
  AND d.correlation_key_hash = $4 AND d.run_id <> ''
  AND r.state NOT IN ('completed', 'failed', 'canceled', 'timed_out', 'budget_exceeded')
LIMIT 1;

-- TriggerHasActiveRun reports whether ANY delivery of a trigger has a non-terminal run — the singleton
-- (trigger-wide) gate.
-- name: TriggerHasActiveRun
SELECT 1
FROM trigger_deliveries d
JOIN runs r ON r.id = d.run_id AND r.organization_id = d.organization_id AND r.project_id = d.project_id
WHERE d.trigger_id = $1 AND d.organization_id = $2 AND d.project_id = $3
  AND d.run_id <> ''
  AND r.state NOT IN ('completed', 'failed', 'canceled', 'timed_out', 'budget_exceeded')
LIMIT 1;

-- DeferredDeliveryGroups lists the (trigger, scope, correlation-key) groups that hold at least one
-- deferred delivery — the reconciler's per-key FIFO sweep unit (system-wide, not tenant-scoped: the
-- reconciler is a system loop, like the webhook pump's fan-out).
-- name: DeferredDeliveryGroups
SELECT DISTINCT trigger_id, organization_id, project_id, correlation_key_hash
FROM trigger_deliveries
WHERE state = 'deferred';

-- OldestDeferredForKey resolves the FIFO head of a group's deferred deliveries (earliest received) plus
-- the state the reconciler needs to admit it: the pinned revision, the accepting principal, and the
-- stored mapped input.
-- name: OldestDeferredForKey
SELECT id, principal_id, trigger_revision_id, mapped_input
FROM trigger_deliveries
WHERE trigger_id = $1 AND organization_id = $2 AND project_id = $3
  AND correlation_key_hash = $4 AND state = 'deferred'
ORDER BY received_at, id
LIMIT 1;

-- StuckMappedDeliveries lists deliveries stranded in 'mapped' past a grace window — crash remnants that
-- reached mapping but never took the concurrency decision. The reconciler re-decides them.
-- name: StuckMappedDeliveries
SELECT id, organization_id, project_id, principal_id, trigger_id, trigger_revision_id, correlation_key_hash, mapped_input
FROM trigger_deliveries
WHERE state = 'mapped' AND updated_at < clock_timestamp() - make_interval(secs => $1)
ORDER BY updated_at
LIMIT $2;

-- ActiveDeliveryRunForKey resolves the response + run of the (trigger, key) group's currently-active
-- delivery — the run a `replace` policy cancels before admitting the new event.
-- name: ActiveDeliveryRunForKey
SELECT d.response_id, d.run_id
FROM trigger_deliveries d
JOIN runs r ON r.id = d.run_id AND r.organization_id = d.organization_id AND r.project_id = d.project_id
WHERE d.trigger_id = $1 AND d.organization_id = $2 AND d.project_id = $3
  AND d.correlation_key_hash = $4 AND d.run_id <> ''
  AND r.state NOT IN ('completed', 'failed', 'canceled', 'timed_out', 'budget_exceeded')
ORDER BY d.received_at DESC
LIMIT 1;

-- RunHasIrreversibleExecuted reports whether a run's E10 tool-call ledger (000018 rider on 000001's
-- tool_calls) holds an EXECUTED irreversible action — a replay_class='irreversible' row in a completed or
-- uncertain state (the plan-pinned "executed" definition). The post-irreversible guard (§32.6) reads this
-- so replace/coalesce never cancels or subsumes a run that already performed an irreversible side effect.
-- name: RunHasIrreversibleExecuted
SELECT 1
FROM tool_calls
WHERE run_id = $1 AND organization_id = $2 AND project_id = $3
  AND replay_class = 'irreversible' AND state IN ('completed', 'uncertain')
LIMIT 1;

-- LatestDeferredForKey resolves the COALESCE survivor: the newest deferred delivery of a group (a burst
-- of events collapses into the latest — the deterministic reducer).
-- name: LatestDeferredForKey
SELECT id, principal_id, trigger_revision_id, mapped_input
FROM trigger_deliveries
WHERE trigger_id = $1 AND organization_id = $2 AND project_id = $3
  AND correlation_key_hash = $4 AND state = 'deferred'
ORDER BY received_at DESC, id DESC
LIMIT 1;

-- SkipCoalescedDeferred terminalizes every OTHER deferred delivery of a coalesce group `skipped`, linked
-- to the surviving delivery (duplicate_of) — the subsumed rows are recorded, not lost (AUT-005).
-- name: SkipCoalescedDeferred
UPDATE trigger_deliveries
SET state = 'skipped', duplicate_of = $5, reason = 'coalesced into ' || $5, updated_at = clock_timestamp()
WHERE trigger_id = $1 AND organization_id = $2 AND project_id = $3
  AND correlation_key_hash = $4 AND state = 'deferred' AND id <> $5;

-- FindCorrelatedSession resolves the session a bounded_key_reuse / reject_if_active delivery correlates
-- onto: the most recent OTHER delivery (of this trigger, in scope) that carries the same correlation hash
-- and a resolved session. Only THIS tenant's deliveries are queried, so a correlation can never reach a
-- foreign session (authz is not bypassed).
-- name: FindCorrelatedSession
SELECT session_id FROM trigger_deliveries
WHERE trigger_id = $1 AND organization_id = $2 AND project_id = $3
  AND correlation_key_hash = $4 AND session_id <> '' AND id <> $5
ORDER BY received_at DESC, id DESC
LIMIT 1;

-- GetDeliveryPin reads a delivery's pinned revision + state (the AGT-002 assertion + pipeline read).
-- name: GetDeliveryPin
SELECT trigger_revision_id, state
FROM trigger_deliveries
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- GetTrigger reads a trigger + its active revision number for the management GET, plus the inbound-auth
-- surface (created_by + the two source-secret ref HANDLES — never bytes; the resolver redeems them).
-- name: GetTrigger
SELECT t.name, t.type, t.enabled,
       COALESCE((SELECT MAX(revision_number) FROM trigger_revisions WHERE trigger_id = t.id), 0),
       t.created_by, t.inbound_secret_ref, t.inbound_secret_ref_next
FROM triggers t
WHERE t.id = $1 AND t.organization_id = $2 AND t.project_id = $3;

-- ResolveInboundTrigger resolves a trigger GLOBALLY by its server-minted id (the unauthenticated inbound
-- route carries no tenant scope — the source signature is the auth). Returns the tenant scope + the fields
-- the receiver gates on: enabled, type (must be 'webhook'), created_by (the run principal), and the two
-- source-secret refs. An unresolvable/non-webhook/disabled/secret-less trigger is a generic 404 upstream.
-- name: ResolveInboundTrigger
SELECT organization_id, project_id, enabled, type, created_by, inbound_secret_ref, inbound_secret_ref_next
FROM triggers WHERE id = $1;

-- SetInboundSecretRefs rotates a trigger's inbound source-secret HANDLES in place (ref + overlap ref),
-- WITHOUT minting a pipeline revision (rotation is a mutable-endpoint-column write, not a config edit).
-- name: SetInboundSecretRefs
UPDATE triggers
SET inbound_secret_ref = $4, inbound_secret_ref_next = $5
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- InsertInboundDelivery durably records a signed inbound event as the CANONICAL delivery: the source
-- envelope (source/source_tenant/source_event_id) + the raw payload + the run principal (trigger.created_by)
-- + the pinned revision, born 'received'. The pre-provisioned UNIQUE partial index
-- (trigger_deliveries_source_dedupe_idx) makes a second live event for the same source key a 23505 — the
-- caller falls through to InsertInboundDuplicate. This INSERT committing is the durable-ack point (2xx).
-- name: InsertInboundDelivery
INSERT INTO trigger_deliveries
    (id, organization_id, project_id, trigger_id, trigger_revision_id, principal_id,
     source, source_tenant, source_event_id, raw_payload)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- InsertInboundDuplicate records a redelivered/duplicate source event linked to its canonical original and
-- terminalized 'duplicate'. duplicate_of is set, so this row is exempt from the source-dedupe index (WHERE
-- duplicate_of IS NULL) and never self-conflicts. It stores raw_payload + source cols for the delivery view.
-- name: InsertInboundDuplicate
INSERT INTO trigger_deliveries
    (id, organization_id, project_id, trigger_id, trigger_revision_id, principal_id,
     source, source_tenant, source_event_id, raw_payload, state, duplicate_of, reason)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'duplicate', $11, $12);

-- FindCanonicalInboundDelivery resolves the surviving canonical original a duplicate links to: the
-- earliest live canonical row for the (trigger, source, source_tenant, source_event_id).
-- name: FindCanonicalInboundDelivery
SELECT id FROM trigger_deliveries
WHERE trigger_id = $1 AND organization_id = $2 AND project_id = $3
  AND source = $4 AND source_tenant = $5 AND source_event_id = $6 AND duplicate_of IS NULL
ORDER BY received_at, id
LIMIT 1;

-- InboundBacklogDepth reports a trigger's durable non-terminal inbound backlog COUNT and its oldest row's
-- age in seconds — the AUT-010 admission-report gauge (429 + Retry-After carries both). Per-trigger, so a
-- flooded trigger sheds load while others keep flowing.
-- ponytail: a per-request COUNT — a cached gauge is the upgrade if inbound rate ever matters.
-- name: InboundBacklogDepth
SELECT count(*), COALESCE(EXTRACT(EPOCH FROM (clock_timestamp() - MIN(received_at))), 0)::bigint
FROM trigger_deliveries
WHERE trigger_id = $1 AND source_event_id <> ''
  AND state IN ('received', 'authenticated', 'deduplicated', 'mapped', 'admitted', 'deferred');

-- StuckInboundDeliveries lists ack'ed inbound deliveries stranded pre-map (received/authenticated/
-- deduplicated) past a grace window with a durable raw_payload — crash remnants the inbound sweep re-drives
-- from the raw envelope (the T2 zombie ceiling closes for inbound: the payload IS durable here).
-- name: StuckInboundDeliveries
SELECT id, organization_id, project_id, principal_id, trigger_id, trigger_revision_id, source_tenant, raw_payload, state
FROM trigger_deliveries
WHERE state IN ('received', 'authenticated', 'deduplicated')
  AND source_event_id <> '' AND raw_payload IS NOT NULL
  AND updated_at < clock_timestamp() - make_interval(secs => $1)
ORDER BY updated_at
LIMIT $2;

-- ScrubInboundRawPayload NULLs the raw_payload of TERMINAL inbound deliveries older than the retention TTL
-- (short-retention is a behavior, not a caption — encryption-at-rest is E13). A scrubbed terminal row is
-- never re-driven (the sweep needs a non-terminal state), so dropping its raw envelope is safe.
-- name: ScrubInboundRawPayload
UPDATE trigger_deliveries
SET raw_payload = NULL
WHERE source_event_id <> '' AND raw_payload IS NOT NULL
  AND state IN ('run_created', 'rejected', 'duplicate', 'failed', 'skipped')
  AND updated_at < clock_timestamp() - make_interval(secs => $1);

-- GetTriggerDelivery reads a delivery's operator-facing projection (GET /v1/trigger-deliveries/{id}).
-- callback_state exposes the post-run callback's own terminal (independent of the delivery state — a
-- callback that dead-letters never rewinds run_created; AUT-011 link-half).
-- name: GetTriggerDelivery
SELECT trigger_id, trigger_revision_id, state, response_id, run_id, session_id,
       COALESCE(duplicate_of, ''), reason, callback_state, received_at, updated_at
FROM trigger_deliveries
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- CallbackDueDeliveries lists run_created deliveries whose revision names a callback endpoint, whose
-- response has reached a terminal state, and whose callback has not yet been armed (callback_state = '').
-- It is a system-wide sweep (like the reconciler's other sweeps — each row carries its own scope). The
-- response's terminal state + output are the callback source projection. (spec §20.2.2, §32.1)
-- name: CallbackDueDeliveries
SELECT d.id, d.organization_id, d.project_id, d.session_id, d.response_id, d.run_id, d.trigger_id,
       rev.callback_endpoint_id, rev.output_mapping, r.state, r.output
FROM trigger_deliveries d
JOIN trigger_revisions rev ON rev.id = d.trigger_revision_id
JOIN responses r ON r.id = d.response_id AND r.organization_id = d.organization_id AND r.project_id = d.project_id
WHERE d.callback_state = ''
  AND d.state = 'run_created'
  AND rev.callback_endpoint_id IS NOT NULL
  AND r.state IN ('completed', 'failed', 'canceled', 'timed_out', 'budget_exceeded')
ORDER BY d.updated_at
LIMIT $1;

-- ArmDeliveryCallback marks a delivery's callback armed (callback_state → 'pending') once its signed
-- webhook_deliveries row is enqueued in the SAME tx. The InsertDelivery ON CONFLICT + this filter make a
-- re-sweep idempotent (an armed delivery is excluded from CallbackDueDeliveries).
-- name: ArmDeliveryCallback
UPDATE trigger_deliveries SET callback_state = 'pending', updated_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- DeadDeliveryCallback marks a callback dead WITHOUT enqueuing — the output-mapping failed at callback
-- time (a schema-invalid shape). The run result stays intact; only the callback has its own dead terminal.
-- name: DeadDeliveryCallback
UPDATE trigger_deliveries SET callback_state = 'dead', reason = $4, updated_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- MirrorCallbackState mirrors the pump's terminal webhook_deliveries.state onto the delivery's
-- callback_state (pending → delivered/dead). It is a set-based sweep over armed callbacks — callback_state
-- is a bounded MIRROR of the delivery-half state the pump already drives, NOT a second state machine.
-- name: MirrorCallbackState
UPDATE trigger_deliveries d
SET callback_state = whd.state, updated_at = clock_timestamp()
FROM webhook_deliveries whd
WHERE whd.event_id = 'cb:' || d.id
  AND whd.organization_id = d.organization_id AND whd.project_id = d.project_id
  AND d.callback_state = 'pending'
  AND whd.state IN ('delivered', 'dead');

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

-- SkipDelivery terminalizes a delivery `skipped` with a reason and an optional survivor link (a
-- drop_if_running skip, or a coalesce-subsumed row) — a policy skip, distinct from a rejection (AUT-005).
-- name: SkipDelivery
UPDATE trigger_deliveries
SET state = 'skipped', duplicate_of = $4, reason = $5, updated_at = clock_timestamp()
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
ORDER BY received_at, id
LIMIT 1;

-- MarkDeliveryDuplicate links a losing delivery to its canonical original and terminalizes it
-- 'duplicate'. duplicate_of is set, so the dedupe_key it also records stays exempt from the canonical
-- index (WHERE duplicate_of IS NULL).
-- name: MarkDeliveryDuplicate
UPDATE trigger_deliveries
SET state = 'duplicate', duplicate_of = $4, dedupe_key = $5, reason = $6, updated_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3;
