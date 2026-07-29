package fleet_test

import (
	"testing"

	"github.com/palgroup/palai/apps/control-plane/internal/fleet"
)

// TestResolvePoolTakesTheFourStepsInOrder pins the placement precedence. Every case names a pool at
// MORE than one level, because a test that sets one field at a time proves each source is read and
// proves nothing at all about which of two wins — and which of two wins is the entire decision.
//
// E24 T4 added a step between the project policy and the constant, and the last two cases are why: the
// constant is the BOOTSTRAP tenant's pool id, so a second tenant that had configured nothing resolved
// to a pool belonging to somebody else — which the runner plane's tenant check then refuses, correctly,
// leaving those runs parked forever.
func TestResolvePoolTakesTheFourStepsInOrder(t *testing.T) {
	for name, tc := range map[string]struct {
		req  fleet.PoolRequest
		want string
	}{
		"the run's own placement beats everything, because a resume returns to it": {
			req:  fleet.PoolRequest{RunPoolID: "pool_run", RevisionPoolID: "pool_rev", PolicyPoolID: "pool_policy", TenantDefaultPoolID: "pool_tenant"},
			want: "pool_run",
		},
		"the agent revision's binding beats the project policy": {
			req:  fleet.PoolRequest{RevisionPoolID: "pool_rev", PolicyPoolID: "pool_policy", TenantDefaultPoolID: "pool_tenant"},
			want: "pool_rev",
		},
		"the project policy is what an operator configures": {
			req:  fleet.PoolRequest{PolicyPoolID: "pool_policy", TenantDefaultPoolID: "pool_tenant"},
			want: "pool_policy",
		},
		"nothing configured anywhere is THIS TENANT's own default pool, never the constant": {
			req:  fleet.PoolRequest{TenantDefaultPoolID: "pool_tenant"},
			want: "pool_tenant",
		},
		"the constant is the last resort, for a deployment whose default pool is not seeded": {
			req:  fleet.PoolRequest{},
			want: fleet.DefaultPoolID,
		},
		// The two upper sources come from operator-authored JSON, where a value that is only spaces is
		// a typo. Falling through to the next step is the honest reading; treating "  " as a pool id
		// would place the run somewhere no pool exists and the enrolment path would refuse forever.
		"a whitespace-only policy value says nothing and falls through": {
			req:  fleet.PoolRequest{PolicyPoolID: "   "},
			want: fleet.DefaultPoolID,
		},
		"the tenant's own default beats the constant even when the policy is blank": {
			req:  fleet.PoolRequest{PolicyPoolID: "  ", TenantDefaultPoolID: "pool_tenant"},
			want: "pool_tenant",
		},
		"a whitespace-only revision binding does not mask the policy below it": {
			req:  fleet.PoolRequest{RevisionPoolID: " ", PolicyPoolID: "pool_policy"},
			want: "pool_policy",
		},
		"a configured id is trimmed rather than taken literally": {
			req:  fleet.PoolRequest{PolicyPoolID: " pool_policy "},
			want: "pool_policy",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := fleet.ResolvePool(tc.req); got != tc.want {
				t.Fatalf("ResolvePool(%+v) = %q, want %q", tc.req, got, tc.want)
			}
		})
	}
}
