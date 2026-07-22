//go:build component

// Migration 000032 (E13 Task 6, BIL-001/BIL-003/QUO-001): the append-only usage_ledger every settled
// meter lands in, plus the two durable admission limits read against it — budgets and quotas. These
// tests pin the two properties the SQL asserts LAST in the chain, both of which regress silently
// without them (main.go re-runs the whole chain on every boot):
//
//  1. the append-only grant, which 000001's and 000029's blanket `GRANT ... ON ALL TABLES` re-hand on
//     boot #2 now that the table exists (the 000015/000031 precedent);
//  2. the org-level RLS policy, which 000029's catalogue sweep re-derives as PROJECT-aware on boot #2
//     because usage_ledger carries a project_id column — and a project-narrowed policy would make an
//     organization-wide budget silently under-count.
package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/packages/contracts"

	"github.com/palgroup/palai/storage"
)

// TestMigration32UsageLedgerAppendOnlyAcrossReboots proves the settlement ledger stays append-only for
// the runtime role across a SECOND boot. A ledger whose rows can be updated or deleted by the process
// that writes them is not a settlement record; 000032's REVOKE (it runs LAST, self-re-asserting every
// boot) is what keeps UPDATE/DELETE withheld after the earlier blanket grants re-run.
func TestMigration32UsageLedgerAppendOnlyAcrossReboots(t *testing.T) {
	cs := openHarness(t)
	ctx := storage.WithSystemScope(context.Background())
	pool := cs.Pool()

	// The second boot: this is the one that re-exposes the blanket grants to the now-existing table.
	if err := cs.Migrate(context.Background()); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}

	assertPriv := func(priv string, want bool) {
		t.Helper()
		var got bool
		if err := pool.QueryRow(ctx, `SELECT has_table_privilege('palai_app', 'usage_ledger', $1)`, priv).Scan(&got); err != nil {
			t.Fatalf("has_table_privilege(%s) error = %v", priv, err)
		}
		if got != want {
			t.Fatalf("palai_app %s on usage_ledger = %v, want %v (append-only grant eroded across reboots)", priv, got, want)
		}
	}
	assertPriv("SELECT", true)
	assertPriv("INSERT", true)
	assertPriv("UPDATE", false)
	assertPriv("DELETE", false)

	// Behavioral half: as the runtime role, an UPDATE/DELETE is refused by the privilege check (42501)
	// before RLS is even consulted — a compromised handler cannot restate or erase settled usage.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET ROLE palai_app`); err != nil {
		t.Fatalf("SET ROLE palai_app error = %v", err)
	}
	defer func() { _, _ = conn.Exec(ctx, `RESET ROLE`) }()

	if got := pgCode(mustFail(conn.Exec(ctx, `UPDATE usage_ledger SET quantity = 0`))); got != "42501" {
		t.Fatalf("usage_ledger UPDATE code = %q, want 42501 (append-only: UPDATE withheld)", got)
	}
	if got := pgCode(mustFail(conn.Exec(ctx, `DELETE FROM usage_ledger`))); got != "42501" {
		t.Fatalf("usage_ledger DELETE code = %q, want 42501 (append-only: DELETE withheld)", got)
	}
}

// TestMigration32MeteringPolicyStaysOrgLevelAcrossReboots proves the deliberate exception 000032 makes:
// its three tables are secured at the ORGANIZATION level even though they carry a project_id column, so
// an organization-wide budget can be summed from the project-narrowed connection that admits a run. That
// is exactly the shape 000029's catalogue sweep would overwrite with a project-aware policy on boot #2,
// so this migrates TWICE and then asserts a sibling project's row is still visible within the org — and
// that the ORGANIZATION boundary itself is untouched.
func TestMigration32MeteringPolicyStaysOrgLevelAcrossReboots(t *testing.T) {
	cs := openHarness(t)
	ctx := storage.WithSystemScope(context.Background())
	pool := cs.Pool()
	if err := cs.Migrate(context.Background()); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}

	orgA, orgB := newID("org"), newID("org")
	projectA, projectB := newID("prj"), newID("prj")
	seedLedgerRow(t, pool, orgA, projectA, "model.output_tokens", 40)
	seedLedgerRow(t, pool, orgA, projectB, "model.output_tokens", 2)
	seedLedgerRow(t, pool, orgB, newID("prj"), "model.output_tokens", 900)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET ROLE palai_app`); err != nil {
		t.Fatalf("SET ROLE palai_app error = %v", err)
	}
	defer func() { _, _ = conn.Exec(ctx, `RESET ROLE`) }()
	// The scope a run's admission publishes: org-A, narrowed to project-B. The connection was acquired
	// under the seeding system scope, so palai.system is cleared first — otherwise every policy admits
	// and the assertions below would pass vacuously.
	if _, err := conn.Exec(ctx, `SELECT set_config('palai.system', '', false), set_config('palai.org_id', $1, false), set_config('palai.project_id', $2, false)`, orgA, projectB); err != nil {
		t.Fatalf("publish scope: %v", err)
	}

	var orgTotal float64
	if err := conn.QueryRow(ctx, `SELECT coalesce(sum(quantity), 0) FROM usage_ledger WHERE organization_id = $1`, orgA).Scan(&orgTotal); err != nil {
		t.Fatalf("sum org-A ledger: %v", err)
	}
	if orgTotal != 42 {
		t.Fatalf("org-A ledger total from a project-B-narrowed connection = %v, want 42 (the org-level metering policy did not survive the 000029 sweep)", orgTotal)
	}

	var foreign int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM usage_ledger WHERE organization_id = $1`, orgB).Scan(&foreign); err != nil {
		t.Fatalf("count org-B ledger: %v", err)
	}
	if foreign != 0 {
		t.Fatalf("org-A's connection saw %d org-B ledger row(s); the tenant boundary leaked", foreign)
	}
}

