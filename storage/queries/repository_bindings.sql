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
       allowed_operations, policy, data_classification, region_constraint, created_at, archived_at
FROM repository_bindings
WHERE id = $1 AND project_id = $2;

-- ListRepositoryBindings pages a project's bindings newest-first (spec §30.1, E13 T4). Tenant-scoped
-- by RLS; the org/project predicate is defence-in-depth. Bindings carry no lifecycle state, so there
-- is no status filter — only the created_at bounds ($3/$4) and the ($5,$6) keyset, $7 the row cap.
-- name: ListRepositoryBindings
SELECT id, provider, repository_identity, clone_url, default_branch, connection_ref,
       allowed_operations, policy, data_classification, region_constraint, created_at, archived_at
FROM repository_bindings
WHERE project_id = $1
  -- ARCHIVED ROWS ARE HIDDEN BY DEFAULT AND THE SWITCH IS A PARAMETER, NOT A SECOND STATEMENT ($8). A
  -- forked "list archived too" query is a second place for the tenant predicate to be got wrong, which is
  -- the class of defect this corpus keeps finding. `deleted_at IS NULL`-style filtering is an APPLICATION
  -- decision and nothing beneath it enforces it — no policy, no constraint — so it is asserted directly
  -- in tests/security/tenancy.
  AND ($7::boolean OR archived_at IS NULL)
  AND ($2::timestamptz IS NULL OR created_at >= $2)
  AND ($3::timestamptz IS NULL OR created_at <= $3)
  AND ($4::timestamptz IS NULL OR (created_at, id) < ($4, $5))
ORDER BY created_at DESC, id DESC
LIMIT $6;

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
--
-- merge_method rides the same read since E23 T6, and it rides it for the same reason the base does: HOW a
-- pull request lands is a deployment decision an operator made once on the binding, not a per-call choice
-- an agent argues for. Unset reads as '' and MergePullRequest defaults it to `merge`.
--
-- connection_ref and repository_identity ride it since 2026-08-01, and for the reason the merge_method
-- comment gives twice over: WHICH CREDENTIAL a write is made under is a deployment decision an operator
-- made once on the binding, not a per-call choice. Until then the publish half could not see either —
-- clone resolved the binding's own credential (E13 T9) while push and pull request used the
-- deployment-global App, so a tenant's panel-provisioned credential was honoured inbound and ignored
-- outbound. repository_identity replaces PALAI_GITHUB_REPO for the pull-request client, which is why one
-- stack could previously open pull requests against exactly one repository.
SELECT rb.clone_url, pr.branch, rb.default_branch, coalesce(rb.policy->>'merge_method', ''),
       coalesce(rb.connection_ref, ''), coalesce(rb.repository_identity, '')
FROM preparation_receipts pr
JOIN repository_bindings rb ON rb.id = pr.repository_binding_id
WHERE pr.run_id = $1 AND pr.project_id = $2
ORDER BY pr.prepared_at DESC, pr.id DESC
LIMIT 1;

-- name: RepositoryBindingExists
-- Existence check within tenant scope (spec §30.1, §39.2): a response's `repository` field is verified
-- at admit so a bad or foreign binding_id is a 404 there, not a run that fails when the clone cannot
-- resolve the binding. Returns no row for an unknown OR foreign id (existence not disclosed cross-tenant).
--
-- AND NO ROW FOR AN ARCHIVED ONE, WHICH IS WHAT MAKES ARCHIVING MEAN ANYTHING (migration 000057). A new
-- durable state makes every OLD verb that ignores it a bypass, so the clause belongs HERE — at the one
-- site a run's repository attachment passes through — rather than in the list, which only decides what an
-- operator is shown. Without it, `archived_at` would be a display flag and a run naming a retired binding
-- would clone exactly as before.
--
-- An archived binding is therefore answered the same way a foreign one is: not found. That is deliberate
-- rather than lazy — the alternative, a distinct "this binding is archived" code, is a third answer the
-- admission path would have to carry, and the operator-facing distinction already exists on the READ
-- route, which returns the row with its archived_at set.
SELECT 1 FROM repository_bindings
WHERE id = $1 AND project_id = $2 AND archived_at IS NULL;

-- name: SetRepositoryBindingConnection
-- Point a binding at a different credential, or at none (E30, migration 000057). This is the ONLY column
-- of a binding any statement updates, and the migration header carries the argument: identity (provider /
-- repository_identity / clone_url) is what preparation_receipts rows have already asserted about past
-- runs, so a mutable identity would falsify recorded provenance; a credential says only HOW the fetch
-- authenticated and can never do that.
--
-- $4 is allowed to be '' — detaching is a real operator move (the repository became public, or the token
-- was retired) and refusing it would leave the same dead end one step further along.
--
-- ARCHIVED ROWS ARE EXCLUDED. Re-crediting a retired binding is a contradiction: nothing can run against
-- it, so the write would silently do nothing useful. The caller unarchives first, deliberately.
-- Tenant-scoped by RLS; the org/project predicate is defence-in-depth, and the RETURNING drives the
-- handler's 404 for an unknown, foreign, or archived id without a second round trip.
UPDATE repository_bindings
SET connection_ref = $3
WHERE id = $1 AND project_id = $2 AND archived_at IS NULL
RETURNING id;

-- name: ArchiveRepositoryBinding
-- Retire a binding: it leaves the default list and, through RepositoryBindingExists above, refuses new
-- runs. It is NOT a delete and could not be — preparation_receipts.repository_binding_id is a foreign key
-- onto this table, so removing the row would break or cascade away the provenance those receipts exist to
-- keep, and this API has no destructive verb anywhere else.
--
-- IDEMPOTENT BY PREDICATE, NOT BY COALESCE: `archived_at IS NULL` means archiving twice leaves the FIRST
-- timestamp standing and returns no row the second time, so the handler can tell "I retired it" from "it
-- was already retired" — and a second call can never rewrite when it happened.
UPDATE repository_bindings
SET archived_at = clock_timestamp()
WHERE id = $1 AND project_id = $2 AND archived_at IS NULL
RETURNING id;

-- name: UnarchiveRepositoryBinding
-- Put a retired binding back in service. Archiving is operator hygiene rather than a security decision —
-- "stop offering me this row" — and a one-way door in a table that also has no delete would be worse than
-- the duplicates it exists to clear. Revoking a binding's ACCESS is a different operation that already
-- exists: rotate or detach its connection_ref.
--
-- Symmetric to the archive above: `archived_at IS NOT NULL` makes it idempotent and lets the handler tell
-- a restore from a no-op.
UPDATE repository_bindings
SET archived_at = NULL
WHERE id = $1 AND project_id = $2 AND archived_at IS NOT NULL
RETURNING id;
