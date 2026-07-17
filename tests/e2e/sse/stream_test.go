//go:build e2e

package sse

import (
	"strings"
	"testing"
	"time"
)

// TestHeartbeatKeepsIdleStreamAlive proves the server emits a keep-alive comment on
// an idle stream (no new events) within the configured max-idle interval, so proxies
// do not drop a quiet-but-live session.
func TestHeartbeatKeepsIdleStreamAlive(t *testing.T) {
	h := newHarness(t)
	sessionID, _ := h.createSession() // seq 1 (run.queued.v1); the run stays queued, so the stream idles

	conn := h.openStream(sessionID, nil)
	defer conn.close()

	if _, ok := conn.next(t); !ok { // drain the replayed queued event
		t.Fatal("stream closed before the initial event")
	}

	// After the last event the stream is idle; a heartbeat comment must arrive. The
	// interval is server-injected (short in the e2e config), so this is a bounded wait.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		line, ok := conn.nextRawLine()
		if !ok {
			t.Fatal("stream closed while idle, want a heartbeat")
		}
		if strings.HasPrefix(line, ":") {
			return // heartbeat comment observed
		}
	}
	t.Fatal("no heartbeat within the idle window")
}

// TestAfterSequenceQueryResumes proves the explicit after_sequence query param
// resumes the stream from the next sequence, the numeric alternative to the
// Last-Event-ID header.
func TestAfterSequenceQueryResumes(t *testing.T) {
	h := newHarness(t)
	sessionID := h.seedSession()
	h.seedBulkEvents(sessionID, 3, 16) // seq 1..3 (run.running.v1), seq 4 terminal

	conn := h.openStreamQuery(sessionID, "after_sequence=2", nil)
	defer conn.close()

	var collected []sseEvent
	for {
		ev, ok := conn.next(t)
		if !ok {
			break
		}
		collected = append(collected, ev)
	}
	if len(collected) != 2 {
		t.Fatalf("collected %d events, want 2 (seq 3 and 4)", len(collected))
	}
	assertContiguous(t, collected, 3) // first delivered is seq 3, not 1
	if last := collected[len(collected)-1]; last.event != "run.completed.v1" {
		t.Fatalf("final event = %q, want terminal", last.event)
	}
}

// TestUnknownEventTypePreserved proves the envelope is open: an event whose type is
// not one the server knows, carrying extra data fields, streams through intact so a
// forward-compatible client can preserve it.
func TestUnknownEventTypePreserved(t *testing.T) {
	h := newHarness(t)
	sessionID := h.seedSession()
	h.seedEvent(sessionID, 1, "widget.frobnicated.v7", `{"custom":"xyz","nested":{"a":1}}`)

	conn := h.openStream(sessionID, nil)
	defer conn.close()

	ev, ok := conn.next(t)
	if !ok {
		t.Fatal("stream closed before the unknown event")
	}
	if ev.event != "widget.frobnicated.v7" {
		t.Fatalf("event type = %q, want widget.frobnicated.v7 (unknown type must pass through)", ev.event)
	}
	if got, _ := ev.data["custom"].(string); got != "xyz" {
		t.Fatalf("data.custom = %v, want xyz (unknown data must be preserved)", ev.data["custom"])
	}
	nested, ok := ev.data["nested"].(map[string]any)
	if !ok || nested["a"] == nil {
		t.Fatalf("data.nested = %v, want the nested object preserved", ev.data["nested"])
	}
}
