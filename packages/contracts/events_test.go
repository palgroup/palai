package contracts

import (
	"bytes"
	"strings"
	"testing"
)

// TestMarshalSSESingleLineData proves the invariant SSE relies on: the envelope
// serializes to exactly one data line even when a data value contains a newline,
// and the frame carries the event id and type as SSE fields.
func TestMarshalSSESingleLineData(t *testing.T) {
	e := Event{
		Specversion: "1.0",
		ID:          "evt_abc",
		Source:      "/v1/sessions/ses_x",
		Type:        "run.completed.v1",
		Time:        "2026-07-16T12:00:00Z",
		Sequence:    3,
		Data:        map[string]any{"note": "line1\nline2"},
	}
	frame, err := e.MarshalSSE()
	if err != nil {
		t.Fatalf("MarshalSSE() error = %v", err)
	}

	got := string(frame)
	if !strings.HasPrefix(got, "id: evt_abc\nevent: run.completed.v1\ndata: ") {
		t.Fatalf("frame prefix = %q", got)
	}
	if !strings.HasSuffix(got, "\n\n") {
		t.Fatalf("frame must end in a blank line, got %q", got)
	}

	// Exactly one data line: id, event, data, then the terminating blank line.
	data, ok := lineAfter(got, "data: ")
	if !ok {
		t.Fatalf("no data line in %q", got)
	}
	if strings.Contains(data, "\n") {
		t.Fatalf("data line contains a raw newline: %q", data)
	}
	if !bytes.Contains([]byte(data), []byte(`line1\nline2`)) {
		t.Fatalf("embedded newline was not escaped: %q", data)
	}
}

func lineAfter(frame, prefix string) (string, bool) {
	for _, line := range strings.Split(frame, "\n") {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			return rest, true
		}
	}
	return "", false
}
