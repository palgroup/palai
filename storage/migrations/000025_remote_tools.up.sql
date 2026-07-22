-- Remote HTTP tool async-operation ledger (spec §28.24-28.25, E12 Task 4). A remote_http tool invoke
-- that a customer server answers 202 (accepted, result later) opens ONE durable operation row here: the
-- broker-minted operation id (the callback URL path segment), the one-use audience-bound callback token
-- (stored ONLY as its sha256 hash — the raw token lives only in the signed invoke envelope), the
-- deadline the invoke's timeout_ms sets (a callback after it is LATE — reconciled, never silently
-- committed), and the fence the invoke ran under (the audit/reconcile bond to the ledger's tool_call).
--
-- The result commits to the run through the WAITING executor under a live fence (dispatchTool ->
-- CommitToolResult, TOL-017) — this table NEVER commits to the ledger itself. A late callback writes
-- late_result here and the RemoteToolProber (spec §26.7) carries the uncertain tool_call to
-- reconciled_completed. secret_ref is the tool_revision.secret_ref handle (000024 — NOT a new secret
-- column) the callback endpoint re-resolves to VERIFY the callback signature (the raw credential never
-- enters a row); it is the audience binding for the callback's HMAC. Every object IF NOT EXISTS keeps the
-- migration re-run-safe (Migrate re-runs per boot); tenant scope is the composite (organization_id,
-- project_id) FK (the 000024 idiom).

CREATE TABLE IF NOT EXISTS remote_tool_operations (
    id TEXT PRIMARY KEY,                              -- rop_<hex>; broker-minted, the callback URL path segment
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    -- The ledger tool_call the invoke belongs to (the correlation key the prober + reconcile read by). NOT
    -- an FK: the executor opens this operation BEFORE the invoke — before the callback can race — but a
    -- pure/idempotent tool's tool_calls row is not written until the broker COMMITS it (after the result),
    -- so an FK to tool_calls would be a lifecycle mismatch. The id is app-controlled, tenant-scoped below.
    tool_call_id TEXT NOT NULL,
    -- The tool_revision.secret_ref handle (000024) the callback endpoint re-resolves to verify the
    -- callback signature — a handle, never the raw credential bytes (spec §28.4 secret hygiene).
    secret_ref TEXT NOT NULL DEFAULT '',
    -- sha256(one-use callback token); the raw token is carried only in the signed invoke envelope. The
    -- callback consumes it with a constant-time compare + an atomic state flip (one-use, audience-bound).
    callback_token_hash TEXT NOT NULL,
    deadline TIMESTAMPTZ NOT NULL,                    -- from timeout_ms; a callback after it is LATE
    -- pending -> completed (callback before deadline) | timed_out (executor gave up) | late_result
    -- (callback after the executor timed out). App-validated, no CHECK — a later state adds without a
    -- migration (the 000024 executor-kind idiom).
    state TEXT NOT NULL DEFAULT 'pending',
    external_operation_id TEXT NOT NULL DEFAULT '',   -- the server's own 202 operation id (cancel/progress correlation, deferred §28.25)
    result JSONB,
    result_hash TEXT NOT NULL DEFAULT '',             -- sha256(canonical result); the duplicate-callback sameness gate
    fence BIGINT NOT NULL DEFAULT 0,                  -- the attempt fence the invoke ran under (audit/reconcile bond)
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    completed_at TIMESTAMPTZ,
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

-- At most one PENDING operation per tool_call: a second concurrent live invoke can never open a
-- duplicate (a kill-after-invoke re-drive of a side-effecting call goes uncertain via the ledger, not a
-- new operation). A resolved (completed/timed_out/late_result) row is not indexed, so a fresh pending one
-- may open past it.
CREATE UNIQUE INDEX IF NOT EXISTS remote_tool_operations_one_pending
    ON remote_tool_operations (tool_call_id) WHERE state = 'pending';

-- The reconcile sweep + prober read operations for uncertain calls by tool_call_id.
CREATE INDEX IF NOT EXISTS remote_tool_operations_by_call ON remote_tool_operations (tool_call_id);

GRANT SELECT, INSERT, UPDATE, DELETE ON remote_tool_operations TO palai_app;

INSERT INTO schema_migrations (version) VALUES (25) ON CONFLICT DO NOTHING;
