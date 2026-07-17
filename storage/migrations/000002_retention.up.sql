-- Retention tombstones for store:false responses (spec §8.3, §20.9). A transient
-- response is retained briefly, then its content is purged, leaving only a tombstone:
-- the response row keeps its identity and purge time but no customer content, and the
-- idempotency record keeps its request hash plus the resource tombstone and outcome
-- fingerprint so a replay is answered 410 without re-execution.
--
-- Only ALTERs on tables created by 000001, so no new grants are needed and the
-- palai_app privileges already cover the new columns. Every ADD COLUMN IF NOT EXISTS
-- keeps the migration safe to re-run (Migrate is idempotent).

ALTER TABLE responses
    -- store defaults true (§8.3): a response is persistent unless the request opts out.
    ADD COLUMN IF NOT EXISTS store BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN IF NOT EXISTS purged_at TIMESTAMPTZ;

ALTER TABLE idempotency_records
    ADD COLUMN IF NOT EXISTS result_purged_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS resource_tombstone TEXT,
    ADD COLUMN IF NOT EXISTS outcome_fingerprint TEXT;

INSERT INTO schema_migrations (version) VALUES (2) ON CONFLICT DO NOTHING;
