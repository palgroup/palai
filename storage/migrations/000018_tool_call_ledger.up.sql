-- Tool-call replay ledger rider (spec §26.6-26.7, E10 Task 7). 000001 created tool_calls with only
-- id/state/name/arguments/result + the fence, and UpsertToolCall only ever wrote state 'completed'.
-- The durable execution ledger needs the per-operation REPLAY CLASS and reconciliation columns §26.6's
-- last paragraph names, so a kill-after-execute row is classified (pure re-runs cached; irreversible
-- enters `uncertain` and NEVER auto-replays) and an uncertain row is reconciled against its destination
-- before its result can re-enter reasoning. This is an ALTER rider, not a new table.
--
-- All ADD COLUMN IF NOT EXISTS with a benign DEFAULT, so the whole chain stays re-runnable (the 000014
-- rider pattern) and every legacy 'completed' row backfills without a rewrite of dependent code. There
-- is deliberately NO approvals rider here: approvals.expires_at + the 'expired' publication state
-- forward-declared in 000013 already carry approval-expiry; E10 T7 adds only its ENFORCEMENT (code), no
-- schema.
--
--   replay_class             the operation's kill-recovery class, copied from the tool registration at
--                            execute time (pure|idempotent|reversible|irreversible|interactive, §26.6).
--                            'pure' default keeps a legacy completed row re-runnable-cached (safe).
--   request_hash             the canonical sha256 of (name, arguments) — the same digest the broker
--                            computes — so a duplicate tool_call_id is recognised by content (TOL-016).
--   external_idempotency_key a stable destination key an idempotent tool resends under so the external
--                            side settles ONE object across retries (TOL-002); '' when not idempotent.
--   lease_owner              the attempt fence that holds the execution lease, so a late callback after
--                            the fence advanced is a stale writer (TOL-017); '' when unleased.
--   reconciliation_state     the uncertain-row reconciliation sub-state
--                            (''|reconciling|reconciled_completed|reconciled_not_applied|manual_resolution)
--                            — the ledger half of the §26.7 tool-call SM's uncertain path.
--   commit_boundary          the model_request_id boundary a tool result commits at, so a result that
--                            arrives after that step already committed is rejected as stale (TOL-017);
--                            '' when the row carries no boundary yet.

ALTER TABLE tool_calls
    ADD COLUMN IF NOT EXISTS replay_class TEXT NOT NULL DEFAULT 'pure',
    ADD COLUMN IF NOT EXISTS request_hash TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS external_idempotency_key TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS lease_owner TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS reconciliation_state TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS commit_boundary TEXT NOT NULL DEFAULT '';

INSERT INTO schema_migrations (version) VALUES (18) ON CONFLICT DO NOTHING;
