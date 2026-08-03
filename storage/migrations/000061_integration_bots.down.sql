-- Reverse 000061. One table, created by this migration alone and referenced by no other (the plan's own
-- rule — see the up file), so the reversal is a single guarded DROP; the RLS policy and grants go with
-- the table, and nothing else in the chain names `integration_bots`.
DROP TABLE IF EXISTS integration_bots;
