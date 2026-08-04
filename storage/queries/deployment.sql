-- The desired-configuration journal (E29, migration 000052): what this MACHINE should be running with,
-- written by the admin panel and applied by the next bring-up.
--
-- TWO STATEMENTS AND NO THIRD. There is no update and no delete, because the table grants neither
-- (000052's self-re-asserting REVOKE) — a statement here that tried would be a runtime permission error
-- dressed as a feature. "Change a setting" and "stop deciding a setting" are both a new revision holding
-- the whole document, which is what makes removal expressible as absence.
--
-- BOTH RUN UNDER storage.WithSystemScope. deployment_desired carries no tenant column, so there is no
-- tenant predicate to write and no RLS policy to run under; the authority is the `provision` capability on
-- the API key, checked before either statement is reached.

-- InsertDeploymentDesired appends one revision holding the WHOLE document FOR ONE SCOPE, and returns the
-- revision it was given so the caller can report it without a second read (which would be a second read of
-- a table another writer may have appended to in between).
--
-- The plane and scope_id are the caller's, and the CHECK in 000052 is what refuses a malformed pair —
-- `runner_pool` with no id, or `control_plane` with one — at the database rather than only in Go.
-- name: InsertDeploymentDesired
INSERT INTO deployment_desired (plane, scope_id, document, written_by)
VALUES ($1, $2, $3, $4)
RETURNING revision, written_at;

-- LatestDeploymentDesired reads the current desired document FOR ONE SCOPE: the highest revision under
-- that (plane, scope_id).
--
-- THE SCOPE PREDICATE IS AS LOAD-BEARING AS THE ORDER BY. Without it this returns the tip of the WHOLE
-- journal, so a pool's document written after the control plane's would be handed to the control plane's
-- bring-up — one machine's configuration applied to a different plane, which is worse than no
-- configuration at all because it would look like it worked.
--
-- THE ORDER BY IS NOT DEFENSIVE STYLE EITHER. An unordered LIMIT 1 has decided a security outcome in this
-- tree TWICE, and here the row it returns is the one a bring-up turns into the process's environment — so
-- an arbitrary row is an arbitrary deployment. `revision` is a generated identity and therefore total, so
-- this needs no tiebreak: two writes inside one clock tick are two revisions, which is exactly why the
-- ordering key is not written_at. deployment_desired_scope_tip_idx serves this query's exact shape.
-- name: LatestDeploymentDesired
SELECT revision, document, written_at, written_by
FROM deployment_desired
WHERE plane = $1 AND scope_id = $2
ORDER BY revision DESC
LIMIT 1;
