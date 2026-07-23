// Package sdkconf_test is the mechanical cross-language equality harness for the shared SDK
// fixture corpus (spec API-012, E16 T2). The corpus under corpus/*.json is language-AGNOSTIC
// data: every vector is {name, input, expected}, where `expected` is the NORMALIZED decoded
// output the SDK must produce. This harness proves equality in ONE place (the design
// invariant: no SDK asserts "same as TS" in its own suite):
//
//   - a Go REFERENCE decode, using the server's own packages/contracts types + the reference
//     webhook.Verify, validates every vector's expected output (implementation #1);
//   - each registered language RUNNER (the TS SDK now; Python/Go in T3/T4) is fed the same
//     vectors over a stable stdin/stdout contract, and its normalized output is canonical-bytes
//     diffed against the same expected (implementation #2, #3, ...).
//
// A vector whose decoded output diverges FAILS mechanically — the diff is a real
// canonical-JSON comparison, and TestHarnessFailsOnDivergence proves the harness CAN fail
// (a deliberately-wrong expected is detected). "Three languages equal" is NOT claimed here:
// T2 ships TS + reference; the T8 gate makes the three-language claim once Py/Go runners land
// against this exact corpus and contract (see README.md).
package sdkconf_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/webhook"
	"github.com/palgroup/palai/packages/contracts"
)

// categories are the corpus files. Each is one decode surface the SDKs must agree on.
var categories = []string{
	"request-encode",
	"event-decode",
	"error-map",
	"signature-verify",
	"unknown-field",
	"envelope-decode",
}

type vector struct {
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
	Expected json.RawMessage `json:"expected"`
}

func loadCategory(t *testing.T, category string) []vector {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("corpus", category+".json"))
	if err != nil {
		t.Fatalf("read corpus %s: %v", category, err)
	}
	var file struct {
		Vectors []vector `json:"vectors"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		t.Fatalf("decode corpus %s: %v", category, err)
	}
	if len(file.Vectors) == 0 {
		t.Fatalf("corpus %s has no vectors", category)
	}
	return file.Vectors
}

// --- canonical comparison (the mechanical equality) -------------------------------------

// canon renders any value as canonical JSON: a round-trip through the generic decoder sorts
// every map key and normalizes number forms, so two structurally-equal values compare equal
// byte-for-byte regardless of field order or which language produced them.
func canon(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	var round any
	if err := json.Unmarshal(b, &round); err != nil {
		return "", err
	}
	out, err := json.Marshal(round)
	return string(out), err
}

func canonRaw(raw json.RawMessage) (string, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	return canon(v)
}

// diff reports whether got and want are canonically equal; on mismatch it returns both
// canonical forms so a failing vector shows exactly where it diverged.
func diff(got any, want json.RawMessage) (equal bool, gotCanon, wantCanon string, err error) {
	gotCanon, err = canon(got)
	if err != nil {
		return false, "", "", fmt.Errorf("canonicalize got: %w", err)
	}
	wantCanon, err = canonRaw(want)
	if err != nil {
		return false, "", "", fmt.Errorf("canonicalize want: %w", err)
	}
	return gotCanon == wantCanon, gotCanon, wantCanon, nil
}

// --- reference decode: the server's OWN types validate the corpus --------------------------

var terminalRe = regexp.MustCompile(`^(run|response)\.(completed|failed|canceled|timed_out|budget_exceeded)\.v[0-9]+$`)

// referenceValidate is the in-process implementation #1: it independently derives each
// vector's normalized output from the server's contracts (or, for request-encode, checks the
// SDK-produced wire body is a valid, canonical server request) and asserts it matches expected.
func referenceValidate(category string, input, expected json.RawMessage) error {
	if category == "request-encode" {
		return referenceRequestEncode(input, expected)
	}
	got, err := referenceDecode(category, input)
	if err != nil {
		return err
	}
	equal, gotCanon, wantCanon, err := diff(got, expected)
	if err != nil {
		return err
	}
	if !equal {
		return fmt.Errorf("reference decode diverged\n got: %s\nwant: %s", gotCanon, wantCanon)
	}
	return nil
}

func referenceDecode(category string, input json.RawMessage) (any, error) {
	switch category {
	case "event-decode":
		var in struct {
			Transcript string `json:"transcript"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, err
		}
		return referenceEvents(in.Transcript), nil
	case "error-map":
		var in struct {
			Status    int    `json:"status"`
			Body      string `json:"body"`
			RequestID string `json:"request_id"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, err
		}
		return referenceErrorMap(in.Status, in.Body, in.RequestID), nil
	case "signature-verify":
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
			return nil, err
		}
		valid := webhook.Verify([]byte(in.Secret), in.WebhookID, time.Unix(in.Timestamp, 0),
			[]byte(in.Body), in.Signature, time.Unix(in.Now, 0), time.Duration(in.Tolerance)*time.Second)
		out := map[string]any{"valid": valid}
		if in.ExpectSignature {
			header := webhook.NewSigner([]byte(in.Secret)).Headers(in.WebhookID, time.Unix(in.Timestamp, 0), 1, []byte(in.Body))[webhook.HeaderSignature]
			out["signature"] = strings.TrimPrefix(header, "v1=")
		}
		return out, nil
	case "unknown-field":
		var in struct {
			Value json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, err
		}
		var v any
		if err := json.Unmarshal(in.Value, &v); err != nil {
			return nil, err
		}
		return v, nil // decode-then-re-emit preserves the unknown field
	case "envelope-decode":
		var in struct {
			Envelope json.RawMessage `json:"envelope"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, err
		}
		return referenceEnvelope(in.Envelope)
	default:
		return nil, fmt.Errorf("no reference decode for category %q", category)
	}
}

