package palai

import "testing"

// The typed error mapping mirrors errors.ts and is pinned cross-language by the corpus; this unit
// test covers the family classes, retryable rules, and the synthesized-problem degradation for a
// non-problem body (a gateway's plain-text 5xx still yields a typed, retryable error).
func TestErrorForResponseMapping(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		requestID string
		class     string
		code      string
		retryable bool
		reqID     string
	}{
		{"invalid-400", 400, `{"code":"invalid_request","status":400,"request_id":"req_400"}`, "", "InvalidRequestError", "invalid_request", false, "req_400"},
		{"schema-422", 422, `{"code":"schema_validation_failed","status":422,"request_id":"req_422"}`, "", "InvalidRequestError", "schema_validation_failed", false, "req_422"},
		{"auth-401", 401, `{"code":"invalid_token","status":401,"request_id":"req_401"}`, "", "AuthenticationError", "invalid_token", false, "req_401"},
		{"perm-403", 403, `{"code":"permission_denied","status":403,"request_id":"req_403"}`, "", "PermissionDeniedError", "permission_denied", false, "req_403"},
		{"notfound-404", 404, `{"code":"not_found","status":404,"request_id":"req_404"}`, "", "NotFoundError", "not_found", false, "req_404"},
		{"conflict-409", 409, `{"code":"idempotency_mismatch","status":409,"request_id":"req_409"}`, "", "ConflictError", "idempotency_mismatch", false, "req_409"},
		{"gone-410", 410, `{"code":"retention_expired","status":410,"request_id":"req_410"}`, "", "GoneError", "retention_expired", false, "req_410"},
		{"rate-429", 429, `{"code":"rate_limited","status":429,"request_id":"req_429"}`, "", "RateLimitError", "rate_limited", true, "req_429"},
		{"server-500", 500, `{"code":"internal_error","status":500,"request_id":"req_500"}`, "", "InternalServerError", "internal_error", true, "req_500"},
		{"nonproblem-502", 502, "Bad Gateway", "req_502", "InternalServerError", "internal_error", true, "req_502"},
		{"nonproblem-429", 429, "Too Many Requests", "req_429b", "RateLimitError", "rate_limited", true, "req_429b"},
		{"explicit-retryable", 400, `{"code":"invalid_request","status":400,"retryable":true,"request_id":"req_ovr"}`, "", "InvalidRequestError", "invalid_request", true, "req_ovr"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := ErrorForResponse(tc.status, tc.body, tc.requestID)
			if e.Class() != tc.class {
				t.Errorf("class = %q, want %q", e.Class(), tc.class)
			}
			if e.Code != tc.code {
				t.Errorf("code = %q, want %q", e.Code, tc.code)
			}
			if e.Retryable() != tc.retryable {
				t.Errorf("retryable = %v, want %v", e.Retryable(), tc.retryable)
			}
			if e.RequestID != tc.reqID {
				t.Errorf("request_id = %q, want %q", e.RequestID, tc.reqID)
			}
			if e.Status != tc.status {
				t.Errorf("status = %d, want %d", e.Status, tc.status)
			}
		})
	}
}

// An unknown code from a newer server is preserved verbatim, not rejected (open code set, API-009).
func TestUnknownProblemCodePreserved(t *testing.T) {
	e := ErrorForResponse(400, `{"code":"a_future_code_we_do_not_know","status":400,"request_id":"r"}`, "")
	if e.Code != "a_future_code_we_do_not_know" {
		t.Fatalf("unknown code not preserved: %q", e.Code)
	}
}
