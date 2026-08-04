//go:build component

package coordinator

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/storage"
)

// E29 T7 — WHICH limit a 429 names, when two of them are exhausted at once.
//
// The schema's own scope model GUARANTEES the tie. budgets/quotas are UNIQUE on
// (organization_id, project_id, meter_prefix) (migration 000032), so an ORGANIZATION-wide row
// (project_id = '') and a PROJECT row on the SAME meter_prefix are both legal and both live at once;
// ExhaustedBudget's `WHERE b.project_id IN ('', $2)` takes BOTH, the HAVING exhausts BOTH, and the
// ordering key — meter_prefix — is EQUAL for the two. `LIMIT 1` then picks one, and until this task
// nothing in the query said which.
//
// WHAT ACTUALLY DECIDED IT, MEASURED RATHER THAN ASSUMED (2026-08-01, postgres 16.14, the digest
// scripts/test/component:25 pins). Against a real Postgres, EXPLAIN (COSTS OFF) on the pre-fix query:
//
//	Limit -> Sort (Sort Key: b.meter_prefix)
//	        -> GroupAggregate (Group Key: b.id)
//	              -> Sort (Sort Key: b.id)
//
// The bounded top-1 sort keeps the FIRST row it sees among equal keys, and the row it sees first is
// whichever the GroupAggregate emitted first — that is, the one with the lower b.id. So the answer was
// decided by an id comparison, and budget ids come from middleware.NewID (crypto/rand,
// apps/control-plane/api/middleware/request_context.go:40-44): a coin flip, fixed at creation time,
// never revisited. Which limit an operator's 429 names was therefore a property of the sixteen random
// bytes minted when they POSTed the budget. That is not a plan the tree may depend on either: at another
// table size the same query plans a HashAggregate, and then the emission order is hash order — which is
// why the two tests below vary BOTH knobs (insert order and id order) and require ONE answer from all
// four arrangements.
//
// THE ANSWER THIS TASK SHIPS: the NARROWEST limit — the project row. A 429's remediation body must name
// the limit the CALLER CAN ACT ON; an organization budget is not something a project operator can raise,
// so naming it hands back an answer that cannot be turned into an action. See storage/queries/usage.sql.
//
// HONEST CEILING, and it is the difference between two very different sentences: this fixes a REPORTING
// ambiguity, not an enforcement hole. Both rows were enforced before this change and are enforced after
// — the HAVING catches both and either one produces the 429. What changes is which one is NAMED.

// limitArrangement is one legal production state of the same two budget rows: which was written first,
// and which drew the lower id. Both are things production varies on its own — the insert order is the
// operator's, and the id order is crypto/rand's — so all four are states a real installation reaches.
type limitArrangement struct {
	name string
	// orgWrittenFirst inserts the organization-wide row before the project row (heap order).
	orgWrittenFirst bool
	// orgLowerID gives the organization-wide row the lexically smaller primary key.
	orgLowerID bool
}

func limitArrangements() []limitArrangement {
	return []limitArrangement{
		{name: "org written first, org holds the lower id", orgWrittenFirst: true, orgLowerID: true},
		{name: "org written first, project holds the lower id", orgWrittenFirst: true, orgLowerID: false},
		{name: "project written first, org holds the lower id", orgWrittenFirst: false, orgLowerID: true},
		{name: "project written first, project holds the lower id", orgWrittenFirst: false, orgLowerID: false},
	}
}

// The two limits are given DIFFERENT quantities on purpose. LimitExceeded carries no scope field, so the
// only way an operator (or this test) can tell which row answered is the pair of numbers the 429's
// detail line renders — `(<used> of <limit> used)`, apps/control-plane/api/responses.go:142-150. A
// sibling project's spend is seeded so the two rows differ in USED as well as in LIMIT: the org row sums
// every project of the organization, the project row sums only its own.
const (
	narrowLimit = 100 // the project budget/quota: the one a project operator can raise
	narrowUsed  = 150 // this project's own settled spend
	wideLimit   = 600 // the organization budget/quota: not this caller's to change
	wideUsed    = 650 // narrowUsed + the sibling project's spend
	siblingUsed = 500
)