// referenceEvents is an independent SSE framer mirroring the SDK's parseEventStream: blank-line
// dispatch, comment skip, one-space-after-colon fields, multi-line data joined with "\n", CRLF
// tolerance. Each data line decodes to a canonical event iff it is a JSON object with a string
// `type` (unknown types/fields preserved); the first terminal event's index is reported.
func referenceEvents(transcript string) any {
	events := []any{}
	terminalIndex := -1
	var data string
	var hasData bool

	for _, rawLine := range strings.Split(transcript, "\n") {
		line := strings.TrimSuffix(rawLine, "\r")
		if line == "" { // blank line dispatches the buffered frame
			if hasData && data != "" {
				var parsed any
				if json.Unmarshal([]byte(data), &parsed) == nil {
					if m, ok := parsed.(map[string]any); ok {
						if tp, ok := m["type"].(string); ok {
							if terminalIndex == -1 && terminalRe.MatchString(tp) {
								terminalIndex = len(events)
							}
							events = append(events, m)
						}
					}
				}
			}
			data, hasData = "", false
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // comment / heartbeat
		}
		field, value := line, ""
		if colon := strings.IndexByte(line, ':'); colon != -1 {
			field, value = line[:colon], strings.TrimPrefix(line[colon+1:], " ")
		}
		if field == "data" { // id/event fields do not affect event extraction
			if hasData {
				data += "\n" + value
			} else {
				data, hasData = value, true
			}
		}
	}
	return map[string]any{"events": events, "terminal_index": terminalIndex}
}

// referenceErrorMap mirrors the SDK errors.ts mapping: family class by HTTP status, code from
// the problem body (or synthesized), retryable = explicit problem.retryable else status class,
// request_id = problem.request_id or the header fallback.
func referenceErrorMap(status int, body, headerRequestID string) any {
	var (
		code      string
		requestID string
		retryable bool
	)
	code, requestID, retryable, hasProblem := parseProblem(body)
	if !hasProblem {
		code = statusCode(status)
		requestID = headerRequestID
		retryable = isRetryableStatus(status)
	} else {
		if requestID == "" {
			requestID = headerRequestID
		}
	}
	return map[string]any{
		"class":      apiErrorClass(status),
		"status":     status,
		"code":       code,
		"retryable":  retryable,
		"request_id": requestID,
	}
}

