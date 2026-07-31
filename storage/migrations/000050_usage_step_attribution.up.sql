-- 000050 (usage step attribution): make the ledger's per-step identity a COLUMN instead of a substring.
--
-- NUMBERING: 000049 (usage series) was head when this was written, on the same branch. Two other
-- branches were in flight; if either lands a 000050 first, this file renumbers — the integrator's rule.
--
-- ---------------------------------------------------------------------------------------------------
-- WHAT WAS ACTUALLY MISSING, MEASURED — because it is NOT what it looks like
-- ---------------------------------------------------------------------------------------------------
--
-- The reported defect was "one ledger row per run, so which model call spent what is gone". Measured on
-- the live stack, 2026-07-31:
--
--   SELECT meter, count(*), count(DISTINCT run_id) FROM usage_ledger GROUP BY meter;
--     -> model.input_tokens 61 rows / 61 runs      model.output_tokens 61 / 61
--
-- One row per run. But that number is ALSO exactly what a per-STEP ledger produces when every run makes
-- exactly one model call, and that is what this deployment has:
--
--   SELECT calls_per_run, count(*) FROM (
--     SELECT run_id, count(*) AS calls_per_run FROM model_requests GROUP BY run_id) s
--    GROUP BY calls_per_run;
--     -> calls_per_run=1 : 61 runs        (and 61 model_requests in total)
--
-- Settlement has been per model REQUEST all along. packages/coordinator/usage.go:126 keys each entry
-- 'mreq:' || <model_request_id> || ':' || <meter>, and apps/control-plane/internal/execution/
-- model_dispatch.go:281 commits result.Usage — the PER-CALL usage — not the st.usage accumulator that
-- line 246 maintains for the response projection. Every one of the 122 model-meter rows resolves:
--
--   SELECT count(*) FILTER (WHERE EXISTS (
--            SELECT 1 FROM model_requests r
--             WHERE l.dedupe_key = 'mreq:'||r.id||':'||l.meter)) FROM usage_ledger l
--    WHERE l.meter LIKE 'model.%';
--     -> 122 of 122
--
-- So the attribution EXISTS and is exact. What does not exist is a way to READ it: joining a cost to a
-- turn means string-parsing dedupe_key, a field whose format is an idempotency detail and which nothing
-- constrains. That is the real defect and it is one column wide.
--
-- ---------------------------------------------------------------------------------------------------
-- IDEMPOTENCY: UNCHANGED, and this is the part worth being explicit about
-- ---------------------------------------------------------------------------------------------------
--
-- Adding this column does NOT change what "the same settlement" means, because the identity was already
-- per-step: the unique key (organization_id, project_id, dedupe_key) already reads "one row per (model
-- request, meter)". This column duplicates, as a queryable value, information the key already carried.
-- A redelivered step still re-derives the same dedupe_key, still hits ON CONFLICT DO NOTHING, and still
-- settles exactly once (BIL-001). Nothing about the conflict target moves.
--
-- Had settlement been per RUN, this would have been a different migration entirely — splitting one row
-- into many changes which rows collide, and the dedupe_key would have had to change with it.
--
-- ---------------------------------------------------------------------------------------------------
-- THE BACKFILL, AND WHAT NULL MEANS AFTERWARDS
-- ---------------------------------------------------------------------------------------------------
--
-- The 187 existing rows CAN be attributed, contrary to first appearance: 122 of them carry the model
-- request id inside dedupe_key and every one resolves to a live model_requests row (measured above).
-- The backfill below extracts it and — deliberately — only where the extracted id genuinely EXISTS,
-- so a malformed key leaves NULL rather than inventing an identifier.
--
-- The remaining 65 rows are `run.admitted`, keyed 'run:<run_id>:admitted'. They have no model step
-- because they are not one: run.admitted is the ADMISSION reservation, settled before the run executes.
-- So NULL here is not "unknown", it is "this meter does not describe a model call" — the meter column
-- says which, and the two are never ambiguous:
--
--   model.input_tokens / model.output_tokens / step.interrupted  -> a model request, always
--   run.admitted                                                 -> NULL, always
--
-- That contract is asserted by a test rather than a CHECK constraint on purpose. A constraint would
-- abort the INSERT, and this INSERT runs inside the transaction that commits the model result — a
-- metering rule that can kill a completed step is a worse failure than an unattributed row. The
-- invariant is enforced where it is cheap (Go, plus a component test) and not where it is dangerous.
ALTER TABLE usage_ledger ADD COLUMN IF NOT EXISTS model_request_id TEXT;

COMMENT ON COLUMN usage_ledger.model_request_id IS
    'The model request (turn) this settlement is attributed to. NULL means the meter does not describe '
    'a model call — run.admitted is the admission reservation and has no step. Not a foreign key: the '
    'ledger outlives retention of the rows it prices.';

-- Backfill from the identity dedupe_key already carries. split_part is exact here because a model
-- request id contains no colon and a meter name contains none either, so field 2 is the whole id.
-- The EXISTS is the honest part: it attributes only what can be shown to be a real step.
UPDATE usage_ledger l
   SET model_request_id = split_part(l.dedupe_key, ':', 2)
 WHERE l.model_request_id IS NULL
   AND l.dedupe_key LIKE 'mreq:%'
   AND EXISTS (SELECT 1 FROM model_requests r WHERE r.id = split_part(l.dedupe_key, ':', 2));

-- NO INDEX. The column is rendered on the ledger page (which pages by the shared (occurred_at, id)
-- keyset and narrows by session_id/meter — both already indexed), and no shipped query filters on
-- model_request_id. Adding one now would be an index with no reader; 000049's was added because its
-- query was measured to need it, and the same standard refuses one here. A per-step lookup route, if
-- it lands, brings its own measurement.
--
-- No grant and no policy call: usage_ledger already carries 000032's REVOKE-narrowed grants and
-- 000029's tenant policy, and a new COLUMN inherits both.

INSERT INTO schema_migrations (version) VALUES (50) ON CONFLICT DO NOTHING;
