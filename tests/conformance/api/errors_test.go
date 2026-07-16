package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/contracts"
)

// decodeProblem asserts the RFC 9457 media type and decodes the problem document.
func decodeProblem(t *testing.T, resp *http.Response) contracts.Problem {
	t.Helper()
	body := readBody(t, resp)
	if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content-type = %q, want application/problem+json (body=%s)", ct, body)
	}
	var p contracts.Problem
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("decode problem: %v (body=%s)", err, body)
	}
	// Every problem carries the stable required fields (spec §20.10).
	if p.Type == "" || p.Title == "" || p.Status == 0 || p.Code == "" {
		t.Fatalf("problem missing required fields: %+v", p)
	}
	if !p.RequestID.Valid() {
		t.Fatalf("problem request_id %q is not canonical", p.RequestID)
	}
	return p
}

func TestProblemMissingAuthReturns401(t *testing.T) {
	srv := newTestServer(t)

	// No Authorization header at all.
	resp := postResponses(t, srv, map[string]string{"Idempotency-Key": "k"}, `{"input":"x"}`)
	problem := decodeProblem(t, resp)

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if problem.Code != "authentication_required" {
		t.Fatalf("code = %q, want authentication_required", problem.Code)
	}
	if problem.Status != http.StatusUnauthorized {
		t.Fatalf("problem.status = %d, want 401", problem.Status)
	}
	if problem.Retryable {
		t.Fatalf("authentication_required must not be retryable")
	}
	// Even an error carries the version header.
	if resp.Header.Get("API-Version") != middleware.APIVersion {
		t.Fatalf("API-Version = %q, want %q", resp.Header.Get("API-Version"), middleware.APIVersion)
	}
}

func TestProblemInvalidTokenReturns401(t *testing.T) {
	srv := newTestServer(t)

	headers := map[string]string{
		"Authorization":   "Bearer not-the-real-key",
		"Idempotency-Key": "k",
	}
	resp := postResponses(t, srv, headers, `{"input":"x"}`)
	problem := decodeProblem(t, resp)

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if problem.Code != "invalid_token" {
		t.Fatalf("code = %q, want invalid_token", problem.Code)
	}
}

func TestProblemMissingIdempotencyKeyReturns400(t *testing.T) {
	srv := newTestServer(t)

	// Valid auth, but no Idempotency-Key.
	headers := map[string]string{"Authorization": "Bearer " + testToken}
	resp := postResponses(t, srv, headers, `{"input":"x"}`)
	problem := decodeProblem(t, resp)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if problem.Code != "missing_idempotency_key" {
		t.Fatalf("code = %q, want missing_idempotency_key", problem.Code)
	}
}

func TestProblemInvalidSchemaReturns400(t *testing.T) {
	srv := newTestServer(t)

	// Authenticated, keyed, but the body omits the required "input" field.
	resp := postResponses(t, srv, authedHeaders("key-badschema"), `{"model":"fast"}`)
	problem := decodeProblem(t, resp)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if problem.Code != "invalid_request" {
		t.Fatalf("code = %q, want invalid_request", problem.Code)
	}
}
