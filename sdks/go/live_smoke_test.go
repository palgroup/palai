package palai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLiveSmokeRoundTrip drives the REAL SDK over a REAL loopback HTTP socket (httptest.Server —
// real net/http transport, real SSE streaming), doing the create → stream → retrieve round-trip a
// caller performs against a running stack. It is the SDK-level live proof.
//
// Honest ceiling: this is a loopback socket, not the compose control-plane with a provisioned model
// route and a real provider run — that full three-SDK journey (63.1) is E16 T8's gate, and the two
// real providers in .env.local are its live legs. This smoke deliberately uses NO credential and NO
// Docker, so it never trips the known Docker Desktop hang; it proves the wire path end-to-end.
func TestLiveSmokeRoundTrip(t *testing.T) {
	completed := `{"id":"resp_live","object":"response","status":"completed","model":"fake-1",` +
		`"output":[{"type":"output_text","text":"lazy is efficient"}],` +
		`"usage":{"input_tokens":7,"output_tokens":4,"total_tokens":11},"created_at":"2026-07-24T00:00:00Z"}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/responses":
			if r.Header.Get("Idempotency-Key") == "" {
				t.Error("create must carry an Idempotency-Key")
			}
			if r.Header.Get("API-Version") != APIVersion {
				t.Errorf("API-Version header = %q", r.Header.Get("API-Version"))
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"id":"resp_live","object":"response","status":"queued","session_id":"ses_live"}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/sessions/"):
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			// A progress event (unknown-to-caller extra field), then the terminal.
			_, _ = w.Write([]byte("id: e1\nevent: run.progress.v1\ndata: {\"type\":\"run.progress.v1\",\"id\":\"e1\",\"sequence\":1,\"data\":{\"pct\":50},\"x_future\":\"kept\"}\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			_, _ = w.Write([]byte("id: e2\nevent: run.completed.v1\ndata: {\"type\":\"run.completed.v1\",\"id\":\"e2\",\"sequence\":2,\"data\":{}}\n\n"))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/responses/resp_live":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(completed))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()

	client, err := New(WithAPIKey("sk-live"), WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ctx := context.Background()

	// create
	created, err := client.Responses.Create(ctx, ResponseCreateRequest{Input: "be lazy", Model: "fake-1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID != "resp_live" || created.SessionID != "ses_live" {
		t.Fatalf("create result wrong: %+v", created)
	}

	// stream → iterate events over the real socket
	stream, err := client.Responses.Stream(ctx, ResponseCreateRequest{Input: "be lazy", Model: "fake-1"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var seen []string
	var keptUnknown bool
	for e, err := range stream.Events(ctx) {
		if err != nil {
			t.Fatalf("stream event error: %v", err)
		}
		seen = append(seen, e.Type)
		if e.Type == "run.progress.v1" {
			if v, _ := e.Extra["x_future"]; string(v) == `"kept"` {
				keptUnknown = true
			}
		}
	}
	if len(seen) != 2 || seen[1] != "run.completed.v1" {
		t.Fatalf("expected progress then completed, got %v", seen)
	}
	if !keptUnknown {
		t.Fatal("the stream must preserve an unknown event field over the wire")
	}

	// FinalResponse on a fresh stream drains to the terminal then retrieves the canonical Response.
	stream2, err := client.Responses.Stream(ctx, ResponseCreateRequest{Input: "be lazy", Model: "fake-1"})
	if err != nil {
		t.Fatalf("stream2: %v", err)
	}
	final, err := stream2.FinalResponse(context.Background())
	if err != nil {
		t.Fatalf("final response: %v", err)
	}
	if final.Status != "completed" || final.Usage == nil || final.Usage.TotalTokens != 11 {
		t.Fatalf("final response wrong: %+v", final)
	}
	if len(final.Output) != 1 || final.Output[0].Type() != "output_text" {
		t.Fatalf("final output wrong: %+v", final.Output)
	}
}
