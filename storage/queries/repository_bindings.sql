-- Repository binding + preparation-receipt queries (spec §30.1, §30.3). The migration owns the
-- constraints; these statements are the read/write paths the coordinator store issues against
-- them. Binding resolve is tenant-scoped (a foreign id discloses nothing); the receipt record is
-- the model-independent provenance the preparation step persists (REP-001).

-- name: CreateRepositoryBinding
-- Register a durable binding for a project's external repository (spec §30.1).
INSERT INTO repository_bindings
    (id, organization_id, project_id, provider, repository_identity, clone_url, default_branch,
     connection_ref, allowed_operations, policy, data_classification, region_constraint)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12);

-- name: GetRepositoryBinding
-- Resolve a binding within tenant scope (spec §30.3 step 1). A foreign or unknown id returns no
-- rows — existence is not disclosed across tenants.
SELECT id, provider, repository_identity, clone_url, default_branch, connection_ref,
       allowed_operations, policy, data_classification, region_constraint, created_at
FROM repository_bindings
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- ListRepositoryBindings pages a project's bindings newest-first (spec §30.1, E13 T4). Tenant-scoped
-- by RLS; the org/project predicate is defence-in-depth. Bindings carry no lifecycle state, so there
-- is no status filter — only the created_at bounds ($3/$4) and the ($5,$6) keyset, $7 the row cap.
-- name: ListRepositoryBindings
SELECT id, provider, repository_identity, clone_url, default_branch, connection_ref,
       allowed_operations, policy, data_classification, region_constraint, created_at
FROM repository_bindings
WHERE organization_id = $1 AND project_id = $2
  AND ($3::timestamptz IS NULL OR created_at >= $3)
  AND ($4::timestamptz IS NULL OR created_at <= $4)
  AND ($5::timestamptz IS NULL OR (created_at, id) < ($5, $6))
ORDER BY created_at DESC, id DESC
LIMIT $7;

-- name: RecordPreparationReceipt
-- Record the model-independent preparation provenance (spec §30.3 step 10, REP-001): base commit,
-- tree hash, and work branch of the exact tree the engine was handed. Append-only per attempt.
INSERT INTO preparation_receipts
    (id, repository_binding_id, organization_id, project_id, run_id, requested_ref, base_commit, tree_hash, branch)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: GetPreparationReceipt
-- The latest preparation receipt for a binding+run — its recorded exact-commit provenance.
SELECT requested_ref, base_commit, tree_hash, branch, prepared_at
FROM preparation_receipts
WHERE repository_binding_id = $1 AND run_id = $2
ORDER BY prepared_at DESC
LIMIT 1;

-- name: RunPublicationTarget
-- The destination a run's publications push to (spec §30.9): the clean remote URL + base branch from
-- the binding, and the work branch from the run's latest preparation receipt. The remote is
-- infrastructure-owned (never model-supplied), so an agent cannot redirect a push.
SELECT rb.clone_url, pr.branch, rb.default_branch
FROM preparation_receipts pr
JOIN repository_bindings rb ON rb.id = pr.repository_binding_id
WHERE pr.run_id = $1 AND pr.organization_id = $2 AND pr.project_id = $3
ORDER BY pr.prepared_at DESC, pr.id DESC
LIMIT 1;

-- name: RepositoryBindingExists
-- Existence check within tenant scope (spec §30.1, §39.2): a response's `repository` field is verified
-- at admit so a bad or foreign binding_id is a 404 there, not a run that fails when the clone cannot
-- resolve the binding. Returns no row for an unknown OR foreign id (existence not disclosed cross-tenant).
SELECT 1 FROM repository_bindings WHERE id = $1 AND organization_id = $2 AND project_id = $3;
