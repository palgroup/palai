-- A repository binding's credential can be CHANGED, and a binding can be RETIRED (E30, spec §30.1).
--
-- THE GAP THIS CLOSES WAS MEASURED, NOT REASONED ABOUT. On the live stack, 2026-08-02:
--
--   curl '.../v1/repository-bindings?limit=100' -> 20 rows, connection_ref absent on ALL of them,
--                                                  and 8 of the 20 the same repository_identity.
--
-- Those twenty could never be given a credential. Not by the API (router mounted POST + GET + GET and
-- no PATCH), not by the store seam (BindingRegistrar was Create/Get/List), not by SQL (one INSERT, no
-- UPDATE), and not by the CLI (no `repository` verb at all). The column was ordinary and writable the
-- whole time — GRANT UPDATE has been there since 000009 — so this was the absence of a code path rather
-- than a constraint, which is the kind of gap that reads as a decision when it is an omission.
--
-- The eight duplicate rows are what the dead end looks like after a few attempts: with no way to fix a
-- binding and no way to remove one, "register another" is the only move, and the picker keeps every
-- attempt forever.
--
-- ONLY THE CREDENTIAL BECOMES MUTABLE, AND THE LINE IS DRAWN AT PROVENANCE. `provider`,
-- `repository_identity` and `clone_url` are the binding's IDENTITY, and every preparation_receipts row
-- already written against it asserts "this run cloned THAT repository". Making identity mutable would
-- retroactively turn recorded provenance into a lie — the receipt would still say `base_commit`
-- abc1234 while the binding now names a different repository entirely, and nothing in the receipt could
-- tell a reader that happened. `connection_ref` says HOW the fetch authenticates, never WHAT it fetched,
-- so changing it cannot falsify a receipt that has already been written.
--
-- `allowed_operations` and `policy` stay immutable too, and that is a decision rather than an oversight:
-- they are CEILINGS a run reads at preparation time, and lowering one under a session that is already
-- running is a different question with a different answer (it wants a boundary, not an UPDATE). Naming
-- them here is cheaper than leaving a reader to wonder whether they were forgotten.
--
-- ARCHIVED_AT IS A TIMESTAMP AND NOT A BOOLEAN, for the reason `schedules.deleted_at` (000022) already
-- is one: WHEN a binding was retired is the question an operator asks second, immediately after WHETHER,
-- and a boolean has to be joined to an audit log to answer it.
--
-- IT IS NOT A DELETE, AND IT COULD NOT BE. preparation_receipts.repository_binding_id is a FOREIGN KEY
-- onto this table, so a hard delete either fails or cascades away the provenance the receipt exists to
-- keep. It would also be the only destructive verb in an API whose posture everywhere else is
-- append-only — decisions are immutable, journals are append-only, secret refs are versioned rather
-- than overwritten. The runner fleet already has the shape this borrows: a machine is `cordoned` and
-- later `resumed` (000045), never deleted, and its history stays readable throughout.
--
-- AND ARCHIVING IS REVERSIBLE, deliberately. It is operator hygiene — "stop offering me this row" —
-- rather than a security decision, and a one-way door in a table that also has no delete would be worse
-- than the duplicates it exists to clear. Revoking a binding's ACCESS is a different operation and
-- already exists: rotate or detach its connection_ref.
--
-- WHAT MAKES THIS MORE THAN A DISPLAY FLAG lives in queries/repository_bindings.sql rather than here:
-- RepositoryBindingExists — the admission guard a run's `repository` attachment passes through — gains
-- `archived_at IS NULL`, so an archived binding REFUSES NEW RUNS. A new durable state makes every old
-- verb that ignores it a bypass, and the guard belongs at the admission site rather than in the list.
ALTER TABLE repository_bindings
  ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ;

-- The list and the admission guard both filter on it, and both are already confined to one project, so
-- the index is (organization_id, project_id, archived_at) rather than a bare column index.
DO $$
BEGIN
    -- GUARDED BY A.2 TASK 6 (see 000067): the chain re-applies IN FULL on every boot, and 000067 drops
    -- organization_id. A bare `CREATE INDEX IF NOT EXISTS` still RESOLVES its column list even when the
    -- index already exists, so the second boot would fail here with 42703. 000065 already rebuilt this
    -- index project-keyed under the SAME name; the statement below is the fresh-install path only.
    IF EXISTS (SELECT 1 FROM pg_attribute att
                 JOIN pg_class cls ON cls.oid = att.attrelid
                 JOIN pg_namespace ns ON ns.oid = cls.relnamespace
                WHERE ns.nspname = 'public' AND cls.relname = 'repository_bindings'
                  AND att.attname = 'organization_id' AND att.attnum > 0 AND NOT att.attisdropped) THEN
        CREATE INDEX IF NOT EXISTS repository_bindings_live_idx
            ON repository_bindings (organization_id, project_id, archived_at);
    END IF;
END
$$;

INSERT INTO schema_migrations (version) VALUES (59) ON CONFLICT DO NOTHING;
