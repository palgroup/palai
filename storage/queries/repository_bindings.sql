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
