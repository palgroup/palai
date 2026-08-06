package execution_test

// fakeRegistry is the in-memory fleet.Registry the gateway wire proofs drive. It is the "fake in
// tests" half of the reason the registry is an interface at all: the claim under proof is what the
// GATEWAY records when a machine enrols, and putting a real Postgres behind it would make a
// Docker-free wire proof a Docker-bound one without strengthening the claim by one bit.

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/fleet"
)

type fakeRegistry struct {
	mu sync.Mutex
	// rows is insertion-ordered so a proof can assert on WHICH machines are recorded, not just how
	// many. byDNS is the RecordSeen index, mirroring the store's lookup by certificate SAN.
	rows  []*fleet.Runner
	byDNS map[string]*fleet.Runner
	// pools maps each pool this fake knows to its POSTURE. Register refuses an unknown pool and
	// refuses a declared posture the pool does not have, so the wire proofs run against the same two
	// rules the store enforces — a fake that accepts what the store rejects is a proof about nothing.
	pools map[string]string
	// isolation is each pool's REQUIRED session-isolation mode (migration 000007), empty for a pool that
	// requires none — which is every pool that exists today, and the reason this map starts empty rather
	// than being seeded alongside `pools`.
	isolation map[string]string
	// free is the set of pools the PLANE owns: `runner_pools.project_id IS NULL`, which every project may
	// be placed onto. A pool absent from this set belongs to fakeRegistryProject, which is what every pool
	// in this fake was before the free fleet existed.
	//
	// ‼️ THE FAKE COULD NOT SAY THIS, AND A FAKE THAT CANNOT SAY IT PROVES NOTHING ABOUT IT. Pool()
	// returned fakeRegistryProject unconditionally, so no wire proof in this package could tell a plane
	// pool from a tenant's — and the gateway's rendezvous, which reads exactly this field to build its
	// queue key, was measured broken on a live stack while every proof here stayed green. It is seeded by
	// no test today and is kept for that reason: the next wire proof over a shared fleet needs a fake that
	// can express one, and re-deriving that is how the gap reopens.
	free map[string]bool
}

// fakePool is one pool the fake registry knows: an id and the posture that pool IS.
type fakePool struct {
	id      string
	posture string
}

// fakeRegistryTenant is the tenant every pool in this fake belongs to. It is the bootstrap install's,
// restated here rather than invented, because that is whose pool `fleet.DefaultPoolID` names. A proof
// that needs TWO tenants cannot use this fake at all — it needs the real store, which is why the tenant
// claim (E24 T4) is a component test against a real Postgres.
const fakeRegistryOrg, fakeRegistryProject = "org_local", "prj_local"

// defaultPoolPosture is what migration 000045 R6 seeds for the bootstrap tenant's pool, restated here
// so the fake's default matches the database's rather than being invented.
const defaultPoolPosture = "sandboxed-linux"

func newFakeRegistry(pools ...fakePool) *fakeRegistry {
	known := map[string]string{fleet.DefaultPoolID: defaultPoolPosture}
	for _, p := range pools {
		known[p.id] = p.posture
	}
	return &fakeRegistry{byDNS: map[string]*fleet.Runner{}, pools: known, isolation: map[string]string{}}
}

// requireIsolation gives a pool the session-isolation mode a machine must have MEASURED to join it.
func (f *fakeRegistry) requireIsolation(poolID, mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.isolation[poolID] = mode
}

