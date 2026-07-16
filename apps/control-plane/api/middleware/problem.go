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
		Retryable: status >= http.StatusInternalServerError,
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem)
}

// problemTitles maps the stable codes this surface emits to human titles. Codes
// absent here still render (empty title is acceptable), but the LP-0 surface only
// emits these.
var problemTitles = map[string]string{
	"authentication_required": "Authentication required",
	"invalid_token":           "Invalid token",
	"missing_idempotency_key": "Missing idempotency key",
	"invalid_request":         "Invalid request",
	"idempotency_mismatch":    "Idempotency key reused with a different request",
	"internal_error":          "Internal error",
}
