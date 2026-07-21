// Package webhook is the sole outbound-webhook integration adapter (dependency direction, plan §4:
// HTTP egress lives only here). It carries the §21.5 signer and the §21.6 egress-safe sender; the
// control-plane pump composes them. crypto/hmac usage is greenfield in this repo, so the signer is
// stdlib-only and self-contained.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// The four attempt headers (spec §21.5). Canonical HTTP header names.
const (
	HeaderID        = "Webhook-Id"
	HeaderTimestamp = "Webhook-Timestamp"
	HeaderSignature = "Webhook-Signature"
	HeaderAttempt   = "Webhook-Attempt"

	// signatureVersion is the versioned scheme prefix and is bound INTO the signed input, so a
	// receiver cannot be tricked into verifying a different scheme's MAC (spec §21.5). Asymmetric
	// signing MAY be added later as a second scheme without removing this one.
	signatureVersion = "v1"
)

// Signer holds the one or two active signing secrets (spec §21.4: rotation overlaps old + new for a
// bounded period). It computes the HMAC-SHA-256 over the raw body and emits the attempt headers. The
// secret bytes are never logged and never leave this type.
type Signer struct {
	secrets [][]byte
}

// NewSigner binds the active secrets. Pass one secret normally, two during a rotation overlap; the
// first is the primary. An empty secret set is a programming error the caller guards (a delivery
// without a secret cannot be signed), so it panics rather than emitting an unsigned attempt.
func NewSigner(secrets ...[]byte) *Signer {
	live := make([][]byte, 0, len(secrets))
	for _, s := range secrets {
		if len(s) > 0 {
			live = append(live, s)
		}
	}
	if len(live) == 0 {
		panic("webhook: signer needs at least one non-empty secret")
	}
	return &Signer{secrets: live}
}

// Headers returns the four attempt headers for a delivery. During rotation the signature header
// carries a space-separated v1= value per active secret, so a receiver that has advanced to the new
// secret and one still on the old both verify (the standard-webhooks multi-signature convention).
func (s *Signer) Headers(deliveryID string, ts time.Time, attempt int, rawBody []byte) map[string]string {
	unix := ts.Unix()
	parts := make([]string, 0, len(s.secrets))
	for _, secret := range s.secrets {
		parts = append(parts, signatureVersion+"="+sign(secret, deliveryID, unix, rawBody))
	}
	return map[string]string{
		HeaderID:        deliveryID,
		HeaderTimestamp: strconv.FormatInt(unix, 10),
		HeaderSignature: strings.Join(parts, " "),
		HeaderAttempt:   strconv.Itoa(attempt),
	}
}

// sign computes the hex HMAC-SHA-256 over the signed input: version, delivery id, timestamp, and the
// EXACT raw body, joined by "." (spec §21.5). Binding the version and id defeats cross-context replay.
func sign(secret []byte, deliveryID string, unix int64, rawBody []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signatureVersion + "." + deliveryID + "." + strconv.FormatInt(unix, 10) + "."))
	mac.Write(rawBody)
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify is the receiver-side check (spec §21.5): it recomputes the MAC over the raw body under the
// given secret, compares in constant time (hmac.Equal), and enforces the configurable timestamp
// tolerance around now — the replay window. It accepts a header that carries several v1= values, so a
// receiver still on the old secret verifies a rotation-overlap attempt. This is the exact routine the
// SDK webhook helper (spec §23.10) mirrors.
func Verify(secret []byte, deliveryID string, ts time.Time, rawBody []byte, header string, now time.Time, tolerance time.Duration) bool {
	if skew := now.Sub(ts); skew > tolerance || skew < -tolerance {
		return false // outside the replay window
	}
	want := []byte(sign(secret, deliveryID, ts.Unix(), rawBody))
	for _, field := range strings.Fields(header) {
		value, ok := strings.CutPrefix(field, signatureVersion+"=")
		if !ok {
			continue
		}
		if hmac.Equal([]byte(value), want) {
			return true
		}
	}
	return false
}