func (f *fakeRegistry) Register(_ context.Context, reg fleet.Registration) (fleet.Runner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	posture, known := f.pools[reg.PoolID]
	if !known {
		return fleet.Runner{}, fleet.ErrUnknownPool
	}
	// The store's pairing guard, restated: the fake has to refuse what the real one refuses, or a wire
	// proof passes against a store that would not have accepted the write.
	if reg.ID == "" || reg.DNS == "" || !strings.HasPrefix(reg.DNS, reg.ID+".") {
		return fleet.Runner{}, fleet.ErrIdentityMismatch
	}
	// The posture rule, likewise restated: declared-and-different is a refusal, declared-nothing
	// inherits (every pre-E24 machine declares nothing).
	if reg.Posture != "" && reg.Posture != posture {
		return fleet.Runner{}, fleet.ErrPostureMismatch
	}
	// The MEASURED check (migration 000007). Both empties admit, for the two separate reasons the store's
	// isolationSatisfied states: a pool with no requirement is every pool alive today, and a machine that
	// measured nothing is every runner built before packages/device.
	if required := f.isolation[reg.PoolID]; required != "" && len(reg.IsolationModes) > 0 {
		supported := false
		for _, mode := range reg.IsolationModes {
			if mode == required {
				supported = true
			}
		}
		if !supported {
			return fleet.Runner{}, fleet.ErrIsolationUnsupported
		}
	}
	// ‼️ THE DEVICE-KEY RECOVERY RULES, RESTATED — AND THIS FAKE IS NOT THE PROOF OF THEM. The claim
	// "a restart is one machine row" is about what fleet.Store does against a real Postgres, and it is
	// proven there (fleet's component suite, against the 000007 unique index). What the fake exists for is
	// the claim ABOVE it, which the store cannot make: that the GATEWAY signs a certificate for the id the
	// registry returned rather than for the id it minted. A fake that created a new row on every enrolment
	// would let that gateway defect pass unseen, because both ids would be new and equal-looking.
	//
	// Restating rules in a fake is how a fake reproduces production's bug. The mitigation here is that
	// these three lines are the CONTRACT (the error values fleet exports), not a copy of the store's
	// implementation, and the store's own behaviour is measured against a real database.
	if reg.PublicKeySHA256 != "" {
		if existing := f.byFingerprint(reg.PoolID, reg.PublicKeySHA256); existing != nil {
			if existing.State == "revoked" {
				return fleet.Runner{}, fleet.ErrRunnerRevoked
			}
			if reg.RecoverRunnerID != "" && reg.RecoverRunnerID != existing.ID {
				return fleet.Runner{}, fleet.ErrIdentityNotRecoverable
			}
			existing.Label, existing.LastSeenAt = reg.Label, time.Now()
			return *existing, nil
		}
	}
	if reg.RecoverRunnerID != "" {
		return fleet.Runner{}, fleet.ErrIdentityNotRecoverable
	}
	row := &fleet.Runner{
		ID: reg.ID, Project: fakeRegistryProject,
		PoolID: reg.PoolID, Label: reg.Label, DNS: reg.DNS, PublicKeySHA256: reg.PublicKeySHA256,
		State: "active", OS: reg.OS, Arch: reg.Arch, Posture: posture, Capacity: reg.Capacity,
		EnrolledAt: time.Now(), LastSeenAt: time.Now(),
	}
	f.rows = append(f.rows, row)
	f.byDNS[row.DNS] = row
	return *row, nil
}

func (f *fakeRegistry) RecordSeen(_ context.Context, dns string, certNotAfter, at time.Time) (fleet.Runner, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.byDNS[dns]
	if !ok {
		return fleet.Runner{}, false, nil
	}
	row.LastSeenAt = at
	if !certNotAfter.IsZero() {
		row.CertNotAfter = certNotAfter
	}
	return *row, true, nil
}

// Pool answers whose pool this is — the fallback the gateway takes for a machine no row knows, which is
// a runner that enrolled before E24 and has not restarted since. The fake has to answer it the way the
// store does (from the pool row, never from the caller) or a wire proof would pass against a store that
// resolves a different tenant.
func (f *fakeRegistry) Pool(_ context.Context, poolID string) (fleet.Pool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	posture, known := f.pools[poolID]
	if !known {
		return fleet.Pool{}, false, nil
	}
	project := fakeRegistryProject
	if f.free[poolID] {
		project = ""
	}
	return fleet.Pool{
		ID: poolID, Project: project,
		Name: "default", Posture: posture,
	}, true, nil
}

func (f *fakeRegistry) Get(_ context.Context, project, id string) (fleet.Runner, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range f.rows {
		if row.ID == id && row.Project == project {
			return *row, true, nil
		}
	}
	return fleet.Runner{}, false, nil
}

func (f *fakeRegistry) List(_ context.Context, project string, _ fleet.ListWindow) ([]fleet.Runner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []fleet.Runner{}
	for _, row := range f.rows {
		if row.Project == project {
			out = append(out, *row)
		}
	}
	return out, nil
}

// byFingerprint is the fake's index for the device-key lookup. It walks the rows rather than keeping a
// map because the ORDER decides the answer when more than one row could match, and the store's statement
// says newest wins — walking backwards is the same rule with no second index to keep in step.
//
// Called with f.mu already held.
func (f *fakeRegistry) byFingerprint(poolID, fingerprint string) *fleet.Runner {
	for i := len(f.rows) - 1; i >= 0; i-- {
		if f.rows[i].PoolID == poolID && f.rows[i].PublicKeySHA256 == fingerprint {
			return f.rows[i]
		}
	}
	return nil
}

// revoke marks a row revoked, so a proof can measure what a re-enrolment does about it without needing
// the whole lifecycle surface.
func (f *fakeRegistry) revoke(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range f.rows {
		if row.ID == id {
			row.State = "revoked"
		}
	}
}

// snapshot copies the recorded rows for assertion.
func (f *fakeRegistry) snapshot() []fleet.Runner {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fleet.Runner, 0, len(f.rows))
	for _, row := range f.rows {
		out = append(out, *row)
	}
	return out
}
