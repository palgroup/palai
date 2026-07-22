package mcp

import (
	"context"
	"io"
	"runtime"
	"testing"
	"time"
)

// infiniteReader is an endless stream of one newline-delimited notification line — a stand-in for a hostile
// server that keeps flooding notifications after it has already answered. It never blocks and never EOFs, so
// the readLoop keeps ingesting until inbound (cap 32) fills and the next send parks.
type infiniteReader struct{ line []byte }

func (r infiniteReader) Read(p []byte) (int, error) { return copy(p, r.line), nil }

// TestStdioReadLoopExitsAfterCloseUnderFlood proves the reader goroutine cannot leak: with no Call draining
// inbound, a notification flood parks readLoop on a full-channel send; Close must unblock it so it exits.
// Without the done-abort the goroutine stays parked forever (1 goroutine + up to 33×4MiB buffered per call).
func TestStdioReadLoopExitsAfterCloseUnderFlood(t *testing.T) {
	runtime.GC()
	base := runtime.NumGoroutine()

	r := infiniteReader{line: []byte(`{"jsonrpc":"2.0","method":"notifications/progress","params":{}}` + "\n")}
	tr := NewStdioTransport(io.Discard, r, func(context.Context) error { return nil })

	// The readLoop is now running; give it a beat to fill inbound and park on the send.
	if !waitForGoroutines(base+1, 2*time.Second) {
		t.Fatal("readLoop goroutine did not start")
	}
	time.Sleep(50 * time.Millisecond)

	if err := tr.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	// The parked readLoop must exit — the goroutine count returns to baseline.
	if !waitForGoroutines(base, 2*time.Second) {
		t.Fatalf("readLoop did not exit after Close under a notification flood (goroutine leak): now=%d base=%d",
			runtime.NumGoroutine(), base)
	}
}

// waitForGoroutines polls until the goroutine count is at or below target, or the deadline passes.
func waitForGoroutines(target int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= target {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return runtime.NumGoroutine() <= target
}
