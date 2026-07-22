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

// SecretResolver bridges a server-minted (organization, secret ref) to the secret bytes at use time —
// the same shape the webhook / inbound / remote-tool / MCP resolvers already take in their packages. The
// composition root satisfies it from the DB-backed secret-ref store (E13 Task 3). The org is never
// tenant-supplied, so a ref can only ever name a secret provisioned under the caller's OWN organization.
type SecretResolver func(org, ref string) ([]byte, error)

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
	// ConnectionSecrets resolves a binding's connection_ref to its Git credential (E13 Task 9). It is a
	// DEPLOYMENT capability, not run data, and the composition root ALWAYS wires it alongside the workspace
	// provisioner — nil is a test/no-provisioning stack, and there a ref-bearing binding fails closed
	// rather than borrowing the global credential (see bindingBroker). A ref-less binding never consults it.
	//
	// UPGRADING FROM PRE-T9: a ref-bearing binding used to clone with the deployment-global GitHub App
	// credential, because connection_ref had no reader. It now needs its ref actually provisioned — over
	// POST /v1/secret-refs, under the binding's own organization, on a deployment that configured
	// PALAI_SECRET_MASTER_KEY_FILE. Without that the clone FAILS (fail-closed, deliberate); a binding that
	// genuinely wants the deployment credential must carry an EMPTY connection_ref.
	ConnectionSecrets SecretResolver
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
	broker, err = bindingBroker(broker, tenant, binding, in.ConnectionSecrets)
	if err != nil {
		return PreparedRepository{}, err
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

// bindingBroker picks the credential broker one preparation runs behind (E13 Task 9). A binding that
// names a connection_ref clones under THAT tenant's own Git credential, resolved through the secret-ref
// store under the run's server-minted organization — RLS scopes the read to that org, so a binding can
// never resolve another tenant's secret, however it was named. A ref-less binding (every binding written
// before this task) keeps the deployment-global broker, unchanged.
//
// Every other path fails CLOSED, a MISSING resolver included. Falling back to the global credential for a
// binding that deliberately named its own would clone under an authority the tenant did not choose — and
// silently, since nothing downstream can tell the two apart. A composition root that wires the workspace
// provisioner but forgets SetConnectionSecrets is a live hazard (main.go already names a future split
// control-plane/runner deploy), so it is an error here rather than a fallback no test would catch. The
// error names the REF and never the value — the same discipline the secret store's resolver keeps.
//
// HONEST CEILING: this consumes the resolver seam only. There is NO per-tenant GitHub App onboarding
// surface — installing an App per tenant and capturing its installation credential is product/SaaS work
// (explicitly out of scope for this phase). Whatever token the tenant provisioned under the ref is what
// the clone authenticates with; nothing here mints or manages a per-tenant App.
func bindingBroker(global repositories.Broker, tenant coordinator.Tenant, binding contracts.RepositoryBinding, secrets SecretResolver) (repositories.Broker, error) {
	if binding.ConnectionRef == "" {
		return global, nil
	}
	if secrets == nil {
		return nil, fmt.Errorf("prepare repository: binding names connection ref %q but no secret resolver is wired", binding.ConnectionRef)
	}
	token, err := secrets(tenant.Organization, binding.ConnectionRef)
	if err != nil {
		return nil, fmt.Errorf("prepare repository: resolve connection ref %q: %w", binding.ConnectionRef, err)
	}
	if len(token) == 0 {
		return nil, fmt.Errorf("prepare repository: connection ref %q resolved to an empty credential", binding.ConnectionRef)
	}
	return repositories.NewTokenBroker(string(token)), nil
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
