//go:build component

package postgres

import (
	"context"
	"testing"

	"github.com/palgroup/palai/packages/coordinator"

	"github.com/palgroup/palai/storage"
)

// TestRepositoryBindingResolveIsTenantScoped proves a binding resolves only within its own tenant
// (spec §30.3 step 1): a foreign tenant reading the same id gets nothing, disclosing no existence.
func TestRepositoryBindingResolveIsTenantScoped(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()
	tenant, _, _ := seedRun(t, pool)

	bindingID := newID("repo")
	if err := cs.CreateRepositoryBinding(ctx, tenant, coordinator.RepositoryBindingInput{
		BindingID: bindingID, Provider: "local", RepositoryIdentity: "org/repo",
		CloneURL: "file:///srv/repo.git", DefaultBranch: "main",
		AllowedOperations: []string{"read"}, Policy: map[string]any{"allow_submodules": false},
	}); err != nil {
		t.Fatalf("CreateRepositoryBinding() error = %v", err)
	}

	got, found, err := cs.GetRepositoryBinding(ctx, tenant, bindingID)
	if err != nil || !found {
		t.Fatalf("GetRepositoryBinding(owner) found=%v err=%v, want found", found, err)
	}
	if got.CloneUrl != "file:///srv/repo.git" || got.Provider != "local" || len(got.AllowedOperations) != 1 {
		t.Fatalf("GetRepositoryBinding(owner) = %+v, want the stored binding", got)
	}

	// A different tenant (distinct org) sees nothing for the same id.
	other := coordinator.Tenant{Organization: newID("org"), Project: newID("prj")}
	exec(t, pool, `INSERT INTO organizations (id) VALUES ($1)`, other.Organization)
	exec(t, pool, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, other.Project, other.Organization)
	if _, found, err := cs.GetRepositoryBinding(ctx, other, bindingID); err != nil || found {
		t.Fatalf("GetRepositoryBinding(foreign) found=%v err=%v, want not found (no cross-tenant disclosure)", found, err)
	}
}

// TestPreparationReceiptRoundTrip proves the model-independent receipt is durably recorded and reads
// back with its exact commit/tree provenance (spec §30.3 step 10, REP-001 record half).
func TestPreparationReceiptRoundTrip(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()
	tenant, _, runID := seedRun(t, pool)
	// AllocateWorkspace / GetPreparationReceipt are keyed by an opaque id, not by a tenant, so under
	// migration 000029 the CONTEXT is what scopes them — the same way the run worker scopes a claimed
	// job. Declaring it here is what a production caller already does.
	ctx = storage.WithTenant(ctx, tenant.Organization, tenant.Project)

	bindingID := newID("repo")
	if err := cs.CreateRepositoryBinding(ctx, tenant, coordinator.RepositoryBindingInput{
		BindingID: bindingID, Provider: "local", RepositoryIdentity: "org/repo",
		CloneURL: "file:///srv/repo.git", DefaultBranch: "main",
	}); err != nil {
		t.Fatalf("CreateRepositoryBinding() error = %v", err)
	}
	if err := cs.RecordPreparationReceipt(ctx, tenant, coordinator.PreparationReceiptInput{
		ReceiptID: newID("prep"), BindingID: bindingID, RunID: runID,
		RequestedRef: "deadbeef", BaseCommit: "deadbeefcafe", TreeHash: "0011treehash", Branch: "agent/ses_x/run_y",
	}); err != nil {
		t.Fatalf("RecordPreparationReceipt() error = %v", err)
	}

	got, found, err := cs.GetPreparationReceipt(ctx, bindingID, runID)
	if err != nil || !found {
		t.Fatalf("GetPreparationReceipt() found=%v err=%v, want the recorded receipt", found, err)
	}
	if got.BaseCommit != "deadbeefcafe" || got.TreeHash != "0011treehash" || got.Branch != "agent/ses_x/run_y" {
		t.Fatalf("recorded receipt = %+v, want the exact provenance", got)
	}
}