// seedLedgerRow writes one settled ledger row as the migration owner, creating its organization and
// project first so the ledger's tenant foreign key holds.
func seedLedgerRow(t *testing.T, pool *pgxpool.Pool, org, project, meter string, quantity int) {
	t.Helper()
	ctx := storage.WithSystemScope(context.Background())
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO organizations (id) VALUES ($1) ON CONFLICT DO NOTHING`, []any{org}},
		{`INSERT INTO projects (id, organization_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, []any{project, org}},
		{`INSERT INTO usage_ledger (id, organization_id, project_id, meter, quantity, unit, dedupe_key)
		  VALUES ($1, $2, $3, $4, $5, 'token', $1)`, []any{newID("use"), org, project, meter, quantity}},
	}
	for _, stmt := range stmts {
		if _, err := pool.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed ledger row: %v", err)
		}
	}
}

// TestCommitModelResultSettlesUsageExactlyOnce is BIL-001: a model step's usage is settled into the
// ledger in the SAME transaction that commits the step's result, and a REDELIVERED commit of the same
// step re-derives the same deterministic ledger identity and settles nothing new. Metering that is not
// atomic with the fact it meters either loses usage on a crash or double-counts on a redelivery; this
// pins both directions.
func TestCommitModelResultSettlesUsageExactlyOnce(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	tenant, sessionID, runID := seedRun(t, cs.Pool())
	usage := contracts.Usage{InputTokens: 30, OutputTokens: 12, TotalTokens: 42}

	for i := range 2 {
		if _, err := cs.CommitModelResult(ctx, tenant, sessionID, "", runID, "mr_step1",
			[]byte(`{"output":"ok"}`), "model_step.completed.v1", []byte(`{}`), usage); err != nil {
			t.Fatalf("CommitModelResult(%d) error = %v", i, err)
		}
	}

	assertCount(t, cs.Pool(), 2,
		`SELECT count(*) FROM usage_ledger WHERE run_id=$1 AND meter LIKE 'model.%'`, runID)
	var total float64
	if err := cs.Pool().QueryRow(storage.WithSystemScope(ctx),
		`SELECT coalesce(sum(quantity), 0) FROM usage_ledger WHERE run_id=$1 AND meter LIKE 'model.%'`, runID).Scan(&total); err != nil {
		t.Fatalf("sum settled model usage: %v", err)
	}
	if total != 42 {
		t.Fatalf("settled model tokens = %v, want 42 (input+output settled exactly once across a redelivery)", total)
	}
	// The settled rows carry the run they belong to and the session that owns it, so an exporter can
	// attribute spend without re-joining a table retention may already have reaped.
	assertCount(t, cs.Pool(), 2,
		`SELECT count(*) FROM usage_ledger WHERE run_id=$1 AND session_id=$2 AND unit='token' AND schema_version=1`, runID, sessionID)
}

// TestAdmissionReservesTheRunInTheLedger proves the reservation half: an admitted run records itself in
// the ledger inside the admission transaction, so a run-count limit counts runs that have been ADMITTED
// rather than runs that have already finished paying. A replayed admission re-derives the same row.
func TestAdmissionReservesTheRunInTheLedger(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	tenant, principalID := seedTenantWithKey(t, cs.Pool(), "tok-reserve")

	in := admissionInput(principalID, "key-reserve", "hash-A", `{"id":"resp_reserve"}`)
	for i := range 2 {
		if _, err := cs.AdmitResponse(ctx, tenant, in); err != nil {
			t.Fatalf("AdmitResponse(%d) error = %v", i, err)
		}
	}
	assertCount(t, cs.Pool(), 1,
		`SELECT count(*) FROM usage_ledger WHERE run_id=$1 AND meter='run.admitted' AND unit='run' AND quantity=1`, in.RunID)
}

