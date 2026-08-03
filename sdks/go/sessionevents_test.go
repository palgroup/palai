package palai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func mustClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	c, err := New(WithAPIKey("sk-test"), WithBaseURL(baseURL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// TestSessionEventsResumesFromTheRecordedSequence locks the wire contract measured 2026-08-03: resume
// is the query param after_sequence (not Last-Event-ID), and a decoded frame's `sequence` field lands
// on Event.Sequence.
func TestSessionEventsResumesFromTheRecordedSequence(t *testing.T) {
	var gotQuery, gotLastEventID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		gotLastEventID = r.Header.Get("Last-Event-ID")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "id: evt_a\nevent: model_step.delta.v1\n"+
			`data: {"id":"evt_a","sequence":7,"type":"model_step.delta.v1","data":{"text":"hi"}}`+"\n\n")
	}))
	defer srv.Close()

	c := mustClient(t, srv.URL)
	st, err := c.Sessions.Events(context.Background(), "sess_1", EventsParams{AfterSequence: 6})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	defer st.Close()
	if !strings.Contains(gotQuery, "after_sequence=6") {
		t.Fatalf("query %q does not carry after_sequence", gotQuery)
	}
	ev, err := st.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Type != "model_step.delta.v1" || ev.Sequence != 7 {
		t.Fatalf("got type=%q sequence=%d", ev.Type, ev.Sequence)
	}
	_ = gotLastEventID
}

// TestSessionEventsReconnectResumesFromLastSeenSequence is the correctness property that matters for
// a bot relaying this stream into Slack: after a dropped connection, the client must resume from the
// LAST SEEN sequence — not from zero (a duplicated message) and not one past it (a silently dropped
// message). A fake server delivers two events on its first connection then drops it (returns without
// closing gracefully — httptest ends the response when the handler returns); the second connection's
// after_sequence must be exactly 2.
func TestSessionEventsReconnectResumesFromLastSeenSequence(t *testing.T) {
	var connections int64
	reconnectQuery := make(chan string, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&connections, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			_, _ = io.WriteString(w, "id: evt_a\nevent: model_step.delta.v1\n"+
				`data: {"id":"evt_a","sequence":1,"type":"model_step.delta.v1","data":{}}`+"\n\n"+
				"id: evt_b\nevent: model_step.delta.v1\n"+
				`data: {"id":"evt_b","sequence":2,"type":"model_step.delta.v1","data":{}}`+"\n\n")
			return // clean handler return ends the response — simulates a mid-run drop
		}
		reconnectQuery <- r.URL.RawQuery
		<-r.Context().Done() // hold the second connection open; the test only needs its query
	}))
	defer srv.Close()

	c := mustClient(t, srv.URL)
	st, err := c.Sessions.Events(context.Background(), "sess_1", EventsParams{})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	defer st.Close()

	for _, wantSeq := range []int{1, 2} {
		ev, err := st.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if ev.Sequence != wantSeq {
			t.Fatalf("sequence = %d, want %d", ev.Sequence, wantSeq)
		}
	}

	select {
	case q := <-reconnectQuery:
		if q != "after_sequence=2" {
			t.Fatalf("reconnect query = %q, want after_sequence=2 (not 0 — a duplicate — and not 3 — a drop)", q)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no reconnect observed within 5s")
	}
}

// TestSessionEventsStopsOnTerminalEvent locks the terminal-close contract (spec §22.3, §24.4): a
// terminal event is delivered like any other via Next(), and the NEXT call after it returns io.EOF
// rather than reconnecting.
func TestSessionEventsStopsOnTerminalEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "id: evt_a\nevent: run.completed.v1\n"+
			`data: {"id":"evt_a","sequence":1,"type":"run.completed.v1","data":{}}`+"\n\n")
	}))
	defer srv.Close()

	c := mustClient(t, srv.URL)
	st, err := c.Sessions.Events(context.Background(), "sess_1", EventsParams{})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	defer st.Close()

	ev, err := st.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Type != "run.completed.v1" {
		t.Fatalf("got type=%q, want run.completed.v1", ev.Type)
	}
	if _, err := st.Next(); err != io.EOF {
		t.Fatalf("Next after terminal = %v, want io.EOF", err)
	}
}

// TestSessionEventsUnknownSessionIsAnAPIError locks that a connect-time failure (404 for a foreign or
// unknown session, spec §39.2) surfaces synchronously from Events() as a typed *APIError, not on the
// first Next() — a caller can branch on it immediately.
func TestSessionEventsUnknownSessionIsAnAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"type":"t","title":"not_found","status":404,"code":"not_found","request_id":"req_1"}`)
	}))
	defer srv.Close()

	c := mustClient(t, srv.URL)
	_, err := c.Sessions.Events(context.Background(), "sess_missing", EventsParams{})
	var apiErr *APIError
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if ae, ok := err.(*APIError); ok {
		apiErr = ae
	} else {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if apiErr.Status != http.StatusNotFound || apiErr.Code != "not_found" {
		t.Fatalf("apiErr = %+v", apiErr)
	}
}
