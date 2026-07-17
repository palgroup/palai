//go:build fault

// Package coordinator holds the fault-injection proof for the durable job
// coordinator. It runs only under `make test-fault TEST=coordinator`, which starts
// a throwaway PostgreSQL container and exports PALAI_FAULT_POSTGRES_URL plus the
// short, injected lease timers (PALAI_FAULT_*). The build tag keeps these
// kill/reclaim tests out of the credential-free, Docker-free unit tier.
//
// Every test exercises packages/coordinator against real Postgres: a worker claims
// a fenced lease, is killed mid-lease (its goroutine context is cancelled so it
// stops heartbeating), the lease lapses by database time, and a second worker
// reclaims the same logical job at a strictly higher fence. The superseded holder's
// completion is then rejected as a lease_conflict. Lease expiry is polled through
// the database clock, never a wall-clock sleep, so the suite is deterministic under
// `-count=20`.
package coordinator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/packages/coordinator"
)

func faultURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("PALAI_FAULT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_FAULT_POSTGRES_URL is required; run make test-fault TEST=coordinator")
	}
	return url
}

// faultLease is the short lease the injected timer sets for both a claim and a
// heartbeat renewal. The fault script exports PALAI_FAULT_LEASE so the suite proves
// expiry/reclaim without waiting on a production interval.
func faultLease(t *testing.T) time.Duration {
	t.Helper()
	if d, err := time.ParseDuration(os.Getenv("PALAI_FAULT_LEASE")); err == nil && d > 0 {
		return d
	}
	return 150 * time.Millisecond
}

// openStore returns a migrated durable-spine store. Migrate is idempotent, so every
// test starts from applied schema.
func openStore(t *testing.T) *coordinator.Store {
	t.Helper()
	store, err := coordinator.Open(context.Background(), faultURL(t))
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(store.Close)
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return store
}

func newID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

// seedTenant creates org -> project and returns the scope. A durable job's tenant
// columns reference projects (organization_id, id), so the queue cannot hold a job
// for a tenant that does not exist.
//
// The claim loop is cross-tenant infrastructure — ClaimNext leases the oldest ready
// job in the whole queue — so a job a test leaves behind would be visible to the next
// test's ClaimNext. The tests run sequentially, so a cleanup that clears this tenant's
// jobs (cascading their attempt ledger) keeps the shared queue clean between them and
// deterministic under -count.
func seedTenant(t *testing.T, pool *pgxpool.Pool) coordinator.Tenant {
	t.Helper()
	ctx := context.Background()
	tenant := coordinator.Tenant{Organization: newID("org"), Project: newID("prj")}
	exec := func(sql string, args ...any) {
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec %q error = %v", sql, err)
		}
	}
	exec(`INSERT INTO organizations (id) VALUES ($1)`, tenant.Organization)
	exec(`INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, tenant.Project, tenant.Organization)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM durable_jobs WHERE organization_id = $1`, tenant.Organization)
	})
	return tenant
}

// enqueueJob inserts one queued job in the tenant and returns its id.
func enqueueJob(t *testing.T, store *coordinator.Store, tenant coordinator.Tenant) string {
	t.Helper()
	jobID := newID("job")
	if err := store.Enqueue(context.Background(), tenant, jobID, "response.run"); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	return jobID
}

// waitLeaseExpiry polls the database clock until the job's lease has lapsed. It
// never sleeps a fixed wall-clock duration, so a slow CI host cannot make the suite
// flaky across repeated runs.
func waitLeaseExpiry(t *testing.T, store *coordinator.Store, tenant coordinator.Tenant, jobID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		expired, err := store.LeaseExpired(ctx, tenant, jobID)
		if err != nil {
			t.Fatalf("LeaseExpired() error = %v", err)
		}
		if expired {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("lease did not expire: %v", ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func readSnapshot(t *testing.T, store *coordinator.Store, tenant coordinator.Tenant, jobID string) coordinator.Snapshot {
	t.Helper()
	snap, err := store.Snapshot(context.Background(), tenant, jobID)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	return snap
}
