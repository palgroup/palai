-- DB-backed model routing (E13 Task 8, spec §27.2/§27.6/§27.7; LP §7.3 carve-out). These are the FIRST
-- queries over migration 000001's model_connections / model_routes / model_route_revisions — the tables
-- have carried no reader since the schema landed, so this file needs no migration of its own.
--
-- Tenancy: every statement runs under the caller's (organization, project) scope, so migration 000029's
-- row-level security is what isolates them; the org/project predicates below are the second, application
-- half of the same boundary (a foreign id then matches zero rows and renders a non-disclosing 404).
--
-- Credentials: a connection stores a secret REFERENCE only. The value lives in the E13 T3 secret store and
-- is redeemed at call time by the broker; no query here selects, returns, or could carry a credential.
--
-- Publication state rides the revision's `config` JSONB (spec §27.6 lists publication state as part of a
-- ModelRouteRevision) because 000001 gave the table no published_at column and this task adds no migration.
-- Immutability is by discipline, exactly as for agent_revisions (000019): the ROUTING fields (model,
-- connection_id) are never rewritten — a revise INSERTs a new revision — and the single conditional
-- published_at merge below is the ONE legitimate mutation.

-- name: InsertModelConnection
INSERT INTO model_connections (id, organization_id, project_id, provider, secret_ref)
VALUES ($1, $2, $3, $4, $5);

-- ModelConnectionExists verifies a connection is in the caller's scope before a revision binds it, so a
-- revision can never name a foreign/unknown connection.
-- name: ModelConnectionExists
SELECT 1 FROM model_connections WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- GetModelRouteByName resolves an alias to its route id. Create is get-or-create on this lookup: an alias
-- names one lineage per project.
-- name: GetModelRouteByName
SELECT id FROM model_routes WHERE organization_id = $1 AND project_id = $2 AND name = $3
ORDER BY id LIMIT 1;

-- name: InsertModelRoute
INSERT INTO model_routes (id, organization_id, project_id, name) VALUES ($1, $2, $3, $4);

-- ModelRouteExists verifies the route named in a path is in the caller's scope. A foreign route is
-- indistinguishable from an absent one — the store renders both as NotFound.
-- name: ModelRouteExists
SELECT 1 FROM model_routes WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- name: NextModelRouteRevision
SELECT coalesce(max(revision), 0) + 1 FROM model_route_revisions WHERE route_id = $1;

-- name: InsertModelRouteRevision
INSERT INTO model_route_revisions (id, route_id, revision, config) VALUES ($1, $2, $3, $4)
RETURNING created_at;

-- PublishModelRouteRevision merges the publication timestamp into the revision's config. It is conditional
-- on the revision still being a draft, so a re-publish affects no row (the store renders that as an
-- idempotent success once the revision is confirmed to exist).
-- name: PublishModelRouteRevision
UPDATE model_route_revisions
SET config = config || jsonb_build_object('published_at', clock_timestamp())
WHERE id = $1 AND route_id = $2 AND config->>'published_at' IS NULL
RETURNING id;

-- name: ModelRouteRevisionExists
SELECT 1 FROM model_route_revisions WHERE id = $1 AND route_id = $2;

-- The E16 T1 read-back queries (the E13 T10 write-only gap). Each carries the org/project predicate as the
-- application half of the tenant boundary — the SAME discipline the write queries above use — so a foreign
-- id matches zero rows (a non-disclosing 404) even independently of RLS. Revisions carry no tenant column,
-- so they are reached through a route the caller's scope has already been verified to own (requireModelRoute).
-- No query here selects a credential value: a connection returns its secret REFERENCE name only.

-- name: ListModelConnections
SELECT id, provider, secret_ref, created_at FROM model_connections
WHERE organization_id = $1 AND project_id = $2
ORDER BY id;

-- name: GetModelConnection
SELECT id, provider, secret_ref, created_at FROM model_connections
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- name: ListModelRoutes
SELECT id, name, created_at FROM model_routes
WHERE organization_id = $1 AND project_id = $2
ORDER BY id;

-- name: GetModelRoute
SELECT id, name, created_at FROM model_routes
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- name: ListModelRouteRevisions
SELECT id, revision, config, created_at FROM model_route_revisions
WHERE route_id = $1
ORDER BY revision;

-- name: GetModelRouteRevision
SELECT id, revision, config, created_at FROM model_route_revisions
WHERE id = $1 AND route_id = $2;

-- ResolveProjectModelRoute is the dispatch-path read: the project's highest PUBLISHED revision of the named
-- alias, joined to the connection that carries the provider and the credential reference.
--
-- The join is LEFT so a revision naming a connection that no longer resolves in this tenant returns a row
-- with a NULL provider rather than no row at all: the caller then FAILS the step instead of silently falling
-- back to the deployment default credential (spec §27.7 — a route cannot silently select something else).
-- The join carries the tenant predicate itself, so the fail-closed behaviour does NOT depend on RLS being in
-- force: on a system-scoped connection (or a BYPASSRLS role) an unqualified join would silently hand back a
-- FOREIGN connection's provider + secret_ref instead of failing.
-- ORDER BY is fully determined (revision, then id) so selection is deterministic even if an alias were ever
-- to name two lineages.
-- name: ResolveProjectModelRoute
SELECT rev.id, rev.revision, rev.config->>'model', conn.provider, conn.secret_ref
FROM model_routes r
JOIN model_route_revisions rev ON rev.route_id = r.id
LEFT JOIN model_connections conn
       ON conn.id = rev.config->>'connection_id'
      AND conn.organization_id = r.organization_id
      AND conn.project_id = r.project_id
WHERE r.organization_id = $1 AND r.project_id = $2 AND r.name = $3
  AND rev.config->>'published_at' IS NOT NULL
ORDER BY rev.revision DESC, rev.id DESC
LIMIT 1;
