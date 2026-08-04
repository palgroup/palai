-- 000003 (lease occupancy): runner_leases records an OCCUPANCY — the interval in which one session held
-- one machine. The table is in the baseline's own table list and has never had a writer:
--
--     grep -rniE 'INSERT INTO runner_leases' --include='*.go' --include='*.sql' . | grep -v node_modules | wc -l
--     -> 0   (2026-08-04, immediately before this file)
--
-- That measurement is written down rather than asserted because two decisions below rest on it: the two
-- NOT NULL columns are added to a table that cannot have a row to backfill, and `session_id` is NOT NULL
-- for the same reason. Re-run it before trusting either.
--
-- IT IS THE FIRST MIGRATION AFTER THE SQUASH. The chain was collapsed to 000001_core + 000002_row_level_security
-- on 2026-08-04; this is the third link, and 000002's catalogue sweep already secured `runner_leases`
-- (`CALL palai_apply_tenant_policy('runner_leases', 'project_id')`) because the table existed before it ran.
-- This file CREATES NO TABLE, so it owes no policy call and no grant of its own — the rule in
-- storage/tenant.go binds a migration that creates a table carrying project_id, and its static guard
-- (storage.TestEveryLateTenantTableCarriesItsOwnPolicyAndGrant) selects on exactly that. 000001's
-- table-level `GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE runner_leases TO palai_app` already covers every
-- column added here, table grants not being per-column.

-- WHY session_id, WHEN run_id WAS ALREADY THERE. They are not the same subject and neither replaces the
-- other. A session holds a machine, is closed for idleness, has its machine account destroyed, and RESUMES
-- on whichever machine is free — so one session owns N occupancies over its life and the bill is their SUM,
-- never its wall clock. Within ONE occupancy many runs come and go, which is why run_id cannot key the
-- interval and is left exactly as it was found.
ALTER TABLE runner_leases ADD COLUMN IF NOT EXISTS session_id text NOT NULL;

ALTER TABLE runner_leases ADD COLUMN IF NOT EXISTS started_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL;

-- last_activity_at HAS ONE WRITER AND TWO READERS, and the second reader is the reason it is a column
-- rather than a derived guess. The idle reaper reads it to know when to fire. The BILL reads it to know
-- where to stop: an occupancy closed for idleness is billed to the customer's last activity, and the quiet
-- tail after it is the platform's cost of holding the machine open in case they came back.
--
-- WHAT IS SUBTRACTED IS THIS COLUMN AND NEVER A CONSTANT. A TTL is a setting, so it changes; a reaper is a
-- sweep, so it fires late by however long its tick is. `released_at - <ttl>` is wrong under either, and
-- being wrong here is being wrong about money.
ALTER TABLE runner_leases ADD COLUMN IF NOT EXISTS last_activity_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL;

-- released_at is NULL for exactly as long as the occupancy is open, which is what makes "still holding a
-- machine" a predicate rather than an inference from timestamps.
ALTER TABLE runner_leases ADD COLUMN IF NOT EXISTS released_at timestamp with time zone;
ALTER TABLE runner_leases ADD COLUMN IF NOT EXISTS release_reason text;

-- NO STORED DURATION, DELIBERATELY. A billed interval is derivable from the three timestamps above, and a
-- stored copy is a second truth that can disagree with the one it was derived from — after a correction,
-- after a rule change, or simply because one write landed and the other did not. The derivation lives in
-- one place, `storage/queries/leases.sql`.

-- The reason vocabulary is CONSTRAINED because the billing rule branches on one of its words. A free-text
-- column would let 'Idle' or 'idle ' bill the whole quiet tail to the customer, silently and per typo.
--   idle      the reaper ended it; the tail after last_activity_at is not billed
--   closed    the work finished or the session ended
--   preempted an operator took the machine back (cordon, revoke)
--   lost      the machine stopped answering
ALTER TABLE runner_leases DROP CONSTRAINT IF EXISTS runner_leases_release_reason_check;
ALTER TABLE runner_leases ADD CONSTRAINT runner_leases_release_reason_check
    CHECK (release_reason IS NULL OR release_reason IN ('idle', 'closed', 'preempted', 'lost'));

-- An occupancy that names a session no row can join is an interval billed to nobody, which for this table
-- is the whole failure mode. The other three references on runner_leases are already foreign keys; this
-- matches them, including in having no ON DELETE clause.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'runner_leases_session_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY runner_leases
            ADD CONSTRAINT runner_leases_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id);
    END IF;
END
$$;

-- The index for the one read this task adds: a session's occupancies in the order they happened, which is
-- what a bill for that session IS. runner_leases carried no index but its primary key before this.
CREATE INDEX IF NOT EXISTS runner_leases_session_idx
    ON runner_leases USING btree (project_id, session_id, started_at);

INSERT INTO schema_migrations (version) VALUES (3) ON CONFLICT DO NOTHING;
