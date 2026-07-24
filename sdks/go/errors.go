package palai

import (
	"encoding/json"
	"fmt"
)

// The typed RFC 9457 error surface (spec §23.7), mirroring the TS SDK's errors.ts: a base
// *APIError carrying the parsed Problem, HTTP status, stable code, correlation request id, and a
// retryable class flag, plus a Class() family label so a caller can branch on the HTTP class
// without a distinct Go type per stable code. The code stays OPEN: an unknown code from a newer
// server is preserved as a plain string, never rejected.

// Error is the base every SDK error implements, so a caller can type-switch the whole surface.
type Error interface {
	error
	// Retryable reports whether this class of failure may be safely retried.
	Retryable() bool
}

// ConnectionError is a transport failure before any HTTP status was seen — a dropped socket, DNS
// failure, or an aborted per-attempt timeout. It is always retryable.
type ConnectionError struct {
	Message string
	Cause   error
}

func (e *ConnectionError) Error() string {
	if e.Cause != nil {
		return e.Message + ": " + e.Cause.Error()
	}
	return e.Message
}
func (e *ConnectionError) Unwrap() error   { return e.Cause }
func (e *ConnectionError) Retryable() bool { return true }

// Problem is a parsed RFC 9457 application/problem+json document (spec §23). It preserves unknown
// fields (forward-compat) via Extra, like the other decoded models.
type Problem struct {
	Type        string                     `json:"type"`
	Title       string                     `json:"title"`
	Status      int                        `json:"status"`
	Code        string                     `json:"code"`
	Detail      string                     `json:"detail,omitempty"`
	Instance    string                     `json:"instance,omitempty"`
	RequestID   string                     `json:"request_id"`
	Retryable   *bool                      `json:"retryable,omitempty"`
	Context     map[string]any             `json:"context,omitempty"`
	FieldErrors []map[string]any           `json:"field_errors,omitempty"`
	Extra       map[string]json.RawMessage `json:"-"`
}

func (p *Problem) UnmarshalJSON(b []byte) error {
	type alias Problem
	var a alias
	if err := forwardUnmarshal(b, &a, &a.Extra); err != nil {
		return err
	}
	*p = Problem(a)
	return nil
}

func (p Problem) MarshalJSON() ([]byte, error) {
	type alias Problem
	return forwardMarshal(alias(p), p.Extra)
}

// APIError is a typed RFC 9457 problem response.
type APIError struct {
	Status    int
	Code      string
	Problem   Problem
	RequestID string
	retryable bool
}

func (e *APIError) Error() string {
	if e.Problem.Detail != "" {
		return e.Problem.Detail
	}
	if e.Problem.Title != "" {
		return e.Problem.Title
	}
	if e.Code != "" {
		return e.Code
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

func (e *APIError) Retryable() bool { return e.retryable }

// Class is the ergonomic family label for the HTTP status, matching the TS SDK subclass names
// (InvalidRequestError, AuthenticationError, …) so cross-language error mapping is byte-identical
// in the conformance corpus. The exact stable code is always on Code.
func (e *APIError) Class() string { return apiErrorClass(e.Status) }

// isRetryableStatus is the default retry predicate (spec §23.7): a request timeout, a rate limit,
// or any server-side 5xx may be retried; everything else is terminal.
func isRetryableStatus(status int) bool {
	return status == 408 || status == 429 || status >= 500
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

// ErrorForResponse builds the typed *APIError for a non-2xx (status, body, request id). Exported so
// a caller decoding a raw upstream response reuses the SDK's exact RFC 9457 mapping.
func ErrorForResponse(status int, body, requestID string) *APIError {
	return errorForResponse(status, body, requestID)
}

// errorForResponse builds the typed error for a non-2xx response. A well-formed problem body is
// parsed; a missing or non-problem body degrades to a synthesized Problem carrying the stable code
// the status implies, so a gateway's plain-text 502 still yields a typed, retryable error.
func errorForResponse(status int, body string, requestID string) *APIError {
	problem, ok := parseProblem(body)
	if !ok {
		problem = syntheticProblem(status, requestID)
	}
	retryable := isRetryableStatus(status)
	if problem.Retryable != nil {
		retryable = *problem.Retryable
	}
	reqID := problem.RequestID
	if reqID == "" {
		reqID = requestID
	}
	return &APIError{
		Status:    status,
		Code:      problem.Code,
		Problem:   problem,
		RequestID: reqID,
		retryable: retryable,
	}
}

// parseProblem returns (problem, ok): ok is false unless the body is a JSON object with a string
// code and a numeric status (mirrors errors.ts parseProblem — the two fields that make a document
// a Problem).
func parseProblem(body string) (Problem, bool) {
	if body == "" {
		return Problem{}, false
	}
	var probe struct {
		Code   *string  `json:"code"`
		Status *float64 `json:"status"`
	}
	if err := json.Unmarshal([]byte(body), &probe); err != nil {
		return Problem{}, false
	}
	if probe.Code == nil || probe.Status == nil {
		return Problem{}, false
	}
	var problem Problem
	if err := json.Unmarshal([]byte(body), &problem); err != nil {
		return Problem{}, false
	}
	return problem, true
}

func syntheticProblem(status int, requestID string) Problem {
	code := statusCode(status)
	return Problem{
		Type:      "https://docs.palai.dev/problems/" + code,
		Title:     code,
		Status:    status,
		Code:      code,
		RequestID: requestID,
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
