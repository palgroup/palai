package coordinator

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// TestSupervisedLoopRecoversPanicWithCounter proves the kill-matrix panic path (E08 §7.2 M2):
// a supervised loop that PANICS must not take the process down with it. The panic is recovered
// into a restart — counted, backed off, and re-run — exactly as a returned error is, so a loop
// that panics twice then finishes cleanly leaves the supervisor alive with a restart count of 2.
func TestSupervisedLoopRecoversPanicWithCounter(t *testing.T) {
	sup := NewSupervisor(nil, time.Millisecond)
	var calls atomic.Int32
	fn := func(context.Context) error {
		if n := calls.Add(1); n <= 2 {
			panic(fmt.Sprintf("boom %d", n))
		}
		return nil // the third run finishes its work and is not restarted
	}

	done := make(chan struct{})
	go func() {
		sup.Supervise(context.Background(), "loop", fn)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervised loop did not survive the panics (no recover: the goroutine would crash the process)")
	}

	if got := calls.Load(); got != 3 {
		t.Fatalf("fn called %d times, want 3 (two panics recovered, third clean)", got)
	}
	if got := sup.Restarts()["loop"]; got != 2 {
		t.Fatalf("restart count = %d, want 2 (each recovered panic is a counted restart)", got)
	}
}

// TestSupervisedPanicLoopStopsOnCancel proves a loop that always panics still shuts down cleanly
// on cancellation — the recover must not turn a cancel into an infinite restart spin.
func TestSupervisedPanicLoopStopsOnCancel(t *testing.T) {
	sup := NewSupervisor(nil, time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	fn := func(context.Context) error { panic("always") }

	done := make(chan struct{})
	go func() {
		sup.Supervise(ctx, "loop", fn)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervised panic loop did not stop on cancel")
	}
}
