-- Reverse 000052. Guarded on the target still existing, because MigrationDown() is one concatenated chain
-- and re-running it must stay a clean no-op — 000051's, 000050's and 000049's reversals are written the same
-- way.
--
-- THE HONEST SHAPE OF THIS ROLLBACK, AND IT IS THE SHARP KIND. Restoring the foreign key restores
-- ON DELETE CASCADE with it, which is what the constraint said before this migration ran. So crossing back
-- over this line does not merely restore a constraint: it re-arms the erasure this migration exists to
-- prevent, and the next endpoint deleted takes its whole delivery trail with it again.
--
-- Worse, the ADD CONSTRAINT itself can fail, and failing is the better of its two outcomes. Any delivery row
-- orphaned while this migration was applied — the ordinary result of deleting an endpoint, which is the
-- capability this migration shipped — violates the key being restored, so Postgres refuses the whole
-- statement and the rollback stops here rather than half-applying. NOT VALID is deliberately not used: it
-- would let the constraint attach while leaving those rows in place, which buys a clean rollback by
-- accepting a table that no longer satisfies its own declared key, and a constraint the data does not meet
-- is worse than a rollback that stops.
--
-- To cross this line: decide what the orphaned deliveries are worth. Export them if the audit trail is
-- wanted, then delete them; the ADD CONSTRAINT succeeds once no delivery names a missing endpoint.
-- Guarded on the table existing AND the constraint not already being there, so re-running the concatenated
-- chain is a clean no-op. The guard is a PRESENCE check and deliberately NOT an exception handler: swallowing
-- errors here would swallow the orphan failure above, which is the one outcome this statement must keep.
DO $$
BEGIN
    IF to_regclass('public.webhook_deliveries') IS NOT NULL
       AND NOT EXISTS (
           SELECT 1 FROM pg_constraint
           WHERE conname = 'webhook_deliveries_endpoint_id_fkey'
             AND conrelid = 'public.webhook_deliveries'::regclass
       ) THEN
        ALTER TABLE webhook_deliveries
            ADD CONSTRAINT webhook_deliveries_endpoint_id_fkey
            FOREIGN KEY (endpoint_id) REFERENCES webhook_endpoints (id) ON DELETE CASCADE;
    END IF;
END
$$;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 52;
    END IF;
END
$$;
