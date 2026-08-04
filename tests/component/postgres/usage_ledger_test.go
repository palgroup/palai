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
	"github.com/palgroup/palai/packages/coordinator"

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

// TestMeteringPolicyStaysWiderThanTheProjectAcrossReboots proves the deliberate exception 000032 made and
// A.2 Task 6's 000066 carried forward: usage_ledger is secured WIDER than the project even though it
// carries a project_id column, so an installation-wide budget can be summed from the project-narrowed
// connection that admits a run. Narrowing it would make that sum miss every sibling project, and a budget
// that under-counts fails OPEN, silently — which is why the exception exists at all.
//
// The second half of the old claim IS GONE, and it is removed rather than weakened. This test used to end
// by asserting that org-A's connection saw zero of org-B's ledger rows — "the tenant boundary leaked".
// 000066 keys usage_ledger on the INSTALLATION, because keying it on project is exactly what would break
// the first half and there is no organization left to key it on: within one installation every project
// now reads every ledger row. What survives, and is asserted below, is that a connection which declared
// NO scope still sees nothing.
//
// It still migrates TWICE: 000029's catalogue sweep would overwrite the exception with a narrower policy
// on boot #2, and that is the regression this test was written to catch.
func TestMeteringPolicyStaysWiderThanTheProjectAcrossReboots(t *testing.T) {
	cs := openHarness(t)
	ctx := storage.WithSystemScope(context.Background())
	pool := cs.Pool()
	if err := cs.Migrate(context.Background()); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}

	projectA, projectB, projectC := newID("prj"), newID("prj"), newID("prj")
	seedLedgerRow(t, pool, projectA, "model.output_tokens", 40)
	seedLedgerRow(t, pool, projectB, "model.output_tokens", 2)
	seedLedgerRow(t, pool, projectC, "model.output_tokens", 900)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET ROLE palai_app`); err != nil {
		t.Fatalf("SET ROLE palai_app error = %v", err)
	}
	defer func() { _, _ = conn.Exec(ctx, `RESET ROLE`) }()
	// The scope a run's admission publishes: narrowed to project-B. The connection was acquired under
	// the seeding system scope, so palai.system is cleared first — otherwise every policy admits and the
	// assertions below would pass vacuously. palai.org_id is NOT published: A.2 Task 6 removed the GUC
	// along with its last reader, so setting it here would be scenery.
	if _, err := conn.Exec(ctx, `SELECT set_config('palai.system', '', false), set_config('palai.project_id', $1, false)`, projectB); err != nil {
		t.Fatalf("publish scope: %v", err)
	}

	// The whole claim, stated on the project axis now that there is no wider one: a connection narrowed
	// to project-B sums a row belonging to project-A. 40 + 2 = 42, and the 40 is the part that could only
	// arrive by reading past this connection's own project.
	var wideTotal float64
	if err := conn.QueryRow(ctx,
		`SELECT coalesce(sum(quantity), 0) FROM usage_ledger WHERE project_id = ANY($1)`,
		[]string{projectA, projectB}).Scan(&wideTotal); err != nil {
		t.Fatalf("sum across projects A and B: %v", err)
	}
	if wideTotal != 42 {
		t.Fatalf("cross-project ledger total from a project-B-narrowed connection = %v, want 42 (the "+
			"wider-than-project metering policy did not survive the 000029/000066 sweeps)", wideTotal)
	}

	// A third project's rows are VISIBLE too, and that is asserted rather than left unmentioned: a test
	// that simply stopped looking would read as if a boundary were still there.
	var other int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM usage_ledger WHERE project_id = $1`, projectC).Scan(&other); err != nil {
		t.Fatalf("count the third project's ledger: %v", err)
	}
	if other != 1 {
		t.Fatalf("the third project's ledger row count = %d, want 1 — usage_ledger is installation-wide after 000066", other)
	}

	// THE DENY-BY-DEFAULT HALF IS NOT ASSERTED HERE, and the reason is a measured property rather than an
	// oversight: this connection has already published palai.project_id, and a custom GUC cannot return to
	// NULL once set in a session — `set_config(name, NULL, false)` and `RESET` both leave it reading as the
	// empty string. So the world this arm would need is unreachable from this connection, and an arm
	// written anyway would be measuring its own setup. It lives where a never-scoped connection exists:
	// tests/security/tenancy's TestConnectionWithoutTenantContextSeesNoTenantRows (every RLS table) and
	// TestInstallationWideTablesAreVisibleToEveryTenant (these four by name).
}