// TestExhaustedBudgetNamesTheNarrowestLimitWhicheverRowWasWrittenFirst drives the SHIPPED admission gate
// — checkDurableLimits, the same function AdmitResponse calls at store.go:845 — against a real Postgres
// holding an organization budget and a project budget on one meter_prefix, both exhausted. All four
// legal arrangements of those two rows must produce the SAME answer, and it must be the project's.
func TestExhaustedBudgetNamesTheNarrowestLimitWhicheverRowWasWrittenFirst(t *testing.T) {
	cs, _ := openSessionListStore(t)
	answers := map[string]string{}
	for _, arrangement := range limitArrangements() {
		project := seedLimitTenant(t, cs)
		writeLimitPair(t, cs, arrangement, func(id, projectID string, limit int) {
			mustExecPin(t, cs, `INSERT INTO budgets (id, project_id, meter_prefix, limit_quantity, period_start)
				VALUES ($1,$2,'model.',$3,$4)`, id, projectID, limit, time.Now().UTC().Add(-time.Hour))
		}, project)
		seedLimitSpend(t, cs, project)

		// Repeated on purpose: an ambiguity that resolves one way per plan-cache generation is still an
		// ambiguity, and a single read cannot tell a stable answer from a lucky one.
		for attempt := 0; attempt < 8; attempt++ {
			got := readDurableLimit(t, cs, project)
			if got == nil {
				t.Fatalf("%s: attempt %d reported no exhausted limit; both rows are over their limit (%d of %d, %d of %d)",
					arrangement.name, attempt, narrowUsed, narrowLimit, wideUsed, wideLimit)
			}
			if got.Kind != "budget" {
				t.Fatalf("%s: attempt %d reported kind %q, want budget", arrangement.name, attempt, got.Kind)
			}
			answers[arrangement.name] = describeLimit(*got)
		}
	}
	assertOneAnswer(t, "budget", answers)
}

// TestExhaustedQuotaNamesTheNarrowestLimitWhicheverRowWasWrittenFirst is the same proof for the rolling
// window. It is a separate query with a separate ORDER BY (storage/queries/usage.sql, ExhaustedQuota),
// so a fix applied to one and not the other leaves half the defect shipped — which is exactly the shape
// this file exists to refuse. No budget rows are seeded: checkDurableLimits reads the budget first and
// returns on it, so the quota query is only reached when no budget is exhausted.
func TestExhaustedQuotaNamesTheNarrowestLimitWhicheverRowWasWrittenFirst(t *testing.T) {
	cs, _ := openSessionListStore(t)
	answers := map[string]string{}
	for _, arrangement := range limitArrangements() {
		project := seedLimitTenant(t, cs)
		writeLimitPair(t, cs, arrangement, func(id, projectID string, limit int) {
			mustExecPin(t, cs, `INSERT INTO quotas (id, project_id, meter_prefix, limit_quantity, window_seconds)
				VALUES ($1,$2,'model.',$3,3600)`, id, projectID, limit)
		}, project)
		seedLimitSpend(t, cs, project)

		for attempt := 0; attempt < 8; attempt++ {
			got := readDurableLimit(t, cs, project)
			if got == nil {
				t.Fatalf("%s: attempt %d reported no exhausted limit; both rows are over their limit (%d of %d, %d of %d)",
					arrangement.name, attempt, narrowUsed, narrowLimit, wideUsed, wideLimit)
			}
			if got.Kind != "quota" {
				t.Fatalf("%s: attempt %d reported kind %q, want quota", arrangement.name, attempt, got.Kind)
			}
			if got.ResetAt == nil {
				t.Fatalf("%s: attempt %d reported an exhausted quota with no reset moment", arrangement.name, attempt)
			}
			answers[arrangement.name] = describeLimit(*got)
		}
	}
	assertOneAnswer(t, "quota", answers)
}

