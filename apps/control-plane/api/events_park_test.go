package api

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/contracts"
)

// E26 T3, RED 4 — THE SDK AND THE CONSOLE DO NOT BREAK ACROSS A PARK.
//
// The console's own comment already claims this for an approval pause (apps/web-console/app/api/palai/
// stream/route.ts: "The stream stays open across a waiting_for_approval pause"). E26 parks a run on a
// background task through the same choreography, so the same claim has to hold — and here it stops
// being a comment.
//
// It is driven over a REAL HTTP connection through the SHIPPED router, because that is what an SDK or
// the console attaches to; the thing being measured is whether the socket is still open, and that is not
// observable from inside a handler call. The journal behind it is a fake, deliberately: what the park
// WRITES into a real journal is asserted against a real Postgres in
// TestTheParkWritesNoRunTerminalIntoTheSessionJournal, beside the park itself.

// journalReader is a session journal a stream can tail.
type journalReader struct{ events []contracts.Event }

func (r *journalReader) SessionExists(context.Context, string, string) (bool, error) {
	return true, nil
}

func (r *journalReader) ResolveCursor(context.Context, string, string, string) (int64, bool, error) {
	return 0, false, nil
}

func (r *journalReader) After(_ context.Context, _, _ string, afterSeq int64, limit int) ([]contracts.Event, error) {
	var out []contracts.Event
	for _, e := range r.events {
		if int64(e.Sequence) > afterSeq && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (r *journalReader) RecordAttachDenied(context.Context, string, string, string) error {
	return nil
}

func parkEvent(seq int, typ string) contracts.Event {
	return contracts.Event{
		ID: contracts.EventID("evt_" + typ), Type: typ, Sequence: seq,
		Time: time.Now().UTC().Format(time.RFC3339), Data: map[string]any{},
	}
}

// attachStream opens the shipped SSE endpoint over a real connection and reads it until the server
// closes the body or `wait` elapses. `closed` is the measurement — "the stream stays open" is exactly
// "the server did not end the response" — and the bytes are the evidence for it.
func attachStream(t *testing.T, journal EventReader, wait time.Duration) (body string, closed bool) {
	t.Helper()
	srv := httptest.NewServer(NewRouter(
		scopedVerifier{middleware.Scope{Project: "prj_1", Principal: "prin_1"}},
		nil, journal, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		SSEConfig{Heartbeat: 20 * time.Millisecond, PollInterval: 5 * time.Millisecond, WriteTimeout: time.Second},
		nil, nil))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/sessions/ses_park/events", nil)
	if err != nil {
		t.Fatalf("build the stream request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("attach to the stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}

	var sb strings.Builder
	reader := bufio.NewReader(resp.Body)
	for {
		line, rerr := reader.ReadString('\n')
		sb.WriteString(line)
		if rerr == io.EOF {
			return sb.String(), true // the SERVER ended the response
		}
		if rerr != nil {
			// The client's own deadline tripped: the server was still holding the connection open.
			return sb.String(), false
		}
	}
}

// TestTheEventStreamStaysOpenAcrossAParkAndClosesOnATerminal is RED 4, and its two halves are one
// function on purpose: "it stayed open" is only a measurement if the same endpoint, the same fixture
// and the same deadline can be shown to CLOSE. A test asserting only the first half would pass just as
// well on an endpoint that never closes at all, which is a different bug and not a feature.
func TestTheEventStreamStaysOpenAcrossAParkAndClosesOnATerminal(t *testing.T) {
	parked := &journalReader{events: []contracts.Event{
		parkEvent(1, "run.running.v1"),
		parkEvent(2, "tool_call.completed.v1"),
		parkEvent(3, "run.waiting.v1"),
	}}
	body, closed := attachStream(t, parked, 300*time.Millisecond)
	if closed {
		t.Fatalf("the stream CLOSED on run.waiting.v1; every attached SDK and console reads that as the run ending. Body:\n%s", body)
	}
	if !strings.Contains(body, "run.waiting.v1") {
		t.Fatalf("the park event never reached the consumer:\n%s", body)
	}
	// The park is not the end of the story: the endpoint kept tailing, which is what makes the wake
	// (T4) deliverable on the SAME connection the model's caller already holds.
	if !strings.Contains(body, ": heartbeat") {
		t.Fatalf("the stream went silent after the park instead of keeping the connection alive:\n%s", body)
	}

	terminal := &journalReader{events: []contracts.Event{
		parkEvent(1, "run.running.v1"),
		parkEvent(2, "run.waiting.v1"),
		parkEvent(3, "run.completed.v1"),
	}}
	body, closed = attachStream(t, terminal, 5*time.Second)
	if !closed {
		t.Fatalf("the stream did NOT close on run.completed.v1, so the assertion above measures nothing:\n%s", body)
	}
	if !strings.Contains(body, "run.completed.v1") {
		t.Fatalf("the terminal event never reached the consumer:\n%s", body)
	}
}
