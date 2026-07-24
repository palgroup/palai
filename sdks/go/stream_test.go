package palai

import (
	"strings"
	"testing"
)

func collect(t *testing.T, transcript string) ([]Event, int) {
	t.Helper()
	var events []Event
	terminal := -1
	if err := ScanEvents(strings.NewReader(transcript), func(e Event) bool {
		if terminal == -1 && IsTerminalEvent(e) {
			terminal = len(events)
		}
		events = append(events, e)
		return true
	}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return events, terminal
}

func TestSSETwoEventsCommentAndCRLF(t *testing.T) {
	transcript := ": keep-alive\nid: e1\nevent: model_step.created.v1\ndata: {\"type\":\"model_step.created.v1\",\"id\":\"e1\",\"sequence\":1,\"data\":{}}\n\n" +
		"id: e2\r\nevent: run.completed.v1\r\ndata: {\"type\":\"run.completed.v1\",\"id\":\"e2\",\"sequence\":2,\"data\":{}}\r\n\r\n"
	events, terminal := collect(t, transcript)
	if len(events) != 2 {
		t.Fatalf("want 2 events, got %d", len(events))
	}
	if terminal != 1 {
		t.Fatalf("terminal index = %d, want 1", terminal)
	}
	if events[0].ID != "e1" || events[1].Type != "run.completed.v1" {
		t.Fatalf("event fields wrong: %+v", events)
	}
}

func TestSSEHeartbeatAndNonJSONSkipped(t *testing.T) {
	transcript := ": ping\ndata: plain text not an event\n\nid: e3\nevent: run.progress.v1\ndata: {\"type\":\"run.progress.v1\",\"id\":\"e3\",\"sequence\":3,\"data\":{\"pct\":42}}\n\n"
	events, terminal := collect(t, transcript)
	if len(events) != 1 {
		t.Fatalf("want 1 event (heartbeat + non-JSON skipped), got %d", len(events))
	}
	if terminal != -1 {
		t.Fatalf("no terminal expected, got index %d", terminal)
	}
}

func TestSSEMultiLineDataJoined(t *testing.T) {
	transcript := "id: e5\nevent: run.progress.v1\ndata: {\"type\":\"run.progress.v1\",\"id\":\"e5\",\ndata: \"sequence\":5,\"data\":{\"note\":\"multi-line\"}}\n\n"
	events, _ := collect(t, transcript)
	if len(events) != 1 {
		t.Fatalf("want 1 event from joined data, got %d", len(events))
	}
	if events[0].Sequence != 5 {
		t.Fatalf("joined-data event decoded wrong: %+v", events[0])
	}
	if events[0].Data["note"] != "multi-line" {
		t.Fatalf("nested data lost: %+v", events[0].Data)
	}
}

func TestSSEUnknownEventTypeDelivered(t *testing.T) {
	transcript := "event: some.brand.new.v9\ndata: {\"type\":\"some.brand.new.v9\",\"id\":\"e1\",\"sequence\":1,\"data\":{}}\n\n"
	events, terminal := collect(t, transcript)
	if len(events) != 1 || events[0].Type != "some.brand.new.v9" {
		t.Fatalf("unknown event type must be delivered, not dropped: %+v", events)
	}
	if terminal != -1 {
		t.Fatalf("an unknown type is not terminal, got index %d", terminal)
	}
}

// TestSSEUnterminatedTrailingFrameDiscarded pins the WHATWG/TS behavior: a frame with a complete,
// VALID data line but no blank-line terminator (a graceful close mid-frame) is DISCARDED, not
// dispatched. Dispatching it would deliver a truncated event and — worse — advance Last-Event-ID
// (its id: line) past an event that was never delivered, silently dropping it on resume.
func TestSSEUnterminatedTrailingFrameDiscarded(t *testing.T) {
	terminated := "id: e1\nevent: run.progress.v1\ndata: {\"type\":\"run.progress.v1\",\"id\":\"e1\",\"sequence\":1,\"data\":{}}\n\n"
	// e2 is valid JSON on its data line but the stream ends with a single '\n' — no blank terminator.
	unterminated := "id: e2\nevent: run.progress.v1\ndata: {\"type\":\"run.progress.v1\",\"id\":\"e2\",\"sequence\":2,\"data\":{}}\n"

	events, _ := collect(t, terminated+unterminated)
	if len(events) != 1 {
		t.Fatalf("an unterminated trailing frame must be discarded: got %d events, want 1", len(events))
	}
	if events[0].ID != "e1" {
		t.Fatalf("only the blank-terminated event should be delivered, got %q", events[0].ID)
	}
	// Sanity: the SAME e2 frame, once blank-terminated, IS delivered — proving it was the missing
	// terminator, not the content, that suppressed it.
	full, _ := collect(t, terminated+unterminated+"\n")
	if len(full) != 2 {
		t.Fatalf("a blank-terminated e2 must be delivered: got %d events, want 2", len(full))
	}
}

func TestFullJitterBackoffBounds(t *testing.T) {
	base, max := 100, 5000
	for attempt := 0; attempt < 8; attempt++ {
		ceiling := max
		if exp := base << attempt; attempt < 31 && exp < ceiling {
			ceiling = exp
		}
		for s := 0; s < 200; s++ {
			d := fullJitterBackoff(attempt, base, max)
			ms := int(d.Milliseconds())
			if ms < 0 || ms > ceiling {
				t.Fatalf("attempt %d: %dms not in [0,%d]", attempt, ms, ceiling)
			}
		}
	}
	if fullJitterBackoff(3, 0, max) != 0 {
		t.Fatal("a non-positive base disables backoff")
	}
	if d := fullJitterBackoff(20, base, max); int(d.Milliseconds()) > max {
		t.Fatal("the exponential ceiling must saturate at max")
	}
}