// assertOneAnswer is where the defect becomes visible: it prints every arrangement's answer, so a red
// run states the CONFLICT (two legal states, two different limits named) rather than only a mismatch.
func assertOneAnswer(t *testing.T, kind string, answers map[string]string) {
	t.Helper()
	want := describeLimit(LimitExceeded{Kind: kind, MeterPrefix: "model.", Limit: narrowLimit, Used: narrowUsed})
	distinct := map[string]bool{}
	names := make([]string, 0, len(answers))
	for name, answer := range answers {
		distinct[answer] = true
		names = append(names, name)
	}
	sort.Strings(names)
	if len(distinct) == 1 && answers[names[0]] == want {
		return
	}
	report := ""
	for _, name := range names {
		report += fmt.Sprintf("\n  %-46s -> %s", name, answers[name])
	}
	t.Fatalf("the exhausted %s reported %d DISTINCT limits across four legal arrangements of the same two rows; "+
		"want all four to name the narrowest (%s), which is the only one a project operator can raise:%s",
		kind, len(distinct), want, report)
}

func describeLimit(l LimitExceeded) string {
	return fmt.Sprintf("%s meter_prefix=%q limit=%g used=%g", l.Kind, l.MeterPrefix, l.Limit, l.Used)
}

// readDurableLimit drives the production gate exactly as AdmitResponse does: the tenant-scoped context,
// a ReadCommitted transaction, and checkDurableLimits itself. Nothing here re-implements the query.
func readDurableLimit(t *testing.T, cs *Store, project string) *LimitExceeded {
	t.Helper()
	tenant := Tenant{Project: project}
	ctx := storage.ScopeToTenant(context.Background(), tenant.Project)
	tx, err := cs.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	limit, err := checkDurableLimits(ctx, tx, tenant)
	if err != nil {
		t.Fatalf("checkDurableLimits: %v", err)
	}
	return limit
}

// seedLimitTenant gives each arrangement its own organization, so the four are independent states rather
// than four readings of one table, and a sibling project whose spend the organization-wide row sums.
func seedLimitTenant(t *testing.T, cs *Store) (project string) {
	t.Helper()
	project = pinTestID("prj")
	mustExecPin(t, cs, `INSERT INTO projects (id) VALUES ($1)`, project)
	return project
}

// writeLimitPair inserts the organization-wide row and the project row in the arrangement's order, with
// the arrangement's id ordering. The ids are shaped rank-first so the comparison is deterministic here;
// production's are 16 random bytes, which is exactly why both relative orders occur in the field.
func writeLimitPair(t *testing.T, cs *Store, a limitArrangement, insert func(id, projectID string, limit int), project string) {
	t.Helper()
	rankedID := func(rank int) string { return fmt.Sprintf("bdg_%d_%s", rank, pinTestID("r")) }
	orgRank, projectRank := 1, 0
	if a.orgLowerID {
		orgRank, projectRank = 0, 1
	}
	writeOrg := func() { insert(rankedID(orgRank), "", wideLimit) }
	writeProject := func() { insert(rankedID(projectRank), project, narrowLimit) }
	if a.orgWrittenFirst {
		writeOrg()
		writeProject()
		return
	}
	writeProject()
	writeOrg()
}

// seedLimitSpend exhausts BOTH rows and makes them report different numbers: this project spends
// narrowUsed, a sibling project spends siblingUsed, and only the organization-wide row sums the two.
func seedLimitSpend(t *testing.T, cs *Store, project string) {
	t.Helper()
	sibling := pinTestID("prj")
	mustExecPin(t, cs, `INSERT INTO projects (id) VALUES ($1)`, sibling)
	insertSpend := func(projectID string, quantity int) {
		mustExecPin(t, cs, `INSERT INTO usage_ledger (id, project_id, meter, quantity, unit, dedupe_key)
			VALUES ($1,$2,'model.input_tokens',$3,'token',$4)`,
			pinTestID("use"), projectID, quantity, pinTestID("dk"))
	}
	insertSpend(project, narrowUsed)
	insertSpend(sibling, siblingUsed)
}
