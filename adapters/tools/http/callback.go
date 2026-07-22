package remotehttp

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/palgroup/palai/adapters/integrations/webhook"
	"github.com/palgroup/palai/packages/contracts"
)

// HeaderCallbackToken carries the one-use audience-bound callback token (separate from the signature
// headers, spec §28.24). It is constant-time compared to the operation row's stored token hash.
const HeaderCallbackToken = "Tool-Callback-Token"

// The callback verify-before-persist failure modes ParseCallback distinguishes, so the handler maps each
// to the right outcome WITHOUT leaking a config oracle: a bad/stale signature is an UNAUTHENTICATED
// reject (the caller does not hold the secret); a signed-but-unusable envelope is a client error.
var (
	// ErrCallbackBadSignature is a MAC mismatch (wrong secret, or a tampered body/operation id).
	ErrCallbackBadSignature = errors.New("remotehttp: callback signature does not verify")
	// ErrCallbackStale is a timestamp outside the replay-window tolerance.
	ErrCallbackStale = errors.New("remotehttp: callback timestamp outside tolerance")
	// ErrCallbackMalformed is a signed-but-unusable envelope (non-JSON, wrong protocol/operation id, or
	// neither/both of result and problem).
	ErrCallbackMalformed = errors.New("remotehttp: callback envelope is malformed")
)

// ParseCallback is the verify-before-persist gate for a tool-http.v1 result callback (spec §28.24,
// mirroring webhook.ParseInbound): it verifies the HMAC signature over the raw body under the operation's
// secret (constant-time, timestamp-tolerant — the SAME webhook.Verify, no new MAC) STRICTLY before a
// single byte is trusted, then decodes the envelope and enforces its shape. The signature id is the
// operationID (the callback URL's path segment), binding the MAC to this exact operation. It does NOT
// touch the token (the handler constant-time compares that against the stored hash) or the store.
func ParseCallback(operationID string, headers map[string]string, rawBody, secret []byte, now time.Time, tolerance time.Duration) (contracts.ToolHTTPCallback, error) {
	if len(secret) == 0 {
		return contracts.ToolHTTPCallback{}, ErrCallbackBadSignature // no secret => nothing verifies
	}
	sig := headers[webhook.HeaderSignature]
	unix, err := strconv.ParseInt(headers[webhook.HeaderTimestamp], 10, 64)
	if err != nil {
		return contracts.ToolHTTPCallback{}, ErrCallbackBadSignature // a missing/garbled timestamp cannot anchor a MAC
	}
	ts := time.Unix(unix, 0)
	if skew := now.Sub(ts); skew > tolerance || skew < -tolerance {
		return contracts.ToolHTTPCallback{}, ErrCallbackStale
	}
	if !webhook.Verify(secret, operationID, ts, rawBody, sig, now, tolerance) {
		return contracts.ToolHTTPCallback{}, ErrCallbackBadSignature
	}

	// Signature good: decode + enforce the envelope shape. Decode is lenient on unknown fields (the schema
	// is additionalProperties:true, forward-compatible), but strict on the load-bearing invariants.
	var cb contracts.ToolHTTPCallback
	if err := json.NewDecoder(bytes.NewReader(rawBody)).Decode(&cb); err != nil {
		return contracts.ToolHTTPCallback{}, ErrCallbackMalformed
	}
	if cb.Protocol != Protocol {
		return contracts.ToolHTTPCallback{}, ErrCallbackMalformed
	}
	if cb.OperationID != operationID {
		return contracts.ToolHTTPCallback{}, ErrCallbackMalformed // a body operation id that disagrees with the signed path
	}
	// Exactly one of result / problem (the schema's mutual exclusion).
	if (cb.Result == nil) == (cb.Problem == nil) {
		return contracts.ToolHTTPCallback{}, ErrCallbackMalformed
	}
	return cb, nil
}

// Payload returns the callback's carried object — the result, or the problem when the tool failed — and
// whether it is a problem. The handler persists this blob + its ResultHash; the executor surfaces it.
func Payload(cb contracts.ToolHTTPCallback) (payload map[string]any, isProblem bool) {
	if cb.Result != nil {
		return cb.Result, false
	}
	return cb.Problem, true
}
