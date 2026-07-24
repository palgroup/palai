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
