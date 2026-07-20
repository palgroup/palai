-- Append-only security audit (spec §50.3). The event stream records a denied attach
-- here when a caller reaches for a session outside its verified scope. The row is keyed
-- to the ACTOR's tenant and names only the caller-supplied id, so a cross-tenant session
-- and an unknown one produce the identical content-free denial — no existence is disclosed
-- (spec §39.2). detail defaults to '{}'; palai_app may INSERT but never UPDATE/DELETE.

-- name: InsertAttachDenial
INSERT INTO audit_events (organization_id, project_id, actor, action, outcome, resource)
VALUES ($1, $2, $3, 'session.attach', 'denied', $4);
