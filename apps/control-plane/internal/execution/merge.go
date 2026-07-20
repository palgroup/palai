package execution

import (
	"context"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/packages/coordinator"
)

// MergeStore records merge outcomes (the coordinator seam; *coordinator.Store in prod, a fake in the
// unit test — the ReconcileStore idiom).
type MergeStore interface {
	RecordMerge(ctx context.Context, tenant coordinator.Tenant, in coordinator.MergeRecordInput) error
}

// MergeChildBranchInput is the infrastructure-owned input to an explicit child-branch merge. RepoDir
// is the parent worktree; ChildBranch is the child's agent/<session>/<run> branch (spec §30.5).
type MergeChildBranchInput struct {
	MergeID          string
	ParentRunID      string
	SourceChildRunID string
	RepoDir          string
	ChildBranch      string
}

// MergeChildBranch performs an EXPLICIT conflict-aware merge of a child's branch into the parent
// worktree and records the outcome, naming the source child run (spec §30.5, REP-011). A conflict is
// a recorded merged=false result — the parent worktree is left consistent (the merge aborted), never
// silently overwritten. The merge is a local Git operation; no credential is involved.
func MergeChildBranch(ctx context.Context, store MergeStore, tenant coordinator.Tenant, in MergeChildBranchInput) (repositories.MergeResult, error) {
	res, err := repositories.MergeBranch(ctx, in.RepoDir, in.ChildBranch)
	if err != nil {
		return repositories.MergeResult{}, err
	}
	if err := store.RecordMerge(ctx, tenant, coordinator.MergeRecordInput{
		MergeID:          in.MergeID,
		ParentRunID:      in.ParentRunID,
		SourceChildRunID: in.SourceChildRunID,
		ChildBranch:      in.ChildBranch,
		Merged:           res.Merged,
		MergeCommit:      res.MergeCommit,
		ConflictPaths:    res.ConflictPaths,
	}); err != nil {
		return res, err
	}
	return res, nil
}
