package execution

// THE RENDEZVOUS: the (tenant, pool) key a machine parks under and an attempt asks for. They must be the
// SAME key or the two never meet, and "never meet" is indistinguishable from "there is no machine".
//
// ‼️ THIS WAS MEASURED BROKEN ON A LIVE STACK, 2026-08-06, with a Mac enrolled, online, counted in the
// runner listing's `connections`, sitting in the pool the run had already been placed into — and every
// run parking instantly. The gateway logged nothing, because from the queue's point of view there
// genuinely was no machine: the machine had joined ("", pool) and every attempt asked for ("prj_x", pool).
//
// The machine side takes its tenant from the REGISTRY ROW, and a machine on the free fleet carries no
// project. So the dial side has to derive the same value from the same durable fact —
// `runner_pools.project_id` — rather than from the caller's own scope. That is rendezvousTenant, and this
// file is what keeps the two sides reading one source.
//
// THE SECURITY PROPERTY queueKey WAS BUILT FOR IS UNCHANGED and is asserted here rather than assumed: a
// cross-tenant offer stays UNREACHABLE rather than refused, because a private pool's key is still its
// owner's. What changed is only that a pool whose row says "no owner" is one both sides agree has none.

import (
	"context"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/fleet"
	"github.com/palgroup/palai/packages/coordinator"
)

// rendezvousOwners is a fleet.Registry that answers ONE question and refuses the rest. It lives here
// rather than reusing the gateway's wire fake because that fake is in the external test package and this
// property is a property of an unexported method — and because a stub that can only answer `Pool` cannot
// accidentally make some other part of the gateway look wired.
//
// A pool absent from the map is one the registry cannot answer for, which is the fallback arm.
type rendezvousOwners map[string]fleet.Pool

func (r rendezvousOwners) Pool(_ context.Context, poolID string) (fleet.Pool, bool, error) {
	p, ok := r[poolID]
	return p, ok, nil
}

func (rendezvousOwners) Register(context.Context, fleet.Registration) (fleet.Runner, error) {
	panic("the rendezvous asks only for a pool")
}

func (rendezvousOwners) RecordSeen(context.Context, string, time.Time, time.Time) (fleet.Runner, bool, error) {
	panic("the rendezvous asks only for a pool")
}

func (rendezvousOwners) Get(context.Context, string, string) (fleet.Runner, bool, error) {
	panic("the rendezvous asks only for a pool")
}

func (rendezvousOwners) List(context.Context, string, fleet.ListWindow) ([]fleet.Runner, error) {
	panic("the rendezvous asks only for a pool")
}

// rendezvousOwnerProject is the tenant a PRIVATE pool belongs to in these proofs.
const rendezvousOwnerProject = "prj_owner"

// rendezvousGateway builds a gateway over a registry that knows one private pool and one free pool.
func rendezvousGateway(private, free string) *RunnerGateway {
	// pools is initialised because queueFor writes into it — a gateway built by NewRunnerGateway always
	// has it, and a zero-value struct here would panic on the first rendezvous rather than measure one.
	return &RunnerGateway{pools: map[string]*poolQueue{}, registry: rendezvousOwners{
		private: {ID: private, Project: rendezvousOwnerProject, Posture: "unsandboxed-host"},
		// The plane's: `runner_pools.project_id IS NULL`, which fleet.Store renders as "".
		free: {ID: free, Project: "", Posture: "unsandboxed-host"},
	}}
}

// TestTheDialAsksTheRendezVOUSTheMachineJoined is the property in one assertion per pool kind: the key an
// attempt computes equals the key the machine's own row would produce.
//
// The machine side is not mocked out — it is restated from the one fact recordSeen returns (the row's
// project) — because the whole defect was two sides deriving the "same" value from two different places.
func TestTheDialAsksTheRendezVOUSTheMachineJoined(t *testing.T) {
	const private, free = "pool_private", "pool_free"
	g := rendezvousGateway(private, free)

	for _, tc := range []struct {
		name string
		pool string
		// machineTenant is what recordSeen hands queueFor: the project on the machine's registry row,
		// which a machine inherits from its pool at enrolment.
		machineTenant string
	}{
		{"the plane's pool", free, ""},
		// The owner's OWN attempt on its own pool: the one case where a private pool's two sides meet.
		{"a private pool, asked by its owner", private, rendezvousOwnerProject},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The MACHINE side first, exactly as handleConnect does it: the project on its registry row.
			joined := g.queueFor(coordinator.Tenant{Project: tc.machineTenant}, tc.pool)
			// Then the DIAL side, through the method Dial itself calls. Identity, not equality of two
			// recomputed strings: this is the same object or the two never meet.
			// The caller is the pool's owner for a private pool, and anybody for the plane's — which is
			// the whole difference the rendezvous encodes.
			caller := coordinator.Tenant{Project: tc.machineTenant}
			if tc.pool == free {
				caller = coordinator.Tenant{Project: "prj_any_tenant"}
			}
			asked, queue := g.queueForAttempt(context.Background(), AttemptDescriptor{Tenant: caller, PoolID: tc.pool})
			if queue != joined {
				t.Fatalf("the attempt asks rendezvous %q and the machine joined %q — one pool, two queues, "+
					"and a Mac that is enrolled, online and idle while every run parks with "+
					"ErrPoolHasNoRunner and nothing is logged",
					queueKey(asked, tc.pool), queueKey(coordinator.Tenant{Project: tc.machineTenant}, tc.pool))
			}
		})
	}
}

