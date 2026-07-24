//go:build component

// Package workers_test holds the real-PostgreSQL component tests for the E17 Task 9 CapabilityWorker contract
// (spec §31.2-31.6, WRK-001..007). They run only under `make test-component TEST=postgres` (which starts a
// throwaway container and exports PALAI_COMPONENT_POSTGRES_URL); the build tag keeps them out of the
// credential-free, Docker-free unit tier. They prove the durable half of the contract end to end through the
// workers.Store — the same store the outbound-enrolled gateway drives: the append-only job journal, the fence
// discipline (§31.6), the job-scoped short-lived secret handle, the no-tunnel typed-operation gate, and
// cross-tenant isolation.
//
// The crown security tests are RED-first: each names the one guard whose removal turns it green.
package workers_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/palgroup/palai/apps/control-plane/internal/workers"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

func componentURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	return url
}

// openHarness returns a migrated spine store. Migrate is idempotent, so every test starts from applied schema.
func openHarness(t *testing.T) *coordinator.Store {
	t.Helper()
	cs, err := coordinator.Open(context.Background(), componentURL(t))
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return cs
}

func newID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

// seedTenant creates org -> project (the FK capability_workers/_jobs need) under the system scope and returns
// the workers.Tenant view.
func seedTenant(t *testing.T, cs *coordinator.Store) workers.Tenant {
	t.Helper()
	ctx := storage.WithSystemScope(context.Background())
	org, project := newID("org"), newID("prj")
	if _, err := cs.Pool().Exec(ctx, `INSERT INTO organizations (id) VALUES ($1)`, org); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := cs.Pool().Exec(ctx, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, project, org); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return workers.Tenant{Organization: org, Project: project}
}

// newStore builds a workers.Store over the harness pool with a controllable clock.
func newStore(cs *coordinator.Store, secrets workers.SecretResolver, now func() time.Time) *workers.Store {
	return workers.NewStore(cs.Pool(), secrets, newID, now)
}

// fakeSecrets is a deterministic SecretResolver standing in for identity.SecretStore, so the secret-handle
// tests can assert the resolved value NEVER lands in a receipt or the journal. The value is a distinctive
// marker so a leak is unmissable.
type fakeSecrets struct{ vals map[string]string }

func (f fakeSecrets) Resolve(_ context.Context, _ string, name string) ([]byte, bool, error) {
	v, ok := f.vals[name]
	return []byte(v), ok, nil
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }

func enrolledWorker(t *testing.T, store *workers.Store, tenant workers.Tenant, digests map[string]string) workers.Worker {
	t.Helper()
	w, err := store.Enroll(context.Background(), tenant, workers.WorkerSpec{
		Capability: "swift-toolchain", CapabilityVersion: "0.1.0", OS: "darwin", Arch: "arm64",
		ToolchainDigests: digests, Capacity: 1, TrustLabel: "sandbox",
	})
	if err != nil {
		t.Fatalf("Enroll() error = %v", err)
	}
	return w
}

func tableExists(t *testing.T, cs *coordinator.Store, name string) bool {
	t.Helper()
	var reg *string
	if err := cs.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT to_regclass('public.' || $1)::text`, name).Scan(&reg); err != nil {
		t.Fatalf("to_regclass(%s): %v", name, err)
	}
	return reg != nil
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func mustFail(_ pgconn.CommandTag, err error) error { return err }

func latestJobEntry(t *testing.T, cs *coordinator.Store, jobID string) (string, string) {
	t.Helper()
	var kind, receipt string
	if err := cs.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT entry_kind, receipt::text FROM capability_jobs WHERE job_id = $1 ORDER BY entry_seq DESC LIMIT 1`, jobID).
		Scan(&kind, &receipt); err != nil {
		t.Fatalf("read latest job entry: %v", err)
	}
	return kind, receipt
}

func assertJournalKinds(t *testing.T, cs *coordinator.Store, jobID string, want []string) {
	t.Helper()
	rows, err := cs.Pool().Query(storage.WithSystemScope(context.Background()),
		`SELECT entry_kind FROM capability_jobs WHERE job_id = $1 ORDER BY entry_seq`, jobID)
	if err != nil {
		t.Fatalf("read journal kinds: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("scan kind: %v", err)
		}
		got = append(got, k)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("journal kinds = %v, want %v", got, want)
	}
}

