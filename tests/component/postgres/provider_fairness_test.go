//go:build component

// A.6 Task 2 — a SHARED provider key is a shared resource, and a refusal must say WHICH pool it drained.
//
// In cloud the model keys are ours (spec §4.2), so provider capacity is pooled across every project and
// one project's blowout must not hand every other project a 429 it did not cause. The mechanism for that
// is not new: `quotas` has carried an installation-wide scope since E13 Task 6 (a row with project_id='',
// which the JOIN below sums across every project) and `checkDurableLimits` has enforced it since. What was
// missing is measured rather than assumed, and it is two things:
//
//  1. NOTHING PROVED THE ROLLING WINDOW. tests/component/postgres/usage_ledger_test.go:399
//     (TestBudgetScopeNarrowingAcrossSiblingProjects) drives the real AdmitResponse through all three
//     scope legs — a sibling's exhausted limit must not bind us, our own limit must not be charged the
//     sibling's spend, an installation-wide limit must sum both — but it drives them against BUDGETS only.
//     ExhaustedQuota is a SEPARATE statement with its own WHERE and its own JOIN, and this tree's standing
//     rule about the pair is that a property proven on one of them and not the other leaves half of it
//     shipped. Cross-project fairness in the rolling window had no proof at all.
//
//  2. THE REFUSAL DID NOT NAME ITS SCOPE, and the tree had already written that down without reading it
//     as a defect: packages/coordinator/usage_component_test.go said "LimitExceeded carries no scope
//     field, so the only way an operator (or this test) can tell which row answered is the pair of
//     numbers" — and then used the numbers, because that was all there was. The 429 read `the quota for
//     meters starting with "model." is exhausted (110 of 100 used)`, which a project operator cannot act
//     on: raising THEIR quota fixes it in one case and does nothing in the other, and the body never said
//     which. That undercut E29 T7's own justification for the narrowest-first ordering — it picks the row
//     a caller can act on, then declined to say what that row is. The field exists now, so that comment
//     has been corrected where it stands rather than left as a true-sounding sentence about a gone gap.
//
// WHAT IS NOT CLAIMED HERE. This gate reads SETTLED usage and closes at ADMISSION, so the burst inside a
// single already-admitted run is not caught by it — that needs a concurrency limit and is separate work.
// The honest ceiling is written down in docs/operations/known-gaps-1.0.md rather than left for a reader to
// infer from a green test.
//
// EVERY TEST HERE MINTS ITS OWN METER VOCABULARY, and that is not hygiene — it is what makes the
// installation-wide row usable in a shared database at all. A project_id='' row means EVERY project by
// definition, so a wide row seeded on 'model.' would bind every other test against this database exactly
// as it binds a real deployment's every project. A prefix unique to one test run is matched by no other
// test's meters, and the wide row is dropped again in Cleanup so nothing inherits an exhausted pool.
package postgres

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/packages/coordinator"
)

// fairnessMeters mints a meter vocabulary no other test can settle into: a prefix unique to this run and
// the one meter beneath it. Since A.6 Task 1 the prefix is compared with left(), so it is a LITERAL
// prefix — there is no pattern here for the random hex to accidentally spell.
func fairnessMeters(t *testing.T) (prefix, meter string) {
	t.Helper()
	prefix = newID("fair") + "."
	return prefix, prefix + "input_tokens"
}

// seedQuota writes one rolling-window limit. An empty project is the INSTALLATION-WIDE scope (it sums
// every project's spend); a concrete project narrows both the limit and the sum to that project.
//
// The name says quota deliberately: this file's subject is the rolling window, the half of the pair that
// had no cross-project proof.
func seedQuota(t *testing.T, pool *pgxpool.Pool, project, prefix string, limit int) string {
	t.Helper()
	id := newID("qta")
	exec(t, pool, `INSERT INTO quotas (id, project_id, meter_prefix, limit_quantity, window_seconds)
	     VALUES ($1, $2, $3, $4, 3600)`, id, project, prefix, limit)
	return id
}

// dropInstallationWideQuota removes a project_id='' row again. Callers run it from Cleanup because a
// t.Fatal must not leave an exhausted pool bound to every project in this shared database — including
// the projects of every test that runs after this one.
func dropInstallationWideQuota(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	exec(t, pool, `DELETE FROM quotas WHERE id = $1 AND project_id = ''`, id)
}