// TestAStrangerNamingAPrivatePoolMeetsNobody REPLACES A TEST THAT ASSERTED THE BUG, and the swap is the
// most expensive thing in this file.
//
// It was TestAPrivatePoolStillBelongsToItsOwner, and it demanded that a STRANGER's attempt naming a
// private pool resolve to that pool's OWNER. Read as a sentence about queue keys that sounds right; read
// as a sentence about tenancy it says "put this tenant's attempt in the queue another tenant's machines
// are parked in". The implementation obliged, and
// TestPlacementNeverOffersOneTenantsAttemptToAnothersRunner — on the component tier, which `make verify`
// does not run — caught tenant B's attempt being handed tenant A's machine.
//
// The tenant half of queueKey was the ONLY thing making a cross-tenant offer UNREACHABLE rather than
// refused. Resolving both sides from the pool row handed that away, and a guard written from the same
// misunderstanding could not see it.
//
// The property, stated so it cannot be misread: a rendezvous WIDENS for a free pool and for nothing
// else. A private pool leaves the caller's own tenant in the key, so a stranger's attempt matches no
// queue at all.
func TestAStrangerNamingAPrivatePoolMeetsNobody(t *testing.T) {
	const private, free = "pool_private", "pool_free"
	g := rendezvousGateway(private, free)
	stranger := coordinator.Tenant{Project: "prj_stranger"}

	got := g.rendezvousTenant(context.Background(), AttemptDescriptor{Tenant: stranger, PoolID: private})
	if got != stranger {
		t.Fatalf("a stranger naming a private pool resolved to %q, want its own %q — anything else puts "+
			"its attempt in the queue the OWNER's machines are parked in", got.Project, stranger.Project)
	}
	// And the queue it would wait on is not the one the owner's machine joined. Identity, because that is
	// what "unreachable rather than refused" means here.
	ownerQueue := g.queueFor(coordinator.Tenant{Project: rendezvousOwnerProject}, private)
	_, strangerQueue := g.queueForAttempt(context.Background(), AttemptDescriptor{Tenant: stranger, PoolID: private})
	if strangerQueue == ownerQueue {
		t.Fatal("a stranger's attempt and the pool owner's machines share one queue: the cross-tenant " +
			"offer queueKey exists to make unreachable is reachable again")
	}
}

// TestAnUnreadablePoolNarrowsRatherThanWidens is the failure direction, and it is the one that hid the
// defect for a day. rendezvousTenant falls back to the caller's own tenant when the registry cannot answer
// — which is the pre-free-fleet behaviour exactly, so a broken read can only ever make the rendezvous
// NARROWER (nobody meets) and never wider (the wrong people meet).
//
// It is worth a test because the fallback is what made a genuine bug invisible: fleet.Store.Pool scanned
// six of ResolveRunnerPool's seven columns and had returned an error on every call since migration 000007,
// so this arm was the ONLY one ever taken on a real stack while the code above read as correct.
func TestAnUnreadablePoolNarrowsRatherThanWidens(t *testing.T) {
	g := rendezvousGateway("pool_private", "pool_free")
	caller := coordinator.Tenant{Project: "prj_caller"}

	got := g.rendezvousTenant(context.Background(), AttemptDescriptor{Tenant: caller, PoolID: "pool_nobody_knows"})
	if got != caller {
		t.Fatalf("an unknown pool resolved to %q, want the caller's own %q: a read this gateway could not "+
			"answer must not widen the rendezvous", got.Project, caller.Project)
	}
}