// parseProblem returns (code, requestID, retryable, ok) mirroring errors.ts parseProblem +
// the retryable ?? statusClass fallback: ok is false unless the body is a JSON object with a
// string code and a numeric status.
func parseProblem(body string) (code, requestID string, retryable, ok bool) {
	if body == "" {
		return "", "", false, false
	}
	var raw struct {
		Code      *string  `json:"code"`
		Status    *float64 `json:"status"`
		Retryable *bool    `json:"retryable"`
		RequestID *string  `json:"request_id"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return "", "", false, false
	}
	if raw.Code == nil || raw.Status == nil {
		return "", "", false, false
	}
	code = *raw.Code
	if raw.RequestID != nil {
		requestID = *raw.RequestID
	}
	// retryable ?? isRetryableStatus — the caller applies the status fallback when absent.
	retryable = raw.Retryable != nil && *raw.Retryable
	if raw.Retryable == nil {
		retryable = isRetryableStatus(int(*raw.Status))
	}
	return code, requestID, retryable, true
}

func apiErrorClass(status int) string {
	switch status {
	case 400, 422:
		return "InvalidRequestError"
	case 401:
		return "AuthenticationError"
	case 403:
		return "PermissionDeniedError"
	case 404:
		return "NotFoundError"
	case 409:
		return "ConflictError"
	case 410:
		return "GoneError"
	case 429:
		return "RateLimitError"
	default:
		if status >= 500 {
			return "InternalServerError"
		}
		return "PalaiAPIError"
	}
}

func statusCode(status int) string {
	switch status {
	case 401:
		return "authentication_required"
	case 403:
		return "permission_denied"
	case 404:
		return "not_found"
	case 409:
		return "active_run_conflict"
	case 410:
		return "gone"
	case 429:
		return "rate_limited"
	case 503:
		return "capacity_unavailable"
	case 504:
		return "operation_timed_out"
	default:
		if status >= 500 {
			return "internal_error"
		}
		return "invalid_request"
	}
}

func isRetryableStatus(status int) bool {
	return status == 408 || status == 429 || status >= 500
}

// referenceEnvelope classifies the two list envelopes via the server's contracts.Page and the
// {object:"list"} admin shape, projecting the same normalized form the runners emit.
func referenceEnvelope(envelope json.RawMessage) (any, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(envelope, &probe); err != nil {
		return nil, err
	}
	if _, isPage := probe["has_more"]; isPage {
		var page contracts.Page
		if err := json.Unmarshal(envelope, &page); err != nil {
			return nil, err
		}
		out := map[string]any{"kind": "page", "has_more": page.HasMore, "data": page.Data}
		if page.NextCursor != nil {
			out["next_cursor"] = *page.NextCursor
		}
		if page.PreviousCursor != nil {
			out["previous_cursor"] = *page.PreviousCursor
		}
		return out, nil
	}
	var list struct {
		Object string `json:"object"`
		Data   []any  `json:"data"`
	}
	if err := json.Unmarshal(envelope, &list); err != nil {
		return nil, err
	}
	if list.Object != "list" {
		return nil, fmt.Errorf("envelope is neither a Page nor a ListView: %s", envelope)
	}
	return map[string]any{"kind": "list", "object": list.Object, "data": list.Data}, nil
}

// referenceRequestEncode validates the SDK-produced wire request is a well-formed, canonical
// server request: a non-null body must decode into contracts.ResponseCreateRequest and
// canonically round-trip to itself. The {method,path} are the SDK's routing (validated by the
// runners); the reference only proves the body is server-valid.
func referenceRequestEncode(_ json.RawMessage, expected json.RawMessage) error {
	var exp struct {
		Method string          `json:"method"`
		Path   string          `json:"path"`
		Body   json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(expected, &exp); err != nil {
		return err
	}
	if exp.Method == "" || exp.Path == "" {
		return fmt.Errorf("request-encode expected missing method/path: %s", expected)
	}
	if len(exp.Body) == 0 || string(exp.Body) == "null" {
		return nil
	}
	var req contracts.ResponseCreateRequest
	if err := json.Unmarshal(exp.Body, &req); err != nil {
		return fmt.Errorf("body is not a valid ResponseCreateRequest: %w", err)
	}
	roundtrip, err := json.Marshal(req)
	if err != nil {
		return err
	}
	gotCanon, err := canonRaw(roundtrip)
	if err != nil {
		return err
	}
	wantCanon, err := canonRaw(exp.Body)
	if err != nil {
		return err
	}
	if gotCanon != wantCanon {
		return fmt.Errorf("wire body is not canonical for the server type\n got: %s\nwant: %s", gotCanon, wantCanon)
	}
	return nil
}

// --- Test 1: the reference validates the whole corpus -----------------------------------

func TestCorpusReferenceEquality(t *testing.T) {
	for _, category := range categories {
		for _, v := range loadCategory(t, category) {
			t.Run(category+"/"+v.Name, func(t *testing.T) {
				if err := referenceValidate(category, v.Input, v.Expected); err != nil {
					t.Errorf("%v", err)
				}
			})
		}
	}
}

// --- Test 2: the TS SDK runner matches the corpus (implementation #2) --------------------

type runnerVector struct {
	Category string          `json:"category"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
}

type runnerOutput struct {
	Category string          `json:"category"`
	Name     string          `json:"name"`
	Output   json.RawMessage `json:"output"`
}

// runExternalRunner feeds the whole corpus to a language runner over the stable contract and
// returns its outputs keyed by category+name. This is the exact entry point T3 (Python) and T4
// (Go) register against — a new runner is one argv here, no corpus change.
func runExternalRunner(t *testing.T, argv []string, all []runnerVector) map[string]json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(struct {
		Vectors []runnerVector `json:"vectors"`
	}{Vectors: all})
	if err != nil {
		t.Fatalf("marshal runner payload: %v", err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("runner %v failed: %v\nstderr: %s", argv, err, stderr.String())
	}
	var resp struct {
		Outputs []runnerOutput `json:"outputs"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		t.Fatalf("runner %v emitted invalid JSON: %v\nstdout: %s\nstderr: %s", argv, err, stdout.String(), stderr.String())
	}
	out := make(map[string]json.RawMessage, len(resp.Outputs))
	for _, o := range resp.Outputs {
		out[o.Category+"\x00"+o.Name] = o.Output
	}
	return out
}

func TestCorpusTypeScriptRunnerEquality(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; TS runner leg skipped (reference still validated the corpus)")
	}
	runnerPath, err := filepath.Abs(filepath.Join("..", "..", "..", "sdks", "typescript", "test", "conformance-runner.ts"))
	if err != nil {
		t.Fatalf("resolve runner path: %v", err)
	}
	if _, err := os.Stat(runnerPath); err != nil {
		t.Fatalf("TS runner missing at %s: %v", runnerPath, err)
	}

	// The categories the TS SDK is expected to cover (it ships no webhook verify, so
	// signature-verify is intentionally omitted and validated by the reference alone).
	tsCovers := map[string]bool{
		"request-encode": true, "event-decode": true, "error-map": true,
		"unknown-field": true, "envelope-decode": true,
	}

	var all []runnerVector
	byKey := map[string]vector{}
	for _, category := range categories {
		for _, v := range loadCategory(t, category) {
			all = append(all, runnerVector{Category: category, Name: v.Name, Input: v.Input})
			byKey[category+"\x00"+v.Name] = v
		}
	}

	outputs := runExternalRunner(t, []string{node, "--experimental-strip-types", runnerPath}, all)

	covered := 0
	for key, v := range byKey {
		category := strings.SplitN(key, "\x00", 2)[0]
		got, ok := outputs[key]
		if !ok {
			if tsCovers[category] {
				t.Errorf("TS runner produced no output for %s (expected coverage)", key)
			}
			continue // signature-verify: expected to be unsupported
		}
		covered++
		equal, gotCanon, wantCanon, err := diff(rawAny(got), v.Expected)
		if err != nil {
			t.Errorf("%s: %v", key, err)
			continue
		}
		if !equal {
			t.Errorf("TS runner diverged for %s\n got: %s\nwant: %s", key, gotCanon, wantCanon)
		}
	}
	if covered == 0 {
		t.Fatal("TS runner covered zero vectors — the harness must not pass by running nothing")
	}
	t.Logf("TS runner matched %d/%d vectors (signature-verify is reference-only)", covered, len(byKey))
}

// rawAny lets a json.RawMessage flow through diff()'s canonicalizer unchanged.
func rawAny(raw json.RawMessage) any { return raw }

// --- Test 3: the harness CAN fail (negative / anti-fabrication) --------------------------

// TestHarnessFailsOnDivergence proves the mechanical equality is genuine: it runs a real
// vector through the real reference pipeline, then mutates one byte of the expected output and
// asserts the diff DETECTS the divergence. A harness that cannot fail would silently pass a
// corrupted corpus; this is the guard against that.
func TestHarnessFailsOnDivergence(t *testing.T) {
	var target vector
	for _, v := range loadCategory(t, "error-map") {
		if v.Name == "gone-410-retention-purged" {
			target = v
		}
	}
	if target.Name == "" {
		t.Fatal("negative-test fixture gone-410-retention-purged not found")
	}
	got, err := referenceDecode("error-map", target.Input)
	if err != nil {
		t.Fatalf("reference decode: %v", err)
	}

	// Sanity: the correct expected still matches (the pipeline passes when it should).
	if equal, _, _, err := diff(got, target.Expected); err != nil || !equal {
		t.Fatalf("correct expected should match: equal=%v err=%v", equal, err)
	}

	// Corrupt one field of the expected output — the decoded code is authoritative, so the
	// mutated expected must be detected as a mismatch.
	mutated := bytes.Replace(target.Expected, []byte(`"retention_expired"`), []byte(`"not_expired"`), 1)
	if bytes.Equal(mutated, target.Expected) {
		t.Fatal("mutation did not change the fixture; the negative test is inert")
	}
	equal, _, _, err := diff(got, mutated)
	if err != nil {
		t.Fatalf("diff on mutated expected: %v", err)
	}
	if equal {
		t.Fatal("harness FAILED to detect a divergent expected output — mechanical equality is not genuine")
	}
}
