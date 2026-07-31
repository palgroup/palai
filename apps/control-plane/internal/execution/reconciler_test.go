package execution

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/coordinator"
)

// fakeReclaimer records the ceiling the reconciler sweeps with and returns a fixed
// dead-letter count. It also records that the dead-letter bridge sweep ran.
type fakeReclaimer struct {
	sawMaxAttempts int
	swept          int
	bridgeSweeps   int
	approvalSweeps int
	toolSweeps     int
	// capacitySweeps counts the passes and capacityTTL records the TTL each pass was given, so a test can
	// assert that an UNCONFIGURED deployment still gets the call (with zero, which the store treats as a
	// no-op) rather than the sweep being skipped by the caller — a skip that would make the knob look
	// wired while doing nothing.
	capacitySweeps int
	capacityTTL    time.Duration
	capacityBody   []byte
	// backgroundSweeps counts E26 T4's passes and backgroundObserved records whether the pass carried an
	// observer, for the same reason capacityTTL is recorded: the opt-out must reach the store as a nil
	// rather than as a call the caller skipped.
	backgroundSweeps   int
	backgroundObserved bool
	// logRetentionReads counts E26 T5's log garbage passes.
	logRetentionReads int
}

func (f *fakeReclaimer) ReclaimExpired(_ context.Context, maxAttempts int) (int, error) {
	f.sawMaxAttempts = maxAttempts
	return f.swept, nil
}

func (f *fakeReclaimer) SweepDeadLetteredRuns(_ context.Context) (int, error) {
	f.bridgeSweeps++
	return 0, nil
}

func (f *fakeReclaimer) SweepExpiredApprovals(_ context.Context) (int, error) {
	f.approvalSweeps++
	return 0, nil
}

func (f *fakeReclaimer) SweepExpiredToolApprovals(_ context.Context) (int, error) {
	f.toolSweeps++
	return 0, nil
}

func (f *fakeReclaimer) SweepExpiredCapacityParks(_ context.Context, ttl time.Duration, projection []byte) (int, error) {
	f.capacitySweeps++
	f.capacityTTL = ttl
	f.capacityBody = projection
	return 0, nil
}

// SweepFinishedBackgroundTasks records the observer the pass was handed (E26 T4). The OBSERVER is what
// is recorded rather than a bare counter, because the interesting property of this sweep is that a
// deployment with no background runner hands it nil — and a nil that still reached the store is the
// difference between "opted out" and "silently broken".
func (f *fakeReclaimer) SweepFinishedBackgroundTasks(_ context.Context, observe coordinator.BackgroundObserver) (int, error) {
	f.backgroundSweeps++
	f.backgroundObserved = observe != nil
	return 0, nil
}

// BackgroundLogRetention counts the log garbage sweep's reads (E26 T5). It returns NO roots, so the pass
// walks nothing: what this fake is for is proving the reconciler asks at all, and the deletion itself is
// proven against a real filesystem in the component tier.
func (f *fakeReclaimer) BackgroundLogRetention(context.Context) ([]string, map[string]bool, error) {
	f.logRetentionReads++
	return nil, nil, nil
}

// TestReconcilerSweepsCapacityParksWithTheConfiguredTTL is E24 T5's half of this file. The two assertions
// are: an unconfigured deployment still REACHES the sweep (so the no-op lives in one place — the store —
// rather than being a caller-side skip somebody has to remember), and a configured TTL arrives intact along
// with the terminal projection whose detail names the reason.
func TestReconcilerSweepsCapacityParksWithTheConfiguredTTL(t *testing.T) {
	rec := &fakeReclaimer{}
	if _, err := NewReconciler(rec, time.Second, 5).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep with no park TTL: %v", err)
	}
	if rec.capacitySweeps != 1 || rec.capacityTTL != 0 {
		t.Fatalf("unconfigured pass = %d sweep(s) with ttl %v, want 1 and 0", rec.capacitySweeps, rec.capacityTTL)
	}
	if _, err := NewReconciler(rec, time.Second, 5).WithCapacityParkTTL(9 * time.Minute).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep with a park TTL: %v", err)
	}
	if rec.capacityTTL != 9*time.Minute {
		t.Fatalf("configured pass carried ttl %v, want 9m", rec.capacityTTL)
	}
	if !strings.Contains(string(rec.capacityBody), "no runner joined this run's pool") {
		t.Fatalf("the terminal projection does not name the reason: %s — an expiry a caller cannot read is a silent death", rec.capacityBody)
	}
}

func TestReconcilerSweepReportsDeadLetteredWithConfiguredCeiling(t *testing.T) {
	rec := &fakeReclaimer{swept: 3}
	r := NewReconciler(rec, time.Second, 5)
	got, err := r.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}
	if got != 3 {
		t.Fatalf("swept = %d, want 3", got)
	}
	if rec.sawMaxAttempts != 5 {
		t.Fatalf("sweep ceiling = %d, want 5", rec.sawMaxAttempts)
	}
	if rec.bridgeSweeps != 1 {
		t.Fatalf("dead-letter bridge sweeps = %d, want 1 (each pass must bridge dead-lettered runs)", rec.bridgeSweeps)
	}
	if rec.approvalSweeps != 1 {
		t.Fatalf("expired-approval sweeps = %d, want 1 (each pass must expire idle-elapsed approvals, E10 T7)", rec.approvalSweeps)
	}
	// The gated-tool half is not optional and not decorative: it is the only thing that releases a run
	// parked on a question nobody answered (E23 T1). A pass that skipped it would leave that run waiting
	// for as long as the deployment lives.
	if rec.toolSweeps != 1 {
		t.Fatalf("expired-tool-approval sweeps = %d, want 1 (each pass must release runs parked on lapsed approvals)", rec.toolSweeps)
	}
}
