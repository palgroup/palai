-- The desired-configuration journal (E29, migration 000052): what this MACHINE should be running with,
-- written by the admin panel and applied by the next bring-up.
--
-- TWO STATEMENTS AND NO THIRD. There is no update and no delete, because the table grants neither
-- (000052's self-re-asserting REVOKE) — a statement here that tried would be a runtime permission error
-- dressed as a feature. "Change a setting" and "stop deciding a setting" are both a new revision holding
-- the whole document, which is what makes removal expressible as absence.
--
-- BOTH RUN UNDER storage.WithSystemScope. deployment_desired carries no organization_id, so there is no
-- tenant predicate to write and no RLS policy to run under; the authority is the `provision` capability on
-- the API key, checked before either statement is reached.

-- InsertDeploymentDesired appends one revision holding the WHOLE document, and returns the revision it
-- was given so the caller can report it without a second read (which would be a second read of a table
-- another writer may have appended to in between).
-- name: InsertDeploymentDesired
INSERT INTO deployment_desired (document, written_by)
VALUES ($1, $2)
RETURNING revision, written_at;

-- LatestDeploymentDesired reads the current desired document: the highest revision.
--
-- THE ORDER BY IS LOAD-BEARING AND IT IS NOT DEFENSIVE STYLE. An unordered LIMIT 1 has decided a security
-- outcome in this tree TWICE, and here the row it returns is the one a bring-up turns into the process's
-- environment — so an arbitrary row is an arbitrary deployment. `revision` is a generated identity and
-- therefore total, so this needs no tiebreak: two writes inside one clock tick are two revisions, which is
-- exactly why the ordering key is not written_at.
-- name: LatestDeploymentDesired
SELECT revision, document, written_at, written_by
FROM deployment_desired
ORDER BY revision DESC
LIMIT 1;
