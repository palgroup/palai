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
}

// fakePool is one pool the fake registry knows: an id and the posture that pool IS.
type fakePool struct {
	id      string
	posture string
}

// defaultPoolPosture is what migration 000045 R6 seeds for the bootstrap tenant's pool, restated here
// so the fake's default matches the database's rather than being invented.
const defaultPoolPosture = "sandboxed-linux"

func newFakeRegistry(pools ...fakePool) *fakeRegistry {
	known := map[string]string{fleet.DefaultPoolID: defaultPoolPosture}
	for _, p := range pools {
		known[p.id] = p.posture
	}
	return &fakeRegistry{byDNS: map[string]*fleet.Runner{}, pools: known}
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
	row := &fleet.Runner{
		ID: reg.ID, Organization: "org_local", Project: "prj_local",
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

func (f *fakeRegistry) Get(_ context.Context, org, project, id string) (fleet.Runner, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range f.rows {
		if row.ID == id && row.Organization == org && row.Project == project {
			return *row, true, nil
		}
	}
	return fleet.Runner{}, false, nil
}

func (f *fakeRegistry) List(_ context.Context, org, project string, _ fleet.ListWindow) ([]fleet.Runner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []fleet.Runner{}
	for _, row := range f.rows {
		if row.Organization == org && row.Project == project {
			out = append(out, *row)
		}
	}
	return out, nil
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
