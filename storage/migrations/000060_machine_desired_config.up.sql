-- 000060 (E29 follow-on): a MACHINE receives its configuration from the panel, and says back what it did
-- with it.
--
-- WHAT WAS MISSING, measured on the live stack 2026-08-03 rather than recalled:
--
--   docker exec …-postgres-1 psql -At -c "SELECT plane, scope_id, revision, document FROM deployment_desired
--                                         ORDER BY plane, scope_id, revision"
--     control_plane||1|{"PALAI_DISPATCH_WORKERS": "2"}
--     control_plane||3|{"PALAI_DISPATCH_WORKERS": "4"}
--     runner_pool|pool_default|2|{"PALAI_RUNNER_CONCURRENCY": "4"}
--
--   doc written_at   2026-08-03 08:08:03+00
--   runner container started  2026-08-02 21:47:40Z
--
-- The document asking for concurrency 4 was written TEN HOURS after the machine that is supposed to obey it
-- started. Delivery rides ENROLMENT (runner_gateway.go settingsFor), so that machine has never seen it and
-- will not until it enrols again. The store's own journey test writes the limit down in prose:
-- "A MACHINE ALREADY RUNNING does NOT. Delivery rides enrolment and nothing pushes a later revision at a
-- live runner."
--
-- AND THE MEASUREMENT ALMOST LIED, which is worth recording because it is CLAUDE.md's harness rule wearing
-- fleet clothes. `docker inspect` on that runner shows PALAI_RUNNER_CONCURRENCY=4 — the value the document
-- asks for — so a check that read the machine's environment would have reported delivery WORKING. It is
-- compose's 4, set in the runner's environment block, and planeIntDefault falls back to exactly that when
-- the plane sent nothing. The agreeing number was the deployment's own default agreeing with the panel by
-- coincidence. Any proof of delivery must therefore use a value the environment does NOT hold.
--
-- ------------------------------------------------------------------------------------------------------
-- 1. THE MACHINE SCOPE, AND WHY IT IS A PLANE RATHER THAN A NEW AXIS
--
-- "This Mac" is not a pool. A rented Mac that takes four sessions and a Mac that takes one belong to the
-- same pool (posture `unsandboxed-host`) and differ in their configuration, and today the only way to say
-- so is to mint a pool per machine — which makes the pool, the unit that carries POSTURE and ENROLMENT
-- KEYS, into a per-machine container it was not designed to be.
--
-- SO THE KEY GAINS `runner_machine`, WITH scope_id = THE RUNNER ID. And the honest note about the modelling,
-- because the next reader will otherwise re-derive it: `plane` in this schema conflates TWO questions —
-- which PROCESS reads the setting, and which SCOPE the document configures. `runner_pool` and
-- `runner_machine` answer the first identically (both are read by cmd/runner) and differ only in the
-- second. The clean model is two columns. This migration deliberately does NOT introduce them:
--
--   * The pair (plane, scope_id) is the key of a journal that already holds rows, is already the index
--     (deployment_desired_scope_tip_idx), is already a CHECK constraint's subject, and is already an
--     EXPORTED Go constant (api.PlaneRunnerPool) that this migration's CHECK and that constant are
--     deliberately two spellings of one value.
--   * A scope-kind column would have to be back-filled for every existing row and would leave `plane`
--     meaning something different before and after the back-fill.
--
-- The conflation is therefore EXTENDED rather than fixed, and the cost is named here so that a later task
-- that does want the two-axis model knows what it is paying off rather than discovering it.
--
-- PRECEDENCE IS MACHINE-OVER-POOL, KEY BY KEY, and the overlay is computed in the store rather than stored
-- flattened. A flattened document would freeze the pool's values into the machine's row at write time, so a
-- later pool edit would silently not reach a machine that had ever been individually configured — which is
-- the "declared, and nothing happens" defect one layer down.
--
-- THE SCOPE IS NOT VALIDATED AGAINST runners.id BY A FOREIGN KEY, and that is a decision rather than an
-- omission. An operator configures a machine they are ABOUT to enrol as readily as one already enrolled,
-- and a foreign key would refuse the first. What must not happen is a write that matches no machine
-- landing SILENTLY — RecordRunPool's lesson — so the refusal lives at the write path, where it can say
-- which id was not found and offer to write it anyway, rather than in the schema, where it can only 500.
--
-- ------------------------------------------------------------------------------------------------------
-- 2. WHAT THE MACHINE SAYS BACK, AND WHY THE COLUMNS ARE ON `runners`
--
-- The panel must not show "saved" for a value no machine has taken. Three columns on the runner row carry
-- the machine's own answer: which revision it has, what it APPLIED, and what it is holding until a
-- restart. They are on `runners` rather than in a second journal because they are CURRENT STATE of one
-- machine — the latest answer replaces the previous one, exactly like `last_seen_at` beside them — and a
-- journal would grow one row per poll per machine forever to answer a question only ever asked about the
-- tip.
--
-- config_applied IS NOT A COPY OF THE DOCUMENT. It is the machine's verdict, per setting: `applied` means
-- this process changed its behaviour, `pending_restart` means it holds the value and is still running the
-- old one. That distinction is the entire point — a panel that accepted an edit and showed it as live when
-- the machine had merely RECEIVED it would be the defect this tree has spent a week removing, moved one
-- hop further from the operator where it is harder to see.
--
-- runners is NOT tenant-scoped (it is on tests/security/tenancy's nonTenantTables allow-list — a machine
-- serves every tenant on the deployment), so these columns need no policy call and no GRANT: they inherit
-- the table's.

