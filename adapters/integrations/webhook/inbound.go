package webhook

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

// The receiver-side failure modes ParseInbound distinguishes, so the caller maps each to the right
// HTTP outcome: a bad/stale signature is an UNAUTHENTICATED reject (401, no persistence — AUT-002); a
// malformed but validly-SIGNED envelope is an authenticated client error (400, still no run). A
// well-formed envelope whose opaque Data fails the trigger's mapping is NOT decided here — that is the
// poison path the pipeline terminalizes `failed` (§34.3), after the durable insert.
var (
	// ErrBadSignature is a MAC mismatch (wrong/rotated-out secret, or a tampered body/id).
	ErrBadSignature = errors.New("webhook: inbound signature does not verify")
	// ErrStaleTimestamp is a timestamp outside the replay-window tolerance.
	ErrStaleTimestamp = errors.New("webhook: inbound timestamp outside tolerance")
	// ErrMalformedInbound is a signed-but-unusable envelope (non-JSON, unknown field, missing source, or a
	// body source_event_id that disagrees with the signed Webhook-Id).
	ErrMalformedInbound = errors.New("webhook: inbound event envelope is malformed")
)

// InboundEvent is the normalized §21.7 source envelope. Source names the source family (dedupe scope);
// SourceTenant is the sender's optional sub-tenant; SourceEventID is the idempotency id, taken from the
// SIGNED Webhook-Id header (the MAC binds it, so a body-supplied id is only a redundant echo we reject on
// mismatch); Data is the opaque payload the trigger's bounded mapping consumes (never decoded here).
type InboundEvent struct {
	Source        string          `json:"source"`
	SourceTenant  string          `json:"source_tenant"`
	SourceEventID string          `json:"source_event_id"`
	Data          json.RawMessage `json:"data"`
}

// ParseInbound verifies a POSTed inbound event's signature against the active secrets (constant-time,
// timestamp-tolerant, multi-secret rotation — the T4 Verify, no new MAC code) and normalizes the §21.7
// envelope. Verification runs STRICTLY before decode, and decode is strict (an unknown top-level field is
// rejected). secrets carries the trigger's 1–2 active source secrets; now/tolerance bound the replay
// window. The returned event's SourceEventID is the signed Webhook-Id (authoritative).
func ParseInbound(headers map[string]string, rawBody []byte, secrets [][]byte, now time.Time, tolerance time.Duration) (InboundEvent, error) {
	id := headers[HeaderID]
	sig := headers[HeaderSignature]
	unix, err := strconv.ParseInt(headers[HeaderTimestamp], 10, 64)
	if err != nil {
		return InboundEvent{}, ErrBadSignature // a missing/garbled timestamp cannot anchor a MAC
	}
	ts := time.Unix(unix, 0)

	// The replay window is checked here so it is a DISTINCT typed reason (stale ≠ bad MAC); Verify enforces
	// it too, so a within-window event still fails Verify on a real MAC mismatch.
	if skew := now.Sub(ts); skew > tolerance || skew < -tolerance {
		return InboundEvent{}, ErrStaleTimestamp
	}
	verified := false
	for _, secret := range secrets {
		if Verify(secret, id, ts, rawBody, sig, now, tolerance) {
			verified = true
			break
		}
	}
	if !verified {
		return InboundEvent{}, ErrBadSignature
	}

	// Signature good: normalize the envelope STRICTLY. Data stays opaque (json.RawMessage), so only the
	// wrapper's own fields are strict-decoded — the mapping validates the payload later.
	dec := json.NewDecoder(bytes.NewReader(rawBody))
	dec.DisallowUnknownFields()
	var ev InboundEvent
	if err := dec.Decode(&ev); err != nil {
		return InboundEvent{}, ErrMalformedInbound
	}
	if ev.Source == "" {
		return InboundEvent{}, ErrMalformedInbound // an unroutable event (no dedupe/source scope)
	}
	if id == "" {
		// source_event_id is required: an empty id would skip the source-dedupe index, the stuck-inbound
		// sweep, the backlog gauge, and the raw-payload scrub — an un-deduped, un-sweepable, un-scrubbed row.
		return InboundEvent{}, ErrMalformedInbound
	}
	if ev.SourceEventID != "" && ev.SourceEventID != id {
		return InboundEvent{}, ErrMalformedInbound // a body id that disagrees with the signed Webhook-Id
	}
	ev.SourceEventID = id // the signed header is authoritative
	return ev, nil
}
