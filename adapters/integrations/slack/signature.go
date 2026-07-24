// Package slack is the Slack integration adapter (E17 T1, spec §36). It is a PURE adapter: it verifies
// Slack's v0 request signatures, normalizes Events API / Socket Mode payloads into the SAME canonical
// inbound identity the webhook seam uses (§34.1 — no new run-identity/dedupe mechanism is invented here),
// maps an interactive approval action onto the one-shot approval primitive, and repairs a rate-limited
// outbound post. The durable pipeline (source-dedupe unique index, poison→failed, RLS connection store)
// lives control-plane-side exactly as it does for the webhook adapter; this package holds no database.
//
// SECURITY-CRITICAL: signature verification runs constant-time, inside a bounded replay window, STRICTLY
// before any payload is decoded — the ParseInbound discipline (adapters/integrations/webhook) expressed in
// Slack's v0 signing scheme. The signing secret bytes never leave this file.
package slack

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"time"
)

// Slack's signed-request headers (spec: Verifying requests from Slack). Canonical HTTP header names.
const (
	HeaderTimestamp = "X-Slack-Request-Timestamp"
	HeaderSignature = "X-Slack-Signature"
	// HeaderRetryNum is set (1..) when Slack REDELIVERS an Events API callback whose 3s ack it did not
	// observe. The retry carries the SAME event_id, so it collapses onto the canonical source-dedupe — the
	// header is advisory (a redelivery hint), never the dedupe key.
	HeaderRetryNum = "X-Slack-Retry-Num"

	// sigVersion is the versioned scheme prefix bound INTO the signed base string, so a receiver cannot be
	// tricked into verifying a different scheme's MAC. Slack has only ever shipped v0.
	sigVersion = "v0"

	// DefaultTolerance is Slack's documented replay window: a request whose timestamp is more than five
	// minutes from local time is treated as a possible replay and refused.
	DefaultTolerance = 5 * time.Minute
)

var (
	// ErrBadSignature is a MAC mismatch (wrong/rotated signing secret, or a tampered body/timestamp).
	ErrBadSignature = errors.New("slack: request signature does not verify")
	// ErrStaleTimestamp is a timestamp outside the replay-window tolerance (a possible replay).
	ErrStaleTimestamp = errors.New("slack: request timestamp outside tolerance")
)

// VerifySignature checks a Slack Events API / interactivity HTTP request against the app's signing secret
// (spec: Verifying requests from Slack). The base string is exactly "v0:{timestamp}:{body}", the MAC is
// HMAC-SHA256 over it, and the comparison is constant-time (hmac.Equal). The replay window is checked FIRST
// as a distinct typed reason (stale ≠ bad MAC) and enforced regardless of a valid MAC, so a captured-then-
// replayed request with a good-but-old signature is still refused. secret is the signing-secret bytes
// (resolved from a secret_ref handle by the caller — never inline, never logged).
//
// Socket Mode frames are NOT signed (the WebSocket is authenticated by the app-level token at connect);
// this routine is for the Events API / interactivity HTTP transport only, matching Slack's own SDK which
// skips signature verification in Socket Mode.
func VerifySignature(secret []byte, timestamp, signature string, body []byte, now time.Time, tolerance time.Duration) error {
	if len(secret) == 0 {
		// A verify with no secret can only ever fail-open; refuse rather than accept an unauthenticated body.
		return ErrBadSignature
	}
	unix, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ErrBadSignature // a missing/garbled timestamp cannot anchor a MAC
	}
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	if skew := now.Sub(time.Unix(unix, 0)); skew > tolerance || skew < -tolerance {
		return ErrStaleTimestamp
	}
	want := signatureFor(secret, timestamp, body)
	if !hmac.Equal([]byte(signature), []byte(want)) {
		return ErrBadSignature
	}
	return nil
}

// signatureFor computes the "v0=<hex>" header value for a body under a signing secret and timestamp — the
// exact base string "v0:{timestamp}:{body}". Exported-adjacent so a fake Slack peer fixture (the local
// proof) can sign the requests it pushes with the SAME routine the receiver verifies with.
func signatureFor(secret []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(sigVersion + ":" + timestamp + ":"))
	mac.Write(body)
	return sigVersion + "=" + hex.EncodeToString(mac.Sum(nil))
}
