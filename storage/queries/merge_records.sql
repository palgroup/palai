-- Explicit child->parent merge record queries (spec §30.5, REP-011). The migration owns the
-- constraints; these are the write/read paths the merge operation issues against them.

-- name: RecordMerge
-- Record an explicit child-branch merge and its outcome (merged or conflicted), naming the source
-- child run (spec §30.5 "records source child run", REP-011).
INSERT INTO merge_records
    (id, organization_id, project_id, parent_run_id, source_child_run_id, child_branch, merged, merge_commit, conflict_paths)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);
