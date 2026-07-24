// Command runner is the Go leg of the shared SDK-conformance corpus (E16 T4, API-012). It is NOT a
// test — it is a filter the Go harness (tests/conformance/sdk) drives: it reads
// {"vectors":[{category,name,input}]} on stdin, runs each vector through the REAL github.com/
// palgroup/palai/sdks/go surface, and writes {"outputs":[{category,name,output}]} on stdout as
// NORMALIZED JSON. The harness canonical-bytes-diffs that output against the corpus's expected
// output AND against the TypeScript/Python runners — mechanical cross-language equality in one
// place (the design invariant). A category this SDK does not expose is simply omitted; the Go SDK
// exposes ALL six (incl. signature-verify — it ships a real webhook verify), so it is the first
// runner to give the signature-verify category a second independent implementation.
//
// This mirrors the STABLE runner contract documented in tests/conformance/sdk/README.md, exactly as
// the TypeScript runner (sdks/typescript/test/conformance-runner.ts) does in its language.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	palai "github.com/palgroup/palai/sdks/go"
)

const base = "http://localhost:8080"

type vector struct {
	Category string          `json:"category"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
}

type output struct {
	Category string `json:"category"`
	Name     string `json:"name"`
	Output   any    `json:"output"`
}

func main() {
	// E16 T8 live-retrieve mode: when PALAI_LIVE_RESPONSE_ID is set, this runner is the Go SDK's leg of the
	// four-client parity journey — it retrieves that shared response over the REAL server (base URL + key from
	// PALAI_BASE_URL/PALAI_API_KEY, the SDK's env defaults) and prints its NORMALIZED projection
	// {"id","output_text","status"} on stdout, the exact shape the CLI + TS + Python legs emit. The journey
	// canonical-bytes-diffs the four.
	if id := os.Getenv("PALAI_LIVE_RESPONSE_ID"); id != "" {
		liveRetrieve(id)
		return
	}

	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		fail(err)
	}
	var req struct {
		Vectors []vector `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		fail(err)
	}
	outputs := make([]output, 0, len(req.Vectors))
	for _, v := range req.Vectors {
		out, covered, err := decode(v)
		if err != nil {
			fail(fmt.Errorf("%s/%s: %w", v.Category, v.Name, err))
		}
		if covered {
			outputs = append(outputs, output{Category: v.Category, Name: v.Name, Output: out})
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(struct {
		Outputs []output `json:"outputs"`
	}{Outputs: outputs}); err != nil {
		fail(err)
	}
}

func decode(v vector) (any, bool, error) {
	switch v.Category {
	case "request-encode":
		return requestEncode(v.Input)
	case "event-decode":
		return eventDecode(v.Input)
	case "error-map":
		return errorMap(v.Input)
	case "unknown-field":
		return unknownField(v.Input)
	case "envelope-decode":
		return envelopeDecode(v.Input)
	case "signature-verify":
		return signatureVerify(v.Input)
	default:
		return nil, false, nil // a category this SDK does not expose
	}
}

// --- request-encode: drive the real resource methods through a capturing transport -----------

type captured struct {
	method  string
	url     string
	idemKey string
	body    []byte
}

// capturingTransport records the FIRST outgoing request (so stream() — which fires the create POST
// before opening the SSE — reports its create request) and answers with deterministic canned
// responses so the SDK call completes cleanly.
type capturingTransport struct{ first *captured }

func (t *capturingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.first == nil {
		var body []byte
		if req.Body != nil {
			body, _ = io.ReadAll(req.Body)
		}
		t.first = &captured{
			method:  req.Method,
			url:     req.URL.String(),
			idemKey: req.Header.Get("Idempotency-Key"),
			body:    body,
		}
	}
	if strings.Contains(req.URL.Path, "/events") {
		terminal := "id: e1\nevent: run.completed.v1\ndata: {\"specversion\":\"1.0\",\"id\":\"e1\",\"source\":\"palai\",\"type\":\"run.completed.v1\",\"time\":\"2026-07-18T00:00:00Z\",\"sequence\":1,\"data\":{}}\n\n"
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(terminal)),
		}, nil
	}
	stub := `{"id":"resp_stub","session_id":"sess_stub","object":"response","status":"completed","model":"fake-1","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0},"created_at":"2026-07-18T00:00:00Z"}`
	return &http.Response{
		StatusCode: 202,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(stub)),
	}, nil
}

