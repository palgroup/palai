//go:build component

// Package metering_test holds the real-PostgreSQL component tests for the metering store (E13 Task 6,
// BIL-001/BIL-003/QUO-001). They run only under `make test-component TEST=postgres`; the build tag keeps
// them out of the credential-free unit tier. The handler tests upstream use a fake seam, so THIS is where
// the SQL itself is proven — the prefix matching, the org/project narrowing, and the keyset page.
package metering_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/metering"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

func openHarness(t *testing.T) *coordinator.Store {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	cs, err := coordinator.Open(context.Background(), url)
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return cs
}

func newID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

// tenant seeds an organization with two projects, so every test can prove the intra-organization
// narrowing the SQL (not RLS) is responsible for.
func tenant(t *testing.T, cs *coordinator.Store) (org, projectA, projectB string) {
	t.Helper()
	ctx := storage.WithSystemScope(context.Background())
	org, projectA, projectB = newID("org"), newID("prj"), newID("prj")
	stmts := [][]any{
		{`INSERT INTO organizations (id) VALUES ($1)`, org},
		{`INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, projectA, org},
		{`INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, projectB, org},
	}
	for _, stmt := range stmts {
		if _, err := cs.Pool().Exec(ctx, stmt[0].(string), stmt[1:]...); err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
	}
	return org, projectA, projectB
}

func settle(t *testing.T, cs *coordinator.Store, org, project, meter, unit string, quantity float64) {
	t.Helper()
	if _, err := cs.Pool().Exec(storage.WithSystemScope(context.Background()),
		`INSERT INTO usage_ledger (id, organization_id, project_id, run_id, meter, quantity, unit, dedupe_key)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $1)`,
		newID("use"), org, project, newID("run"), meter, quantity, unit); err != nil {
		t.Fatalf("settle %s: %v", meter, err)
	}
}

// TestSetLimitIsAnUpsertScopedToTheCaller proves a limit belongs to the scope of the key that set it and
// that re-POSTing the same meter prefix RESTATES it rather than minting a rival row — two rows for one
// prefix would make "which limit binds?" a race.
func TestSetLimitIsAnUpsertScopedToTheCaller(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	store := metering.New(cs.Pool())
	org, projectA, _ := tenant(t, cs)
	scope := middleware.Scope{Organization: org, Project: projectA}

	first, err := store.SetBudget(ctx, scope, []byte(`{"meter_prefix":"model.","limit_quantity":1000}`))
	if err != nil {
		t.Fatalf("SetBudget error = %v", err)
	}
	second, err := store.SetBudget(ctx, scope, []byte(`{"meter_prefix":"model.","limit_quantity":2500}`))
	if err != nil {
		t.Fatalf("SetBudget(restate) error = %v", err)
	}
	var a, b struct {
		ID            string  `json:"id"`
		ProjectID     string  `json:"project_id"`
		LimitQuantity float64 `json:"limit_quantity"`
	}
	mustDecode(t, first.Body, &a)
	mustDecode(t, second.Body, &b)
	if a.ID != b.ID {
		t.Fatalf("re-setting the same meter prefix minted a second budget (%s then %s)", a.ID, b.ID)
	}
	if b.LimitQuantity != 2500 {
		t.Fatalf("restated limit = %v, want 2500", b.LimitQuantity)
	}
	if b.ProjectID != projectA {
		t.Fatalf("budget project = %q, want the caller's own %q (the scope is never a body field)", b.ProjectID, projectA)
	}

	// An unknown field is rejected outright: a misspelled limit must not silently store no cap.
	if out, _ := store.SetBudget(ctx, scope, []byte(`{"meter_prefix":"model.","limit":1}`)); !out.BadField {
		t.Fatalf("unknown-field budget = %+v, want a strict-decode reject", out)
	}
	if out, _ := store.SetQuota(ctx, scope, []byte(`{"meter_prefix":"run.","limit_quantity":5}`)); out.MissingField != "window_seconds" {
		t.Fatalf("quota without a window = %+v, want a missing window_seconds reject", out)
	}
}