func seedSpend(t *testing.T, pool *pgxpool.Pool, project, meter string, quantity int) {
	t.Helper()
	id := newID("use")
	exec(t, pool, `INSERT INTO usage_ledger (id, project_id, meter, quantity, unit, dedupe_key)
	     VALUES ($1, $2, $3, $4, 'token', $1)`, id, project, meter, quantity)
}

// admitFairness drives the SHIPPED admission path — coordinator.AdmitResponse, the same call POST
// /v1/responses makes — and returns the durable limit that refused it, or nil when the request was
// admitted. Nothing here re-implements the gate or reaches past it into checkDurableLimits.
func admitFairness(t *testing.T, cs *coordinator.Store, tenant coordinator.Tenant, principal, key string) *coordinator.LimitExceeded {
	t.Helper()
	adm, err := cs.AdmitResponse(context.Background(), tenant,
		admissionInput(principal, key, "hash-fair", `{"id":"resp_`+key+`"}`))
	if err != nil {
		t.Fatalf("AdmitResponse(%s) error = %v", key, err)
	}
	return adm.LimitExceeded
}

// requireQuota fails with the ONE explanation that is not this test's subject, because it is the
// explanation a reader would otherwise chase into the wrong file. checkDurableLimits reads budgets FIRST
// and returns on the first exhausted one, so an installation-wide BUDGET left behind by any other test
// against this shared database answers here instead — a refusal that is real, that names a limit, and
// that has nothing to do with the quota under test.
func requireQuota(t *testing.T, limit *coordinator.LimitExceeded, what string) coordinator.LimitExceeded {
	t.Helper()
	if limit == nil {
		t.Fatalf("%s: the admission was ADMITTED; want a refusal", what)
	}
	if limit.Kind != "quota" {
		t.Fatalf("%s: the refusal is a %q (%+v), not a quota — checkDurableLimits returns on an exhausted "+
			"BUDGET before it reads any quota, so an installation-wide (project_id='') budget left behind "+
			"by another test binds this project too and this assertion never reached its own subject",
			what, limit.Kind, *limit)
	}
	return *limit
}

// TestInstallationWideQuotaSumsEveryProjectAndNamesItsScope is the pooled-capacity proof. The pool is
// drained by two OTHER projects, and the project that pays for it spent nothing at all — which is the
// whole point of a shared provider key, and the reason the refusal has to say so.
func TestInstallationWideQuotaSumsEveryProjectAndNamesItsScope(t *testing.T) {
	cs := openHarness(t)
	pool := cs.Pool()
	prefix, meter := fairnessMeters(t)

	// The pool: 100 units across the whole installation.
	wideID := seedQuota(t, pool, "", prefix, 100)
	t.Cleanup(func() { dropInstallationWideQuota(t, pool, wideID) })

	// Two projects drain it between them. Neither is over any limit of its OWN — neither has one.
	spender, _ := seedTenantWithKey(t, pool, "tok-fair-a")
	sibling, _ := seedTenantWithKey(t, pool, "tok-fair-b")
	seedSpend(t, pool, spender.Project, meter, 60)
	seedSpend(t, pool, sibling.Project, meter, 50)

	// A THIRD project, which has spent NOTHING, now asks to run. The pool is at 110 of 100, so it is
	// refused — the honest consequence of sharing one provider key, and the case a per-project limit
	// cannot express at all.
	quiet, quietPrincipal := seedTenantWithKey(t, pool, "tok-fair-c")
	got := requireQuota(t, admitFairness(t, cs, quiet, quietPrincipal, "fair-wide-1"),
		"a project that spent nothing, against a drained installation pool")

	if !got.InstallationWide {
		t.Fatalf("the refusal reports scope project (%+v), but this project has no quota of its own and "+
			"spent nothing — the only row that can refuse it is the installation-wide pool. A caller told "+
			"'project' here raises its own quota and is refused again", got)
	}
	if got.MeterPrefix != prefix {
		t.Fatalf("the refusal names meter prefix %q, want %q — it does not say which dimension is exhausted",
			got.MeterPrefix, prefix)
	}
	// 110 is the SUM of two other projects' spend, and this caller's own is zero. A wide row that reported
	// only the caller's usage would read `0 of 100` and refuse anyway — a refusal whose numbers cannot be
	// true together, which is worse than an unexplained one.
	if got.Used != 110 || got.Limit != 100 {
		t.Fatalf("the refusal reports %v of %v, want 110 of 100 (60 + 50, settled by two OTHER projects) — "+
			"the installation-wide row is not summing across projects", got.Used, got.Limit)
	}
	if got.ResetAt == nil {
		t.Fatal("a rolling-window refusal carries no reset moment; the caller cannot tell when to retry")
	}
}

