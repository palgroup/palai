//go:build component

// The E17 T11 EXIT-gate WORKER JOURNEY (plan §T11): enroll → typed job → artifact round-trip → fence reject →
// no-tunnel negative, chained as ONE sequence on ONE worker against real PostgreSQL. WRK-001..007 each pin an
// invariant in isolation; this journey proves they hold together over a single worker's lifetime and emits the
// uat.WorkerFenceProof the extensions-0.1.0 bundle carries.
//
// HONEST CEILING, the most important one in E17: NOTHING here is a macOS/iOS BUILD claim. What is proven is
// the outbound-enrolled, lease/FENCED, typed-operation contract plus its two negatives — an untyped operation
// is refused (there is no SOCKS-like passthrough, §31.5) and a job-scoped secret handle expires without its
// value ever entering the append-only journal. `apple-build` has NO catalog entry at all: no signing
// certificate, provisioning profile or store credential exists anywhere in this repo, and the journey asserts
// that absence rather than working around it. A real signed Apple build is §6 leg 3.
package workers_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/workers"
	"github.com/palgroup/palai/tests/uat"
)

// TestWorkerJourney is the E17 CapabilityWorker EXIT journey.
func TestWorkerJourney(t *testing.T) {
	const secretMarker = "JOURNEY-SECRET-VALUE-must-never-appear-4b71"
	clock := &fakeClock{t: time.Now()}
	cs := openHarness(t)
	store := newStore(cs, fakeSecrets{vals: map[string]string{"journey-build-cache": secretMarker}}, clock.Now)
	tenant := seedTenant(t, cs)
	ctx := context.Background()

	// ---- 0. the absence that is never worked around -------------------------------------------------
	// A typed capability the deployment cannot honestly serve must not exist at all. This is asserted
	// FIRST so the rest of the journey cannot quietly rely on an apple-build path appearing later.
	if workers.KnownCapability("apple-build") {
		t.Fatal("step 0: apple-build has a catalog entry — no signing material exists, so the capability must be ABSENT (§6 leg 3)")
	}
	if _, err := store.Enroll(ctx, tenant, workers.WorkerSpec{Capability: "apple-build"}); !errors.Is(err, workers.ErrUnknownCapability) {
		t.Fatalf("step 0: enrolling an apple-build worker = %v, want ErrUnknownCapability", err)
	}

	// ---- 1. ENROLL: outbound enrollment, one worker, no inbound port --------------------------------
	worker := enrolledWorker(t, store, tenant, map[string]string{"swiftc": "sha256:" + strings.Repeat("1", 64)})
	if worker.ID == "" {
		t.Fatal("step 1: enrollment produced no worker id")
	}

	// ---- 2. TYPED JOB with an input artifact + a job-scoped secret handle ---------------------------
	deadline := clock.t.Add(time.Hour)
	jobID, err := store.DispatchJob(ctx, tenant, workers.JobSpec{
		Capability: "swift-toolchain", Operation: "swift.build-check",
		InputRefs:        []string{"art_journey_in"},
		SecretHandleRefs: []string{"journey-build-cache"},
		Deadline:         deadline,
	})
	if err != nil {
		t.Fatalf("step 2: dispatch typed job: %v", err)
	}
	claim, ok, err := store.ClaimNext(ctx, tenant, worker.ID)
	if err != nil || !ok {
		t.Fatalf("step 2: claim = (ok=%v,%v), want the dispatched job", ok, err)
	}
	if claim.JobID != jobID || claim.Operation != "swift.build-check" {
		t.Fatalf("step 2: claim = %+v, want job %s / swift.build-check", claim, jobID)
	}

	// The secret handle is JOB-SCOPED: this job's ref redeems, any other name does not.
	val, err := store.RedeemSecretHandle(ctx, claim, "journey-build-cache")
	if err != nil || string(val) != secretMarker {
		t.Fatalf("step 2: redeem the job-scoped handle = (%q,%v), want the marker", val, err)
	}
	if _, err := store.RedeemSecretHandle(ctx, claim, "some-other-secret"); !errors.Is(err, workers.ErrHandleNotScoped) {
		t.Fatalf("step 2: redeeming an UNSCOPED handle = %v, want ErrHandleNotScoped", err)
	}
	// … and it is SHORT-LIVED: past the job deadline the very same claim redeems nothing. Probed while the
	// lease is still held, so the failure is genuinely the EXPIRY and not a lost lease.
	clock.t = deadline.Add(time.Second)
	if _, err := store.RedeemSecretHandle(ctx, claim, "journey-build-cache"); !errors.Is(err, workers.ErrHandleExpired) {
		t.Fatalf("step 2: redeem after the deadline = %v, want ErrHandleExpired", err)
	}
	clock.t = time.Now()

	// ---- 3. NO-TUNNEL NEGATIVE (§31.5, the crown): an untyped operation is refused ------------------
	// Refused at DISPATCH (the operation is not in the capability's catalog) …
	var refusedOperations []string
	for _, operation := range []string{"tunnel.connect", "shell.exec", "socks.proxy"} {
		if _, err := store.DispatchJob(ctx, tenant, workers.JobSpec{
			Capability: "swift-toolchain", Operation: operation, Deadline: deadline,
		}); !errors.Is(err, workers.ErrUntypedOperation) {
			t.Fatalf("step 3: dispatching %q = %v, want ErrUntypedOperation — a worker is not a tunnel", operation, err)
		}
		refusedOperations = append(refusedOperations, operation)
	}
	// … and again at SUBMIT, so a claimed job cannot be re-typed into a tunnel after the fact.
	if err := store.SubmitResult(ctx, claim, workers.Outcome{Class: "completed", Operation: "tunnel.connect"}); !errors.Is(err, workers.ErrUntypedOperation) {
		t.Fatalf("step 3: re-typed submit = %v, want ErrUntypedOperation", err)
	}

	// ---- 4. ARTIFACT ROUND-TRIP: input in → build-check → output artifact + execution receipt -------
	if err := store.SubmitResult(ctx, claim, workers.Outcome{
		Class: "completed", Operation: "swift.build-check",
		Receipt:    map[string]any{"exit_code": float64(0), "toolchain": "deterministic"},
		OutputRefs: []string{"art_journey_out"},
	}); err != nil {
		t.Fatalf("step 4: submit the typed result: %v", err)
	}
	kind, receipt := latestJobEntry(t, cs, jobID)
	if kind != "completed" || !strings.Contains(receipt, "art_journey_out") || !strings.Contains(receipt, "exit_code") {
		t.Fatalf("step 4: terminal journal entry = (%q,%s), want completed with the output ref + receipt", kind, receipt)
	}
	assertJournalKinds(t, cs, jobID, []string{"dispatched", "leased", "completed"})

	// The secret VALUE never entered the append-only journal — the whole point of a handle.
	assertJournalHasNoSecret(t, cs, jobID, secretMarker)

	// ---- 5. FENCE REJECT: a re-dispatched job fences out the previous leaseholder -------------------
	second := enrolledWorker(t, store, tenant, nil)
	fencedJobID, err := store.DispatchJob(ctx, tenant, workers.JobSpec{
		Capability: "swift-toolchain", Operation: "swift.build-check", Deadline: clock.t.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("step 5: dispatch the fence job: %v", err)
	}
	stale, ok, err := store.ClaimNext(ctx, tenant, worker.ID)
	if err != nil || !ok || stale.JobID != fencedJobID {
		t.Fatalf("step 5: first claim = (%+v,ok=%v,%v)", stale, ok, err)
	}
	newFence, err := store.RedispatchForRetry(ctx, tenant, fencedJobID)
	if err != nil {
		t.Fatalf("step 5: redispatch: %v", err)
	}
	if newFence != stale.JobFence+1 {
		t.Fatalf("step 5: fence = %d, want the monotonic %d", newFence, stale.JobFence+1)
	}
	if err := store.SubmitResult(ctx, stale, workers.Outcome{Class: "completed", Operation: "swift.build-check"}); !errors.Is(err, workers.ErrStaleFence) {
		t.Fatalf("step 5: the SUPERSEDED leaseholder's result = %v, want ErrStaleFence", err)
	}
	fresh, ok, err := store.ClaimNext(ctx, tenant, second.ID)
	if err != nil || !ok || fresh.JobFence != newFence {
		t.Fatalf("step 5: the new leaseholder's claim = (%+v,ok=%v,%v), want fence %d", fresh, ok, err, newFence)
	}
	if err := store.SubmitResult(ctx, fresh, workers.Outcome{Class: "completed", Operation: "swift.build-check"}); err != nil {
		t.Fatalf("step 5: the CURRENT fence's result must be accepted: %v", err)
	}

	// ---- the EXIT-gate proof ------------------------------------------------------------------------
	proof := uat.WorkerFenceProof{
		WorkerID:                  worker.ID,
		Capability:                "swift-toolchain/swift.build-check",
		StaleFenceRejected:        true,
		NoTunnelRefusedOperations: refusedOperations,
		TunnelSucceeded:           false,
		SecretHandleScope:         "job",
		SecretHandleExpired:       true,
		SecretValueInJournal:      false,
		AppleBuildAdvertised:      workers.KnownCapability("apple-build"),
	}
	if !proof.Complete() {
		t.Fatalf("the journey's WorkerFenceProof is not COMPLETE: %+v", proof)
	}
	t.Logf("worker journey PASS: enrolled outbound, typed job round-tripped an artifact with an execution receipt, %d untyped operations REFUSED (no tunnel), stale fence rejected, job-scoped secret handle expired with its value absent from the journal, apple-build ABSENT. A real signed Apple build is §6 leg 3 — NOT claimed.",
		len(proof.NoTunnelRefusedOperations))
}