// TestUsageSummaryTotalsTheCallersScope proves the summary reports the caller's own consumption: a
// project-scoped key sees its project's meters (never its sibling's), an org-scoped key sees the whole
// organization, and both see the limits their totals are measured against.
func TestUsageSummaryTotalsTheCallersScope(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	store := metering.New(cs.Pool())
	org, projectA, projectB := tenant(t, cs)

	settle(t, cs, org, projectA, "model.input_tokens", "token", 30)
	settle(t, cs, org, projectA, "model.output_tokens", "token", 12)
	settle(t, cs, org, projectB, "model.output_tokens", "token", 500)
	if _, err := store.SetBudget(ctx, middleware.Scope{Organization: org, Project: projectA},
		[]byte(`{"meter_prefix":"model.","limit_quantity":1000}`)); err != nil {
		t.Fatalf("SetBudget error = %v", err)
	}

	projectTotal, budgets := summaryTotals(t, store, middleware.Scope{Organization: org, Project: projectA})
	if projectTotal != 42 {
		t.Fatalf("project-A summary total = %v, want 42 (its own two meters, not project-B's 500)", projectTotal)
	}
	if budgets != 1 {
		t.Fatalf("project-A summary carried %d budget(s), want the 1 that binds it", budgets)
	}
	orgTotal, _ := summaryTotals(t, store, middleware.Scope{Organization: org})
	if orgTotal != 542 {
		t.Fatalf("org-scoped summary total = %v, want 542 (both projects)", orgTotal)
	}
}

// TestLedgerPageIsKeysetOrderedAndScoped proves the raw entry page an exporter reads: newest first, no
// row repeated or skipped across the keyset boundary, and confined to the caller's project.
func TestLedgerPageIsKeysetOrderedAndScoped(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	store := metering.New(cs.Pool())
	org, projectA, projectB := tenant(t, cs)
	for i := range 5 {
		settle(t, cs, org, projectA, "model.output_tokens", "token", float64(i+1))
	}
	settle(t, cs, org, projectB, "model.output_tokens", "token", 999)
	scope := middleware.Scope{Organization: org, Project: projectA}

	// Page one asks for 3 (2 + the has_more over-fetch the handler adds).
	first, err := store.ListUsageLedger(ctx, scope, api.ListQuery{Limit: 3})
	if err != nil {
		t.Fatalf("ListUsageLedger error = %v", err)
	}
	if len(first) != 3 {
		t.Fatalf("first page returned %d rows, want the 3 requested", len(first))
	}
	if !first[0].CreatedAt.After(first[2].CreatedAt) {
		t.Fatalf("page is not newest-first: %v then %v", first[0].CreatedAt, first[2].CreatedAt)
	}

	// The page's last row is the keyset position; the next page continues strictly before it.
	boundary := first[1]
	second, err := store.ListUsageLedger(ctx, scope, api.ListQuery{
		Limit: 10, After: &api.ListCursor{CreatedAt: boundary.CreatedAt, ID: boundary.ID},
	})
	if err != nil {
		t.Fatalf("ListUsageLedger(after) error = %v", err)
	}
	if len(second) != 3 {
		t.Fatalf("continuation returned %d rows, want the 3 remaining of project-A's 5", len(second))
	}
	for _, row := range second {
		if row.ID == first[0].ID || row.ID == first[1].ID {
			t.Fatalf("continuation repeated row %s across the keyset boundary", row.ID)
		}
	}
	// Project-B's row never appears in project-A's page, and every row names project-A.
	for _, row := range append(first, second...) {
		var entry struct {
			ProjectID string `json:"project_id"`
			Object    string `json:"object"`
		}
		mustDecode(t, row.Body, &entry)
		if entry.ProjectID != projectA {
			t.Fatalf("page carried a row of project %q, want only %q", entry.ProjectID, projectA)
		}
		if entry.Object != "usage_ledger_entry" {
			t.Fatalf("row object = %q, want usage_ledger_entry", entry.Object)
		}
	}

	// A created_after filter narrows the page the same way every other list's does.
	future := time.Now().UTC().Add(time.Hour)
	empty, err := store.ListUsageLedger(ctx, scope, api.ListQuery{Limit: 10, CreatedGTE: &future})
	if err != nil {
		t.Fatalf("ListUsageLedger(filtered) error = %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("created_after in the future returned %d rows, want 0", len(empty))
	}
}

func summaryTotals(t *testing.T, store *metering.Store, scope middleware.Scope) (total float64, budgets int) {
	t.Helper()
	out, err := store.UsageSummary(context.Background(), scope)
	if err != nil {
		t.Fatalf("UsageSummary error = %v", err)
	}
	var body struct {
		Meters []struct {
			Quantity float64 `json:"quantity"`
		} `json:"meters"`
		Budgets []json.RawMessage `json:"budgets"`
	}
	mustDecode(t, out.Body, &body)
	for _, m := range body.Meters {
		total += m.Quantity
	}
	return total, len(body.Budgets)
}

func mustDecode(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
}
