package execution

import (
	"context"
	"testing"
	"time"
)

// fakeReclaimer records the ceiling the reconciler sweeps with and returns a fixed
// dead-letter count.
type fakeReclaimer struct {
	sawMaxAttempts int
	swept          int
}

func (f *fakeReclaimer) ReclaimExpired(_ context.Context, maxAttempts int) (int, error) {
	f.sawMaxAttempts = maxAttempts
	return f.swept, nil
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
}