// assertJournalHasNoSecret proves the resolved secret VALUE never landed in any journal entry (only the ref
// NAME may appear). The whole journal (the secret-ref + receipt columns) is scanned as text for the marker.
func assertJournalHasNoSecret(t *testing.T, cs *coordinator.Store, jobID, marker string) {
	t.Helper()
	rows, err := cs.Pool().Query(storage.WithSystemScope(context.Background()),
		`SELECT id, entry_kind, secret_handle_refs::text, receipt::text FROM capability_jobs WHERE job_id = $1`, jobID)
	if err != nil {
		t.Fatalf("scan journal for secret: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, kind, refs, receipt string
		if err := rows.Scan(&id, &kind, &refs, &receipt); err != nil {
			t.Fatalf("scan row: %v", err)
		}
		for _, col := range []string{refs, receipt} {
			if strings.Contains(col, marker) {
				t.Fatalf("secret VALUE leaked into a capability_jobs %s entry (%s): %s", kind, id, col)
			}
		}
	}
}

// TestMigration39CapabilityWorkerTables proves 000039 adds capability_workers + capability_jobs idempotently
// and reverses cleanly (the 000037/000038 re-run-safety pattern).
func TestMigration39CapabilityWorkerTables(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	for _, name := range []string{"capability_workers", "capability_jobs"} {
		if !tableExists(t, cs, name) {
			t.Fatalf("after apply, %s is missing", name)
		}
	}
	if err := cs.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	for _, name := range []string{"capability_workers", "capability_jobs"} {
		if tableExists(t, cs, name) {
			t.Fatalf("after rollback, %s still exists", name)
		}
	}
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	for _, name := range []string{"capability_workers", "capability_jobs"} {
		if !tableExists(t, cs, name) {
			t.Fatalf("after reapply, %s is missing", name)
		}
	}
}

// TestMigration39CapabilityJobsJournalIsAppendOnly is the append-only crown (§31.3): across a SECOND boot the
// runtime role keeps SELECT+INSERT on capability_jobs but NOT UPDATE/DELETE, so a stale worker can never
// rewrite an entry to un-fence its result nor delete one to hide a quarantine. 000039's REVOKE runs LAST and
// self-re-asserts after 000001/000029's blanket grants re-run. RED: dropping the REVOKE turns UPDATE/DELETE
// green (both granted again on boot #2).
func TestMigration39CapabilityJobsJournalIsAppendOnly(t *testing.T) {
	cs := openHarness(t)
	ctx := storage.WithSystemScope(context.Background())
	if err := cs.Migrate(context.Background()); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}

	assertPriv := func(priv string, want bool) {
		t.Helper()
		var got bool
		if err := cs.Pool().QueryRow(ctx, `SELECT has_table_privilege('palai_app', 'capability_jobs', $1)`, priv).Scan(&got); err != nil {
			t.Fatalf("has_table_privilege(%s): %v", priv, err)
		}
		if got != want {
			t.Fatalf("palai_app %s on capability_jobs = %v, want %v (append-only grant eroded across reboots)", priv, got, want)
		}
	}
	assertPriv("SELECT", true)
	assertPriv("INSERT", true)
	assertPriv("UPDATE", false)
	assertPriv("DELETE", false)

	conn, err := cs.Pool().Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire(): %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET ROLE palai_app`); err != nil {
		t.Fatalf("SET ROLE palai_app: %v", err)
	}
	defer func() { _, _ = conn.Exec(ctx, `RESET ROLE`) }()

	if got := pgCode(mustFail(conn.Exec(ctx, `UPDATE capability_jobs SET receipt = '{}'::jsonb`))); got != "42501" {
		t.Fatalf("capability_jobs UPDATE code = %q, want 42501 (append-only: UPDATE withheld)", got)
	}
	if got := pgCode(mustFail(conn.Exec(ctx, `DELETE FROM capability_jobs`))); got != "42501" {
		t.Fatalf("capability_jobs DELETE code = %q, want 42501 (append-only: DELETE withheld)", got)
	}
}

// TestWorkerEnrollTypedDispatchAndArtifactRoundTrip is WRK-001/002/005: a worker enrolls for a typed
// capability, a typed job is dispatched, the worker claims it and submits a completed receipt carrying an
// output artifact ref. The journal records the whole lifecycle and the receipt round-trips.
func TestWorkerEnrollTypedDispatchAndArtifactRoundTrip(t *testing.T) {
	cs := openHarness(t)
	store := newStore(cs, fakeSecrets{}, nil)
	tenant := seedTenant(t, cs)
	w := enrolledWorker(t, store, tenant, map[string]string{"swiftc": "sha256:abc"})

	jobID, err := store.DispatchJob(context.Background(), tenant, workers.JobSpec{
		Capability: "swift-toolchain", Operation: "swift.build-check",
		InputRefs: []string{"art_in_1"}, Deadline: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("DispatchJob() error = %v", err)
	}

	claim, ok, err := store.ClaimNext(context.Background(), tenant, w.ID)
	if err != nil || !ok {
		t.Fatalf("ClaimNext() ok=%v err=%v, want a claim", ok, err)
	}
	if claim.JobID != jobID || claim.Operation != "swift.build-check" || !claim.ReadOnly {
		t.Fatalf("claim = %+v, want job %s op swift.build-check read-only", claim, jobID)
	}
	if len(claim.InputRefs) != 1 || claim.InputRefs[0] != "art_in_1" {
		t.Fatalf("claim input refs = %v, want [art_in_1]", claim.InputRefs)
	}

	if err := store.SubmitResult(context.Background(), claim, workers.Outcome{
		Class: "completed", Operation: "swift.build-check",
		Receipt:    map[string]any{"exit_code": float64(0), "toolchain": "real"},
		OutputRefs: []string{"art_out_1"},
	}); err != nil {
		t.Fatalf("SubmitResult() error = %v", err)
	}

	kind, receipt := latestJobEntry(t, cs, jobID)
	if kind != "completed" {
		t.Fatalf("terminal entry kind = %q, want completed", kind)
	}
	if !strings.Contains(receipt, "art_out_1") || !strings.Contains(receipt, "exit_code") {
		t.Fatalf("receipt = %s, want the output ref + exit_code round-tripped", receipt)
	}
	assertJournalKinds(t, cs, jobID, []string{"dispatched", "leased", "completed"})
}

// TestWorkerFenceStaleRejectOnRedispatch is the fence-stale-reject crown (§31.6, WRK-003): a job re-dispatched
// to another worker bumps its fence, and the first worker's result under the OLD fence is REJECTED with
// ErrStaleFence, while the new worker's result under the current fence is accepted. RED: removing the
// `cur.fence != claim.JobFence` guard in guardFences accepts the stale result.
func TestWorkerFenceStaleRejectOnRedispatch(t *testing.T) {
	cs := openHarness(t)
	store := newStore(cs, fakeSecrets{}, nil)
	tenant := seedTenant(t, cs)
	wa := enrolledWorker(t, store, tenant, nil)
	wb := enrolledWorker(t, store, tenant, nil)

	jobID, err := store.DispatchJob(context.Background(), tenant, workers.JobSpec{
		Capability: "swift-toolchain", Operation: "swift.build-check", Deadline: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("DispatchJob() error = %v", err)
	}

	claimA, ok, err := store.ClaimNext(context.Background(), tenant, wa.ID)
	if err != nil || !ok {
		t.Fatalf("ClaimNext(A) ok=%v err=%v", ok, err)
	}

	newFence, err := store.RedispatchForRetry(context.Background(), tenant, jobID)
	if err != nil {
		t.Fatalf("RedispatchForRetry() error = %v", err)
	}
	if newFence != claimA.JobFence+1 {
		t.Fatalf("new fence = %d, want %d (dispatch fence + 1)", newFence, claimA.JobFence+1)
	}

	err = store.SubmitResult(context.Background(), claimA, workers.Outcome{Class: "completed", Operation: "swift.build-check"})
	if !errors.Is(err, workers.ErrStaleFence) {
		t.Fatalf("stale SubmitResult error = %v, want ErrStaleFence (a superseded fence must not complete the job)", err)
	}

	claimB, ok, err := store.ClaimNext(context.Background(), tenant, wb.ID)
	if err != nil || !ok {
		t.Fatalf("ClaimNext(B) ok=%v err=%v", ok, err)
	}
	if claimB.JobFence != newFence {
		t.Fatalf("claim B fence = %d, want the current %d", claimB.JobFence, newFence)
	}
	if err := store.SubmitResult(context.Background(), claimB, workers.Outcome{Class: "completed", Operation: "swift.build-check"}); err != nil {
		t.Fatalf("SubmitResult(B) error = %v, want accepted at the current fence", err)
	}
}

// TestWorkerHealthChangeCutsLease is the worker-fence leg of §31.6: a health/capability change bumps the
// worker's enrollment fence, so a result under the pre-change worker fence is rejected with ErrWorkerFenced.
// RED: removing the worker-fence check in guardFences accepts it.
func TestWorkerHealthChangeCutsLease(t *testing.T) {
	cs := openHarness(t)
	store := newStore(cs, fakeSecrets{}, nil)
	tenant := seedTenant(t, cs)
	w := enrolledWorker(t, store, tenant, nil)

	if _, err := store.DispatchJob(context.Background(), tenant, workers.JobSpec{
		Capability: "swift-toolchain", Operation: "swift.build-check", Deadline: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("DispatchJob() error = %v", err)
	}
	claim, ok, err := store.ClaimNext(context.Background(), tenant, w.ID)
	if err != nil || !ok {
		t.Fatalf("ClaimNext() ok=%v err=%v", ok, err)
	}
	if _, err := store.SetWorkerHealth(context.Background(), tenant, w.ID, "draining"); err != nil {
		t.Fatalf("SetWorkerHealth() error = %v", err)
	}
	if err := store.SubmitResult(context.Background(), claim, workers.Outcome{Class: "completed", Operation: "swift.build-check"}); !errors.Is(err, workers.ErrWorkerFenced) {
		t.Fatalf("SubmitResult after health change error = %v, want ErrWorkerFenced (the lease was cut)", err)
	}
}

// TestSecretHandleScopeAndExpiry is the secret-handle crown (§31.5, WRK-004): a job-scoped, short-lived secret
// handle is redeemable ONLY for its own job and ONLY before the deadline; the resolved value never lands in
// the journal. RED: removing the scope check accepts a foreign handle; removing the deadline check redeems an
// expired one.
func TestSecretHandleScopeAndExpiry(t *testing.T) {
	const marker = "SUPER-SECRET-VALUE-do-not-leak-9f3a"
	// Anchor the fake clock to real now: the deadline is stored as a real timestamp and the DB's
	// clock_timestamp() gates the CLAIM, so the two clocks must agree at claim time. The expiry leg then
	// advances ONLY the store's clock past the deadline — redeem consults s.now(), not the DB.
	clock := &fakeClock{t: time.Now()}
	cs := openHarness(t)
	store := newStore(cs, fakeSecrets{vals: map[string]string{"build-cache-token": marker}}, clock.Now)
	tenant := seedTenant(t, cs)
	w := enrolledWorker(t, store, tenant, nil)

	deadline := clock.t.Add(time.Hour)
	jobID, err := store.DispatchJob(context.Background(), tenant, workers.JobSpec{
		Capability: "swift-toolchain", Operation: "swift.build-check",
		SecretHandleRefs: []string{"build-cache-token"}, Deadline: deadline,
	})
	if err != nil {
		t.Fatalf("DispatchJob() error = %v", err)
	}
	claim, ok, err := store.ClaimNext(context.Background(), tenant, w.ID)
	if err != nil || !ok {
		t.Fatalf("ClaimNext() ok=%v err=%v", ok, err)
	}

	val, err := store.RedeemSecretHandle(context.Background(), claim, "build-cache-token")
	if err != nil {
		t.Fatalf("RedeemSecretHandle() error = %v, want the value", err)
	}
	if string(val) != marker {
		t.Fatalf("redeemed value = %q, want the marker", val)
	}

	if _, err := store.RedeemSecretHandle(context.Background(), claim, "some-other-secret"); !errors.Is(err, workers.ErrHandleNotScoped) {
		t.Fatalf("redeem of an unscoped handle error = %v, want ErrHandleNotScoped", err)
	}

	clock.t = deadline.Add(time.Second)
	if _, err := store.RedeemSecretHandle(context.Background(), claim, "build-cache-token"); !errors.Is(err, workers.ErrHandleExpired) {
		t.Fatalf("redeem after the deadline error = %v, want ErrHandleExpired", err)
	}

	assertJournalHasNoSecret(t, cs, jobID, marker)
}

// TestQuarantineOnUncertain is WRK-007 (§31.6): an UNCERTAIN failure quarantines the job rather than
// completing or blindly retrying it — a side effect may or may not have happened.
func TestQuarantineOnUncertain(t *testing.T) {
	cs := openHarness(t)
	store := newStore(cs, fakeSecrets{}, nil)
	tenant := seedTenant(t, cs)
	w := enrolledWorker(t, store, tenant, nil)

	jobID, err := store.DispatchJob(context.Background(), tenant, workers.JobSpec{
		Capability: "swift-toolchain", Operation: "swift.build-check", Deadline: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("DispatchJob() error = %v", err)
	}
	claim, ok, err := store.ClaimNext(context.Background(), tenant, w.ID)
	if err != nil || !ok {
		t.Fatalf("ClaimNext() ok=%v err=%v", ok, err)
	}
	if err := store.SubmitResult(context.Background(), claim, workers.Outcome{
		Class: "uncertain", Operation: "swift.build-check", Receipt: map[string]any{"reason": "host lost mid-op"},
	}); err != nil {
		t.Fatalf("SubmitResult(uncertain) error = %v", err)
	}
	if kind, _ := latestJobEntry(t, cs, jobID); kind != "quarantined" {
		t.Fatalf("terminal entry for an uncertain outcome = %q, want quarantined", kind)
	}
}

// TestNoTunnelSubmitForForeignOperationRefused is the no-tunnel crown, submit leg (§31.5, WRK-006): a worker
// cannot repurpose a claim by submitting a result for a DIFFERENT operation than assigned — the submit is
// refused and no terminal entry is written. RED: removing the operation-match guard in SubmitResult accepts
// the re-typed submit.
func TestNoTunnelSubmitForForeignOperationRefused(t *testing.T) {
	cs := openHarness(t)
	store := newStore(cs, fakeSecrets{}, nil)
	tenant := seedTenant(t, cs)
	w := enrolledWorker(t, store, tenant, nil)

	jobID, err := store.DispatchJob(context.Background(), tenant, workers.JobSpec{
		Capability: "swift-toolchain", Operation: "swift.build-check", Deadline: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("DispatchJob() error = %v", err)
	}
	claim, ok, err := store.ClaimNext(context.Background(), tenant, w.ID)
	if err != nil || !ok {
		t.Fatalf("ClaimNext() ok=%v err=%v", ok, err)
	}
	if err := store.SubmitResult(context.Background(), claim, workers.Outcome{Class: "completed", Operation: "tunnel.connect"}); !errors.Is(err, workers.ErrUntypedOperation) {
		t.Fatalf("re-typed submit error = %v, want ErrUntypedOperation (no tunnel)", err)
	}
	assertJournalKinds(t, cs, jobID, []string{"dispatched", "leased"})
}

// TestCapabilityWorkerCrossTenantIsolation proves RLS confines workers + jobs across tenants, and that
// DispatchJob is idempotent within a tenant.
func TestCapabilityWorkerCrossTenantIsolation(t *testing.T) {
	cs := openHarness(t)
	store := newStore(cs, fakeSecrets{}, nil)
	tenantA := seedTenant(t, cs)
	tenantB := seedTenant(t, cs)
	wa := enrolledWorker(t, store, tenantA, nil)

	if _, ok, err := store.GetWorker(context.Background(), tenantB, wa.ID); err != nil || ok {
		t.Fatalf("GetWorker(B, A's worker) ok=%v err=%v, want not-found (RLS)", ok, err)
	}

	spec := workers.JobSpec{Capability: "swift-toolchain", Operation: "swift.build-check", IdempotencyKey: "idem-1", Deadline: time.Now().Add(time.Hour)}
	job1, err := store.DispatchJob(context.Background(), tenantA, spec)
	if err != nil {
		t.Fatalf("DispatchJob() error = %v", err)
	}
	job2, err := store.DispatchJob(context.Background(), tenantA, spec)
	if err != nil {
		t.Fatalf("DispatchJob() re-dispatch error = %v", err)
	}
	if job1 != job2 {
		t.Fatalf("idempotent dispatch produced two jobs %s / %s", job1, job2)
	}

	wb := enrolledWorker(t, store, tenantB, nil)
	if _, ok, err := store.ClaimNext(context.Background(), tenantB, wb.ID); err != nil || ok {
		t.Fatalf("ClaimNext(B) ok=%v err=%v, want no cross-tenant job", ok, err)
	}
}

// --- fence-TOCTOU: the guarded append closes the guard-then-append window ----------------------------------
//
// The §31.6 fence guard (guardFences) is one read; the journal append is a second statement. Under real
// concurrency a re-dispatch / health change can commit BETWEEN them, so a guard that passed at fence N lets a
// stale terminal (or a stale lease) land after the fence moved to N+1 — the crown security hole. The tests
// below make that interleave DETERMINISTIC with a test-only seam (SetAfterFenceGuardHook) that fires in
// exactly that window, then assert the GUARDED append rejects the stale write and the re-dispatch stays the
// journal's latest/claimable entry. RED against an unguarded append (the stale write lands); GREEN with the
// in-statement fence predicate (AppendGuardedJobEntry).

// TestSubmitFenceTOCTOUInterleaveRejected is the submit leg: a re-dispatch bumps the job fence in the window
// between SubmitResult's guard and its append. The stale 'completed' must NOT land (ErrStaleFence), and the
// re-dispatch stays the latest entry, claimable by the next worker.
func TestSubmitFenceTOCTOUInterleaveRejected(t *testing.T) {
	cs := openHarness(t)
	store := newStore(cs, fakeSecrets{}, nil)
	tenant := seedTenant(t, cs)
	wa := enrolledWorker(t, store, tenant, nil)
	wb := enrolledWorker(t, store, tenant, nil)

	jobID, err := store.DispatchJob(context.Background(), tenant, workers.JobSpec{
		Capability: "swift-toolchain", Operation: "swift.build-check", Deadline: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("DispatchJob() error = %v", err)
	}
	claimA, ok, err := store.ClaimNext(context.Background(), tenant, wa.ID)
	if err != nil || !ok {
		t.Fatalf("ClaimNext(A) ok=%v err=%v", ok, err)
	}

	var newFence int64
	workers.SetAfterFenceGuardHook(func() {
		workers.SetAfterFenceGuardHook(nil) // fire once
		nf, e := store.RedispatchForRetry(context.Background(), tenant, jobID)
		if e != nil {
			t.Errorf("interleaved RedispatchForRetry() error = %v", e)
		}
		newFence = nf
	})
	t.Cleanup(func() { workers.SetAfterFenceGuardHook(nil) })

	err = store.SubmitResult(context.Background(), claimA, workers.Outcome{Class: "completed", Operation: "swift.build-check"})
	if !errors.Is(err, workers.ErrStaleFence) {
		t.Fatalf("interleaved stale SubmitResult error = %v, want ErrStaleFence (the guarded append must reject the terminal that raced a re-dispatch)", err)
	}
	// The stale 'completed' never landed: the latest entry is the re-dispatch, not a terminal.
	assertJournalKinds(t, cs, jobID, []string{"dispatched", "leased", "dispatched"})
	// And the re-dispatch is NOT buried — worker B claims it at the current fence and completes it.
	claimB, ok, err := store.ClaimNext(context.Background(), tenant, wb.ID)
	if err != nil || !ok {
		t.Fatalf("ClaimNext(B) after interleave ok=%v err=%v (the re-dispatch was buried under a stale terminal)", ok, err)
	}
	if claimB.JobFence != newFence {
		t.Fatalf("claim B fence = %d, want the re-dispatch fence %d", claimB.JobFence, newFence)
	}
	if err := store.SubmitResult(context.Background(), claimB, workers.Outcome{Class: "completed", Operation: "swift.build-check"}); err != nil {
		t.Fatalf("SubmitResult(B) error = %v, want accepted at the current fence", err)
	}
}

// TestSubmitWorkerFenceTOCTOUInterleaveRejected is the worker-fence leg: a health change bumps the worker's
// enrollment fence between SubmitResult's guard and its append. The stale terminal must be rejected
// (ErrWorkerFenced) and no terminal entry lands.
func TestSubmitWorkerFenceTOCTOUInterleaveRejected(t *testing.T) {
	cs := openHarness(t)
	store := newStore(cs, fakeSecrets{}, nil)
	tenant := seedTenant(t, cs)
	w := enrolledWorker(t, store, tenant, nil)

	jobID, err := store.DispatchJob(context.Background(), tenant, workers.JobSpec{
		Capability: "swift-toolchain", Operation: "swift.build-check", Deadline: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("DispatchJob() error = %v", err)
	}
	claim, ok, err := store.ClaimNext(context.Background(), tenant, w.ID)
	if err != nil || !ok {
		t.Fatalf("ClaimNext() ok=%v err=%v", ok, err)
	}

	workers.SetAfterFenceGuardHook(func() {
		workers.SetAfterFenceGuardHook(nil)
		if _, e := store.SetWorkerHealth(context.Background(), tenant, w.ID, "draining"); e != nil {
			t.Errorf("interleaved SetWorkerHealth() error = %v", e)
		}
	})
	t.Cleanup(func() { workers.SetAfterFenceGuardHook(nil) })

	err = store.SubmitResult(context.Background(), claim, workers.Outcome{Class: "completed", Operation: "swift.build-check"})
	if !errors.Is(err, workers.ErrWorkerFenced) {
		t.Fatalf("interleaved worker-fence SubmitResult error = %v, want ErrWorkerFenced (the guarded append must reject a terminal from a just-drained worker)", err)
	}
	assertJournalKinds(t, cs, jobID, []string{"dispatched", "leased"})
}

// TestClaimFenceTOCTOUInterleaveDoesNotBuryRedispatch is the claim leg: a re-dispatch bumps the fence between
// ClaimNext's ready read and its 'leased' append. The stale lease must NOT land (ClaimNext misses cleanly),
// so the re-dispatched job stays the latest entry and is claimable by the next worker — never wedged under a
// stale 'leased' at a superseded fence.
func TestClaimFenceTOCTOUInterleaveDoesNotBuryRedispatch(t *testing.T) {
	cs := openHarness(t)
	store := newStore(cs, fakeSecrets{}, nil)
	tenant := seedTenant(t, cs)
	wa := enrolledWorker(t, store, tenant, nil)
	wb := enrolledWorker(t, store, tenant, nil)

	jobID, err := store.DispatchJob(context.Background(), tenant, workers.JobSpec{
		Capability: "swift-toolchain", Operation: "swift.build-check", Deadline: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("DispatchJob() error = %v", err)
	}

	var newFence int64
	workers.SetAfterFenceGuardHook(func() {
		workers.SetAfterFenceGuardHook(nil)
		nf, e := store.RedispatchForRetry(context.Background(), tenant, jobID)
		if e != nil {
			t.Errorf("interleaved RedispatchForRetry() error = %v", e)
		}
		newFence = nf
	})
	t.Cleanup(func() { workers.SetAfterFenceGuardHook(nil) })

	claimA, ok, err := store.ClaimNext(context.Background(), tenant, wa.ID)
	if err != nil {
		t.Fatalf("ClaimNext(A) during interleave error = %v, want a clean no-claim", err)
	}
	if ok {
		t.Fatalf("ClaimNext(A) landed a lease at a stale fence (%+v); the guarded append must have missed", claimA)
	}
	// The stale 'leased' never landed — only the two dispatch entries exist.
	assertJournalKinds(t, cs, jobID, []string{"dispatched", "dispatched"})
	// The re-dispatch is claimable by the next worker at the current fence.
	claimB, ok, err := store.ClaimNext(context.Background(), tenant, wb.ID)
	if err != nil || !ok {
		t.Fatalf("ClaimNext(B) ok=%v err=%v (the re-dispatch was buried under a stale lease)", ok, err)
	}
	if claimB.JobFence != newFence {
		t.Fatalf("claim B fence = %d, want the re-dispatch fence %d", claimB.JobFence, newFence)
	}
}

// TestForeignLeaseholderSubmitAndRedeemRefused is the SHOULD-FIX 2 crown: fences are small, guessable ints, so
// a caller can construct a claim for a job it never leased. Such a foreign claim (worker B against worker A's
// leased job) must be refused at BOTH submit and redeem — it does not hold the current lease — even though its
// job/worker fences match. Without the leaseholder check it could terminalize the job or redeem its secret.
func TestForeignLeaseholderSubmitAndRedeemRefused(t *testing.T) {
	const marker = "LEASE-SECRET-do-not-leak-7c1d"
	cs := openHarness(t)
	store := newStore(cs, fakeSecrets{vals: map[string]string{"build-cache-token": marker}}, nil)
	tenant := seedTenant(t, cs)
	wa := enrolledWorker(t, store, tenant, nil)
	wb := enrolledWorker(t, store, tenant, nil)

	jobID, err := store.DispatchJob(context.Background(), tenant, workers.JobSpec{
		Capability: "swift-toolchain", Operation: "swift.build-check",
		SecretHandleRefs: []string{"build-cache-token"}, Deadline: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("DispatchJob() error = %v", err)
	}
	claimA, ok, err := store.ClaimNext(context.Background(), tenant, wa.ID)
	if err != nil || !ok {
		t.Fatalf("ClaimNext(A) ok=%v err=%v", ok, err)
	}

	// B forges a claim for A's job at the same (guessable) fences, having never held the lease.
	forged := claimA
	forged.WorkerID = wb.ID
	forged.WorkerFence = wb.Fence

	if err := store.SubmitResult(context.Background(), forged, workers.Outcome{Class: "completed", Operation: "swift.build-check"}); !errors.Is(err, workers.ErrNotLeaseholder) {
		t.Fatalf("foreign-leaseholder submit error = %v, want ErrNotLeaseholder", err)
	}
	if _, err := store.RedeemSecretHandle(context.Background(), forged, "build-cache-token"); !errors.Is(err, workers.ErrNotLeaseholder) {
		t.Fatalf("foreign-leaseholder redeem error = %v, want ErrNotLeaseholder", err)
	}
	// Nothing terminalized under B, and A's genuine lease still completes.
	assertJournalKinds(t, cs, jobID, []string{"dispatched", "leased"})
	if err := store.SubmitResult(context.Background(), claimA, workers.Outcome{Class: "completed", Operation: "swift.build-check"}); err != nil {
		t.Fatalf("SubmitResult(A) the genuine leaseholder error = %v, want accepted", err)
	}
}

// TestDispatchRefusesReopeningExistingJob is MINOR 4: a dispatch onto a job_id that already has entries would
// re-open a terminal job into a wedged state (a fresh 'dispatched' at fence 1 whose fence is below MAX). It is
// refused; a re-dispatch is RedispatchForRetry.
func TestDispatchRefusesReopeningExistingJob(t *testing.T) {
	cs := openHarness(t)
	store := newStore(cs, fakeSecrets{}, nil)
	tenant := seedTenant(t, cs)

	jobID, err := store.DispatchJob(context.Background(), tenant, workers.JobSpec{
		Capability: "swift-toolchain", Operation: "swift.build-check", Deadline: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("DispatchJob() error = %v", err)
	}
	if _, err := store.DispatchJob(context.Background(), tenant, workers.JobSpec{
		JobID: jobID, Capability: "swift-toolchain", Operation: "swift.build-check", Deadline: time.Now().Add(time.Hour),
	}); !errors.Is(err, workers.ErrJobExists) {
		t.Fatalf("re-dispatch onto an existing job_id error = %v, want ErrJobExists", err)
	}
	assertJournalKinds(t, cs, jobID, []string{"dispatched"})
}
