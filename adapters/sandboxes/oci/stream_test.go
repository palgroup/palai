package oci

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestContainerExitDrainsStreamBeforeReap proves the REC-001 tail-frame fix at the unit
// level: destroy() must not tear down the attach connection (reap: ContainerRemove +
// attach.Close) until the stdcopy demux goroutine has reached EOF, so a fast-exit engine's
// buffered run.terminal is fully copied into the stdout pipe before the stream is severed.
// The ordering is the whole point — a reap before demux EOF is exactly the lost-terminal
// race. It is also bounded: a wedged demux must not hang teardown forever.
func TestContainerExitDrainsStreamBeforeReap(t *testing.T) {
	t.Run("reap waits for demux EOF then fires", func(t *testing.T) {
		demuxDone := make(chan struct{})
		var reaped atomic.Bool
		done := make(chan struct{})
		go func() {
			drainThenReap(demuxDone, time.Second, func() { reaped.Store(true) })
			close(done)
		}()

		// While the demux is still draining, the reap MUST NOT have fired.
		time.Sleep(20 * time.Millisecond)
		if reaped.Load() {
			t.Fatal("reap fired before the demux reached EOF: the tail frame can be severed mid-drain")
		}

		close(demuxDone) // demux reaches EOF: the stdout pipe is fully drained
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("reap did not fire after the demux reached EOF")
		}
		if !reaped.Load() {
			t.Fatal("reap never fired after demux EOF")
		}
	})

	t.Run("reap fires after the drain timeout when demux is wedged", func(t *testing.T) {
		demuxDone := make(chan struct{}) // never closed: a wedged demux
		var reaped atomic.Bool
		start := time.Now()
		drainThenReap(demuxDone, 30*time.Millisecond, func() { reaped.Store(true) })
		if !reaped.Load() {
			t.Fatal("reap never fired: a wedged demux must not hang teardown forever")
		}
		if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
			t.Fatalf("reap fired after %s, before the drain timeout: the drain gate is not bounded correctly", elapsed)
		}
	})
}
