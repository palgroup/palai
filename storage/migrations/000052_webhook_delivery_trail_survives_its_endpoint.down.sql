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
ALTER TABLE IF EXISTS webhook_deliveries
    ADD CONSTRAINT webhook_deliveries_endpoint_id_fkey
    FOREIGN KEY (endpoint_id) REFERENCES webhook_endpoints (id) ON DELETE CASCADE;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 52;
    END IF;
END
$$;