-- The plane vocabulary gains its third member. The scope shape is restated in full rather than amended
-- because a CHECK constraint is replaced whole, and stating all three arms together is what makes the
-- pairing readable: the control plane is the SINGLETON and takes no scope; both runner planes name what
-- they configure and so must carry one.
ALTER TABLE deployment_desired
  DROP CONSTRAINT IF EXISTS deployment_desired_plane_check;
ALTER TABLE deployment_desired
  DROP CONSTRAINT IF EXISTS deployment_desired_scope_shape_check;

ALTER TABLE deployment_desired
  ADD CONSTRAINT deployment_desired_plane_check
  CHECK (plane IN ('control_plane', 'runner_pool', 'runner_machine'));

ALTER TABLE deployment_desired
  ADD CONSTRAINT deployment_desired_scope_shape_check
  CHECK (
       (plane = 'control_plane'  AND scope_id =  '')
    OR (plane = 'runner_pool'    AND scope_id <> '')
    OR (plane = 'runner_machine' AND scope_id <> '')
  );

-- The machine's own answer about the configuration it holds.
--
-- config_revision is the revision of the EFFECTIVE document the machine resolved (the highest revision that
-- contributed to it, pool or machine), so a panel can say "this Mac is running what you saved" without
-- re-deriving the overlay. NULL means the machine has never reported — every runner enrolled before this
-- migration, and every runner too old to poll.
ALTER TABLE runners
  ADD COLUMN IF NOT EXISTS config_revision BIGINT,
  ADD COLUMN IF NOT EXISTS config_applied JSONB,
  ADD COLUMN IF NOT EXISTS config_reported_at TIMESTAMPTZ;

-- The document is an object of setting-name -> verdict. Constrained to an object for the same reason
-- deployment_desired.document is: a JSONB column that will be ranged over must not be able to hold a scalar
-- or an array, or the first reader to range over it is the one that discovers it.
ALTER TABLE runners
  DROP CONSTRAINT IF EXISTS runners_config_applied_is_object;
ALTER TABLE runners
  ADD CONSTRAINT runners_config_applied_is_object
  CHECK (config_applied IS NULL OR jsonb_typeof(config_applied) = 'object');

INSERT INTO schema_migrations (version) VALUES (60) ON CONFLICT DO NOTHING;