func requestEncode(input json.RawMessage) (any, bool, error) {
	var in struct {
		Resource string          `json:"resource"`
		Method   string          `json:"method"`
		Args     json.RawMessage `json:"args"`
		Options  struct {
			IdempotencyKey string `json:"idempotencyKey"`
		} `json:"options"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, false, err
	}
	if in.Resource != "responses" {
		return nil, false, nil
	}
	cap := &capturingTransport{}
	client, err := palai.New(
		palai.WithAPIKey("conf"),
		palai.WithBaseURL(base),
		palai.WithHTTPClient(&http.Client{Transport: cap}),
	)
	if err != nil {
		return nil, false, err
	}
	ctx := context.Background()
	var opts []palai.CallOption
	if in.Options.IdempotencyKey != "" {
		opts = append(opts, palai.WithIdempotencyKey(in.Options.IdempotencyKey))
	}

	switch in.Method {
	case "create":
		var reqBody palai.ResponseCreateRequest
		if err := json.Unmarshal(in.Args, &reqBody); err != nil {
			return nil, false, err
		}
		if _, err := client.Responses.Create(ctx, reqBody, opts...); err != nil {
			return nil, false, err
		}
	case "stream":
		var reqBody palai.ResponseCreateRequest
		if err := json.Unmarshal(in.Args, &reqBody); err != nil {
			return nil, false, err
		}
		stream, err := client.Responses.Stream(ctx, reqBody, opts...)
		if err != nil {
			return nil, false, err
		}
		// Drain to the terminal so the stream path is exercised end-to-end; the create POST is
		// already captured (Stream fires it first), so an incidental drain error is irrelevant here.
		_, _ = stream.FinalResponse(ctx)
	case "list":
		var params palai.ListParams
		if err := json.Unmarshal(in.Args, &params); err != nil {
			return nil, false, err
		}
		if _, err := client.Responses.List(ctx, params); err != nil {
			return nil, false, err
		}
	case "retrieve":
		var args struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(in.Args, &args); err != nil {
			return nil, false, err
		}
		if _, err := client.Responses.Retrieve(ctx, args.ID); err != nil {
			return nil, false, err
		}
	default:
		return nil, false, nil
	}

	if cap.first == nil {
		return nil, false, fmt.Errorf("no request captured for %s", in.Method)
	}
	path := strings.TrimPrefix(cap.first.url, base)
	out := map[string]any{"method": cap.first.method, "path": path}
	if cap.first.idemKey != "" {
		out["idempotency_key"] = cap.first.idemKey
	}
	if len(cap.first.body) > 0 {
		var body any
		if err := json.Unmarshal(cap.first.body, &body); err != nil {
			return nil, false, err
		}
		out["body"] = body
	}
	return out, true, nil
}

// --- event-decode: frame the SSE transcript through the SDK parser --------------------------

func eventDecode(input json.RawMessage) (any, bool, error) {
	var in struct {
		Transcript string `json:"transcript"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, false, err
	}
	events := []any{}
	terminalIndex := -1
	if err := palai.ScanEvents(strings.NewReader(in.Transcript), func(e palai.Event) bool {
		if terminalIndex == -1 && palai.IsTerminalEvent(e) {
			terminalIndex = len(events)
		}
		events = append(events, e)
		return true
	}); err != nil {
		return nil, false, err
	}
	return map[string]any{"events": events, "terminal_index": terminalIndex}, true, nil
}

// --- error-map: project a wire (status, body) to the typed error surface ---------------------

