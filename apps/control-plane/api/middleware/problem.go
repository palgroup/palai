package middleware

import (
	"encoding/json"
	"net/http"

	"github.com/palgroup/palai/packages/contracts"
)

// problemTypePrefix namespaces stable codes into dereferenceable problem types.
const problemTypePrefix = "https://docs.palai.dev/problems/"

// WriteProblem renders an RFC 9457 problem document (spec §20.10). The body
// carries only a stable code, request id, retryability, and a fixed human detail —
// never provider messages, stack traces, paths, or credentials.
func WriteProblem(w http.ResponseWriter, r *http.Request, status int, code, detail string) {
	problem := contracts.Problem{
		Type:      problemTypePrefix + code,
		Title:     problemTitles[code],
		Status:    status,
		Detail:    detail,
		Code:      code,
		RequestID: contracts.RequestID(RequestID(r.Context())),
		// A 429 is inherently retryable (after the Retry-After the caller pairs with it, spec §20.12),
		// so it joins the 5xx class in advertising retryability; 4xx are otherwise terminal.
		Retryable: status >= http.StatusInternalServerError || status == http.StatusTooManyRequests,
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem)
}

// problemTitles maps the stable codes this surface emits to human titles. Codes
// absent here still render (empty title is acceptable), but the LP-0 surface only
// emits these.
var problemTitles = map[string]string{
	"authentication_required":    "Authentication required",
	"invalid_token":              "Invalid token",
	"missing_idempotency_key":    "Missing idempotency key",
	"invalid_request":            "Invalid request",
	"idempotency_mismatch":       "Idempotency key reused with a different request",
	"not_found":                  "Not found",
	"idempotency_result_expired": "Idempotent result expired",
	"retention_expired":          "Retention expired",
	"internal_error":             "Internal error",
	"rate_limited":               "Rate limit exceeded",
	"concurrency_exceeded":       "Concurrent run limit reached",
}
