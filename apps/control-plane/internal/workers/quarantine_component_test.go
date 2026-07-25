//go:build component

package workers_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/workers"
)

// quarantine_component_test.go is SEC-102's finding->quarantine BEHAVIOUR arm (E18 T7).
//
// WRK-007 (TestQuarantineOnUncertain) already proves the quarantine is RECORDED: an uncertain outcome
// appends a `quarantined` journal entry rather than `completed` or `failed`. What nothing proved is
// the CONSEQUENCE — and the consequence is the whole point. A quarantine that any worker can pick up
// again on its next poll is a comment, not a control: the §31.6 hazard is precisely a blind retry of
// an operation whose side effect may or may not have landed.
//
// So this arm asserts what happens AFTER the finding: no worker re-claims the job on its own, the
// quarantine is still the job's state once they have tried, and leaving quarantine takes a DELIBERATE
// re-dispatch that fences the worker whose outcome was uncertain in the first place.
//
// This is the "and when something IS uncertain, it quarantines" half of the escape suite. Its sibling
// is the E10 substrate half — a failed destroy quarantines the host and DENIES the next placement —
// which SAN-008 already owns and the suite runs as its own arm.

// TestUncertainOutcomeQuarantineIsNotSilentlyReclaimed drives the E17 T9 uncertain-failure quarantine
// seam and asserts the denial that must follow it.
//
// RED: delete the terminal-kind filter from the ready-job query and the quarantined job becomes
// claimable again — a worker retries an operation whose side effect is unknown.
func TestUncertainOutcomeQuarantineIsNotSilentlyReclaimed(t *testing.T) {
	cs := openHarness(t)
	store := newStore(cs, fakeSecrets{}, nil)
	tenant := seedTenant(t, cs)
	first := enrolledWorker(t, store, tenant, nil)
	// A SECOND compatible worker: the interesting failure is not "the same worker retries" but "some
	// other healthy worker helpfully picks up the poisoned job".
	second := enrolledWorker(t, store, tenant, nil)
	ctx := context.Background()

	jobID, err := store.DispatchJob(ctx, tenant, workers.JobSpec{
		Capability: "swift-toolchain", Operation: "swift.build-check", Deadline: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("DispatchJob() error = %v", err)
	}
	claim, ok, err := store.ClaimNext(ctx, tenant, first.ID)
	if err != nil || !ok {
		t.Fatalf("ClaimNext() ok=%v err=%v", ok, err)
	}

	// The FINDING: the worker cannot say whether its side effect landed.
	if err := store.SubmitResult(ctx, claim, workers.Outcome{
		Class: "uncertain", Operation: "swift.build-check",
		Receipt: map[string]any{"reason": "worker host lost between execute and record"},
	}); err != nil {
		t.Fatalf("SubmitResult(uncertain) error = %v", err)
	}
	if kind, _ := latestJobEntry(t, cs, jobID); kind != "quarantined" {
		t.Fatalf("terminal entry for an uncertain outcome = %q, want quarantined", kind)
	}

	// THE CONSEQUENCE: nobody picks it back up on their own.
	for _, w := range []struct {
		name string
		id   string
	}{{"the worker that reported uncertain", first.ID}, {"a second healthy worker", second.ID}} {
		if _, ok, err := store.ClaimNext(ctx, tenant, w.id); err != nil {
			t.Fatalf("ClaimNext(%s) error = %v", w.name, err)
		} else if ok {
			t.Fatalf("%s re-claimed a QUARANTINED job — an uncertain side effect would be blindly retried", w.name)
		}
	}
	// ... and the attempts left the quarantine standing, rather than quietly reopening it.
	if kind, _ := latestJobEntry(t, cs, jobID); kind != "quarantined" {
		t.Fatalf("after two claim attempts the job's state is %q, want it still quarantined", kind)
	}

	// LEAVING quarantine is a deliberate act, and it fences the worker whose outcome was uncertain:
	// that worker's stale claim can no longer submit anything against the re-dispatched job.
	if _, err := store.RedispatchForRetry(ctx, tenant, jobID); err != nil {
		t.Fatalf("RedispatchForRetry() error = %v", err)
	}
	if err := store.SubmitResult(ctx, claim, workers.Outcome{
		Class: "completed", Operation: "swift.build-check",
	}); !errors.Is(err, workers.ErrStaleFence) {
		t.Fatalf("the pre-quarantine claim submitted after a re-dispatch: err = %v, want ErrStaleFence", err)
	}
	if _, ok, err := store.ClaimNext(ctx, tenant, second.ID); err != nil || !ok {
		t.Fatalf("after a deliberate re-dispatch the job is not claimable: ok=%v err=%v", ok, err)
	}
}
