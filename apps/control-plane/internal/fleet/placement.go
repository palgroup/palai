package fleet

// Placement: WHERE a run goes, decided in one function.
//
// The four sources are ordered, and the order is the whole content of this file. It lives in one
// function rather than at each caller because this tree has shipped the same check written twice and
// diverging more than once — and a placement rule written twice is worse than most, because the two
// copies disagree only for the runs that happen to exercise the step they differ on.

import "strings"

// PoolRequest is everything a placement decision reads, gathered by the caller. It is a struct of
// resolved FACTS rather than a set of lookups so the decision itself is pure: what it does with them
// is testable without a database, which is what makes the order provable rather than described.
type PoolRequest struct {
	// RunPoolID is runs.pool_id — the pool this run was ALREADY placed in. It wins because a resume
	// must return to the same pool: a run that woke up in a different posture would not find its
	// workspace, and a run that changed posture mid-flight would be two different runs wearing one id.
	RunPoolID string
	// RevisionPoolID is the agent revision's own binding.
	//
	// MEASURED, AND IT IS A CORRECTION TO THE PLAN: there is nowhere to read this from today.
	// agent_revisions (000019, +000024/000027 riders) carries model/tools/instructions/tool_sets/
	// mcp_connections/skills/hooks and NO pool column, and E24 has exactly one migration — T1's
	// 000045 — which did not add one. So this step is ORDERED here and is fed "" by every caller until
	// a column exists (`FLT-P3`). Keeping the slot is deliberate: dropping it would silently promote
	// the project policy above a per-agent binding, and the next person to add the column would have
	// to rediscover which side of it their new step goes.
	RevisionPoolID string
	// PolicyPoolID is the project's config_policy `pool` (coordinator.ConfigPolicy.Pool). A project-
	// scoped JSONB with a shipped write path is what E23 T2 used for `approvers`, for the same reason:
	// a migration for one string would be one more thing to keep in step with nothing.
	PolicyPoolID string
	// TenantDefaultPoolID is THIS TENANT's own pool named 'default' — the row identity.Store.provision
	// seeds with every organization.
	//
	// IT IS A CORRECTION MEASURED IN E24 T4, not a fourth preference: DefaultPoolID below is a CONSTANT,
	// and the constant is the BOOTSTRAP tenant's pool id (migration 000045 R6 seeds `pool_default` for
	// org_local/prj_local; every later organization gets a minted `pool_…` id instead). So before this
	// field, every tenant that had configured nothing resolved to a pool belonging to somebody else —
	// which the tenant check on the runner plane then refuses, correctly, leaving those runs parked
	// forever. Reading the tenant's own default pool is what makes "configured nothing" mean the same
	// thing for the second tenant as for the first. Empty leaves the constant as the last resort, so a
	// deployment whose default pool has not been seeded behaves exactly as it did.
	TenantDefaultPoolID string
}

// ResolvePool answers WHICH POOL, in the one order there is: the run's own placement, then the agent
// revision's binding, then the project policy, then the tenant's own default, then the constant.
//
// The default is never "no pool". A run with nothing configured anywhere lands in its tenant's default
// pool, which is where every machine that enrols with no pool configuration also lands — so the answer
// for today's single-runner install is the same answer it has always had, expressed as a decision
// instead of an absence (§2).
func ResolvePool(req PoolRequest) string {
	for _, candidate := range []string{req.RunPoolID, req.RevisionPoolID, req.PolicyPoolID, req.TenantDefaultPoolID} {
		// Trimmed because two of the three arrive from operator-authored JSON, where a stray space is a
		// typo and not a different pool. An id that is only whitespace says nothing and falls through.
		if id := strings.TrimSpace(candidate); id != "" {
			return id
		}
	}
	return DefaultPoolID
}