// seedLedgerRow writes one settled ledger row as the migration owner, creating its project first so the
// ledger's tenant foreign key holds.
func seedLedgerRow(t *testing.T, pool *pgxpool.Pool, project, meter string, quantity int) {
	t.Helper()
	ctx := storage.WithSystemScope(context.Background())
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO projects (id) VALUES ($1) ON CONFLICT DO NOTHING`, []any{project}},
		{`INSERT INTO usage_ledger (id, project_id, meter, quantity, unit, dedupe_key)
		  VALUES ($1, $2, $3, $4, 'token', $1)`, []any{newID("use"), project, meter, quantity}},
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
	exec(t, cs.Pool(), `INSERT INTO budgets (id, project_id, meter_prefix, limit_quantity) VALUES ($1, $2, 'model.', 100)`,
		newID("bdg"), tenant.Project)

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
	exec(t, cs.Pool(), `INSERT INTO usage_ledger (id, project_id, run_id, meter, quantity, unit, dedupe_key)
	     VALUES ($1, $2, $3, 'model.output_tokens', 140, 'token', $1)`,
		newID("use"), tenant.Project, first.RunID)

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

	exec(t, cs.Pool(), `UPDATE budgets SET limit_quantity = 1000 WHERE project_id=$1`, tenant.Project)
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
	exec(t, cs.Pool(), `INSERT INTO quotas (id, project_id, meter_prefix, limit_quantity, window_seconds)
	     VALUES ($1, $2, 'run.', 1, 3600)`, newID("quo"), tenant.Project)

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

// TestExhaustedLimitStillReplaysAnAcceptedRequest is the §20.9 contract a durable limit must not break:
// a limit is a gate on NEW work, never on the client's right to learn what it was already given. The
// sequence is the one a lost response produces — admit under a quota of 1, then retry the SAME key with
// the SAME request — and the retry must replay the stored body, not 429. Rejecting it would strand the
// tenant's one accepted run: the client can never learn its response id, while the run is executing (and
// spending) anyway.
func TestExhaustedLimitStillReplaysAnAcceptedRequest(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	tenant, principalID := seedTenantWithKey(t, cs.Pool(), "tok-replay")
	exec(t, cs.Pool(), `INSERT INTO quotas (id, project_id, meter_prefix, limit_quantity, window_seconds)
	     VALUES ($1, $2, 'run.', 1, 3600)`, newID("quo"), tenant.Project)

	in := admissionInput(principalID, "key-replay", "hash-A", `{"id":"resp_replay"}`)
	first, err := cs.AdmitResponse(ctx, tenant, in)
	if err != nil {
		t.Fatalf("AdmitResponse(first) error = %v", err)
	}
	if first.LimitExceeded != nil {
		t.Fatalf("first admission = %+v, want an admit (the quota allows one run)", first.LimitExceeded)
	}

	// The quota is now exhausted by the run just admitted. The retry is the SAME request.
	replay, err := cs.AdmitResponse(ctx, tenant, in)
	if err != nil {
		t.Fatalf("AdmitResponse(replay) error = %v", err)
	}
	if replay.LimitExceeded != nil {
		t.Fatalf("the retry of an ALREADY-ACCEPTED request was refused by the exhausted limit (%+v); it must replay the stored body — the run exists and is spending, so a 429 strands the client",
			*replay.LimitExceeded)
	}
	if !replay.Replayed || decodeID(t, replay.Body) != "resp_replay" {
		t.Fatalf("retry = %+v, want a replay of the original resp_replay", replay)
	}
	// And the replay settled no second reservation: one accepted request, one metered run.
	assertCount(t, cs.Pool(), 1,
		`SELECT count(*) FROM usage_ledger WHERE run_id=$1 AND meter='run.admitted'`, in.RunID)

	// A DIVERGENT reuse of the same key is still a conflict, not a limit rejection — the limit check
	// must not have displaced the idempotency contract in either direction.
	conflict, err := cs.AdmitResponse(ctx, tenant, admissionInput(principalID, "key-replay", "hash-B", `{"id":"resp_other"}`))
	if err != nil {
		t.Fatalf("AdmitResponse(conflict) error = %v", err)
	}
	if !conflict.Conflict || conflict.LimitExceeded != nil {
		t.Fatalf("divergent reuse = %+v, want a conflict (not a limit rejection)", conflict)
	}
}

// TestInterruptedModelStepIsMetered closes the budget-evasion half of the interrupt path. An interrupt
// aborts the provider call mid-stream, so the provider bills the prompt and the partial completion while
// the control plane never reaches CommitModelResult — the spend exists and the ledger never hears about
// it. Interrupting is USER-TRIGGERABLE, so without a record a tenant could spend past its budget
// indefinitely by interrupting every step, and the gate would never see it.
//
// The provider's token counts are genuinely unavailable at this seam (usage arrives only in the final
// stream chunk, which a canceled stream never reaches), so this does NOT settle tokens — inventing a
// count in a money path would be worse than recording none. It settles the one thing that IS a fact:
// the interrupted step itself, on its own meter and its own unit, so the spend is VISIBLE in the ledger
// and CAPPABLE by a `step.` quota. The row is deterministic on the aborted step's id, so a redelivered
// interrupt records it once.
func TestInterruptedModelStepIsMetered(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	tenant, sessionID, runID := seedRun(t, cs.Pool())
	cmdID := seedQueuedSendMessage(t, cs, tenant, sessionID, runID, "interrupt", "stop and do Y")

	if err := cs.CommitModelRequest(ctx, tenant, sessionID, "", runID, "mr_aborted", "model_step.created.v1", []byte(`{}`)); err != nil {
		t.Fatalf("CommitModelRequest() error = %v", err)
	}
	if _, err := cs.InterruptModelStep(ctx, tenant, sessionID, "", runID, cmdID, "mr_aborted",
		"model_step.interrupted.v1", []byte(`{"output":"partial so far"}`)); err != nil {
		t.Fatalf("InterruptModelStep() error = %v", err)
	}

	assertCount(t, cs.Pool(), 1,
		`SELECT count(*) FROM usage_ledger WHERE run_id=$1 AND meter='step.interrupted' AND unit='step' AND quantity=1`, runID)
	// It must NOT land on a model.* meter: a `model.` budget is denominated in TOKENS, and folding a
	// step count into it would corrupt the very total the gate reads.
	assertCount(t, cs.Pool(), 0,
		`SELECT count(*) FROM usage_ledger WHERE run_id=$1 AND meter LIKE 'model.%'`, runID)
}

// TestBudgetScopeNarrowingAcrossSiblingProjects pins the project narrowing the LIMIT queries perform in
// SQL. Migration 000032 secures the metering tables at the ORGANIZATION level on purpose — that is what
// lets an org-wide budget sum sibling projects from a project-narrowed admission connection — so RLS is
// NOT what keeps one project's spend off another project's budget. These predicates are, and deleting any
// of them mis-charges a tenant rather than failing loudly. A single-project fixture cannot see that, so
// this drives a two-project organization through the real admission gate:
//
//	(a) a SIBLING project's exhausted budget must not bind us     (the WHERE project_id IN ('', $2));
//	(b) our OWN project budget must not count the sibling's spend (the JOIN's project predicate);
//	(c) an ORG-WIDE budget must count it                          (the '' half of both).
func TestBudgetScopeNarrowingAcrossSiblingProjects(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	tenant, principalID := seedTenantWithKey(t, cs.Pool(), "tok-scope")
	sibling := newID("prj")
	exec(t, cs.Pool(), `INSERT INTO projects (id) VALUES ($1)`, sibling)
	// The sibling has spent 140 tokens; our project has spent none.
	exec(t, cs.Pool(), `INSERT INTO usage_ledger (id, project_id, meter, quantity, unit, dedupe_key)
	     VALUES ($1, $2, 'model.output_tokens', 140, 'token', $1)`, newID("use"), sibling)

	// Every budget below opens its period BEFORE that spend. Without this the spend would sit outside the
	// period a just-created budget starts (correct behaviour — a new budget does not retroactively charge
	// history) and all three assertions would pass vacuously, pinning nothing.
	setBudget := func(project string, limit int) {
		t.Helper()
		exec(t, cs.Pool(), `INSERT INTO budgets (id, project_id, meter_prefix, limit_quantity, period_start)
		     VALUES ($1, $2, 'model.', $3, now() - interval '1 hour')`,
			newID("bdg"), project, limit)
	}

	admit := func(t *testing.T, key string) *coordinator.LimitExceeded {
		t.Helper()
		adm, err := cs.AdmitResponse(ctx, tenant, admissionInput(principalID, key, "hash-A", `{"id":"resp_`+key+`"}`))
		if err != nil {
			t.Fatalf("AdmitResponse(%s) error = %v", key, err)
		}
		return adm.LimitExceeded
	}

	// (a) The sibling's own budget is exhausted (limit 1 against its 140). It must not bind our project.
	setBudget(sibling, 1)
	if limit := admit(t, "scope-a"); limit != nil {
		t.Fatalf("a SIBLING project's exhausted budget refused our admission (%+v); the limit query is not narrowed to the caller's project", *limit)
	}

	// (b) Our own budget of 100 has full headroom — the sibling's 140 belongs to the sibling.
	setBudget(tenant.Project, 100)
	if limit := admit(t, "scope-b"); limit != nil {
		t.Fatalf("our project's budget was charged the SIBLING's spend (%+v); the usage join is not narrowed to the budget's project", *limit)
	}

	// (c) An installation-wide budget ('' project) DOES sum every project, so the sibling's 140 is inside
	// the total — the deliberate consequence of the wider-than-project metering policy, and the reason it
	// exists.
	//
	// THE EXPECTED TOTAL IS MEASURED, NOT WRITTEN DOWN, and A.2 Task 6 is why. This used to assert
	// `Used == 140` exactly, which held while usage_ledger was secured per ORGANIZATION and the fixture
	// owned its own. 000066 keys it on the INSTALLATION, and this suite shares ONE database across dozens
	// of openHarness calls, so an installation-wide budget legitimately sums every other test's model.*
	// rows too (1264 when this was first re-run, and a number that moves whenever a test is added). The
	// claim being pinned is the SIBLING's contribution, so the baseline is read first and the assertion is
	// the difference — which fails just as loudly if the join stops crossing projects.
	var baseline float64
	if err := cs.Pool().QueryRow(storage.WithSystemScope(ctx),
		`SELECT coalesce(sum(quantity), 0) FROM usage_ledger WHERE meter LIKE 'model.%'`).Scan(&baseline); err != nil {
		t.Fatalf("read the installation-wide model.* baseline: %v", err)
	}
	// Clear only the two budgets THIS test set, so the installation-wide one below is the only limit in
	// play. It is keyed by project rather than by the tenant's organization (gone with Task 6), and it
	// deliberately does NOT delete the '' installation-wide rows: those are shared with every other test
	// against this database, and a blanket DELETE here would reach into them.
	exec(t, cs.Pool(), `DELETE FROM budgets WHERE project_id = ANY($1)`, []string{tenant.Project, sibling})
	setBudget("", 100)
	limit := admit(t, "scope-c")
	if limit == nil {
		t.Fatal("an installation-wide budget did not see the sibling project's spend; the wide limit under-counts (it fails OPEN)")
	}
	if limit.Used != baseline || limit.Limit != 100 {
		t.Fatalf("installation-wide rejection = %+v, want used %v of 100 (every project's model.* spend, the sibling's 140 among them)", *limit, baseline)
	}
	if baseline < 140 {
		t.Fatalf("the baseline is %v, below the sibling's own 140 — the sum is not crossing projects at all", baseline)
	}
}

// TestTwoModelCallsInOneRunAreAttributedToTheirOwnSteps is the attribution contract, and it is written
// as two calls in ONE run because that is the only shape that can tell the two possible worlds apart.
//
// THE MEASUREMENT THAT PROVOKED IT SAID "one ledger row per run":
//
//	SELECT meter, count(*), count(DISTINCT run_id) FROM usage_ledger GROUP BY meter;   (live, 2026-07-31)
//	  -> model.input_tokens 61 rows / 61 runs
//
// which reads as an aggregate that has thrown per-step detail away. It is not. The same numbers appear
// when settlement is per-step and every run happens to make exactly one model call, and that is this
// deployment: 61 model_requests across 61 runs, calls_per_run = 1 for every one of them. A run with a
// SECOND call is the discriminator, and no run on that stack had ever made one.
//
// So this test asserts what the reported defect claimed was impossible: two calls, two attributed rows
// per meter, quantities that differ (so nothing is silently summing them), each naming its OWN step —
// and the run-level total still equal to their sum, because a per-step ledger that double-counts is
// worse than the aggregate it was thought to be.
func TestTwoModelCallsInOneRunAreAttributedToTheirOwnSteps(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	tenant, sessionID, runID := seedRun(t, cs.Pool())

	// Deliberately different quantities: equal ones would let a bug that attributes both rows to one
	// step still produce a plausible-looking total.
	first := contracts.Usage{InputTokens: 30, OutputTokens: 12, TotalTokens: 42}
	second := contracts.Usage{InputTokens: 7, OutputTokens: 100, TotalTokens: 107}
	for _, c := range []struct {
		requestID string
		usage     contracts.Usage
	}{{"mr_turn1", first}, {"mr_turn2", second}} {
		if _, err := cs.CommitModelResult(ctx, tenant, sessionID, "", runID, c.requestID,
			[]byte(`{"output":"ok"}`), "model_step.completed.v1", []byte(`{}`), c.usage); err != nil {
			t.Fatalf("CommitModelResult(%s) error = %v", c.requestID, err)
		}
	}

	// Four rows: two meters x two steps. Three would mean a step was folded into another.
	assertCount(t, cs.Pool(), 4,
		`SELECT count(*) FROM usage_ledger WHERE run_id=$1 AND meter LIKE 'model.%'`, runID)

	// Each row names the step it came from, in a COLUMN — not inside dedupe_key, which is an idempotency
	// detail whose format nothing constrains and which a reader would have to string-parse.
	for _, c := range []struct {
		requestID string
		meter     string
		want      float64
	}{
		{"mr_turn1", "model.input_tokens", 30},
		{"mr_turn1", "model.output_tokens", 12},
		{"mr_turn2", "model.input_tokens", 7},
		{"mr_turn2", "model.output_tokens", 100},
	} {
		var got float64
		if err := cs.Pool().QueryRow(storage.WithSystemScope(ctx),
			`SELECT quantity FROM usage_ledger WHERE run_id=$1 AND model_request_id=$2 AND meter=$3`,
			runID, c.requestID, c.meter).Scan(&got); err != nil {
			t.Fatalf("read %s/%s: %v", c.requestID, c.meter, err)
		}
		if got != c.want {
			t.Fatalf("%s %s = %v, want %v", c.requestID, c.meter, got, c.want)
		}
	}

	// The run-level total is unchanged by being attributed: it is still the sum of its steps.
	var total float64
	if err := cs.Pool().QueryRow(storage.WithSystemScope(ctx),
		`SELECT coalesce(sum(quantity), 0) FROM usage_ledger WHERE run_id=$1 AND meter LIKE 'model.%'`, runID).Scan(&total); err != nil {
		t.Fatalf("sum settled model usage: %v", err)
	}
	if want := float64(first.TotalTokens + second.TotalTokens); total != want {
		t.Fatalf("run total = %v, want %v (the sum of the two steps — a per-step ledger must not double-count)", total, want)
	}

	// Redelivering ONE of the two steps settles nothing new: the identity is still per (step, meter), so
	// attribution and idempotency are the same fact rather than two that can disagree.
	if _, err := cs.CommitModelResult(ctx, tenant, sessionID, "", runID, "mr_turn2",
		[]byte(`{"output":"ok"}`), "model_step.completed.v1", []byte(`{}`), second); err != nil {
		t.Fatalf("CommitModelResult(redeliver turn2) error = %v", err)
	}
	assertCount(t, cs.Pool(), 4,
		`SELECT count(*) FROM usage_ledger WHERE run_id=$1 AND meter LIKE 'model.%'`, runID)
}

// TestNullModelRequestIdMeansNotAModelStep pins the meaning of the NULL, which is the half of a
// nullable column that usually goes unwritten and later reads as "unknown". Here it is not unknown: a
// run.admitted row is the ADMISSION reservation, settled before the run executes, and there is no model
// call for it to belong to. The invariant is one sentence — every `model.` and `step.` row carries a
// step, every `run.` row does not — and it is asserted here rather than as a CHECK constraint because
// this INSERT rides inside the transaction that commits a model result: a metering rule that can abort
// a completed step is a worse failure than an unattributed row.
func TestNullModelRequestIdMeansNotAModelStep(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	tenant, sessionID, runID := seedRun(t, cs.Pool())
	if _, err := cs.CommitModelResult(ctx, tenant, sessionID, "", runID, "mr_step1",
		[]byte(`{"output":"ok"}`), "model_step.completed.v1", []byte(`{}`),
		contracts.Usage{InputTokens: 3, OutputTokens: 4, TotalTokens: 7}); err != nil {
		t.Fatalf("CommitModelResult error = %v", err)
	}

	// Not one model-step row may be unattributed...
	assertCount(t, cs.Pool(), 0,
		`SELECT count(*) FROM usage_ledger WHERE run_id=$1 AND (meter LIKE 'model.%' OR meter LIKE 'step.%') AND model_request_id IS NULL`, runID)
	// ...and not one run-level row may claim a step it never had. seedRun's admission wrote that row.
	assertCount(t, cs.Pool(), 0,
		`SELECT count(*) FROM usage_ledger WHERE run_id=$1 AND meter LIKE 'run.%' AND model_request_id IS NOT NULL`, runID)
}

// TestCacheTokensSettleAsTheirOwnMeters closes the last step of a number that has been arriving from
// both provider families all along: the adapters decode it, model_dispatch.addUsage sums it across
// steps, and until now NOTHING settled it — a cache read was a value the platform carried and never
// recorded.
//
// The test is written as THREE steps in one run because the two provider families report cache tokens
// in shapes that are not merely different numbers, they are different RELATIONS to input_tokens, and a
// single-shape test cannot tell a raw settlement from a renormalized one:
//
//	provider-one (OpenAI)      prompt_tokens INCLUDES cached_tokens
//	                           -> CacheReadTokens is a SUBSET of InputTokens, and there is no write
//	                              counter at all (adapters/models/provider_one/adapter.go:141-147)
//	provider-two (Anthropic)   input_tokens EXCLUDES both cache counters
//	                           -> CacheReadTokens/CacheWriteTokens are ADDITIVE to InputTokens
//	                              (adapters/models/provider_two/adapter.go:243-251)
//
// The settled quantity is the provider's OWN number in both cases, and the assertions on
// model.input_tokens below are the load-bearing half of that: they fail if settlement ever tries to
// "helpfully" normalize the two families onto one invariant, which it must not do, because
// model.input_tokens is what the durable budget gate reads and re-basing it would silently move every
// limit already configured against it.
//
// The third step is the one that decides the shape of the whole feature: a step that cached nothing
// writes NO cache row rather than a zero row. settleUsage already skips quantity <= 0, so this is the
// existing rule rather than a new one — asserted here because a dashboard DIVIDES by these, and a zero
// row would make a cache-hit rate look measured on a provider that never reported one.
func TestCacheTokensSettleAsTheirOwnMeters(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	tenant, sessionID, runID := seedRun(t, cs.Pool())

	// Deliberately different quantities per meter, and a cache read EQUAL across the two families:
	// equal-per-family numbers would let a bug that settles one family's shape for both still look
	// right, and it is precisely the surrounding input_tokens that tells them apart.
	disjoint := contracts.Usage{InputTokens: 200, OutputTokens: 12, TotalTokens: 212, CacheReadTokens: 800, CacheWriteTokens: 500}
	subset := contracts.Usage{InputTokens: 1000, OutputTokens: 30, TotalTokens: 1030, CacheReadTokens: 800}
	uncached := contracts.Usage{InputTokens: 40, OutputTokens: 9, TotalTokens: 49}
	for _, c := range []struct {
		requestID string
		usage     contracts.Usage
	}{{"mr_disjoint", disjoint}, {"mr_subset", subset}, {"mr_uncached", uncached}} {
		if _, err := cs.CommitModelResult(ctx, tenant, sessionID, "", runID, c.requestID,
			[]byte(`{"output":"ok"}`), "model_step.completed.v1", []byte(`{}`), c.usage); err != nil {
			t.Fatalf("CommitModelResult(%s) error = %v", c.requestID, err)
		}
	}

	for _, c := range []struct {
		requestID string
		meter     string
		want      float64
	}{
		// The additive family: all four meters, and input_tokens is the provider's 200 — NOT 1500.
		{"mr_disjoint", "model.cache_read_tokens", 800},
		{"mr_disjoint", "model.cache_write_tokens", 500},
		{"mr_disjoint", "model.input_tokens", 200},
		// The subset family: the cache read is settled at its full 800 even though those same 800
		// tokens are already inside the 1000 on the line above it. Both numbers are the provider's.
		{"mr_subset", "model.cache_read_tokens", 800},
		{"mr_subset", "model.input_tokens", 1000},
	} {
		var got float64
		var unit string
		if err := cs.Pool().QueryRow(storage.WithSystemScope(ctx),
			`SELECT quantity, unit FROM usage_ledger WHERE run_id=$1 AND model_request_id=$2 AND meter=$3`,
			runID, c.requestID, c.meter).Scan(&got, &unit); err != nil {
			t.Fatalf("read %s/%s: %v", c.requestID, c.meter, err)
		}
		if got != c.want {
			t.Fatalf("%s %s = %v, want %v", c.requestID, c.meter, got, c.want)
		}
		if unit != "token" {
			t.Fatalf("%s %s unit = %q, want %q", c.requestID, c.meter, unit, "token")
		}
	}

	// A provider that reports no write counter leaves NO write row — not a zero one. This is the
	// OpenAI family's permanent state, and it is why an ABSENT meter cannot be read as "nothing was
	// cached": here it means "this provider does not report the counter at all".
	assertCount(t, cs.Pool(), 0,
		`SELECT count(*) FROM usage_ledger WHERE run_id=$1 AND model_request_id='mr_subset' AND meter='model.cache_write_tokens'`, runID)
	// A step that cached nothing leaves NEITHER row.
	assertCount(t, cs.Pool(), 0,
		`SELECT count(*) FROM usage_ledger WHERE run_id=$1 AND model_request_id='mr_uncached' AND meter LIKE 'model.cache_%'`, runID)

	// Three cache rows across the run, and no more: 2 (disjoint) + 1 (subset) + 0 (uncached). A fourth
	// would mean a zero row was written somewhere the count above did not look.
	assertCount(t, cs.Pool(), 3,
		`SELECT count(*) FROM usage_ledger WHERE run_id=$1 AND meter LIKE 'model.cache_%'`, runID)

	// The cache meters settle under the same per-(step, meter) identity as the token meters, so a
	// redelivered step settles nothing new (BIL-001).
	if _, err := cs.CommitModelResult(ctx, tenant, sessionID, "", runID, "mr_disjoint",
		[]byte(`{"output":"ok"}`), "model_step.completed.v1", []byte(`{}`), disjoint); err != nil {
		t.Fatalf("CommitModelResult(redeliver disjoint) error = %v", err)
	}
	assertCount(t, cs.Pool(), 3,
		`SELECT count(*) FROM usage_ledger WHERE run_id=$1 AND meter LIKE 'model.cache_%'`, runID)

	// 000050's invariant covers the new meters too: they are `model.` rows, so every one names the step
	// it came from. This is the same sentence TestNullModelRequestIdMeansNotAModelStep pins, re-asserted
	// over rows that test's usage never produces.
	assertCount(t, cs.Pool(), 0,
		`SELECT count(*) FROM usage_ledger WHERE run_id=$1 AND meter LIKE 'model.cache_%' AND model_request_id IS NULL`, runID)
}
