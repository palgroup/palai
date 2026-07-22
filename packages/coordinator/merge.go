package coordinator

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/palgroup/palai/storage"
)

// MergeRecordInput is one explicit child->parent merge and its outcome (spec §30.5, REP-011). A
// conflict is a merged=false row with the conflicting paths — the parent worktree was left
// consistent (the merge aborted), so this is the explicit-resolution signal, not a half-applied state.
type MergeRecordInput struct {
	MergeID          string
	ParentRunID      string
	SourceChildRunID string
	ChildBranch      string
	Merged           bool
	MergeCommit      string
	ConflictPaths    []string
}

// RecordMerge persists a merge outcome, naming the source child run (spec §30.5, REP-011).
func (s *Store) RecordMerge(ctx context.Context, tenant Tenant, in MergeRecordInput) error {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	conflicts, err := json.Marshal(nonNilSlice(in.ConflictPaths))
	if err != nil {
		return fmt.Errorf("marshal conflict paths: %w", err)
	}
	_, err = s.pool.Exec(ctx, storage.Query("RecordMerge"),
		in.MergeID, tenant.Organization, tenant.Project, in.ParentRunID, in.SourceChildRunID,
		in.ChildBranch, in.Merged, in.MergeCommit, conflicts)
	if err != nil {
		return fmt.Errorf("record merge: %w", err)
	}
	return nil
}