// TestOneProjectsExhaustedQuotaDoesNotCloseAnotherProject is THE proof this task exists for, and its last
// assertion is the one that cannot be satisfied by an implementation that simply refuses everything.
//
// Both scopes are configured at once, which is the arrangement fairness actually needs: the pool caps the
// installation, and a project limit under it stops any single project from taking the whole pool. The
// effective ceiling is the smaller REMAINING one (§43.7) and both are evaluated — so the project that
// exhausted its own share is refused while the pool still has headroom, and its sibling, which is under
// both, is ADMITTED.
func TestOneProjectsExhaustedQuotaDoesNotCloseAnotherProject(t *testing.T) {
	cs := openHarness(t)
	pool := cs.Pool()
	prefix, meter := fairnessMeters(t)

	wideID := seedQuota(t, pool, "", prefix, 100)
	t.Cleanup(func() { dropInstallationWideQuota(t, pool, wideID) })

	greedy, greedyPrincipal := seedTenantWithKey(t, pool, "tok-share-a")
	seedQuota(t, pool, greedy.Project, prefix, 40)
	seedSpend(t, pool, greedy.Project, meter, 40)

	// The greedy project is at its own ceiling. The pool is at 40 of 100 and has 60 left.
	got := requireQuota(t, admitFairness(t, cs, greedy, greedyPrincipal, "fair-share-1"),
		"the project that exhausted its own share")
	if got.InstallationWide {
		t.Fatalf("the refusal names the INSTALLATION-wide pool (%+v), but the pool is at 40 of 100 and this "+
			"project is at its own 40 of 40 — naming the pool hands a project operator a limit it cannot "+
			"raise, and a number that is not the one that refused it", got)
	}
	if got.Used != 40 || got.Limit != 40 {
		t.Fatalf("the refusal reports %v of %v, want 40 of 40 (this project's own settled spend against its "+
			"own share)", got.Used, got.Limit)
	}

	// AND THE POINT OF THE WHOLE TASK: a project that caused none of that spend is admitted. An
	// implementation that refuses on any exhausted row anywhere, or that sums a sibling's spend into this
	// project's share, fails HERE and nowhere else — every other assertion above passes under it.
	neighbour, neighbourPrincipal := seedTenantWithKey(t, pool, "tok-share-b")
	if limit := admitFairness(t, cs, neighbour, neighbourPrincipal, "fair-share-2"); limit != nil {
		t.Fatalf("a project that spent NOTHING was refused by %+v while the installation pool still had 60 "+
			"of 100 free — one project's exhausted share is closing another project, which is the exact "+
			"cross-project 429 this limit exists to prevent", *limit)
	}
}

// TestAProjectQuotaBindsOnItsOwnWithNoInstallationWideCeiling holds the pre-existing behaviour: the
// project-scoped limit is not a thing that only works BECAUSE a pool is configured above it. No
// installation-wide row exists anywhere in this test, so a green run says the new pooled ceiling is an
// addition rather than a replacement — and a red one says the scope arithmetic broke the single-scope
// case that shipped in E13 Task 6.
func TestAProjectQuotaBindsOnItsOwnWithNoInstallationWideCeiling(t *testing.T) {
	cs := openHarness(t)
	pool := cs.Pool()
	prefix, meter := fairnessMeters(t)

	capped, cappedPrincipal := seedTenantWithKey(t, pool, "tok-solo-a")
	seedQuota(t, pool, capped.Project, prefix, 25)
	seedSpend(t, pool, capped.Project, meter, 25)

	got := requireQuota(t, admitFairness(t, cs, capped, cappedPrincipal, "fair-solo-1"),
		"a project at its own ceiling with no pool above it")
	if got.InstallationWide {
		t.Fatalf("the refusal names an installation-wide scope (%+v), and this test seeded no such row at "+
			"all — the scope the caller is told is not read from the row that refused it", got)
	}
	if got.Used != 25 || got.Limit != 25 || got.MeterPrefix != prefix {
		t.Fatalf("the refusal reports %q %v of %v, want %q 25 of 25", got.MeterPrefix, got.Used, got.Limit, prefix)
	}

	neighbour, neighbourPrincipal := seedTenantWithKey(t, pool, "tok-solo-b")
	if limit := admitFairness(t, cs, neighbour, neighbourPrincipal, "fair-solo-2"); limit != nil {
		t.Fatalf("a sibling project was refused by %+v though the only limit configured belongs to another "+
			"project", *limit)
	}
}
