package execution

import (
	"context"
	"testing"
	"time"
)

// fakeReclaimer records the ceiling the reconciler sweeps with and returns a fixed
// dead-letter count. It also records that the dead-letter bridge sweep ran.
type fakeReclaimer struct {
	sawMaxAttempts int
	swept          int
	bridgeSweeps   int
	approvalSweeps int
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
}
