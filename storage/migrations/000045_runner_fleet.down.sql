-- Reverse 000045. Every statement is guarded on its table still existing, because MigrationDown() is
-- one concatenated chain and re-running it (or running it after 000001 has already dropped the runner
-- tables) must stay a clean no-op — 000044's and 000040's reversals are written the same way.
--
-- THE HONEST SHAPE OF THIS ROLLBACK: runner_pool_keys and runner_enrollments are DROPPED, because
-- 000045 created them. runner_pools and runners are NOT dropped — 000001 created those and owns their
-- reversal — so this file returns them to the 000001 shape instead, which means restoring the
-- `status` column it replaced. A rollback across this line loses every enrolled runner's registry row
-- content and the whole enrolment journal; the certificates those runners hold keep working, because
-- renewal authenticates with the certificate and never with a database row (§3.6 D5). That asymmetry
-- is the point worth reading twice: rolling back the registry does not disarm the fleet, it blinds it.

DROP TABLE IF EXISTS runner_enrollments;

-- THE POLICY COMES OFF FIRST, AND THE ORDER IS THE WHOLE POINT. 000045 gave `runners` a project_id and
-- re-CALLed palai_apply_tenant_policy so tenant_isolation narrows on it; a POLICY is an "other object"
-- that ALTER TABLE ... DROP COLUMN refuses to cascade through (2BP01), unlike an index, which Postgres
-- drops on its own. Written the other way round — the 000001-shape policy re-asserted at the BOTTOM of
-- this file, after the column was gone — the whole reversal aborted on the first DROP COLUMN, and with
-- it MigrationDown() for every migration in the chain. Re-asserting the org-only rule here removes the
-- dependency and leaves the table secured for every instant in between.
DO $$
BEGIN
    IF to_regclass('public.runners') IS NOT NULL THEN
        CALL palai_apply_tenant_policy('runners', 'organization_id', false);
    END IF;
END
$$;

-- Dropped BEFORE runners loses enrolled_via_key_id would fail on the FK, so the column goes first.
DO $$
BEGIN
    IF to_regclass('public.runners') IS NOT NULL THEN
        ALTER TABLE runners DROP CONSTRAINT IF EXISTS runners_state_check;
        ALTER TABLE runners DROP COLUMN IF EXISTS enrolled_via_key_id;
        ALTER TABLE runners DROP COLUMN IF EXISTS last_seen_at;
        ALTER TABLE runners DROP COLUMN IF EXISTS enrolled_at;
        ALTER TABLE runners DROP COLUMN IF EXISTS cert_not_after;
        ALTER TABLE runners DROP COLUMN IF EXISTS capacity;
        ALTER TABLE runners DROP COLUMN IF EXISTS posture;
        ALTER TABLE runners DROP COLUMN IF EXISTS arch;
        ALTER TABLE runners DROP COLUMN IF EXISTS os;
        ALTER TABLE runners DROP COLUMN IF EXISTS public_key_sha256;
        ALTER TABLE runners DROP COLUMN IF EXISTS runner_dns;
        ALTER TABLE runners DROP COLUMN IF EXISTS label;
        ALTER TABLE runners DROP COLUMN IF EXISTS project_id;
        ALTER TABLE runners DROP COLUMN IF EXISTS state;
        -- Back to 000001's column, with 000001's default.
        ALTER TABLE runners ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'enrolled';
    END IF;
END
$$;

DROP INDEX IF EXISTS runners_tenant_page_idx;
DROP INDEX IF EXISTS runners_pool_idx;
DROP INDEX IF EXISTS runners_dns_key;

DROP TABLE IF EXISTS runner_pool_keys;

-- R5: a run's placement column goes with the pools it referenced.
DO $$
BEGIN
    IF to_regclass('public.runs') IS NOT NULL THEN
        ALTER TABLE runs DROP COLUMN IF EXISTS pool_id;
    END IF;
END
$$;

-- R6 + R1: the seeded pool is deleted and runner_pools returns to its 000001 shape. The seed is
-- deleted rather than kept because after the columns below are gone it would be a pool with no
-- posture — a row describing nothing.
DO $$
BEGIN
    IF to_regclass('public.runner_pools') IS NOT NULL THEN
        DELETE FROM runner_pools WHERE id = 'pool_default';
        ALTER TABLE runner_pools DROP CONSTRAINT IF EXISTS runner_pools_posture_check;
        ALTER TABLE runner_pools DROP COLUMN IF EXISTS strict_enrollment;
        ALTER TABLE runner_pools DROP COLUMN IF EXISTS arch;
        ALTER TABLE runner_pools DROP COLUMN IF EXISTS os;
        ALTER TABLE runner_pools DROP COLUMN IF EXISTS posture;
    END IF;
END
$$;

DROP INDEX IF EXISTS runner_pools_name_key;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 45;
    END IF;
END
$$;