// TestAdmissionRejectsWhenTheDurableBudgetIsExhausted is the durable half of BIL-003 and the shape the
// live smoke exercises: a project whose settled spend has reached its budget is refused at admission —
// before any run exists — and the refusal leaves NOTHING behind, so raising the budget makes the very
// same request admit. It also proves the limit is denominated by meter PREFIX (a 'model.' budget is
// unaffected by a run-count meter).
func TestAdmissionRejectsWhenTheDurableBudgetIsExhausted(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	tenant, principalID := seedTenantWithKey(t, cs.Pool(), "tok-budget")
	exec(t, cs.Pool(), `INSERT INTO budgets (id, organization_id, project_id, meter_prefix, limit_quantity) VALUES ($1, $2, $3, 'model.', 100)`,
		newID("bdg"), tenant.Organization, tenant.Project)

	// Under the limit: the run admits normally.
	first := admissionInput(principalID, "key-b1", "hash-A", `{"id":"resp_b1"}`)
	adm, err := cs.AdmitResponse(ctx, tenant, first)
	if err != nil {
		t.Fatalf("AdmitResponse(first) error = %v", err)
	}
	if adm.LimitExceeded != nil {
		t.Fatalf("first admission = %+v, want an admit (the budget still has headroom)", adm.LimitExceeded)
	}

	// The run settles past the budget, exactly as a real completion would.
	exec(t, cs.Pool(), `INSERT INTO usage_ledger (id, organization_id, project_id, run_id, meter, quantity, unit, dedupe_key)
	     VALUES ($1, $2, $3, $4, 'model.output_tokens', 140, 'token', $1)`,
		newID("use"), tenant.Organization, tenant.Project, first.RunID)

	second := admissionInput(principalID, "key-b2", "hash-A", `{"id":"resp_b2"}`)
	adm, err = cs.AdmitResponse(ctx, tenant, second)
	if err != nil {
		t.Fatalf("AdmitResponse(second) error = %v", err)
	}
	if adm.LimitExceeded == nil {
		t.Fatal("second admission was accepted; the exhausted budget did not reject it")
	}
	if adm.LimitExceeded.Kind != "budget" || adm.LimitExceeded.MeterPrefix != "model." ||
		adm.LimitExceeded.Limit != 100 || adm.LimitExceeded.Used != 140 {
		t.Fatalf("rejection = %+v, want budget/model./limit 100/used 140", *adm.LimitExceeded)
	}
	// The rejected admission left no run, no response, and no idempotency record: the key is free to
	// retry once the budget is raised.
	assertCount(t, cs.Pool(), 1, `SELECT count(*) FROM runs WHERE project_id=$1`, tenant.Project)
	assertCount(t, cs.Pool(), 0, `SELECT count(*) FROM idempotency_records WHERE idempotency_key='key-b2' AND project_id=$1`, tenant.Project)

	exec(t, cs.Pool(), `UPDATE budgets SET limit_quantity = 1000 WHERE organization_id=$1`, tenant.Organization)
	if adm, err = cs.AdmitResponse(ctx, tenant, second); err != nil || adm.LimitExceeded != nil {
		t.Fatalf("after raising the budget, AdmitResponse = %+v err = %v, want an admit", adm.LimitExceeded, err)
	}
}

// TestAdmissionRejectsWhenTheRollingQuotaIsExhausted is QUO-001: a run-count quota over a rolling window
// refuses the run that would exceed it and reports STABLE remediation — the limit, what was used, and
// when the oldest in-window row releases capacity. The quota is what makes the reservation row earn its
// keep: the count is of runs ADMITTED in the window, not of runs that have settled.
func TestAdmissionRejectsWhenTheRollingQuotaIsExhausted(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	tenant, principalID := seedTenantWithKey(t, cs.Pool(), "tok-quota")
	exec(t, cs.Pool(), `INSERT INTO quotas (id, organization_id, project_id, meter_prefix, limit_quantity, window_seconds)
	     VALUES ($1, $2, $3, 'run.', 1, 3600)`, newID("quo"), tenant.Organization, tenant.Project)

	if adm, err := cs.AdmitResponse(ctx, tenant, admissionInput(principalID, "key-q1", "hash-A", `{"id":"resp_q1"}`)); err != nil || adm.LimitExceeded != nil {
		t.Fatalf("first admission = %+v err = %v, want an admit (the quota allows one run)", adm.LimitExceeded, err)
	}
	adm, err := cs.AdmitResponse(ctx, tenant, admissionInput(principalID, "key-q2", "hash-A", `{"id":"resp_q2"}`))
	if err != nil {
		t.Fatalf("AdmitResponse(second) error = %v", err)
	}
	if adm.LimitExceeded == nil {
		t.Fatal("second admission was accepted; the exhausted run quota did not reject it")
	}
	if adm.LimitExceeded.Kind != "quota" || adm.LimitExceeded.Limit != 1 || adm.LimitExceeded.Used != 1 {
		t.Fatalf("rejection = %+v, want quota/limit 1/used 1", *adm.LimitExceeded)
	}
	if adm.LimitExceeded.ResetAt == nil || !adm.LimitExceeded.ResetAt.After(time.Now()) {
		t.Fatalf("quota rejection reset_at = %v, want a future instant (the window's oldest row aging out)", adm.LimitExceeded.ResetAt)
	}
}
