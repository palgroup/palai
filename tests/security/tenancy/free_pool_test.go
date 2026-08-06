//go:build security

package tenancy

// THE FREE FLEET, and the four things that must NOT come with it.
//
// A device enrols into the PLANE and serves every project on it: that is the owner's shape, and a pool
// with no project is how the schema spells it. Everything below exists because that sentence is one
// widening away from four different disclosures, and the widening is the same edit in every case.
//
// ‼️ IT WAS UNREACHABLE BEFORE 000002 GREW A SECOND EXPRESSION, and both halves refused it for opposite
// reasons — measured 2026-08-06:
//   * `ResolveRunnerPool` has always admitted a free pool (`p.project_id IS NULL OR p.project_id = $2`).
//   * The tenant expression spells shared as the EMPTY STRING, so it saw NULL as neither the tenant's
//     nor shared, and a free pool was invisible to the tenant whose run needed it.
//   * And `project_id = ''` cannot be stored: it violates runner_pools_project_id_fkey.
// So the free pool could be written and never read, or read and never written. That is why "the fleet
// must not be project-bound" could not be configured — it had to be built.
//
// This file is the negative half. `palai_apply_fleet_policy` reaches exactly two tables, and the cost of
// reaching a third is measured here rather than reasoned about: an api_keys row with no project is a
// CREDENTIAL that would become every tenant's, and it is asserted invisible in the SAME transaction that
// sees the free pool — so a policy applied too widely cannot pass this by making the free pool visible.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// freeFleet seeds, as the OWNER, the four rows every claim below reads: a free pool and a machine in it,
// a private pool belonging to tenant B, and a project-less API key. It returns the ids.
//
// Seeding as the owner is deliberate and is the reason this helper exists rather than the tests writing
// their own rows: the free rows CANNOT be written from a tenant scope (that is the last claim in this
// file), so a fixture that wrote them through the app pool would either fail or prove the opposite of
// what it was seeding for.
func freeFleet(t *testing.T, s *suite) (freePool, freeRunner, privatePool, orphanKey string) {
	t.Helper()
	ctx := context.Background()
	freePool, freeRunner = newID("pool"), newID("rnr")
	privatePool, orphanKey = newID("pool"), newID("key")
	principal := newID("prn")
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO runner_pools (id, project_id, name, posture) VALUES ($1, NULL, $2, 'unsandboxed-host')`,
			[]any{freePool, newID("free")}},
		{`INSERT INTO runners (id, pool_id, project_id, runner_dns, state) VALUES ($1, $2, NULL, $3, 'active')`,
			[]any{freeRunner, freePool, freeRunner + ".runners.palai.internal"}},
		{`INSERT INTO runner_pools (id, project_id, name, posture) VALUES ($1, $2, $3, 'unsandboxed-host')`,
			[]any{privatePool, s.projectB, newID("private")}},
		{`INSERT INTO principals (id, kind, project_id) VALUES ($1, 'service', NULL)`,
			[]any{principal}},
		{`INSERT INTO api_keys (id, project_id, principal_id, key_hash) VALUES ($1, NULL, $2, $3)`,
			[]any{orphanKey, principal, newID("hash")}},
	} {
		if _, err := s.owner.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed %q: %v", stmt.sql, err)
		}
	}
	return freePool, freeRunner, privatePool, orphanKey
}

// TestAFreePoolServesEveryTenantAndItsNEIGHBOURSStayHidden is the whole property in one transaction per
// tenant: the free pool and its machine are visible to BOTH, another tenant's private pool is visible to
// neither, and the project-less API key is visible to neither — asserted while the free pool IS visible,
// so "the policy went too wide" and "the policy works" cannot both read as a pass.
func TestAFreePoolServesEveryTenantAndItsNEIGHBOURSStayHidden(t *testing.T) {
	s := newSuite(t)
	ctx := context.Background()
	freePool, freeRunner, foreignPool, orphanKey := freeFleet(t, s)

	for _, tenant := range []string{s.projectA, s.projectB} {
		s.asProject(t, tenant, func(tx pgx.Tx) {
			visible := func(sql, id string) bool {
				t.Helper()
				var n int
				if err := tx.QueryRow(ctx, sql, id).Scan(&n); err != nil {
					t.Fatalf("count %s: %v", id, err)
				}
				return n > 0
			}
			const pools = `SELECT count(*) FROM runner_pools WHERE id = $1`
			const runners = `SELECT count(*) FROM runners WHERE id = $1`
			const keys = `SELECT count(*) FROM api_keys WHERE id = $1`

			if !visible(pools, freePool) {
				t.Fatalf("tenant %s cannot see the free pool: a machine enrolled to the PLANE is one this "+
					"tenant's runs can never be placed on, which is the whole shape being built", tenant)
			}
			if !visible(runners, freeRunner) {
				t.Fatalf("tenant %s sees the free pool but not the MACHINE in it: placement resolves a pool "+
					"and then reads its runners, so a fleet that is half visible parks every run", tenant)
			}
			// The foreign private pool is the control that keeps the claim above from being vacuous: a
			// policy that had simply stopped filtering would show the free pool too.
			if tenant != s.projectB && visible(pools, foreignPool) {
				t.Fatalf("tenant %s can see another tenant's PRIVATE pool: the free-pool arm has widened into "+
					"a wildcard and the boundary is gone", tenant)
			}
			// ‼️ THE NEGATIVE CONTROL, in the same transaction as the positive one. `OR col IS NULL` added
			// to palai_tenant_policy_expression instead of a second expression would pass every assertion
			// above and fail only here — and what it would disclose is a credential.
			if visible(keys, orphanKey) {
				t.Fatalf("tenant %s can see a project-less API KEY. The free-fleet arm has reached "+
					"palai_apply_tenant_policy's tables; api_keys, audit_events, model_connections and "+
					"principals all carry a nullable project and every one of them is now every tenant's", tenant)
			}
		})
	}
}

// TestATenantCannotMintAFreePool is the write half, and it is a separate policy arm rather than a
// consequence of the one above: `palai_apply_fleet_policy` passes a WIDER expression to USING than to
// WITH CHECK, which is the only reason this refusal exists. Handing both arms the same expression — what
// every other policy in 000002 does, and therefore the natural thing to write — would let a tenant INSERT
// a pool with no project: a fleet row visible to every other tenant on the installation, minted from
// inside one tenant's boundary.
//
// A tenant that could do this would not be reading anything it should not. It would be OFFERING —
// publishing a pool other tenants' runs can be placed into, on hardware it controls.
func TestATenantCannotMintAFreePool(t *testing.T) {
	s := newSuite(t)
	ctx := context.Background()
	s.asProject(t, s.projectA, func(tx pgx.Tx) {
		_, err := tx.Exec(ctx,
			`INSERT INTO runner_pools (id, project_id, name, posture) VALUES ($1, NULL, $2, 'unsandboxed-host')`,
			newID("pool"), newID("planted"))
		if err == nil {
			t.Fatal("a tenant minted a PLANE-WIDE pool: every other tenant on this installation can now be " +
				"placed onto hardware this one supplied")
		}
		if code := sqlState(err); code != "42501" {
			t.Fatalf("the free-pool insert failed with SQLSTATE %q, want 42501 (the policy's WITH CHECK): %v",
				code, err)
		}
	})
	// The same refusal for a MACHINE. Both tables take the same procedure, so asserting one and assuming
	// the other is how the pair drifts: a plane-wide runner row is the same offer as a plane-wide pool.
	s.asProject(t, s.projectA, func(tx pgx.Tx) {
		id := newID("rnr")
		_, err := tx.Exec(ctx,
			`INSERT INTO runners (id, pool_id, project_id, runner_dns, state)
			 SELECT $1, p.id, NULL, $2, 'active' FROM runner_pools p WHERE p.project_id IS NULL LIMIT 1`,
			id, id+".runners.palai.internal")
		if err == nil {
			t.Fatal("a tenant registered a machine into the PLANE's fleet")
		}
		if code := sqlState(err); code != "42501" {
			t.Fatalf("the free-runner insert failed with SQLSTATE %q, want 42501: %v", code, err)
		}
	})
}
