//go:build e2e

package sse

import (
	"errors"
	"net"
	"testing"
	"time"
)

// TestSlowConsumerDroppedWithoutLoss floods the journal, attaches a stalled
// consumer, and proves the server drops it after delivering only a bounded prefix
// (never buffering the whole journal), while a fresh reader still receives every
// event contiguously — the journal, not the connection, is the source of truth.
func TestSlowConsumerDroppedWithoutLoss(t *testing.T) {
	h := newHarness(t)
	sessionID := h.seedSession()

	const nonTerminal = 1000
	const payloadBytes = 4096
	h.seedBulkEvents(sessionID, nonTerminal, payloadBytes) // seq 1..1000 padded, seq 1001 terminal

	// A consumer that never drains the stream.
	stalled := h.dialStalled(sessionID)
	defer stalled.Close()

	// Stay stalled past the injected write deadline; the server must drop us instead
	// of buffering the journal. This waits a safe multiple of the configured timeout.
	time.Sleep(6 * serverWriteTimeout())

	drained := drainUntilClose(t, stalled, 5*time.Second)
	if drained == 0 {
		t.Fatalf("stalled consumer read nothing; expected a bounded prefix then EOF")
	}
	if ceiling := nonTerminal * payloadBytes / 2; drained >= ceiling {
		t.Fatalf("stalled consumer drained %d bytes (>= %d): server did not bound the per-connection buffer", drained, ceiling)
	}

	// No loss: a fresh, fast reader from the start receives every journaled event,
	// contiguous and unique, through the terminal close.
	fast := h.openStream(sessionID, nil)
	defer fast.close()
	var collected []sseEvent
	for {
		ev, ok := fast.next(t)
		if !ok {
			break
		}
		collected = append(collected, ev)
	}
	if want := nonTerminal + 1; len(collected) != want {
		t.Fatalf("fast reader collected %d events, want %d (no loss)", len(collected), want)
	}
	assertContiguous(t, collected, 1)
	if last := collected[len(collected)-1]; last.event != "run.completed.v1" {
		t.Fatalf("final event = %q, want terminal run.completed.v1", last.event)
	}
}

// drainUntilClose reads and discards until the peer closes (EOF or reset) or the
// deadline lapses. A timeout means the server never dropped the stalled consumer.
func drainUntilClose(t *testing.T, conn net.Conn, timeout time.Duration) int {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 32*1024)
	total := 0
	for {
		n, err := conn.Read(buf)
		total += n
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.Fatalf("stalled consumer not dropped within %v (read %d bytes)", timeout, total)
			}
			return total // EOF or connection reset: the server closed us
		}
	}
}