func errorMap(input json.RawMessage) (any, bool, error) {
	var in struct {
		Status    int    `json:"status"`
		Body      string `json:"body"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, false, err
	}
	e := palai.ErrorForResponse(in.Status, in.Body, in.RequestID)
	return map[string]any{
		"class":      e.Class(),
		"status":     e.Status,
		"code":       e.Code,
		"retryable":  e.Retryable(),
		"request_id": e.RequestID,
	}, true, nil
}

// --- unknown-field: decode through the matching lossless typed model -------------------------

// unknownField routes each value through the SDK's real struct-based decoder for that shape, proving
// the typed model preserves an unknown field (a naive struct decode would strip it — the point of
// this category for a struct language). The dispatch is a small structural sniff, not a type hint.
func unknownField(input json.RawMessage) (any, bool, error) {
	var in struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, false, err
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(in.Value, &probe); err != nil {
		return nil, false, err
	}
	switch {
	case has(probe, "specversion"):
		var e palai.Event
		if err := json.Unmarshal(in.Value, &e); err != nil {
			return nil, false, err
		}
		return e, true, nil
	case stringEq(probe, "object", "response"):
		var r palai.Response
		if err := json.Unmarshal(in.Value, &r); err != nil {
			return nil, false, err
		}
		return r, true, nil
	case has(probe, "title") && has(probe, "code") && has(probe, "status"):
		var p palai.Problem
		if err := json.Unmarshal(in.Value, &p); err != nil {
			return nil, false, err
		}
		return p, true, nil
	default:
		var c palai.ContentItem
		if err := json.Unmarshal(in.Value, &c); err != nil {
			return nil, false, err
		}
		return c, true, nil
	}
}

// --- envelope-decode: classify and project the two list envelopes ----------------------------

func envelopeDecode(input json.RawMessage) (any, bool, error) {
	var in struct {
		Envelope json.RawMessage `json:"envelope"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, false, err
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(in.Envelope, &probe); err != nil {
		return nil, false, err
	}
	if _, isPage := probe["has_more"]; isPage {
		var p palai.Page[json.RawMessage]
		if err := json.Unmarshal(in.Envelope, &p); err != nil {
			return nil, false, err
		}
		out := map[string]any{"kind": "page", "has_more": p.HasMore, "data": p.Data}
		if p.NextCursor != nil {
			out["next_cursor"] = *p.NextCursor
		}
		if p.PreviousCursor != nil {
			out["previous_cursor"] = *p.PreviousCursor
		}
		return out, true, nil
	}
	var lv palai.ListView[json.RawMessage]
	if err := json.Unmarshal(in.Envelope, &lv); err != nil {
		return nil, false, err
	}
	if lv.Object != "list" {
		return nil, false, nil
	}
	return map[string]any{"kind": "list", "object": lv.Object, "data": lv.Data}, true, nil
}

// --- signature-verify: the SDK's webhook verify (API-014) ------------------------------------

func signatureVerify(input json.RawMessage) (any, bool, error) {
	var in struct {
		Secret          string `json:"secret"`
		WebhookID       string `json:"webhook_id"`
		Timestamp       int64  `json:"timestamp"`
		Body            string `json:"body"`
		Signature       string `json:"signature"`
		Now             int64  `json:"now"`
		Tolerance       int    `json:"tolerance_seconds"`
		ExpectSignature bool   `json:"expect_signature"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, false, err
	}
	ts := time.Unix(in.Timestamp, 0)
	valid := palai.VerifyWebhook([]byte(in.Secret), in.WebhookID, ts, []byte(in.Body),
		in.Signature, time.Unix(in.Now, 0), time.Duration(in.Tolerance)*time.Second)
	out := map[string]any{"valid": valid}
	if in.ExpectSignature {
		out["signature"] = palai.SignWebhook([]byte(in.Secret), in.WebhookID, ts, []byte(in.Body))
	}
	return out, true, nil
}

func has(m map[string]json.RawMessage, key string) bool { _, ok := m[key]; return ok }

func stringEq(m map[string]json.RawMessage, key, want string) bool {
	raw, ok := m[key]
	if !ok {
		return false
	}
	var s string
	return json.Unmarshal(raw, &s) == nil && s == want
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "go-runner: %v\n", err)
	os.Exit(1)
}

// liveRetrieve is the Go SDK's parity-journey leg: retrieve one response over the real server and print its
// normalized {"id","output_text","status"} projection. A GoneError (410 tombstone) prints a gone marker and
// exits 3, so the journey can assert the TYPED gone surface across the three SDKs. The client reads
// PALAI_BASE_URL + PALAI_API_KEY from the environment (the SDK defaults).
func liveRetrieve(id string) {
	client, err := palai.New()
	if err != nil {
		fail(err)
	}
	resp, err := client.Responses.Retrieve(context.Background(), id)
	if err != nil {
		var apiErr *palai.APIError
		if errors.As(err, &apiErr) && apiErr.Status == 410 {
			fmt.Fprintln(os.Stdout, `{"gone":true,"status":410}`)
			os.Exit(3)
		}
		fail(err)
	}
	var text strings.Builder
	for _, item := range resp.Output {
		if t, ok := item["text"].(string); ok {
			text.WriteString(t)
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(map[string]any{
		"id": string(resp.ID), "output_text": text.String(), "status": resp.Status,
	}); err != nil {
		fail(err)
	}
}
