package execution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
)

// RepositoryStore is the coordinator seam the repository-preparation step resolves bindings and
// records receipts through. *coordinator.Store implements it; a fake implements it in the unit test
// (the ReconcileStore pattern), so the composition is provable without a database.
type RepositoryStore interface {
	GetRepositoryBinding(ctx context.Context, tenant coordinator.Tenant, id string) (contracts.RepositoryBinding, bool, error)
	RecordPreparationReceipt(ctx context.Context, tenant coordinator.Tenant, in coordinator.PreparationReceiptInput) error
}

// PrepareRepositoryInput is the infrastructure-owned input to a run's repository-preparation step
// (spec §30.3). It comes from the resolved binding and the run, never from model output, so the
// recorded provenance does not depend on model behavior (REP-001). TargetDir is the READY workspace's
// repo dir — there is no clone before the workspace is ready.
type PrepareRepositoryInput struct {
	BindingID    string
	RunID        string
	RequestedRef string // empty falls back to the binding's default branch
	WorkBranch   string // the generated agent/<...> branch; empty = detached read-only
	TargetDir    string
	SecretsDir   string // the snapshot-excluded /secrets area for the credential helper (§29.10)
	AttemptFence uint64 // binds the minted read credential to this attempt (§28.11)
	ToolCall     string
}

// PreparedRepository is the outcome of the step: the recorded model-independent receipt plus any
// untrusted-repo containment findings (§30.4).
type PreparedRepository struct {
	Receipt  contracts.PreparationReceipt
	Findings []repositories.Finding
}

// PrepareRepository is the run-start repository-preparation step (spec §30.3): once the workspace is
// READY, it resolves the run's repository binding, runs the infrastructure-owned deterministic
// preparation under a brokered short-lived read credential, and records the model-independent
// receipt. The model never sees the credential (§30.2); the broker revokes it after the fetch.
//
// The root run's auto-provisioning (Orchestrator.provisionFreshAllocation, E09 Task 10) is the
// production caller: it drives the workspace to preparing and calls this to clone @ the attached ref.
// The coding journey (T9) and the live smoke drive the same composed step; nothing waits on it now.
func PrepareRepository(ctx context.Context, store RepositoryStore, broker repositories.Broker, tenant coordinator.Tenant, in PrepareRepositoryInput) (PreparedRepository, error) {
	binding, found, err := store.GetRepositoryBinding(ctx, tenant, in.BindingID)
	if err != nil {
		return PreparedRepository{}, err
	}
	if !found {
		return PreparedRepository{}, fmt.Errorf("prepare repository: binding %s not found in scope", in.BindingID)
	}
	res, err := repositories.Prepare(ctx, broker, repositories.Request{
		CloneURL:      binding.CloneUrl,
		RequestedRef:  in.RequestedRef,
		DefaultBranch: binding.DefaultBranch,
		TargetDir:     in.TargetDir,
		SecretsDir:    in.SecretsDir,
		WorkBranch:    in.WorkBranch,
		Policy:        policyFromBinding(binding.Policy),
		Audience: repositories.Audience{
			Organization: tenant.Organization,
			Project:      tenant.Project,
			Run:          in.RunID,
			AttemptFence: in.AttemptFence,
			ToolCall:     in.ToolCall,
		},
	})
	if err != nil {
		return PreparedRepository{}, err
	}
	if err := store.RecordPreparationReceipt(ctx, tenant, coordinator.PreparationReceiptInput{
		ReceiptID:    "prep_" + randHex16(),
		BindingID:    in.BindingID,
		RunID:        in.RunID,
		RequestedRef: res.Receipt.RequestedRef,
		BaseCommit:   res.Receipt.BaseCommit,
		TreeHash:     res.Receipt.TreeHash,
		Branch:       res.Receipt.Branch,
	}); err != nil {
		return PreparedRepository{}, err
	}
	return PreparedRepository{Receipt: res.Receipt, Findings: res.Findings}, nil
}

// policyFromBinding maps the binding's policy JSONB into the preparation policy. Unknown keys are
// ignored; the zero value is the safe default (hooks disabled, submodules/LFS not materialized, §30.4).
func policyFromBinding(p map[string]any) repositories.Policy {
	return repositories.Policy{
		AllowSubmodules:      boolKey(p, "allow_submodules"),
		AllowLFS:             boolKey(p, "allow_lfs"),
		RequireSignedCommits: boolKey(p, "require_signed_commits"),
	}
}

func boolKey(m map[string]any, key string) bool {
	v, _ := m[key].(bool)
	return v
}

func randHex16() string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}
